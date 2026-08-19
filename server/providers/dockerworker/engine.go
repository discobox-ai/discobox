package dockerworker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/obot-platform/discobox/layout"
	poolagent "github.com/obot-platform/discobox/pool-agent"
	"github.com/obot-platform/discobox/pool-agent/imagereap"
	"github.com/obot-platform/discobox/pool-agent/proxyagent"
	"github.com/obot-platform/discobox/pool-agent/wire"
	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/transport"
)

const (
	// containerLogTailLines and containerLogLimit bound the diagnostic output
	// attached to a failed container: only the tail explains a startup failure.
	containerLogTailLines     = 100
	containerLogLimit         = 16 << 10
	defaultAgentPort          = 3002
	defaultDockerReadyWait    = 3 * time.Minute
	dockerReadyPollDelay      = 2 * time.Second
	noHealthWaitTimeout       = 30 * time.Second
	healthPollDelay           = 500 * time.Millisecond
	dockerSocketPath          = "/var/run/docker.sock"
	hostMountTargetRoot       = "/host"
	workerConfigLayoutVersion = 5

	LabelManaged            = "discobox.vm.managed"
	LabelProjectID          = "discobox.project_id"
	LabelPoolAgent          = "discobox.pool_agent"
	LabelPoolConfig         = "discobox.pool_agent.config_revision"
	LabelProviderInstanceID = "discobox.provider_instance_id"
	LabelPoolID             = "discobox.pool_id"
	// LabelPoolEnvelope records the pool envelope applied to the worker
	// container, so an envelope change recreates the container through the
	// normal label drift check.
	LabelPoolEnvelope = "discobox.pool_envelope"
)

// Config configures the worker runtime engine. It describes the pool-agent
// container, not the VM: VM settings belong to the Driver.
type Config struct {
	// ControlPlaneURL is the URL the in-container worker agent registers with.
	ControlPlaneURL string
	// Image is the pool-agent container image.
	Image string
	// Network is an optional additional Docker network for worker containers.
	Network string
	// AgentPort is the container port the worker agent listens on.
	AgentPort int
	// PublicAgentPort publishes the harness port on all interfaces at the fixed
	// harness port so the control plane can reach it at the VM's address. When
	// false the port is published on a loopback-only ephemeral port, for the
	// local driver.
	PublicAgentPort bool
	// AgentListenURL is the transport URL the pool-agent binds inside the
	// container. It defaults to TCP on AgentPort. When its scheme is not an IP
	// transport — vsock:// for a libkrun microVM, unix:// for a guest whose
	// helper terminates the socket — the engine creates no exposed port and no
	// Docker port binding, because there is no TCP port to publish.
	//
	// The control plane's own address needs no companion field: ControlPlaneURL
	// carries its transport in the scheme.
	AgentListenURL string
	// Privileged overrides the privileged flag, which defaults to true because
	// the worker runs systemd as PID 1.
	Privileged *bool
	// CgroupNSMode overrides the container cgroup namespace mode.
	CgroupNSMode string
	// Command overrides the container command.
	Command []string
	// DockerSocket is the Docker socket path bound into the worker container
	// so the worker agent can manage sandbox containers.
	DockerSocket string
	// RelaySocketDir is a directory on the filesystem of the Docker daemon
	// hosting this pool — for a VM backend that is the guest, not the machine
	// running the control plane — bound into the worker container at the same
	// path. A backend whose control-plane transport is terminated by a relay
	// beside the daemon puts that relay's socket here, so the agent reaches it
	// with an ordinary unix:// URL.
	//
	// Both ends therefore live inside the same guest and the mount is a plain
	// same-kernel bind, which a Unix socket requires: a socket cannot be shared
	// over the 9p/virtiofs mount that crosses the VM boundary. Only regular
	// files (the relay binary itself) travel that way.
	//
	// It is bound at the same path rather than under the host-mount root
	// because the agent only dials it; there is no daemon-side path to
	// translate. Empty for backends whose agent dials the control plane
	// directly.
	RelaySocketDir string
	// HostStateRoot is where this pool's Docker daemon sees
	// layout.ContainerRoot. Empty means the daemon sees the same paths the
	// container does, which is the case for every backend whose guest keeps
	// state at the conventional location. A backend sets it when it must place
	// state elsewhere — wslc puts it on the only disk it persists.
	//
	// It changes only which paths the agent hands the daemon. Container-side
	// paths are invariant; see the layout package.
	HostStateRoot string
	// HostMounts are additional host paths bound under the host-mount root.
	HostMounts []HostMount
	// ExtraHosts adds /etc/hosts entries, such as the Docker host gateway.
	ExtraHosts []string
	// Labels are applied to every worker container, such as the provider type.
	Labels map[string]string
	// DockerReadyTimeout bounds how long EnsureWorker waits for a freshly
	// launched VM's Docker daemon to become reachable.
	DockerReadyTimeout time.Duration
	// DevelopmentImageSync converges watcher-built images onto each destination
	// Docker daemon before the pool-agent container is reconciled.
	DevelopmentImageSync *DevelopmentImageSynchronizer `json:"-"`
	// ImageRetention overrides how long the pool's own Docker daemon keeps an
	// unused Discobox image (ADR 0040). Zero leaves the pool agent on its
	// default, which is deliberate: an unset override must serialize away here
	// so it does not change configRevision and recreate every existing pool.
	ImageRetention time.Duration `json:"imageRetention,omitempty"`
}

// Engine runs pool-agent containers over Driver-provided Docker access. It
// implements the worker provider surface consumed by the worker pool.
type Engine struct {
	driver         Driver
	cfg            Config
	configRevision string
	// imageReclaim throttles the reclamation passes that pool reconciles drive
	// (see reclaimImagesForPool).
	imageReclaim imageReclaimThrottle
}

// New creates a worker runtime engine over a VM driver.
func New(cfg Config, driver Driver) (*Engine, error) {
	if driver == nil {
		return nil, errors.New("dockerworker driver is required")
	}
	if strings.TrimSpace(cfg.Image) == "" {
		return nil, errors.New("dockerworker image is required")
	}
	// The listen URL is the single description of where the agent binds. When a
	// backend does not supply one, the agent listens on TCP so the container's
	// published port keeps working exactly as before.
	if strings.TrimSpace(cfg.AgentListenURL) == "" {
		if cfg.AgentPort == 0 {
			cfg.AgentPort = defaultAgentPort
		}
		cfg.AgentListenURL = wire.TCPListenURL(cfg.AgentPort)
	}
	if _, err := wire.Parse(cfg.AgentListenURL); err != nil {
		return nil, fmt.Errorf("dockerworker agent listen URL: %w", err)
	}
	if strings.TrimSpace(cfg.ControlPlaneURL) != "" {
		if _, err := wire.Parse(cfg.ControlPlaneURL); err != nil {
			return nil, fmt.Errorf("dockerworker control plane URL: %w", err)
		}
	}
	if cfg.DockerSocket = cleanAbsPath(cfg.DockerSocket); cfg.DockerSocket == "" {
		cfg.DockerSocket = dockerSocketPath
	}
	if len(cfg.Command) == 0 {
		cfg.Command = []string{"/usr/local/bin/discobox-pool-agent"}
	}
	cfg.CgroupNSMode = strings.TrimSpace(cfg.CgroupNSMode)
	cfg.HostMounts = NormalizeHostMounts(cfg.HostMounts)
	// Resolved here so every backend gets the same policy without repeating it
	// in five engineConfig functions. An explicit setting always wins; otherwise
	// a daemon the image watcher is driving takes the development window, and
	// everything else stays zero — which is what keeps configRevision, and
	// therefore every existing production pool, unchanged on upgrade.
	if cfg.ImageRetention == 0 {
		retention, err := imagereap.ConfiguredRetention()
		if err != nil {
			return nil, err
		}
		if retention == 0 && cfg.DevelopmentImageSync != nil {
			retention = imagereap.DevelopmentRetention
		}
		cfg.ImageRetention = retention
	}
	return &Engine{driver: driver, cfg: cfg, configRevision: configRevision(cfg)}, nil
}

func (e *Engine) Close() error {
	if e == nil {
		return nil
	}
	return e.driver.Close()
}

// Image returns the pool-agent container image the engine launches.
func (e *Engine) Image() string { return e.cfg.Image }

// HostMounts are the host paths this engine carries into its workers, cleaned
// and deduplicated as the engine will mount them. A worker sees a host
// directory only if it is one of these, so callers deciding what a sandbox can
// reach on this filesystem read them from here rather than re-deriving them
// from a provider's raw configuration.
func (e *Engine) HostMounts() []HostMount { return e.cfg.HostMounts }

// ConfigRevision identifies the desired worker container configuration. It is
// stamped as a label and compared to detect drift.
func (e *Engine) ConfigRevision() string { return e.configRevision }

func (e *Engine) privileged() bool {
	if e.cfg.Privileged != nil {
		return *e.cfg.Privileged
	}
	return true
}

// configRevision hashes every setting that shapes the worker container so
// changing any of them recreates workers.
func configRevision(cfg Config) string {
	payload := struct {
		Config             Config `json:"config"`
		MountLayoutVersion int    `json:"mountLayoutVersion"`
	}{Config: cfg, MountLayoutVersion: workerConfigLayoutVersion}
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (e *Engine) EnsurePool(ctx context.Context, _ *model.Project, provider *model.SandboxProviderInstance, pool *model.Pool, mint poolagent.MintBootstrap) error {
	vmInfo, err := e.driver.EnsureVM(ctx, pool.ID, e.vmSpec(provider, pool))
	if err != nil {
		return err
	}
	lease, err := e.acquireDockerReady(ctx, pool.ID)
	if err != nil {
		return err
	}
	defer lease.Release()
	if err := e.cfg.DevelopmentImageSync.Ensure(ctx, lease.Client); err != nil {
		return err
	}
	inst, recreated, err := e.ensurePoolContainer(ctx, lease.Client, provider, pool, mint, false)
	if err != nil {
		return err
	}
	e.reclaimImagesForPool(ctx, lease.Client, pool.ID)
	return e.recordPoolRuntime(pool, vmInfo, inst, recreated)
}

func (e *Engine) RepairPool(ctx context.Context, _ *model.Project, provider *model.SandboxProviderInstance, pool *model.Pool, mint poolagent.MintBootstrap, _ string) error {
	// Replace the VM only when it is missing or unhealthy; worker-local state
	// such as named volumes survives container replacement on a healthy VM.
	vmInfo, err := e.driver.InspectVM(ctx, pool.ID)
	if err != nil && !errors.Is(err, sandbox.ErrNotFound) {
		return err
	}
	if vmInfo == nil || vmInfo.Status != sandbox.StatusRunning {
		if err := e.driver.StopVM(ctx, pool.ID); err != nil && !errors.Is(err, sandbox.ErrNotFound) {
			return err
		}
	}
	vmInfo, err = e.driver.EnsureVM(ctx, pool.ID, e.vmSpec(provider, pool))
	if err != nil {
		return err
	}
	lease, err := e.acquireDockerReady(ctx, pool.ID)
	if err != nil {
		return err
	}
	defer lease.Release()
	if err := e.cfg.DevelopmentImageSync.Ensure(ctx, lease.Client); err != nil {
		return err
	}
	inst, _, err := e.ensurePoolContainer(ctx, lease.Client, provider, pool, mint, true)
	if err != nil {
		return err
	}
	return e.recordPoolRuntime(pool, vmInfo, inst, true)
}

func (e *Engine) RemovePool(ctx context.Context, _ *model.Project, _ *model.SandboxProviderInstance, pool *model.Pool) error {
	lease, err := e.driver.AcquireDockerClient(ctx, pool.ID)
	if err != nil {
		// Tolerate unreachable Docker only when the VM itself is gone; the
		// local driver's Docker must always be reachable.
		if _, inspectErr := e.driver.InspectVM(ctx, pool.ID); !errors.Is(inspectErr, sandbox.ErrNotFound) {
			return err
		}
	} else {
		removeErr := e.removePoolContainer(ctx, lease.Client, pool.ID)
		lease.Release()
		// A daemon that cannot be reached is not a reason to refuse a delete.
		// Nothing about retrying makes it reachable, so returning here strands
		// the pool row and its disks permanently — which is exactly what happens
		// to a VM guest that boots but never brings Docker up, the case the pool
		// host console exists for.
		//
		// Skipping the removal leaks nothing. On a VM backend, DeleteVM below
		// destroys the guest and every container in it. On the local Docker
		// driver, where DeleteVM is a no-op, the drift watcher reclaims a
		// managed pool container that no longer has a pool row.
		//
		// Only connection failures are tolerated: a daemon that answers and
		// still refuses the removal is reporting something a retry can fix, and
		// that error keeps driving the reconcile as before.
		if removeErr != nil {
			if !client.IsErrConnectionFailed(removeErr) {
				return removeErr
			}
			slog.WarnContext(ctx, "removing pool runtime without reaching its Docker daemon",
				"pool_id", pool.ID, "error", removeErr)
		}
	}
	if err := e.driver.DeleteVM(ctx, pool.ID); err != nil && !errors.Is(err, sandbox.ErrNotFound) {
		return err
	}
	pool.RuntimeState = nil
	pool.Ready = false
	pool.Schedulable = false
	pool.Degraded = false
	return nil
}

func (e *Engine) AcquirePoolAgentClient(ctx context.Context, pool *model.Pool) (*transport.HTTPClientLease, error) {
	if pool == nil || strings.TrimSpace(pool.ID) == "" {
		return nil, errors.New("pool is required")
	}
	return e.driver.AcquirePoolAgentClient(ctx, pool.ID)
}

func (e *Engine) recordPoolRuntime(pool *model.Pool, vmInfo *VMInfo, inst *container.InspectResponse, recreated bool) error {
	instanceID := ""
	if vmInfo != nil {
		instanceID = vmInfo.ID
	}
	state, err := encodeRuntimeState(RuntimeState{InstanceID: instanceID, ContainerID: inst.ID})
	if err != nil {
		return err
	}
	pool.RuntimeState = state
	if recreated {
		pool.Ready = false
		pool.Schedulable = false
		pool.Degraded = false
		pool.SetState(model.PoolStateRegistering)
	}
	return nil
}

func (e *Engine) vmSpec(provider *model.SandboxProviderInstance, pool *model.Pool) VMSpec {
	metadata := map[string]string{
		LabelPoolID:             pool.ID,
		LabelPoolAgent:          "true",
		LabelProjectID:          pool.ProjectID,
		LabelProviderInstanceID: provider.ID,
	}
	return VMSpec{Name: ContainerName(pool.ID), Metadata: metadata}
}

// acquireDockerReady acquires the worker's Docker client and waits for the
// daemon to answer pings, bounding the time a freshly booted VM gets to bring
// Docker up. Drivers do not implement readiness waiting themselves.
func (e *Engine) acquireDockerReady(ctx context.Context, poolID string) (*DockerClientLease, error) {
	timeout := e.cfg.DockerReadyTimeout
	if timeout <= 0 {
		timeout = defaultDockerReadyWait
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		lease, err := e.driver.AcquireDockerClient(ctx, poolID)
		if err == nil {
			_, pingErr := lease.Client.Ping(ctx, client.PingOptions{})
			if pingErr == nil {
				return lease, nil
			}
			lastErr = pingErr
			lease.Release()
		} else {
			lastErr = err
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("worker %s docker daemon not ready: %w", poolID, lastErr)
		}
		timer := time.NewTimer(dockerReadyPollDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// ensureWorkerContainer creates or drift-corrects the pool-agent container
// on the given Docker daemon and waits for it to be ready. It reports whether
// the container was (re)created.
//
// The bootstrap is minted lazily: the healthy-container path below returns
// without calling mint, so a steady-state drift check persists no single-use
// token. Only the create path needs credentials.
func (e *Engine) ensurePoolContainer(ctx context.Context, cli *client.Client, provider *model.SandboxProviderInstance, pool *model.Pool, mint poolagent.MintBootstrap, forceRecreate bool) (*container.InspectResponse, bool, error) {
	name := ContainerName(pool.ID)
	labels := e.containerLabels(provider, pool)
	if existing, err := cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{}); err == nil {
		if forceRecreate || shouldRemoveExistingContainer(existing.Container, e.cfg.Image, labels) {
			if _, err := cli.ContainerRemove(ctx, existing.Container.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
				return nil, false, err
			}
		} else {
			inst, err := e.waitContainerReady(ctx, cli, existing.Container.ID, false)
			return inst, false, err
		}
	} else if !cerrdefs.IsNotFound(err) {
		return nil, false, err
	}

	inst, err := e.createPoolContainer(ctx, cli, pool, name, labels, mint)
	if err != nil {
		return nil, false, err
	}
	return inst, true, nil
}

func (e *Engine) createPoolContainer(ctx context.Context, cli *client.Client, pool *model.Pool, name string, labels map[string]string, mint poolagent.MintBootstrap) (*container.InspectResponse, error) {
	if mint == nil {
		return nil, fmt.Errorf("worker bootstrap minter is required")
	}
	bootstrap, err := mint(ctx)
	if err != nil {
		return nil, err
	}
	bootstrap.ControlPlaneURL = firstNonEmpty(bootstrap.ControlPlaneURL, e.cfg.ControlPlaneURL)
	bootstrap.ProjectID = firstNonEmpty(bootstrap.ProjectID, pool.ProjectID)
	bootstrap.PoolID = firstNonEmpty(bootstrap.PoolID, pool.ID)
	bootstrap.AgentListenURL = firstNonEmpty(bootstrap.AgentListenURL, e.cfg.AgentListenURL)
	// The host-mount root now namespaces only *foreign* paths — the extra host
	// mounts an operator configures, and a developer's own source directory.
	// Discobox's own state is bind-mounted at the path the container already
	// reads, so none of it is translated. An arbitrary host path cannot be
	// mounted at its own location without risking collision with the container's
	// filesystem, which is why this prefix survives for those.
	bootstrap.HostMountPrefix = hostMountTargetRoot
	bootstrap.HostStateRoot = e.cfg.HostStateRoot

	config := &container.Config{
		Image:  e.cfg.Image,
		Labels: labels,
		Env:    envList(e.poolContainerEnv(bootstrap)),
		Cmd:    e.cfg.Command,
	}
	hostConfig := &container.HostConfig{
		Privileged: e.privileged(),
		ExtraHosts: append([]string(nil), e.cfg.ExtraHosts...),
	}
	// Only an IP listener has a port Docker can publish or curl can probe.
	listen, err := wire.Parse(e.cfg.AgentListenURL)
	if err != nil {
		return nil, fmt.Errorf("agent listen URL: %w", err)
	}
	waitForHealth := true
	if listen.IsIP() {
		exposedPort, ok := agentNetworkPort(e.cfg.AgentPort)
		if !ok {
			return nil, fmt.Errorf("invalid harness port %d", e.cfg.AgentPort)
		}
		config.ExposedPorts = network.PortSet{exposedPort: struct{}{}}
		hostConfig.PortBindings = network.PortMap{exposedPort: []network.PortBinding{e.agentPortBinding()}}
	} else {
		// The image's TCP curl healthcheck cannot probe a listener that is not
		// on TCP. Container state plus the control plane's normal agent
		// registration is the readiness signal for those transports.
		config.Healthcheck = &container.HealthConfig{Test: []string{"NONE"}}
		waitForHealth = false
	}
	// The pool envelope is the worker container limit: per-sandbox limits nest
	// inside it, so overcommit falls out of the runtime hierarchy rather than
	// scheduler arithmetic. Zero values leave the container host-sized.
	if pool != nil {
		if pool.CPUVCPUs > 0 {
			hostConfig.NanoCPUs = int64(pool.CPUVCPUs * 1_000_000_000)
		}
		if pool.MemoryBytes > 0 {
			hostConfig.Memory = pool.MemoryBytes
		}
	}
	if e.cfg.CgroupNSMode != "" {
		hostConfig.CgroupnsMode = container.CgroupnsMode(e.cfg.CgroupNSMode)
	} else {
		// systemd (PID 1 in the worker) must create its own cgroup subtree. A
		// private cgroup namespace makes Docker mount a writable cgroup2 hierarchy
		// delegated to the container; bind-mounting the host /sys/fs/cgroup instead
		// drops the container onto the read-only host cgroup root and systemd exits
		// 255 before it can even log.
		hostConfig.CgroupnsMode = container.CgroupnsMode("private")
	}
	hostConfig.Mounts = e.containerMounts(pool.ID)
	hostConfig.Tmpfs = map[string]string{"/run": "rw,noexec,nosuid,size=64m", "/run/lock": "rw,noexec,nosuid,size=64m", "/tmp": "rw,size=64m"}
	networkConfig := &network.NetworkingConfig{}
	if e.cfg.Network != "" {
		networkConfig.EndpointsConfig = map[string]*network.EndpointSettings{e.cfg.Network: {}}
	}
	// Workers run the shared proxy that their sandboxes route through. Create
	// the per-worker internal network so the worker can be aliased as the proxy
	// server name on it and sandboxes can reach only the proxy.
	if err := e.ensureSandboxNetwork(ctx, cli, pool.ID); err != nil {
		return nil, err
	}
	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{Config: config, HostConfig: hostConfig, NetworkingConfig: networkConfig, Name: name})
	if err != nil {
		return nil, err
	}
	if _, err := cli.NetworkConnect(ctx, proxyagent.SandboxNetworkName(pool.ID), client.NetworkConnectOptions{
		Container:      created.ID,
		EndpointConfig: &network.EndpointSettings{Aliases: []string{proxyagent.ServerName}},
	}); err != nil {
		return nil, fmt.Errorf("connect worker to sandbox network: %w", err)
	}
	if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return nil, err
	}
	return e.waitContainerReady(ctx, cli, created.ID, waitForHealth)
}

func (e *Engine) agentPortBinding() network.PortBinding {
	if e.cfg.PublicAgentPort {
		return network.PortBinding{HostPort: strconv.Itoa(e.cfg.AgentPort)}
	}
	return network.PortBinding{HostIP: netip.MustParseAddr("127.0.0.1")}
}

func (e *Engine) removePoolContainer(ctx context.Context, cli *client.Client, poolID string) error {
	name := ContainerName(poolID)
	if _, err := cli.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return err
	}
	// The console is engine-created but reconcile-free, so pool teardown is the
	// only thing that would ever remove it.
	if err := e.removeConsoleContainer(ctx, cli, poolID); err != nil {
		return err
	}
	return e.removeSandboxNetwork(ctx, cli, poolID)
}

// removeSandboxNetwork removes the per-pool internal network. Any container
// still attached — a sandbox that outlived the pool teardown race, or a stale
// endpoint — makes Docker refuse the removal with "network has active
// endpoints", which previously leaked the network and its scarce address block.
// Force-disconnect every endpoint first, then remove; surface a persistent
// failure so the level-triggered pool reconcile retries instead of leaking.
func (e *Engine) removeSandboxNetwork(ctx context.Context, cli *client.Client, poolID string) error {
	name := proxyagent.SandboxNetworkName(poolID)
	result, err := cli.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("inspect sandbox network %s: %w", name, err)
	}
	for containerID := range result.Network.Containers {
		_, _ = cli.NetworkDisconnect(ctx, name, client.NetworkDisconnectOptions{Container: containerID, Force: true})
	}
	if _, err := cli.NetworkRemove(ctx, name, client.NetworkRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove sandbox network %s: %w", name, err)
	}
	return nil
}

// ensureSandboxNetwork creates the per-worker internal bridge network if absent.
func (e *Engine) ensureSandboxNetwork(ctx context.Context, cli *client.Client, poolID string) error {
	name := proxyagent.SandboxNetworkName(poolID)
	if _, err := cli.NetworkInspect(ctx, name, client.NetworkInspectOptions{}); err == nil {
		return nil
	} else if !cerrdefs.IsNotFound(err) {
		return err
	}
	_, err := cli.NetworkCreate(ctx, name, client.NetworkCreateOptions{
		Driver:   "bridge",
		Internal: true,
		Labels:   map[string]string{LabelManaged: "true", LabelPoolID: poolID},
	})
	if err != nil && !cerrdefs.IsConflict(err) && !cerrdefs.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (e *Engine) containerLabels(provider *model.SandboxProviderInstance, pool *model.Pool) map[string]string {
	labels := make(map[string]string, len(e.cfg.Labels)+8)
	for key, value := range e.cfg.Labels {
		labels[key] = value
	}
	labels[LabelManaged] = "true"
	labels[LabelPoolAgent] = "true"
	labels[LabelPoolID] = pool.ID
	labels[LabelProjectID] = pool.ProjectID
	labels[LabelProviderInstanceID] = provider.ID
	labels[LabelPoolConfig] = e.configRevision
	labels[LabelPoolEnvelope] = poolEnvelopeRevision(pool)
	return labels
}

// poolEnvelopeRevision encodes the envelope values applied to the worker
// container, compared through the label drift check so envelope changes
// recreate the container.
func poolEnvelopeRevision(pool *model.Pool) string {
	return fmt.Sprintf("cpu=%.3f,mem=%d", pool.CPUVCPUs, pool.MemoryBytes)
}

// ShouldReconcileWorkerContainer reports whether a worker container drifted
// from the engine's desired image or labels. It is shared with runtime drift
// watchers.
func (e *Engine) ShouldReconcileWorkerContainer(image string, labels map[string]string) bool {
	if image != e.cfg.Image {
		return true
	}
	return labels[LabelPoolConfig] != e.configRevision
}

func shouldRemoveExistingContainer(existing container.InspectResponse, desiredImage string, desiredLabels map[string]string) bool {
	if existing.Config == nil {
		return true
	}
	if strings.TrimSpace(desiredImage) != "" && existing.Config.Image != desiredImage {
		return true
	}
	for key, value := range desiredLabels {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if existing.Config.Labels[key] != value {
			return true
		}
	}
	if existing.State != nil && !existing.State.Running {
		return true
	}
	return false
}

func (e *Engine) containerMounts(poolID string) []mount.Mount {
	mounts := []mount.Mount{{Type: mount.TypeBind, Source: e.cfg.DockerSocket, Target: dockerSocketPath}}
	// Bound at the same path rather than under the host-mount root, like the
	// Docker socket above: the agent only dials it, so there is no host path to
	// translate. The backend's guest-side relay owns the socket in this
	// directory; backends without one leave the field empty.
	if dir := cleanAbsPath(e.cfg.RelaySocketDir); dir != "" {
		mounts = append(mounts, mount.Mount{Type: mount.TypeBind, Source: dir, Target: dir})
	}
	// The state trees come from the shared layout package, so the engine, the
	// agent, and the proxy cannot drift on where anything lives.
	hostState := layout.NewHostMapping(e.cfg.HostStateRoot)
	for _, tree := range layout.MountRoots() {
		if hasHostMountSource(e.cfg.HostMounts, tree) {
			continue
		}
		mounts = append(mounts, mount.Mount{
			Type: mount.TypeBind,
			// The source is where the daemon keeps the tree; the target is the
			// container's invariant view of it. They differ only when a backend
			// relocates its state root.
			Source:      hostState.HostPath(tree),
			Target:      tree,
			BindOptions: &mount.BindOptions{CreateMountpoint: true},
		})
	}
	for _, hostMount := range e.cfg.HostMounts {
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   hostMount.Source,
			Target:   hostMountTarget(hostMount.Source),
			ReadOnly: hostMount.ReadOnly,
		})
	}
	// Do not bind-mount the host /sys/fs/cgroup here: with a private cgroup
	// namespace Docker mounts a writable cgroup2 hierarchy for the container,
	// which systemd requires. The host bind mount would shadow it with the
	// read-only host cgroup root.
	// Only the nested daemon's own storage is a named volume. Discobox state
	// is bind-mounted above, at the same path the container reads, so no
	// path translation is needed anywhere inside the pool.
	mounts = append(mounts,
		mount.Mount{Type: mount.TypeVolume, Source: poolScopedVolumeName(poolID, "docker"), Target: "/var/lib/docker"},
	)
	return mounts
}

func (e *Engine) waitContainerReady(ctx context.Context, cli *client.Client, id string, wait bool) (*container.InspectResponse, error) {
	noHealthDeadline := time.Now().Add(noHealthWaitTimeout)
	for {
		inspect, err := cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if err != nil {
			return nil, mapDockerNotFound(err)
		}
		err = containerReadyError(inspect.Container)
		if err != nil && inspect.Container.State != nil && !inspect.Container.State.Running {
			// A pool-agent container that exits reports only its status, which
			// says nothing about why. Its own output is the only explanation
			// available, and it is gone once the container is replaced.
			err = fmt.Errorf("%w%s", err, containerLogSuffix(ctx, cli, id))
		}
		if err == nil {
			if !wait || containerHasHealth(inspect.Container) || time.Now().After(noHealthDeadline) {
				return &inspect.Container, nil
			}
		} else if !wait || !containerHealthStarting(inspect.Container) {
			return &inspect.Container, err
		}

		timer := time.NewTimer(healthPollDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func containerReadyError(inspect container.InspectResponse) error {
	if inspect.State == nil {
		return fmt.Errorf("container %s has no runtime state", shortContainerID(inspect.ID))
	}
	if !inspect.State.Running {
		message := strings.TrimSpace(inspect.State.Error)
		if message == "" {
			message = fmt.Sprintf("container %s is %s", shortContainerID(inspect.ID), inspect.State.Status)
		}
		return errors.New(message)
	}
	if inspect.State.Health == nil {
		return nil
	}
	switch inspect.State.Health.Status {
	case "healthy":
		return nil
	case "starting":
		return fmt.Errorf("container %s health check is starting", shortContainerID(inspect.ID))
	case "unhealthy":
		return fmt.Errorf("container %s is unhealthy%s", shortContainerID(inspect.ID), healthLogSuffix(inspect.State.Health))
	default:
		return fmt.Errorf("container %s health check status is %q", shortContainerID(inspect.ID), inspect.State.Health.Status)
	}
}

func containerHealthStarting(inspect container.InspectResponse) bool {
	return inspect.State != nil && inspect.State.Running && inspect.State.Health != nil && inspect.State.Health.Status == "starting"
}

func containerHasHealth(inspect container.InspectResponse) bool {
	return inspect.State != nil && inspect.State.Health != nil
}

func healthLogSuffix(health *container.Health) string {
	if health == nil || len(health.Log) == 0 {
		return ""
	}
	output := strings.TrimSpace(health.Log[len(health.Log)-1].Output)
	if output == "" {
		return ""
	}
	if len(output) > 512 {
		output = output[:512] + "..."
	}
	return ": " + output
}

func shortContainerID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// AssignedAgentEndpoint resolves the published host endpoint for the worker
// harness port from a container's port map.
func AssignedAgentEndpoint(ports network.PortMap, agentPort int) (string, int) {
	port, ok := agentNetworkPort(agentPort)
	if !ok {
		return "", 0
	}
	bindings := ports[port]
	if len(bindings) == 0 {
		return "", 0
	}
	host := bindings[0].HostIP.String()
	if host == "" || host == "0.0.0.0" || host == "::" || !bindings[0].HostIP.IsValid() {
		host = "127.0.0.1"
	}
	hostPort, _ := strconv.Atoi(bindings[0].HostPort)
	return host, hostPort
}

func agentNetworkPort(agentPort int) (network.Port, bool) {
	if agentPort <= 0 || agentPort > 65535 {
		return network.Port{}, false
	}
	return network.PortFrom(uint16(agentPort), network.TCP)
}

func mapDockerNotFound(err error) error {
	if err == nil {
		return nil
	}
	if cerrdefs.IsNotFound(err) {
		return sandbox.ErrNotFound
	}
	return err
}

var invalidContainerName = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

// ContainerName is the deterministic pool-agent container name for a worker.
func ContainerName(poolID string) string {
	name := invalidContainerName.ReplaceAllString(poolID, "-")
	name = strings.Trim(name, "-_.")
	if name == "" {
		name = "vm"
	}
	return "discobox-vm-" + name
}

func poolScopedVolumeName(poolID, suffix string) string {
	return scopedVolumeName("pool", poolID, suffix)
}

func projectScopedVolumeName(projectID, suffix string) string {
	return scopedVolumeName("project", projectID, suffix)
}

func scopedVolumeName(scope, id, suffix string) string {
	name := invalidContainerName.ReplaceAllString(id, "-")
	name = strings.Trim(name, "-_.")
	if name == "" {
		name = "unknown"
	}
	return "discobox-" + scope + "-" + name + "-" + suffix
}

func envList(values map[string]string) []string {
	env := make([]string, 0, len(values))
	for key, value := range values {
		env = append(env, key+"="+value)
	}
	sort.Strings(env)
	return env
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// containerLogSuffix returns the tail of a container's output, formatted for
// appending to an error. It is best effort: a diagnostic must never replace the
// failure it is explaining.
func containerLogSuffix(ctx context.Context, cli *client.Client, id string) string {
	logs, err := cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       strconv.Itoa(containerLogTailLines),
	})
	if err != nil {
		return ""
	}
	defer func() { _ = logs.Close() }()

	var buf bytes.Buffer
	// Container output is stream-multiplexed unless the container has a TTY;
	// demultiplexing keeps the frame headers out of the message.
	if _, err := stdcopy.StdCopy(&buf, &buf, io.LimitReader(logs, containerLogLimit)); err != nil && buf.Len() == 0 {
		return ""
	}
	output := strings.TrimSpace(buf.String())
	if output == "" {
		return ""
	}
	return "\ncontainer output:\n" + output
}
