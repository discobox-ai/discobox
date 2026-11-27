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
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/pooloutbound"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/pkg/imagecache"
	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pkg/wire"
	poolagent "github.com/discobox-ai/discobox/pool-agent"
	"github.com/discobox-ai/discobox/pool-agent/bundle"
	"github.com/discobox-ai/discobox/pool-agent/imagereap"
	"github.com/discobox-ai/discobox/pool-agent/provisioner"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/transport"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
)

const (
	// containerLogTailLines and containerLogLimit bound the diagnostic output
	// attached to a failed container: only the tail explains a startup failure.
	containerLogTailLines  = 100
	containerLogLimit      = 16 << 10
	defaultAgentPort       = 3002
	defaultDockerReadyWait = 3 * time.Minute
	dockerReadyPollDelay   = 2 * time.Second
	// dockerAnswerWait bounds repair's check that a running VM's Docker daemon
	// still answers. It is not a boot wait — a VM that booted has had
	// defaultDockerReadyWait by the time anything repairs it — only long enough
	// that a daemon restarting in place is not taken for a dead guest.
	dockerAnswerWait    = 15 * time.Second
	noHealthWaitTimeout = 30 * time.Second
	healthPollDelay     = 500 * time.Millisecond
	dockerSocketPath    = "/var/run/docker.sock"
	hostMountTargetRoot = "/host"
	// workerConfigLayoutVersion is part of every pool's config revision, so a
	// change to the pool container's mounts bumps it and drift recreates each
	// pool on the new layout. 7: no /var/lib/docker volume.
	workerConfigLayoutVersion = 7
	// dockerSocketUnitPath is where the pool's container masks the sandbox
	// base image's docker.socket: that unit would listen at the path the
	// host's socket is bound to, fail, and leave the container degraded.
	dockerSocketUnitPath = "/etc/systemd/system/docker.socket"

	LabelManaged            = "discobox.vm.managed"
	LabelProjectID          = "discobox.project_id"
	LabelPoolAgent          = "discobox.pool_agent"
	LabelPoolConfig         = "discobox.pool_agent.config_revision"
	LabelProviderInstanceID = "discobox.provider_instance_id"
	LabelPoolID             = "discobox.pool_id"
)

// Config configures the pool runtime engine. It describes the pool-agent
// container, not the VM: VM settings belong to the Driver.
type Config struct {
	// ControlPlaneURL is the URL the pool agent reaches the control plane at,
	// from inside its container.
	ControlPlaneURL string
	// SandboxBaseImage is the sandbox base image (ADR 26-10-16-423 §10): what
	// every sandbox delivered its platform runs, which the pool agent is told
	// in its environment (sandboxruntime.EnvSandboxBaseImage), and what the
	// pool's own container runs, provisioned as a pool's box (ADR 26-10-24-953
	// §2). It shapes the pool's container, so it is in configRevision: a pool
	// runs the base image its server names.
	SandboxBaseImage string `json:"sandboxBaseImage,omitempty"`
	// Network is an optional additional Docker network for pool-agent containers.
	Network string
	// AgentPort is the container port the pool agent listens on.
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
	// the pool's container runs systemd as PID 1.
	Privileged *bool
	// CgroupNSMode overrides the container cgroup namespace mode.
	CgroupNSMode string
	// DockerSocket is the Docker socket path bound into the pool-agent
	// container so the pool agent can manage sandbox containers.
	DockerSocket string
	// RelaySocketDir is a directory on the filesystem of the Docker daemon
	// hosting this pool — for a VM backend that is the guest, not the machine
	// running the control plane — bound into the pool-agent container at the same
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
	// DedicatedDaemon says the Docker daemon hosting each pool is that pool's
	// own, in its own VM, with its data on the same disk as the pool's state:
	// every image and container on it is there for the pool, and they are
	// counted against that disk (ADR 26-11-14-896 §4). False is a daemon
	// shared with other pools or the host, or one the engine cannot vouch
	// for.
	DedicatedDaemon bool
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
	// Labels are applied to every pool-agent container, such as the provider type.
	Labels map[string]string
	// DockerReadyTimeout bounds how long acquireDockerReady waits for a freshly
	// launched VM's Docker daemon to become reachable.
	DockerReadyTimeout time.Duration
	// ImageCache is the image store on this machine, which an image absent from
	// the pool's daemon is loaded from before it is pulled (ADR 0113) — but
	// only into a daemon the lease reaching it says is on this machine, which
	// is a property of the driver and often of its configuration rather than of
	// the provider. Nil loads nothing.
	ImageCache *imagecache.Layout
	// ProgressReporter records what bringing this pool host up is doing right
	// now, for a client whose sandbox is waiting for a pool to take it. Nil is
	// a driver that says nothing.
	ProgressReporter sandbox.PoolProgressReporter `json:"-"`

	// DevelopmentImageSync converges watcher-built images onto each destination
	// Docker daemon before the pool-agent container is reconciled.
	DevelopmentImageSync *DevelopmentImageSynchronizer `json:"-"`
	// ImageRetention overrides how long the pool's own Docker daemon keeps an
	// unused Discobox image (ADR 0040). Zero leaves the pool agent on its
	// default, which is deliberate: an unset override must serialize away here
	// so it does not change configRevision and recreate every existing pool.
	ImageRetention time.Duration `json:"imageRetention,omitempty"`
	// ProxyAuditRetention overrides how long the pool proxy keeps an audit row
	// and the recorded body or upgraded stream it names. Zero leaves the pool
	// proxy on its own default, and must serialize away here for the same
	// reason ImageRetention does: materializing a default would change
	// configRevision and recreate every existing pool.
	ProxyAuditRetention time.Duration `json:"proxyAuditRetention,omitempty"`
	// AuditSpoolBudget overrides the pool proxy's audit spool budget (ADR
	// 26-10-08-698). Its unset fields serialize away for the reason
	// ImageRetention does.
	proxyagent.AuditSpoolBudget
	// SandboxIdleTimeout overrides how long a sandbox on this pool runs with
	// nothing happening in it before it powers itself off (ADR 0108). Zero
	// leaves every sandbox-agent on its own default, and serializes away for
	// the reason ImageRetention does.
	SandboxIdleTimeout time.Duration `json:"sandboxIdleTimeout,omitempty"`
}

// Engine runs pool-agent containers over Driver-provided Docker access. It
// is the pool runtime a Docker-backed provider hands to poolruntime.New.
type Engine struct {
	driver         Driver
	cfg            Config
	configRevision string
	// imageReclaim throttles the reclamation passes that pool reconciles drive
	// (see reclaimImagesForPool).
	imageReclaim imageReclaimThrottle
}

// New creates a pool runtime engine over a VM driver.
func New(cfg Config, driver Driver) (*Engine, error) {
	if driver == nil {
		return nil, errors.New("dockerworker driver is required")
	}
	if strings.TrimSpace(cfg.SandboxBaseImage) == "" {
		return nil, errors.New("dockerworker sandbox base image is required: a pool's container runs it")
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
	cfg.CgroupNSMode = strings.TrimSpace(cfg.CgroupNSMode)
	cfg.HostMounts = NormalizeHostMounts(cfg.HostMounts)
	// Resolved here so every backend gets the same policy without repeating it
	// in five engineConfig functions. An explicit setting always wins — the
	// provider's own, or the server's, both of which have already been applied
	// to cfg by the time this runs — otherwise a daemon the image watcher is
	// driving takes the development window, and everything else stays zero,
	// which is what keeps configRevision, and therefore every existing
	// production pool, unchanged on upgrade.
	if cfg.ImageRetention == 0 && cfg.DevelopmentImageSync != nil {
		cfg.ImageRetention = imagereap.DevelopmentRetention
	}
	return &Engine{driver: driver, cfg: cfg, configRevision: configRevision(cfg)}, nil
}

// Check is the driver's check: what the pool's VM needs before it is made.
func (e *Engine) Check(ctx context.Context) error {
	return e.driver.Check(ctx)
}

func (e *Engine) Close() error {
	if e == nil {
		return nil
	}
	return e.driver.Close()
}

// Image returns the image the engine launches a pool's container from: the
// sandbox base image, which the pool is provisioned onto.
func (e *Engine) Image() string { return e.cfg.SandboxBaseImage }

// ConfigRevision identifies the desired pool-agent container configuration. It is
// stamped as a label and compared to detect drift.
func (e *Engine) ConfigRevision() string { return e.configRevision }

func (e *Engine) privileged() bool {
	if e.cfg.Privileged != nil {
		return *e.cfg.Privileged
	}
	return true
}

// configRevision hashes every setting that shapes the pool-agent container so
// changing any of them recreates it.
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

func (e *Engine) EnsurePool(ctx context.Context, _ *model.Project, provider *model.SandboxProviderInstance, pool *model.Pool, maker poolruntime.PoolMaker, begin func(context.Context) error) (resultErr error) {
	defer func() { resultErr = pool.Outbound.RedactError(resultErr) }()
	previousOutboundRevision := pool.OutboundAppliedRevision
	steps := poolruntime.NewHostSteps(pool.ID)
	defer steps.Finish(ctx)
	// The phases of bringing a host up, in the order they happen, for a client
	// whose sandbox is waiting for a pool to take it. On a VM backend starting
	// the machine includes fetching the disk image the first time, which on a
	// cold Mac is minutes; a driver that can say more about that says it from
	// inside EnsureVM.
	vmCtx, releaseVM := e.cfg.ProgressReporter.Hold(ctx, pool.ID, sandbox.PoolPhaseStartingVM)
	vmInfo, err := e.driver.EnsureVM(vmCtx, pool.ID, e.vmSpec(provider, pool))
	releaseVM()
	if err != nil {
		return err
	}
	if vmInfo.Started {
		steps.BringingUp(ctx)
	}
	steps.Step(ctx, "the pool's VM is running")
	dockerCtx, releaseDocker := e.cfg.ProgressReporter.Hold(ctx, pool.ID, sandbox.PoolPhaseWaitingForDocker)
	lease, waited, err := e.acquireDockerReady(dockerCtx, pool.ID, e.dockerReadyTimeout())
	releaseDocker()
	if err != nil {
		return err
	}
	defer lease.Release()
	// A driver that cannot say it started the VM still shows it here: a
	// daemon that did not answer at once is one coming up.
	if waited {
		steps.BringingUp(ctx)
	}
	steps.Step(ctx, "the pool's Docker daemon is ready")
	delivery, err := e.driver.PoolOutbound(ctx, pool.ID, pool.Outbound)
	if err != nil {
		return err
	}
	if err := e.prepareHostOutbound(ctx, lease.Client, pool.ID, &delivery); err != nil {
		return err
	}
	pool.RuntimeOutbound = delivery.Runtime.WithInternalHosts(e.cfg.ControlPlaneURL)
	if delivery.GuestProbes != nil {
		observations := map[string]pooloutbound.Probes{}
		_ = json.Unmarshal(pool.OutboundProbes, &observations)
		observations["guest"] = *delivery.GuestProbes
		encoded, marshalErr := json.Marshal(observations)
		if marshalErr != nil {
			return marshalErr
		}
		pool.OutboundProbes = encoded
	}
	if delivery.RestartRequired && pool.RegisteredAt == nil {
		return fmt.Errorf("%w: pool boot configuration has not applied; waiting for machine initialization or its next restart", sandbox.ErrPoolConfigApply)
	}
	pool.OutboundRestartRequired = delivery.RestartRequired
	if delivery.RestartRequired {
		pool.OutboundAppliedRevision = delivery.AppliedRevision
	}
	err = e.cfg.DevelopmentImageSync.Ensure(ctx, lease.Client, e.cfg.ProgressReporter, pool.ID)
	if err != nil {
		return err
	}
	inst, box, started, err := e.ensurePoolContainer(ctx, lease, provider, pool, maker, false, begin, steps)
	if err != nil {
		return err
	}
	e.reclaimImagesForPool(ctx, lease.Client, pool.ID)
	observePoolOutbound(ctx, lease.Client, inst.ID, pool, previousOutboundRevision)
	return e.recordPoolRuntime(pool, vmInfo, inst, box, started)
}

func (e *Engine) RepairPool(ctx context.Context, _ *model.Project, provider *model.SandboxProviderInstance, pool *model.Pool, maker poolruntime.PoolMaker, _ string, begin func(context.Context) error) (resultErr error) {
	defer func() { resultErr = pool.Outbound.RedactError(resultErr) }()
	previousOutboundRevision := pool.OutboundAppliedRevision
	// Replace the VM only when it is missing or unhealthy, which includes a VM
	// hosted here whose Docker daemon does not answer; pool-local state in the
	// bind-mounted state trees survives container replacement on a healthy VM.
	vmInfo, err := e.driver.InspectVM(ctx, pool.ID)
	if err != nil && !errors.Is(err, sandbox.ErrNotFound) {
		return err
	}
	replaceVM := vmInfo == nil || vmInfo.Status != sandbox.StatusRunning
	if !replaceVM && vmInfo.HostedHere {
		// Running is the hypervisor's word, and it says nothing about the guest.
		// A guest whose storage never mounted runs without Docker, and one wedged
		// in its kernel runs without anything; both report running for as long as
		// the VM exists. Nothing but repair ever restarts one, so repair that
		// trusted the state would retry a guest that cannot come back forever.
		//
		// Only a VM hosted here is judged this way: a daemon behind a network
		// can be silent because of the network, and restarting the VM for that
		// would kill a healthy pool on every retry.
		lease, _, answerErr := e.acquireDockerReady(ctx, pool.ID, dockerAnswerWait)
		if err := ctx.Err(); err != nil {
			return err
		}
		if answerErr != nil {
			slog.WarnContext(ctx, "restarting pool VM whose Docker daemon does not answer",
				"pool_id", pool.ID, "error", answerErr)
			replaceVM = true
		} else {
			lease.Release()
		}
	}
	// A repair replaces the pool's box, and its VM too when replaceVM: every
	// step of it is bringing the host up.
	steps := poolruntime.NewHostSteps(pool.ID)
	steps.BringingUp(ctx)
	if replaceVM {
		if err := e.driver.StopVM(ctx, pool.ID); err != nil && !errors.Is(err, sandbox.ErrNotFound) {
			return err
		}
		steps.Step(ctx, "stopped the pool's VM to replace it")
	}
	vmCtx, releaseVM := e.cfg.ProgressReporter.Hold(ctx, pool.ID, sandbox.PoolPhaseStartingVM)
	vmInfo, err = e.driver.EnsureVM(vmCtx, pool.ID, e.vmSpec(provider, pool))
	releaseVM()
	if err != nil {
		return err
	}
	steps.Step(ctx, "the pool's VM is running")
	dockerCtx, releaseDocker := e.cfg.ProgressReporter.Hold(ctx, pool.ID, sandbox.PoolPhaseWaitingForDocker)
	lease, _, err := e.acquireDockerReady(dockerCtx, pool.ID, e.dockerReadyTimeout())
	releaseDocker()
	if err != nil {
		return err
	}
	defer lease.Release()
	steps.Step(ctx, "the pool's Docker daemon is ready")
	delivery, err := e.driver.PoolOutbound(ctx, pool.ID, pool.Outbound)
	if err != nil {
		return err
	}
	if err := e.prepareHostOutbound(ctx, lease.Client, pool.ID, &delivery); err != nil {
		return err
	}
	pool.RuntimeOutbound = delivery.Runtime.WithInternalHosts(e.cfg.ControlPlaneURL)
	if delivery.GuestProbes != nil {
		observations := map[string]pooloutbound.Probes{}
		_ = json.Unmarshal(pool.OutboundProbes, &observations)
		observations["guest"] = *delivery.GuestProbes
		encoded, marshalErr := json.Marshal(observations)
		if marshalErr != nil {
			return marshalErr
		}
		pool.OutboundProbes = encoded
	}
	if delivery.RestartRequired && pool.RegisteredAt == nil {
		return fmt.Errorf("%w: pool boot configuration has not applied; waiting for machine initialization or its next restart", sandbox.ErrPoolConfigApply)
	}
	pool.OutboundRestartRequired = delivery.RestartRequired
	if delivery.RestartRequired {
		pool.OutboundAppliedRevision = delivery.AppliedRevision
	}
	err = e.cfg.DevelopmentImageSync.Ensure(ctx, lease.Client, e.cfg.ProgressReporter, pool.ID)
	if err != nil {
		return err
	}
	inst, box, _, err := e.ensurePoolContainer(ctx, lease, provider, pool, maker, true, begin, steps)
	if err != nil {
		return err
	}
	observePoolOutbound(ctx, lease.Client, inst.ID, pool, previousOutboundRevision)
	return e.recordPoolRuntime(pool, vmInfo, inst, box, true)
}

func (e *Engine) RemovePool(ctx context.Context, _ *model.Project, _ *model.SandboxProviderInstance, pool *model.Pool) error {
	lease, err := e.driver.AcquireDockerClient(ctx, pool.ID)
	if err != nil {
		// Tolerate unreachable Docker only when the VM is not running — gone,
		// or stopped with its disks, whose containers DeleteVM destroys with
		// it. The local driver's VM is always running, so its Docker must
		// always be reachable.
		vm, inspectErr := e.driver.InspectVM(ctx, pool.ID)
		if inspectErr == nil && vm.Status == sandbox.StatusRunning || inspectErr != nil && !errors.Is(inspectErr, sandbox.ErrNotFound) {
			return err
		}
	} else {
		removeErr := e.removePoolHost(ctx, lease.Client, pool)
		lease.Release()
		// A daemon that cannot be reached is not a reason to refuse a delete.
		// Nothing about retrying makes it reachable, so returning here strands
		// the pool row and its disks permanently — which is exactly what happens
		// to a VM guest that boots but never brings Docker up, the case the pool
		// host console exists for.
		//
		// On a VM backend the skip leaks nothing: DeleteVM below destroys the
		// guest with every container, volume and state tree in it. On a daemon
		// shared with other pools, where DeleteVM is a no-op, it leaves the
		// pool's state trees, and an older pool's legacy volume, behind, which
		// the warning names for an operator to remove.
		//
		// Only connection failures are tolerated: a daemon that answers and
		// still refuses the removal is reporting something a retry can fix, and
		// that error keeps driving the reconcile as before.
		if removeErr != nil {
			if !client.IsErrConnectionFailed(removeErr) {
				return removeErr
			}
			args := []any{"pool_id", pool.ID, "error", removeErr}
			if !e.cfg.DedicatedDaemon {
				args = append(args, "volume", legacyPoolVolumeName(pool.ID),
					"trees", e.daemonPaths(layout.Container().PoolTrees(pool.ProjectID, pool.ID)))
			}
			slog.WarnContext(ctx, "removing pool runtime without reaching its Docker daemon", args...)
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

// AcquirePoolAgentClient reaches the agent of a pool the control plane holds a
// row for. A driver that finds no host for that pool (no VM, no pool
// container) answers sandbox.ErrNotFound, and that is not this call's answer:
// the pool reconciler brings the host back, as a server upgrade's replacement
// of every pool does, so the agent is not reachable yet. Passed on as
// ErrNotFound it read as the sandbox having no runtime on its pool, which sent
// the first start after an upgrade back as "repair it" (#893), and let a
// delete, archive or policy push take a missing host for a sandbox with
// nothing to undo.
func (e *Engine) AcquirePoolAgentClient(ctx context.Context, pool *model.Pool) (*transport.HTTPClientLease, error) {
	if pool == nil || strings.TrimSpace(pool.ID) == "" {
		return nil, errors.New("pool is required")
	}
	lease, err := e.driver.AcquirePoolAgentClient(ctx, pool.ID)
	if errors.Is(err, sandbox.ErrNotFound) {
		// The driver's error is kept as text, not wrapped: still matching
		// ErrNotFound, it would read as the sandbox having no runtime again.
		return nil, fmt.Errorf("%w: pool %s has no host running: %s", sandbox.ErrPoolNotReachable, pool.ID, err.Error())
	}
	return lease, err
}

func (e *Engine) recordPoolRuntime(pool *model.Pool, vmInfo *VMInfo, inst *container.InspectResponse, box poolruntime.BoxRecord, started bool) error {
	instanceID := ""
	if vmInfo != nil {
		instanceID = vmInfo.ID
	}
	state, err := encodeRuntimeState(RuntimeState{InstanceID: instanceID, ContainerID: inst.ID, Box: box})
	if err != nil {
		return err
	}
	pool.RuntimeState = state
	if started {
		markPoolStarted(pool)
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
	return VMSpec{
		Outbound:    pool.Outbound,
		Name:        ContainerName(pool.ID),
		Metadata:    metadata,
		CPUVCPUs:    pool.CPUVCPUs,
		MemoryBytes: pool.MemoryBytes,
	}
}

// dockerReadyTimeout is how long a freshly booted VM gets to bring Docker up.
func (e *Engine) dockerReadyTimeout() time.Duration {
	if e.cfg.DockerReadyTimeout > 0 {
		return e.cfg.DockerReadyTimeout
	}
	return defaultDockerReadyWait
}

// acquireDockerReady acquires the pool's Docker client and waits up to timeout
// for the daemon to answer pings, and says whether it had to: a daemon that
// does not answer the first ping is one coming up. Drivers do not implement
// readiness waiting themselves.
//
// The timeout bounds every attempt, not only the polling between them: a guest
// wedged in its kernel can accept a connection and never answer it, and a
// caller's context (the reconcile engine's lifetime) need not end at all. An
// attempt the deadline cuts off is a daemon not ready, reported as such.
func (e *Engine) acquireDockerReady(ctx context.Context, poolID string, timeout time.Duration) (*DockerClientLease, bool, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		lease, err := e.pingDocker(ctx, poolID, deadline)
		if err == nil {
			return lease, lastErr != nil, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, false, ctxErr
		}
		lastErr = err
		// No attempt starts with no time left: it could only fail on the
		// deadline, and a daemon refusing connections would then be reported
		// by the deadline rather than by its own error.
		wait := min(dockerReadyPollDelay, time.Until(deadline))
		if wait <= 0 {
			return nil, false, fmt.Errorf("pool %s docker daemon not ready: %w", poolID, lastErr)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, false, ctx.Err()
		case <-timer.C:
		}
		if !time.Now().Before(deadline) {
			return nil, false, fmt.Errorf("pool %s docker daemon not ready: %w", poolID, lastErr)
		}
	}
}

// pingDocker makes one readiness attempt that gives up at deadline. The lease
// outlives the attempt's context, as Driver.AcquireDockerClient promises.
func (e *Engine) pingDocker(ctx context.Context, poolID string, deadline time.Time) (*DockerClientLease, error) {
	attemptCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	lease, err := e.driver.AcquireDockerClient(attemptCtx, poolID)
	if err != nil {
		return nil, err
	}
	if _, err := lease.Client.Ping(attemptCtx, client.PingOptions{}); err != nil {
		lease.Release()
		return nil, err
	}
	return lease, nil
}

// ensurePoolContainer creates, starts or drift-corrects the pool's container
// on the given Docker daemon, provisions it as a pool's box when it is new or
// its bundles have moved since (ADR 26-10-24-953 §§2, 5), and waits for it to
// be ready. A stopped container that has not drifted is started in place
// rather than made again (ADR 26-11-17-503). It reports what it placed, and
// whether the box was made or started, so that its agent has not reported
// since, and logs each step to steps.
//
// A box is enrolled each time it is provisioned: its key is made in it on
// first ask, and the same one answered after, so enrolling again changes
// nothing and mends a server that lost the key.
func (e *Engine) ensurePoolContainer(ctx context.Context, lease *DockerClientLease, provider *model.SandboxProviderInstance, pool *model.Pool, maker poolruntime.PoolMaker, forceRecreate bool, begin func(context.Context) error, steps *poolruntime.HostSteps) (*container.InspectResponse, poolruntime.BoxRecord, bool, error) {
	cli := lease.Client
	name := ContainerName(pool.ID)
	labels := e.containerLabels(provider, pool)
	recorded := recordedBox(pool)

	existing, existingErr := cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if existingErr != nil && !cerrdefs.IsNotFound(existingErr) {
		return nil, recorded, false, existingErr
	}
	if existingErr != nil {
		steps.BringingUp(ctx)
		if err := begin(ctx); err != nil {
			return nil, recorded, false, err
		}
	}
	image, err := e.resolvePoolImage(ctx, lease, pool.ID, existing.Container)
	if err != nil {
		return nil, recorded, false, fmt.Errorf("hosting Docker daemon cannot fetch the pool base image; configure proxy and trust on that daemon before creating a pool: %w", err)
	}
	steps.Step(ctx, "the pool's image is ready", "image", image)

	if existingErr == nil {
		// Against the resolved image, not the configured one: a container
		// running a fallback is corrected the moment the configured image can
		// be pulled, and left alone until then. Comparing against the
		// configured image would remove and recreate it on every reconcile for
		// as long as the registry stayed unreachable.
		rebuild := forceRecreate || shouldRemoveExistingContainer(existing.Container, image, labels)
		started := false
		if !rebuild && !containerRunning(existing.Container) {
			// A stopped container that has not drifted boots from its own disk
			// rather than being made and provisioned again (ADR 26-11-17-503).
			if why := stoppedContainerRebuildReason(existing.Container, pool); why != "" {
				slog.InfoContext(ctx, "rebuilding the pool's stopped container", "pool", pool.ID, "container", shortContainerID(existing.Container.ID), "reason", why)
				rebuild = true
			} else if err := e.startPoolContainer(ctx, cli, existing.Container, pool, begin, steps); err != nil {
				var failed *poolStartError
				if !errors.As(err, &failed) {
					return nil, recorded, false, err
				}
				slog.WarnContext(ctx, "the pool's stopped container did not start; rebuilding it", "pool", pool.ID, "container", shortContainerID(existing.Container.ID), "error", failed.err)
				rebuild = true
			} else {
				started = true
			}
		}
		if rebuild {
			// What the new box is made of is resolved before the old one is
			// removed: a release that cannot be resolved right now leaves a
			// working pool running, rather than taking it down and then
			// failing to make its replacement.
			bootstrap, box, err := e.poolBox(ctx, cli, pool, maker)
			if err != nil {
				return nil, recorded, false, err
			}
			steps.BringingUp(ctx)
			if err := begin(ctx); err != nil {
				return nil, recorded, false, err
			}
			if _, err := cli.ContainerRemove(ctx, existing.Container.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
				return nil, recorded, false, err
			}
			steps.Step(ctx, "removed the pool's container to replace it", "container", shortContainerID(existing.Container.ID))
			inst, record, err := e.createPoolContainer(ctx, cli, pool, name, labels, maker, image, bootstrap, box, steps)
			if err != nil {
				return nil, recorded, false, err
			}
			return inst, record, true, nil
		}
		if !started {
			steps.Step(ctx, "kept the pool's container", "container", shortContainerID(existing.Container.ID))
		}
		// A started box is a running one from here: only what moved since it
		// was provisioned is applied (ADR 26-11-17-503 §2.6).
		recorded, err = e.applyMovedPoolBundles(ctx, cli, existing.Container.ID, pool, maker, recorded, begin, steps)
		if err != nil {
			return nil, recorded, false, err
		}
		inst, err := e.waitContainerReady(ctx, cli, existing.Container.ID, false)
		if err == nil {
			steps.Step(ctx, "the pool's container is healthy")
		} else if inst != nil && containerHealthStarting(*inst) {
			steps.Step(ctx, "the pool's container is still starting")
		}
		if err != nil && inst != nil && containerHealthStarting(*inst) {
			// A container whose healthcheck has not passed yet is a host
			// still coming up, not a broken one, and it is reported as
			// such: a failed reconcile of a pool with sandboxes assigned
			// repairs it, which removes and recreates this container and
			// restarts the very healthcheck being waited on. A pool
			// container restarts for ordinary reasons — a Docker restart,
			// a development image rebuild — and something is usually
			// waiting on it when it does.
			return inst, recorded, started, fmt.Errorf("%w: %w", sandbox.ErrPoolNotReachable, err)
		}
		return inst, recorded, started, err
	}

	bootstrap, box, err := e.poolBox(ctx, cli, pool, maker)
	if err != nil {
		return nil, recorded, false, err
	}
	inst, record, err := e.createPoolContainer(ctx, cli, pool, name, labels, maker, image, bootstrap, box, steps)
	if err != nil {
		return nil, recorded, false, err
	}
	return inst, record, true, nil
}

// poolStartError is a stopped pool container's start that failed in a way a
// rebuild mends: Docker refused the start, or the container did not come up
// with its agent (ADR 26-11-17-503 §4).
type poolStartError struct{ err error }

func (e *poolStartError) Error() string { return e.err.Error() }
func (e *poolStartError) Unwrap() error { return e.err }

// stoppedContainerRebuildReason says why a stopped pool container that has
// not drifted is rebuilt rather than started, or nothing when it is started
// (ADR 26-11-17-503 §1): it must be the container the pool's runtime state
// names, with a box recorded as provisioned in it, and stopped, not dead.
func stoppedContainerRebuildReason(existing container.InspectResponse, pool *model.Pool) string {
	state, err := DecodeRuntimeState(pool.RuntimeState)
	switch {
	case err != nil || state.ContainerID != existing.ID:
		return "it is not the container the pool's runtime state names"
	case state.Box.Agent == "":
		return "no box is recorded as provisioned in it"
	case existing.State == nil:
		return "it has no runtime state"
	}
	switch existing.State.Status {
	case container.StateExited, container.StateCreated:
		return ""
	default:
		return fmt.Sprintf("it is %s", existing.State.Status)
	}
}

// startPoolContainer starts a pool's stopped container in place, and waits for
// its agent: its box boots what it last provisioned from its own disk, and
// systemd starts the agent (ADR 26-11-17-503 §2). Placement is closed and the
// pool marked started first, and the box's start is recorded just before
// Docker starts it.
//
// A start that fails in a way a rebuild mends is a *poolStartError; the
// caller rebuilds the container in the same pass. A healthcheck still starting
// when the wait runs out is sandbox.ErrPoolNotReachable. Any other error is
// begin's or the pass's own context's.
func (e *Engine) startPoolContainer(ctx context.Context, cli *client.Client, existing container.InspectResponse, pool *model.Pool, begin func(context.Context) error, steps *poolruntime.HostSteps) error {
	steps.BringingUp(ctx)
	// begin closes placement: its write of the pool row stores the
	// registering state, which is what refuses work from here.
	if err := begin(ctx); err != nil {
		return err
	}
	markPoolStarted(pool)
	startCtx, releaseStart := e.cfg.ProgressReporter.Hold(ctx, pool.ID, sandbox.PoolPhaseStartingPoolAgent)
	// No agent runs in a stopped container, so the pool's agent counts as
	// started from just before it starts (ADR 26-11-15-294 §1).
	sandbox.ReportAgentStarted(startCtx, time.Now(), sandbox.AgentStartBox)
	_, err := cli.ContainerStart(startCtx, existing.ID, client.ContainerStartOptions{})
	releaseStart()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return &poolStartError{err: err}
	}
	steps.Step(ctx, "started the pool's stopped container in place", "container", shortContainerID(existing.ID))
	agentCtx, releaseAgent := e.cfg.ProgressReporter.Hold(ctx, pool.ID, sandbox.PoolPhaseWaitingForPoolAgent)
	defer releaseAgent()
	// Every attempt of the wait is bounded, not only the pauses between
	// them: the pass's context never ends, and a guest whose Docker accepts
	// a connection and never answers would hold it for ever.
	waitCtx, cancel := context.WithTimeout(agentCtx, agentStartTimeout)
	defer cancel()
	if declaresHealthcheck(existing) {
		// The healthcheck the container was created with probes the agent
		// itself, and its start period and retries turn a box whose agent
		// never answers unhealthy well within the bound; the wait returns
		// once it is no longer starting.
		_, err = e.waitContainerReady(waitCtx, cli, existing.ID, true)
	} else {
		// With no healthcheck (an agent listening on no IP: VSOCK on libkrun
		// and vz), a running container says only that systemd is up.
		err = e.waitPoolAgentActive(waitCtx, cli, existing.ID)
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if waitCtx.Err() != nil {
			// A healthcheck still starting at the bound is a host on its way
			// up, as for a running container, and the pass is asked again
			// rather than rebuilding it (ADR 26-11-17-503 §4).
			lookCtx, cancelLook := context.WithTimeout(ctx, startedContainerLookTimeout)
			inspect, inspectErr := cli.ContainerInspect(lookCtx, existing.ID, client.ContainerInspectOptions{})
			cancelLook()
			if inspectErr == nil && containerHealthStarting(inspect.Container) {
				return fmt.Errorf("%w: %w", sandbox.ErrPoolNotReachable, containerReadyError(inspect.Container))
			}
			err = fmt.Errorf("its agent did not come up within %s: %w", agentStartTimeout, err)
		}
		return &poolStartError{err: err}
	}
	steps.Step(ctx, "the started pool container's agent is up")
	return nil
}

// startedContainerLookTimeout bounds the one look at a started container once
// its agent's wait has run out, to tell a healthcheck still starting from one
// that failed.
const startedContainerLookTimeout = 5 * time.Second

// agentStartTimeout bounds the wait for a started box's agent (ADR
// 26-11-17-503 §4): a start that has not brought its agent up by then is
// rebuilt in the same pass. It outlasts the healthcheck's own start period and
// retries, so that a box with a healthcheck is judged by it.
const agentStartTimeout = time.Minute

// waitPoolAgentActive waits, until ctx ends, for the box's agent unit, in
// either slot (bundle.AgentUnits), to be active. The unit is Type=notify, so active is the agent having written what
// its proxy and builder read and said it is ready. An agent that fails is
// restarted by its unit for ever and reads activating, so only ctx, or the
// container stopping, ends a wait for one that never comes up.
func (e *Engine) waitPoolAgentActive(ctx context.Context, cli *client.Client, id string) error {
	// What the last attempt the deadline did not cut off found: an attempt
	// that ends on the deadline says nothing about the agent.
	last := "not asked yet"
	notActive := func() error {
		return fmt.Errorf("the pool agent is not active (%s): %w", last, ctx.Err())
	}
	for {
		code, err := ExecExitCode(ctx, cli, id, client.ExecCreateOptions{Cmd: append([]string{"systemctl", "is-active", "--quiet"}, bundle.AgentUnits()...)})
		if ctx.Err() != nil {
			return notActive()
		}
		if err == nil && code == 0 {
			return nil
		}
		if err != nil {
			last = err.Error()
		} else {
			last = fmt.Sprintf("systemctl is-active exited %d", code)
		}
		inspect, inspectErr := cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if ctx.Err() != nil {
			return notActive()
		}
		if inspectErr != nil {
			return mapDockerNotFound(inspectErr)
		}
		if !containerRunning(inspect.Container) {
			return fmt.Errorf("%w%s", containerReadyError(inspect.Container), containerLogSuffix(ctx, cli, id))
		}
		timer := time.NewTimer(healthPollDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return notActive()
		case <-timer.C:
		}
	}
}

// ExecExitCode runs an exec in the container detached and reads its exit code
// from the daemon, for a command whose output nobody reads: every step is a
// plain request bounded by ctx, and no stream is held open for output that
// would be thrown away.
func ExecExitCode(ctx context.Context, cli *client.Client, id string, options client.ExecCreateOptions) (int, error) {
	created, err := cli.ExecCreate(ctx, id, options)
	if err != nil {
		return -1, err
	}
	if _, err := cli.ExecStart(ctx, created.ID, client.ExecStartOptions{Detach: true}); err != nil {
		return -1, err
	}
	for {
		result, err := cli.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
		if err != nil {
			return -1, err
		}
		if !result.Running {
			return result.ExitCode, nil
		}
		timer := time.NewTimer(execExitPollDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return -1, ctx.Err()
		case <-timer.C:
		}
	}
}

// execExitPollDelay is how often ExecExitCode asks whether a short command
// has exited.
const execExitPollDelay = 20 * time.Millisecond

// markPoolStarted is a pool whose box was just made or started: its agent has
// not reported since.
func markPoolStarted(pool *model.Pool) {
	pool.Ready = false
	pool.Schedulable = false
	pool.Degraded = false
	pool.SetState(model.PoolStateRegistering)
}

// declaresHealthcheck reports whether a container was created with a
// healthcheck of its own, rather than none or "NONE".
func declaresHealthcheck(inspect container.InspectResponse) bool {
	if inspect.Config == nil || inspect.Config.Healthcheck == nil {
		return false
	}
	test := inspect.Config.Healthcheck.Test
	return len(test) > 0 && test[0] != "NONE"
}

func containerRunning(inspect container.InspectResponse) bool {
	return inspect.State != nil && inspect.State.Running
}

// applyMovedPoolBundles applies a pool's moved bundles, or its moved
// placement, in its running box, as a moved pin restarts a sandbox rather
// than rebuilding it: the box keeps its disk, its key and what it
// provisioned (ADR 26-10-24-953 §5). A moved placement — a pool setting its
// agent is told in its bootstrap, such as disableAgentEndpointTls — restarts
// the agent and its proxy, which read it once, at start.
//
// It never fails the reconcile of a running pool: a release that does not
// resolve right now (a registry unreachable after a server restart), or an
// apply that fails, leaves the box running what it last provisioned, and is
// logged, reported to the reconcile (sandbox.ReportBoxKept: the pool still runs
// its previous agent), and tried again on the pool's next reconcile. A failure here that
// failed the reconcile would repair the pool — remove a working container and
// the sandboxes' host with it — over bundles it did not need. Only begin's
// error is returned for bundle changes. Outbound changes return ErrPoolConfigApply
// instead, which records an actionable retry without repairing the running host.
func (e *Engine) applyMovedPoolBundles(ctx context.Context, cli *client.Client, containerID string, pool *model.Pool, maker poolruntime.PoolMaker, recorded poolruntime.BoxRecord, begin func(context.Context) error, steps *poolruntime.HostSteps) (poolruntime.BoxRecord, error) {
	bootstrap, box, err := e.poolBox(ctx, cli, pool, maker)
	if err != nil {
		slog.WarnContext(ctx, "keeping the pool's box as it is: what it is made of does not resolve", "pool", pool.ID, "error", err)
		sandbox.ReportBoxKept(ctx, fmt.Errorf("what its box is made of does not resolve: %w", err))
		if pool.OutboundStatus != nil && pool.OutboundAppliedRevision != pool.Outbound.Revision() {
			return recorded, fmt.Errorf("%w: %w", sandbox.ErrPoolConfigApply, err)
		}
		return recorded, nil
	}
	placement := poolruntime.BoxPlacement{Bootstrap: bootstrap, Box: box, Env: e.poolPolicyEnv(), Outbound: pool.RuntimeOutbound}
	current, err := recorded.Current(placement)
	if err != nil {
		return recorded, err
	}
	if current {
		if !pool.OutboundRestartRequired {
			pool.OutboundAppliedRevision = pool.Outbound.Revision()
		}
		return recorded, nil
	}
	// The apply restarts the agent the moved bundles carry, so placement is
	// closed first, as it is before any agent is replaced.
	steps.BringingUp(ctx)
	if err := begin(ctx); err != nil {
		return recorded, err
	}
	// Recorded before the apply, whose units restart the running agent when
	// its bundles or placement moved (ADR 26-11-15-602 §1).
	sandbox.ReportAgentStarted(ctx, time.Now(), sandbox.AgentStartInPlace)
	guest := provisioner.ContainerGuest{Client: cli, Container: containerID, Env: pool.RuntimeOutbound.Environment()}
	record, err := poolruntime.ProvisionBox(ctx, guest, maker, placement, recorded)
	if err != nil {
		slog.WarnContext(ctx, "keeping the pool's box as it is: its moved bundles did not apply", "pool", pool.ID, "error", err)
		sandbox.ReportBoxKept(ctx, fmt.Errorf("its moved bundles did not apply: %w", err))
		if pool.OutboundStatus != nil && pool.OutboundAppliedRevision != pool.Outbound.Revision() {
			return recorded, fmt.Errorf("%w: %w", sandbox.ErrPoolConfigApply, err)
		}
		return recorded, nil
	}
	steps.Step(ctx, "provisioned the pool's box in place")
	if recorded.OutboundRevision != record.OutboundRevision {
		if err := poolruntime.RestartPoolBuilder(ctx, guest); err != nil {
			slog.WarnContext(ctx, "the pool's builder did not restart on its new outbound routing; the next reconcile places it again", "pool", pool.ID, "error", err)
			sandbox.ReportBoxKept(ctx, fmt.Errorf("its builder did not restart on its new outbound routing: %w", err))
			if pool.OutboundStatus != nil && pool.OutboundAppliedRevision != pool.Outbound.Revision() {
				return recorded, fmt.Errorf("%w: %w", sandbox.ErrPoolConfigApply, err)
			}
			return recorded, nil
		}
		steps.Step(ctx, "restarted the pool's builder on its new outbound routing")
	}
	if !pool.OutboundRestartRequired {
		pool.OutboundAppliedRevision = pool.Outbound.Revision()
	}
	return record, nil
}

// recordedBox is what the pool's runtime state says the engine last placed
// in its box; none for a pool from before, or with no state.
func recordedBox(pool *model.Pool) poolruntime.BoxRecord {
	state, err := DecodeRuntimeState(pool.RuntimeState)
	if err != nil {
		return poolruntime.BoxRecord{}
	}
	return state.Box
}

// resolvePoolImage resolves the image to run a pool's container from, given
// whatever container is already there: the sandbox base image. Both the pool's
// container and the console run it, and both resolve it the same way.
//
// A container already on the configured image settles it without touching the
// daemon's images at all, which keeps the steady-state reconcile as free as it
// was before a fallback was possible. Every other case asks ensureImage, whose
// pull attempt is both how a cold daemon gets the image and how a pool running
// a fallback discovers that the configured one is reachable again.
func (e *Engine) resolvePoolImage(ctx context.Context, lease *DockerClientLease, poolID string, existing container.InspectResponse) (string, error) {
	if existing.Config != nil && strings.TrimSpace(existing.Config.Image) == strings.TrimSpace(e.cfg.SandboxBaseImage) {
		return e.cfg.SandboxBaseImage, nil
	}
	return e.ensureImage(ctx, lease, poolID)
}

// poolBootstrap is the pool's bootstrap as this engine's pools reach their
// control plane and listen.
func (e *Engine) poolBootstrap(ctx context.Context, pool *model.Pool, maker poolruntime.PoolMaker) (poolagent.Bootstrap, error) {
	if maker == nil {
		return poolagent.Bootstrap{}, fmt.Errorf("a pool maker is required")
	}
	bootstrap, err := maker.Bootstrap(ctx)
	if err != nil {
		return poolagent.Bootstrap{}, err
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
	bootstrap.DedicatedDaemon = e.cfg.DedicatedDaemon
	return bootstrap, nil
}

// poolBox is the pool's bootstrap and what its box is placed with, the
// agent's layer for the platform of the daemon it runs on.
func (e *Engine) poolBox(ctx context.Context, cli *client.Client, pool *model.Pool, maker poolruntime.PoolMaker) (poolagent.Bootstrap, sandbox.PoolBox, error) {
	bootstrap, err := e.poolBootstrap(ctx, pool, maker)
	if err != nil {
		return poolagent.Bootstrap{}, sandbox.PoolBox{}, err
	}
	platform, err := provisioner.ContainerPlatform(ctx, cli)
	if err != nil {
		return poolagent.Bootstrap{}, sandbox.PoolBox{}, err
	}
	box, err := maker.Box(ctx, platform)
	if err != nil {
		return poolagent.Bootstrap{}, sandbox.PoolBox{}, err
	}
	return bootstrap, box, nil
}

// poolContainerSpec is the pool's container as createPoolContainer creates
// it, and whether to wait for its healthcheck: image booted with systemd as
// PID 1, the host's Docker socket and the pool's state bound in.
func (e *Engine) poolContainerSpec(pool *model.Pool, image string, labels map[string]string) (*container.Config, *container.HostConfig, bool, error) {
	config := &container.Config{
		Image:  image,
		Env:    pool.RuntimeOutbound.Environment(),
		Labels: labels,
		// The box boots its own init, as a sandbox's does: the pool agent is a
		// unit its bundle lays, and nothing runs before systemd.
		Entrypoint: []string{"/sbin/init"},
		Cmd:        []string{},
	}
	hostConfig := &container.HostConfig{
		Privileged: e.privileged(),
		ExtraHosts: append([]string(nil), e.cfg.ExtraHosts...),
	}
	// Only an IP listener has a port Docker can publish or curl can probe.
	listen, err := wire.Parse(e.cfg.AgentListenURL)
	if err != nil {
		return nil, nil, false, fmt.Errorf("agent listen URL: %w", err)
	}
	waitForHealth := true
	if listen.IsIP() {
		exposedPort, ok := agentNetworkPort(e.cfg.AgentPort)
		if !ok {
			return nil, nil, false, fmt.Errorf("invalid harness port %d", e.cfg.AgentPort)
		}
		config.ExposedPorts = network.PortSet{exposedPort: struct{}{}}
		hostConfig.PortBindings = network.PortMap{exposedPort: []network.PortBinding{e.agentPortBinding()}}
		// The base image declares no healthcheck: the agent's is this one.
		// Its start period is a restart's, when the box's units start its
		// agent at once, so an agent that never comes up reads as unhealthy
		// within a minute. The first start provisions the box before there
		// is an agent to answer, which may read as unhealthy already; the
		// engine is the one waiting then, and waits out the agent the apply
		// has just started (waitAgentAfterApply).
		config.Healthcheck = &container.HealthConfig{
			Test:        []string{"CMD-SHELL", fmt.Sprintf("curl -fsS http://127.0.0.1:%d/healthz >/dev/null || exit 1", e.cfg.AgentPort)},
			Interval:    5 * time.Second,
			Timeout:     2 * time.Second,
			StartPeriod: 5 * time.Second,
			Retries:     6,
		}
	} else {
		// No TCP listener to probe. Container state plus the control plane's
		// first heartbeat from the agent is the readiness signal for those
		// transports.
		config.Healthcheck = &container.HealthConfig{Test: []string{"NONE"}}
		waitForHealth = false
	}
	// No CPU or memory limit, deliberately. The pool's size sizes the VM a
	// pool runs in (see VMSpec), not this container: sandboxes are created on
	// the host daemon through the bind-mounted socket, as siblings of this
	// container rather than nested inside it, so a limit here would bound
	// only the pool's own services - agent, BuildKit, registry, proxy - and
	// starve them first while the sandboxes they serve ran unlimited.
	if e.cfg.CgroupNSMode != "" {
		hostConfig.CgroupnsMode = container.CgroupnsMode(e.cfg.CgroupNSMode)
	} else {
		// systemd (PID 1 in the pool's container) must create its own cgroup
		// subtree. A private cgroup namespace makes Docker mount a writable
		// cgroup2 hierarchy delegated to the container; bind-mounting the host
		// /sys/fs/cgroup instead drops the container onto the read-only host
		// cgroup root and systemd exits 255 before it can even log.
		hostConfig.CgroupnsMode = container.CgroupnsMode("private")
	}
	hostConfig.Mounts = e.containerMounts()
	hostConfig.Tmpfs = map[string]string{"/run": "rw,noexec,nosuid,size=64m", "/run/lock": "rw,noexec,nosuid,size=64m", "/tmp": "rw,size=64m"}
	return config, hostConfig, waitForHealth, nil
}

// createPoolContainer creates the pool's container from image, which the
// caller has already resolved and made present — that pull is the longest step
// in bringing a pool up — starts it, systemd as PID 1, and provisions it as
// the pool's box (poolruntime.ProvisionBox) before it waits for the agent the box's
// bundles start.
func (e *Engine) createPoolContainer(ctx context.Context, cli *client.Client, pool *model.Pool, name string, labels map[string]string, maker poolruntime.PoolMaker, image string, bootstrap poolagent.Bootstrap, box sandbox.PoolBox, steps *poolruntime.HostSteps) (*container.InspectResponse, poolruntime.BoxRecord, error) {
	startCtx, releaseStart := e.cfg.ProgressReporter.Hold(ctx, pool.ID, sandbox.PoolPhaseStartingPoolAgent)
	defer releaseStart()
	// No container of this pool's exists now, whether a reconcile removed it
	// or it was lost, so nothing mounts the volume an older one did.
	e.removeLegacyPoolVolume(ctx, cli, pool.ID)
	config, hostConfig, waitForHealth, err := e.poolContainerSpec(pool, image, labels)
	if err != nil {
		return nil, poolruntime.BoxRecord{}, err
	}
	networkConfig := &network.NetworkingConfig{}
	if e.cfg.Network != "" {
		networkConfig.EndpointsConfig = map[string]*network.EndpointSettings{e.cfg.Network: {}}
	}
	// Pool agents run the shared proxy that their sandboxes route through.
	// Create the per-pool internal network so the pool's container can be
	// aliased as the proxy server name on it and sandboxes can reach only the
	// proxy.
	if err := e.ensureSandboxNetwork(startCtx, cli, pool.ID); err != nil {
		return nil, poolruntime.BoxRecord{}, err
	}
	created, err := cli.ContainerCreate(startCtx, client.ContainerCreateOptions{Config: config, HostConfig: hostConfig, NetworkingConfig: networkConfig, Name: name})
	if err != nil {
		return nil, poolruntime.BoxRecord{}, err
	}
	if _, err := cli.NetworkConnect(startCtx, proxyagent.SandboxNetworkName(pool.ID), client.NetworkConnectOptions{
		Container:      created.ID,
		EndpointConfig: &network.EndpointSettings{Aliases: []string{proxyagent.ServerName}},
	}); err != nil {
		return nil, poolruntime.BoxRecord{}, fmt.Errorf("connect pool to sandbox network: %w", err)
	}
	// A new container runs no agent until it is provisioned, so the pool's
	// agent counts as started from just before it starts (ADR 26-11-15-294
	// §1), told now: its first report can land before this pass settles.
	sandbox.ReportAgentStarted(startCtx, time.Now(), sandbox.AgentStartBox)
	if _, err := cli.ContainerStart(startCtx, created.ID, client.ContainerStartOptions{}); err != nil {
		return nil, poolruntime.BoxRecord{}, err
	}
	steps.Step(ctx, "started the pool's container", "container", shortContainerID(created.ID))
	record, err := poolruntime.ProvisionBox(startCtx, provisioner.ContainerGuest{Client: cli, Container: created.ID, Env: pool.RuntimeOutbound.Environment()}, maker, poolruntime.BoxPlacement{Bootstrap: bootstrap, Box: box, Env: e.poolPolicyEnv(), Outbound: pool.RuntimeOutbound}, poolruntime.BoxRecord{})
	if err != nil {
		return nil, poolruntime.BoxRecord{}, err
	}
	steps.Step(ctx, "provisioned the pool's box")
	if !pool.OutboundRestartRequired {
		pool.OutboundAppliedRevision = pool.Outbound.Revision()
	}
	releaseStart()
	agentCtx, releaseAgent := e.cfg.ProgressReporter.Hold(ctx, pool.ID, sandbox.PoolPhaseWaitingForPoolAgent)
	defer releaseAgent()
	if waitForHealth {
		if err := e.waitAgentAfterApply(agentCtx, cli, created.ID); err != nil {
			return nil, poolruntime.BoxRecord{}, err
		}
	}
	inst, err := e.waitContainerReady(agentCtx, cli, created.ID, waitForHealth)
	if err == nil {
		steps.Step(ctx, "the pool's container is healthy")
	}
	return inst, record, err
}

// agentAfterApplyTimeout is how long the agent a box's apply has just started
// may take to pass its healthcheck once, after a first start whose
// provisioning outlasted the healthcheck's start period and left the container
// reading as unhealthy.
const agentAfterApplyTimeout = time.Minute

// waitAgentAfterApply waits until the container's healthcheck has passed
// since its box was provisioned, or it is not running, or the timeout: an
// unhealthy status from before the apply says nothing about the agent it
// started, and a check passing turns it healthy again.
func (e *Engine) waitAgentAfterApply(ctx context.Context, cli *client.Client, id string) error {
	deadline := time.Now().Add(agentAfterApplyTimeout)
	for {
		inspect, err := cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if err != nil {
			return mapDockerNotFound(err)
		}
		state := inspect.Container.State
		if state == nil || !state.Running || state.Health == nil || state.Health.Status != "unhealthy" || time.Now().After(deadline) {
			return nil
		}
		timer := time.NewTimer(healthPollDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (e *Engine) agentPortBinding() network.PortBinding {
	if e.cfg.PublicAgentPort {
		return network.PortBinding{HostPort: strconv.Itoa(e.cfg.AgentPort)}
	}
	return network.PortBinding{HostIP: netip.MustParseAddr("127.0.0.1")}
}

// removePoolHost removes everything the engine made for a deleted pool on its
// daemon: on a shared daemon its state trees first, then its runtime
// (RemovePoolRuntime). Each step tolerates what is already gone, so a retry
// after a partial removal finishes it.
func (e *Engine) removePoolHost(ctx context.Context, cli *client.Client, pool *model.Pool) error {
	// A dedicated daemon's disk holds the trees, and DeleteVM destroys it.
	if !e.cfg.DedicatedDaemon {
		if err := e.removePoolStateTrees(ctx, cli, pool); err != nil {
			return err
		}
	}
	return e.RemovePoolRuntime(ctx, cli, pool.ID)
}

// RemovePoolRuntime removes a pool's container, console, sandbox network, and
// the legacy named volume a pool made before it stopped mounting one
// (removeLegacyPoolVolume), which ContainerRemove's RemoveVolumes does not
// touch (it drops anonymous volumes only). It leaves
// the pool's state trees: the drift watcher calls it for a pool container
// whose row is gone, and a row that comes back from a restored database needs
// its identity and data (the agent's pool-sync reaper takes the rest, after
// its retention).
func (e *Engine) RemovePoolRuntime(ctx context.Context, cli *client.Client, poolID string) error {
	if _, err := cli.ContainerRemove(ctx, ContainerName(poolID), client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return err
	}
	// The console is engine-created but reconcile-free, so pool teardown is the
	// only thing that would ever remove it.
	if err := e.removeConsoleContainer(ctx, cli, poolID); err != nil {
		return err
	}
	if err := e.removeSandboxNetwork(ctx, cli, poolID); err != nil {
		return err
	}
	volume := legacyPoolVolumeName(poolID)
	if _, err := cli.VolumeRemove(ctx, volume, client.VolumeRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove pool volume %s: %w", volume, err)
	}
	return nil
}

// legacyPoolVolumeName is the named volume pool containers once mounted at
// /var/lib/docker for a nested daemon that no pool container runs. Nothing
// mounts it now; it is named only so the volume an older pool carries is
// removed.
func legacyPoolVolumeName(poolID string) string {
	return scopedVolumeName("pool", poolID, "docker")
}

// removeLegacyPoolVolume removes a pool's legacy volume before its container
// is created, when none exists, so the next container of an older pool frees
// it: the one drift makes when workerConfigLayoutVersion moved past the
// volume, or one made after repair or a container lost out of band. It is
// best effort: the volume holds nothing, and a failure must not stop the pool
// from coming back, so it is logged and left for the pool's deletion to
// remove.
func (e *Engine) removeLegacyPoolVolume(ctx context.Context, cli *client.Client, poolID string) {
	volume := legacyPoolVolumeName(poolID)
	if _, err := cli.VolumeRemove(ctx, volume, client.VolumeRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
		slog.WarnContext(ctx, "leaving a pool's legacy Docker volume", "pool_id", poolID, "volume", volume, "error", err)
	}
}

// removePoolStateTrees removes a deleted pool's own directories of the state
// trees (layout.PoolTrees) on a daemon shared with other pools. Nothing else
// would: the agent's reaper of unknown pools runs only in a live agent of the
// same project, so never for a project's last pool, and no reaper may
// enumerate identity keys (ADR 0063).
//
// The data goes at once, without the 24-hour retention a reaper keeps (ADR
// 26-10-01-876). That retention guards a reaper's inference from a set the
// control plane answers; this is an explicit, authorized pool deletion, and a
// pool with a sandbox row cannot be deleted at all (owner, 2026-11-18).
//
// The trees are on the daemon's filesystem, which the server may not share
// (Docker Desktop's VM) and whose BuildKit state is root's, so a short-lived
// container on the daemon removes them, binding them exactly as the pool
// container did. It runs the pool container's own image, after that container
// is stopped so its agent cannot write into the trees again; the stopped
// container stays until the trees are gone, so a retry still has its image.
func (e *Engine) removePoolStateTrees(ctx context.Context, cli *client.Client, pool *model.Pool) error {
	if !isPathSegment(pool.ProjectID) || !isPathSegment(pool.ID) {
		// An ID that is not one path segment would name a parent of other
		// pools' trees, or nothing of this pool's.
		slog.WarnContext(ctx, "not removing the state trees of a pool without a project and pool ID",
			"project_id", pool.ProjectID, "pool_id", pool.ID)
		return nil
	}
	state := layout.Container()
	trees := state.PoolTrees(pool.ProjectID, pool.ID)
	existing, err := cli.ContainerInspect(ctx, ContainerName(pool.ID), client.ContainerInspectOptions{})
	var image string
	switch {
	case err == nil:
		timeout := 0
		if _, err := cli.ContainerStop(ctx, existing.Container.ID, client.ContainerStopOptions{Timeout: &timeout}); err != nil && !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("stop pool container: %w", err)
		}
		image = existing.Container.Image
	case cerrdefs.IsNotFound(err):
		if image, err = e.localPoolImage(ctx, cli); err != nil {
			return err
		}
	default:
		return err
	}
	if image == "" {
		// Pulling an image only to delete would hold the pool's deletion
		// hostage to a registry.
		slog.WarnContext(ctx, "no pool image on the daemon; leaving the deleted pool's state trees",
			"pool_id", pool.ID, "configured_image", e.cfg.SandboxBaseImage, "trees", e.daemonPaths(trees))
		return nil
	}
	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:       image,
			User:        "0:0",
			Entrypoint:  []string{"rm", "-rf", "--"},
			Cmd:         trees,
			Healthcheck: &container.HealthConfig{Test: []string{"NONE"}},
			Labels:      map[string]string{LabelManaged: "true", LabelPoolID: pool.ID, LabelProjectID: pool.ProjectID},
		},
		HostConfig: &container.HostConfig{NetworkMode: "none", Mounts: e.stateMounts()},
	})
	if err != nil {
		return fmt.Errorf("create pool state removal: %w", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, _ = cli.ContainerRemove(cleanup, created.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	}()
	if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start pool state removal: %w", err)
	}
	result := cli.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-result.Error:
		return fmt.Errorf("wait for pool state removal: %w", err)
	case status := <-result.Result:
		if status.StatusCode != 0 {
			return fmt.Errorf("remove pool state trees: exit status %d%s", status.StatusCode, containerLogSuffix(ctx, cli, created.ID))
		}
	}
	return nil
}

// localPoolImage is the image the engine would launch a pool from that is
// already on cli's daemon, without pulling: the configured sandbox base image,
// or failing that the superseded local one ensureImage falls back to. Empty
// means neither is there.
func (e *Engine) localPoolImage(ctx context.Context, cli *client.Client) (string, error) {
	if _, err := cli.ImageInspect(ctx, e.cfg.SandboxBaseImage); err == nil {
		return e.cfg.SandboxBaseImage, nil
	} else if !cerrdefs.IsNotFound(err) {
		return "", fmt.Errorf("inspect pool image %q: %w", e.cfg.SandboxBaseImage, err)
	}
	image, _ := e.fallbackPoolImage(ctx, cli)
	return image, nil
}

// daemonPaths is where the daemon sees each of paths, for a message an
// operator acts on there.
func (e *Engine) daemonPaths(paths []string) []string {
	hostState := layout.Container().HostMapping(e.cfg.HostStateRoot)
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = hostState.HostPath(p)
	}
	return out
}

// isPathSegment reports whether id is usable as one segment of a state path.
func isPathSegment(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, `/\`)
}

// removeSandboxNetwork removes the per-pool internal network. Any container
// still attached — a sandbox that outlived the pool teardown race, or a stale
// endpoint — makes Docker refuse the removal with "network has active
// endpoints", which would leak the network and its scarce address block.
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

// ensureSandboxNetwork creates the per-pool internal bridge network if absent.
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
	return labels
}

// ShouldReconcileWorkerContainer reports whether a pool-agent container drifted
// from the engine's desired image or labels. It is shared with runtime drift
// watchers.
func (e *Engine) ShouldReconcileWorkerContainer(image string, labels map[string]string) bool {
	if image != e.cfg.SandboxBaseImage {
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
	return false
}

// stateMounts binds the state trees from where the daemon keeps them at the
// path the container reads. The pool container and the container that removes
// a deleted pool's trees both bind exactly these.
func (e *Engine) stateMounts() []mount.Mount {
	var mounts []mount.Mount
	// The state trees come from the shared layout package, so the engine, the
	// agent, and the proxy cannot drift on where anything lives.
	state := layout.Container()
	hostState := state.HostMapping(e.cfg.HostStateRoot)
	for _, tree := range state.MountRoots() {
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
	return mounts
}

func (e *Engine) containerMounts() []mount.Mount {
	mounts := []mount.Mount{
		{Type: mount.TypeBind, Source: e.cfg.DockerSocket, Target: dockerSocketPath},
		// The sandbox base image enables docker.socket for a sandbox's own
		// daemon. Here the host's socket is already bound at the unit's
		// listen path (/var/run is a link to /run), so the unit would fail
		// with "Address already in use" and leave the container degraded.
		// /dev/null over the unit file masks it in this container only;
		// sandboxes on the same image keep their socket-activated daemon.
		{Type: mount.TypeBind, Source: "/dev/null", Target: dockerSocketUnitPath, ReadOnly: true},
	}
	// Bound at the same path rather than under the host-mount root, like the
	// Docker socket above: the agent only dials it, so there is no host path to
	// translate. The backend's guest-side relay owns the socket in this
	// directory; backends without one leave the field empty.
	if dir := cleanAbsPath(e.cfg.RelaySocketDir); dir != "" {
		mounts = append(mounts, mount.Mount{Type: mount.TypeBind, Source: dir, Target: dir})
	}
	mounts = append(mounts, e.stateMounts()...)
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
	//
	// There is no named /var/lib/docker volume: no daemon runs in the pool
	// container. Its agent drives the hosting daemon through the socket bound
	// above, and BuildKit keeps its state in the cache tree (PoolBuild). The
	// sandbox base image's VOLUMEs give it empty anonymous ones, which every
	// removal drops with RemoveVolumes (ADR 26-11-23-993 §4). Pools made before
	// this carry an empty legacy named volume, removed with their next
	// container (removeLegacyPoolVolume).
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

// PoolAgentUnreachable reports why a pool-agent container cannot answer a
// request yet: it is not running, so it publishes nothing, or its healthcheck
// has not passed once, so its agent may not have bound its port. It is the
// verdict a driver that inspects the container on acquire applies.
//
// It is narrower than the readiness wait's containerReadyError on purpose. An
// unhealthy container is still running an agent that may answer, and refusing
// it would cut every request to a live pool whenever its probe flapped.
//
// Every refusal wraps sandbox.ErrPoolNotReachable. Each is a host on its way up,
// and a caller that can wait, such as an attach, must wait rather than fail.
func PoolAgentUnreachable(inspect container.InspectResponse) error {
	if inspect.State != nil && inspect.State.Running && !containerHealthStarting(inspect) {
		return nil
	}
	return fmt.Errorf("%w: %w", sandbox.ErrPoolNotReachable, containerReadyError(inspect))
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

// AssignedAgentEndpoint resolves the published host endpoint for the pool
// agent's port from a container's port map.
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

// ContainerName is the deterministic pool-agent container name for a pool.
func ContainerName(poolID string) string {
	name := invalidContainerName.ReplaceAllString(poolID, "-")
	name = strings.Trim(name, "-_.")
	if name == "" {
		name = "vm"
	}
	return "discobox-vm-" + name
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
