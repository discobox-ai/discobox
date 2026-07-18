package dockerworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	poolagent "github.com/obot-platform/discobox/pool-agent"
	"github.com/obot-platform/discobox/pool-agent/proxyagent"
	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/transport"
)

const (
	defaultAgentPort          = 3002
	defaultDockerReadyWait    = 3 * time.Minute
	dockerReadyPollDelay      = 2 * time.Second
	noHealthWaitTimeout       = 30 * time.Second
	healthPollDelay           = 500 * time.Millisecond
	dockerSocketPath          = "/var/run/docker.sock"
	hostMountTargetRoot       = "/host"
	workerHostSandboxRoot     = "/var/lib/discobox/projects"
	workerConfigLayoutVersion = 4
	// workerHostProxyRoot mirrors proxyagent.Root. The worker writes per-sandbox
	// proxy material here through the host-mount prefix; it must reach the host so
	// the daemon can bind-mount that material into sandbox containers.
	workerHostProxyRoot = "/var/lib/discobox/proxy"

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
	// Systemd runs the image with systemd as PID 1.
	Systemd bool
	// Privileged overrides the privileged flag; defaults to the systemd value.
	Privileged *bool
	// CgroupNSMode overrides the container cgroup namespace mode.
	CgroupNSMode string
	// Command overrides the container command.
	Command []string
	// DockerSocket is the Docker socket path bound into the worker container
	// so the worker agent can manage sandbox containers.
	DockerSocket string
	// HostMounts are additional host paths bound under the host-mount root.
	HostMounts []HostMount
	// ExtraHosts adds /etc/hosts entries, such as the Docker host gateway.
	ExtraHosts []string
	// Labels are applied to every worker container, such as the provider type.
	Labels map[string]string
	// DockerReadyTimeout bounds how long EnsureWorker waits for a freshly
	// launched VM's Docker daemon to become reachable.
	DockerReadyTimeout time.Duration
}

// Engine runs pool-agent containers over Driver-provided Docker access. It
// implements the worker provider surface consumed by the worker pool.
type Engine struct {
	driver         Driver
	cfg            Config
	configRevision string
}

// New creates a worker runtime engine over a VM driver.
func New(cfg Config, driver Driver) (*Engine, error) {
	if driver == nil {
		return nil, errors.New("dockerworker driver is required")
	}
	if strings.TrimSpace(cfg.Image) == "" {
		return nil, errors.New("dockerworker image is required")
	}
	if cfg.AgentPort == 0 {
		cfg.AgentPort = defaultAgentPort
	}
	if cfg.DockerSocket = cleanAbsPath(cfg.DockerSocket); cfg.DockerSocket == "" {
		cfg.DockerSocket = dockerSocketPath
	}
	if len(cfg.Command) == 0 && cfg.Systemd {
		cfg.Command = []string{"/usr/local/bin/discobox-pool-agent"}
	}
	cfg.CgroupNSMode = strings.TrimSpace(cfg.CgroupNSMode)
	cfg.HostMounts = NormalizeHostMounts(cfg.HostMounts)
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

// ConfigRevision identifies the desired worker container configuration. It is
// stamped as a label and compared to detect drift.
func (e *Engine) ConfigRevision() string { return e.configRevision }

func (e *Engine) privileged() bool {
	if e.cfg.Privileged != nil {
		return *e.cfg.Privileged
	}
	return e.cfg.Systemd
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
	inst, recreated, err := e.ensurePoolContainer(ctx, lease.Client, provider, pool, mint, false)
	if err != nil {
		return err
	}
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
		if err := e.driver.DeleteVM(ctx, pool.ID); err != nil && !errors.Is(err, sandbox.ErrNotFound) {
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
		if removeErr != nil {
			return removeErr
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
		pool.Phase = model.PoolPhaseRegistering
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
	if bootstrap.AgentPort == 0 {
		bootstrap.AgentPort = e.cfg.AgentPort
	}
	// The worker manages sandbox containers through the bound Docker socket, so
	// host paths it hands to the daemon must be translated through the
	// host-mount prefix.
	bootstrap.HostMountPrefix = hostMountTargetRoot

	exposedPort, ok := agentNetworkPort(e.cfg.AgentPort)
	if !ok {
		return nil, fmt.Errorf("invalid harness port %d", e.cfg.AgentPort)
	}
	config := &container.Config{
		Image:        e.cfg.Image,
		Labels:       labels,
		Env:          envList(BootEnv(bootstrap)),
		Cmd:          e.cfg.Command,
		ExposedPorts: network.PortSet{exposedPort: struct{}{}},
	}
	hostConfig := &container.HostConfig{
		Privileged:   e.privileged(),
		PortBindings: network.PortMap{exposedPort: []network.PortBinding{e.agentPortBinding()}},
		ExtraHosts:   append([]string(nil), e.cfg.ExtraHosts...),
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
	} else if e.cfg.Systemd {
		// systemd (PID 1 in the worker) must create its own cgroup subtree. A
		// private cgroup namespace makes Docker mount a writable cgroup2 hierarchy
		// delegated to the container; bind-mounting the host /sys/fs/cgroup instead
		// drops the container onto the read-only host cgroup root and systemd exits
		// 255 before it can even log.
		hostConfig.CgroupnsMode = container.CgroupnsMode("private")
	}
	hostConfig.Mounts = e.containerMounts(pool.ID, pool.ProjectID)
	if e.cfg.Systemd {
		hostConfig.Tmpfs = map[string]string{"/run": "rw,noexec,nosuid,size=64m", "/run/lock": "rw,noexec,nosuid,size=64m", "/tmp": "rw,size=64m"}
	}
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
	return e.waitContainerReady(ctx, cli, created.ID, true)
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
	// Best-effort remove the per-pool sandbox network once the pool host (and
	// its sandboxes) are gone. It fails harmlessly if sandboxes are still
	// attached.
	_, _ = cli.NetworkRemove(ctx, proxyagent.SandboxNetworkName(poolID), client.NetworkRemoveOptions{})
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

func (e *Engine) containerMounts(poolID, projectID string) []mount.Mount {
	mounts := []mount.Mount{{Type: mount.TypeBind, Source: e.cfg.DockerSocket, Target: dockerSocketPath}}
	if !hasHostMountSource(e.cfg.HostMounts, workerHostSandboxRoot) {
		mounts = append(mounts, mount.Mount{
			Type:        mount.TypeBind,
			Source:      workerHostSandboxRoot,
			Target:      hostMountTarget(workerHostSandboxRoot),
			BindOptions: &mount.BindOptions{CreateMountpoint: true},
		})
	}
	if !hasHostMountSource(e.cfg.HostMounts, workerHostProxyRoot) {
		mounts = append(mounts, mount.Mount{
			Type:        mount.TypeBind,
			Source:      workerHostProxyRoot,
			Target:      hostMountTarget(workerHostProxyRoot),
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
	if e.cfg.Systemd {
		// Do not bind-mount the host /sys/fs/cgroup here: with a private cgroup
		// namespace Docker mounts a writable cgroup2 hierarchy for the container,
		// which systemd requires. The host bind mount would shadow it with the
		// read-only host cgroup root.
		mounts = append(mounts,
			mount.Mount{Type: mount.TypeVolume, Source: poolScopedVolumeName(poolID, "docker"), Target: "/var/lib/docker"},
			mount.Mount{Type: mount.TypeVolume, Source: projectScopedVolumeName(projectID, "discobox"), Target: "/var/lib/discobox"},
		)
	}
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
