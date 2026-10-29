package poolagent

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/x/id"

	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pkg/platform"
	"github.com/discobox-ai/discobox/pkg/poolassets"
	"github.com/discobox-ai/discobox/pkg/poolauth"
	"github.com/discobox-ai/discobox/pkg/wire"
	"github.com/discobox-ai/discobox/pool-agent/assetcache"
	"github.com/discobox-ai/discobox/pool-agent/buildkitagent"
	poolbundle "github.com/discobox-ai/discobox/pool-agent/bundle"
	"github.com/discobox-ai/discobox/pool-agent/childproc"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	poolserver "github.com/discobox-ai/discobox/pool-agent/server"
	agentsystemd "github.com/discobox-ai/discobox/pool-agent/systemd"
)

// RunProxy runs the pool-scoped proxy server. It is the entrypoint for the
// proxy systemd unit inside the pool container.
func RunProxy(ctx context.Context, logger *slog.Logger) error {
	return proxyagent.RunProxy(ctx, layout.Container(), logger)
}

// RunAgent registers the pool, marks it ready, and serves the pool-agent HTTP
// endpoints over the sandbox runtime its bootstrap names.
func RunAgent(ctx context.Context, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	bootstrap, err := Load()
	if err != nil {
		return err
	}
	// Refused before anything is written: a bootstrap naming a runtime this
	// agent cannot compose must not leave a proxy configured for a pool that
	// never comes up.
	if err := bootstrap.Validate(); err != nil {
		return err
	}
	runtimeKind := bootstrap.EffectiveRuntime()
	// This agent runs in the pool container, or as root on a pool machine on
	// the same paths, so its state is the container's (ADR 0144 §1, ADR
	// 26-10-23-551 §1).
	root := layout.Container()
	// Publish the host-mount prefix for the proxy systemd unit, then prepare the
	// proxy certificate bundle before the proxy unit starts so it and the
	// per-sandbox client certificates share a consistent CA without racing on
	// first-time generation.
	tunnelURL, err := proxyagent.ConfiguredTunnelURL()
	if err != nil {
		return err
	}
	if err := proxyagent.WriteUnitEnvironment(root, bootstrap.HostMountPrefix, bootstrap.ControlPlaneURL, bootstrap.ProjectID, bootstrap.PoolID, tunnelURL); err != nil {
		return err
	}
	bundle, err := proxyagent.PrepareBundle(root, bootstrap.ProjectID, bootstrap.PoolID)
	if err != nil {
		return err
	}
	// The proxy control key is created here for the same reason as the CA: the
	// proxy unit trusts its public half and this process signs with it, so it
	// must exist before systemd starts the unit that reads it.
	if _, err := proxyagent.PrepareControlKey(root, bootstrap.ProjectID, bootstrap.PoolID); err != nil {
		return fmt.Errorf("prepare proxy control key: %w", err)
	}
	// The pool's own services name it as the proxy's server name, which this
	// host is given here, at every start, since an image may rewrite
	// /etc/hosts at boot. Before the builder's units, which start after this.
	if err := ensurePoolName(root.System("/etc/hosts")); err != nil {
		return err
	}
	// Render the shared builder's and its registry's configuration for the
	// same reason and at the same point: both run as systemd units with a
	// clean environment, and every path they own is pool-scoped (ADR 0044).
	// Every pool runs them, a machine pool's included (ADR 26-10-27-767).
	if err := buildkitagent.Prepare(root, bootstrap.ProjectID, bootstrap.PoolID, bundle.MITMCAPath); err != nil {
		return err
	}
	// Only PID 1 in a pool container boots systemd itself; on a pool machine
	// systemd is already the init and this starts nothing.
	systemd, err := agentsystemd.StartNamespace(ctx, logger)
	if err != nil {
		return err
	}
	stopReaper := childproc.StartReaper(ctx, logger, agentsystemd.ManagedChildProcesses(systemd))
	defer stopReaper()
	defer agentsystemd.Stop(systemd)
	// On a pool machine the agent is a unit the proxy's is ordered after, and
	// this is the start systemd waits for: the proxy's environment, CA and
	// control key are all written (ADR 26-10-23-551 §1). Anywhere else nothing
	// is listening, and it does nothing.
	if err := agentsystemd.NotifyReady(); err != nil {
		return fmt.Errorf("tell systemd the agent has started: %w", err)
	}

	// The control plane URL's scheme selects the transport; nothing here knows
	// whether that is IP, VSOCK, or a Unix socket served by a guest helper.
	baseURL, controlPlaneHTTPClient, err := wire.HTTPClient(bootstrap.ControlPlaneURL, 0)
	if err != nil {
		return fmt.Errorf("resolve control plane transport: %w", err)
	}
	// From here the URL is only ever used to address requests, and the dialer
	// already fixes the peer. Keeping the transport URL would send requests to
	// "unix:///run/...sock/api/..." — a path net/http rejects, since "unix" is
	// not an HTTP scheme. The transport URL has already been consumed above, and
	// by the proxy unit's environment written before this point.
	bootstrap.ControlPlaneURL = baseURL
	client := NewHTTPClient(baseURL, WithHTTPClient(controlPlaneHTTPClient))
	// The identity key lives on the pool's durable storage, where its maker
	// had it made and recorded its public half before this agent started (ADR
	// 26-10-24-953 §4, ADR 0063): nothing registers it, and an agent with none
	// cannot be this pool.
	identity, err := LoadIdentity(root.PoolIdentityKey(bootstrap.ProjectID, bootstrap.PoolID))
	if err != nil {
		return err
	}
	logger.Info("pool identity ready", "poolID", bootstrap.PoolID, "runtime", runtimeKind)

	// A machine pool's sandboxes are machines of its own engine (ADR
	// 26-10-23-551 §3), opened before the first report: the pool's status is
	// its driver's.
	status := dockerPoolStatus()
	var machines *engine.Engine
	if runtimeKind == RuntimeMachine {
		if machines, err = openMachineEngine(root, bootstrap, poolbundle.ProvisioningKeyFile); err != nil {
			return err
		}
		status = machinePoolStatus(bootstrap.Driver, machines.Driver.Capabilities())
	}

	// A first report the control plane refuses is a pool whose key it does
	// not hold — a restored database, a recreated pool row — which the
	// pool's maker mends by enrolling the box again, as a repair does; this
	// agent fails its start and systemd tries it again.
	if err := startStatusReporter(ctx, logger, root, bootstrap, identity, client, status, statusReportInterval); err != nil {
		return err
	}

	startResolveTokenRefresher(ctx, logger, root, bootstrap, identity)

	// The bundles its sandboxes are pinned to, fetched from the control plane
	// as this pool and over the transport its URL names (ADR 26-10-16-423 §13).
	assetClient := &poolassets.Client{
		BaseURL:   baseURL,
		HTTP:      controlPlaneHTTPClient,
		ProjectID: bootstrap.ProjectID,
		PoolID:    bootstrap.PoolID,
		Key:       identity.PrivateKey,
	}
	assets := assetcache.New(assetcache.Config{
		Dir:       filepath.Join(root.PoolBuild(bootstrap.ProjectID, bootstrap.PoolID), "assets"),
		Sandboxes: root.PoolSandboxes(bootstrap.ProjectID, bootstrap.PoolID),
		Snapshots: root.PoolSnapshots(bootstrap.ProjectID, bootstrap.PoolID),
		Fetch:     assetClient.Fetch,
	})
	if machines != nil {
		runtime, err := newMachineSandboxRuntime(root, bootstrap, machines, identity.PrivateKey, assets)
		if err != nil {
			return err
		}
		return Serve(ctx, logger, root, bootstrap, identity, runtime, client, assets)
	}
	runtime, err := newDockerSandboxRuntime(root, bootstrap, identity.PrivateKey, assets)
	if err != nil {
		return err
	}
	// Image reclamation is the Docker pool's alone: the images are its daemon's,
	// and a runtime that boots no OCI image has none to reclaim (ADR 0040).
	go runtime.WatchImages(ctx, logger)
	return Serve(ctx, logger, root, bootstrap, identity, runtime, client, assets)
}

// newDockerSandboxRuntime is this pool's runtime: sandboxes as containers on
// the Docker daemon beside the agent, with pool state under root.
//
// The pool's identity key signs the runtime-config documents it delivers, which
// is why the runtime is built only once the key is known to be this pool's.
// Its asset cache is where the agent layer it places in a delivered sandbox
// comes from.
func newDockerSandboxRuntime(root layout.Root, bootstrap Bootstrap, identityKey ed25519.PrivateKey, assets *assetcache.Cache) (*sandboxruntime.DockerSandboxRuntime, error) {
	idleTimeout, err := sandboxruntime.ConfiguredSandboxIdleTimeout()
	if err != nil {
		return nil, err
	}
	tunnelURL, err := proxyagent.ConfiguredTunnelURL()
	if err != nil {
		return nil, err
	}
	return sandboxruntime.NewDockerSandboxRuntime(sandboxruntime.DockerSandboxRuntimeConfig{
		PoolEndpoints:         proxyagent.PoolEndpoints(tunnelURL),
		ProjectID:             bootstrap.ProjectID,
		PoolID:                bootstrap.PoolID,
		ControlPlanePublicKey: bootstrap.ControlPlaneKey,
		Root:                  root,
		HostStateRoot:         bootstrap.HostStateRoot,
		SandboxIdleTimeout:    idleTimeout,
		IdentityKey:           identityKey,
		SharedMemoryBytes:     sandboxSharedMemoryBytes(poolCgroupRoot),
		Platform:              hostedPlatform(),
		SandboxBaseImage:      os.Getenv(sandboxruntime.EnvSandboxBaseImage),
		AgentLayers:           assets,
	})
}

const (
	resolveTokenTTL     = 30 * time.Minute
	resolveTokenRefresh = 20 * time.Minute
)

const (
	// statusReportInterval paces the pool's status heartbeat.
	statusReportInterval = 30 * time.Second
	// statusReportTimeout bounds one report. The control-plane client is built
	// without a timeout, so an unbounded report against a wedged control plane
	// would stall every later beat and silently strand the pool.
	statusReportTimeout = 15 * time.Second
)

// poolStatus is what a pool's composition says of itself on every status
// report: whether it can be scheduled, the kind of image it runs, the runtime
// condition saying what runs its sandboxes, and whether its own host's CPU and
// memory are its capacity.
type poolStatus struct {
	schedulable bool
	imageKind   platform.ImageKind
	runtime     map[string]any
	// hostCapacity reports the agent's own CPU and memory as the pool's: its
	// sandboxes are containers on this host. A machine pool's run on its
	// driver, and reports none of the pool machine's.
	hostCapacity bool
}

// dockerPoolStatus is a Docker pool's: its sandboxes are containers on the
// agent's own host, so it runs OCI images (ADR 26-10-09-106 §4) and its
// capacity is the host's.
func dockerPoolStatus() poolStatus {
	return poolStatus{schedulable: true, imageKind: platform.OCI, hostCapacity: true}
}

// startStatusReporter reports this pool's scheduling status and capacity to the
// control plane: once synchronously, so a pool that cannot mark itself ready
// fails its boot loudly, then on statusReportInterval for as long as the agent
// runs.
//
// The repeat is not merely a liveness signal. Ready/Schedulable are
// agent-reported fields that the control plane clears on its own whenever a
// reconcile fails (see the server's pool reconciler), and nothing there ever
// sets them back. A pool that reported ready only at boot therefore stayed
// unschedulable until its agent restarted, with every sandbox route under it
// answering 409. Re-reporting makes readiness self-healing, and keeps the
// pool's last-seen timestamp fresh enough to distinguish a live pool from an
// agent that died.
//
// Capacity is measured per report rather than captured at boot, so the
// control plane schedules against current free space rather than whatever was
// free when the pool started.
func startStatusReporter(ctx context.Context, logger *slog.Logger, root layout.Root, bootstrap Bootstrap, identity *Identity, client StatusClient, status poolStatus, interval time.Duration) error {
	conditions := map[string]any{
		"agent": map[string]any{
			"version": "dev",
			"status":  "ready",
		},
	}
	if status.runtime != nil {
		conditions["runtime"] = status.runtime
	}
	report := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, statusReportTimeout)
		defer cancel()
		var cpus float64
		var memory int64
		if status.hostCapacity {
			cpus, memory = availableCPUVCPUs(), availableMemoryBytes()
		}
		return client.UpdatePoolStatus(ctx, StatusRequest{
			ControlPlaneURL:      bootstrap.ControlPlaneURL,
			ProjectID:            bootstrap.ProjectID,
			PoolID:               bootstrap.PoolID,
			PrivateKey:           identity.PrivateKey,
			Platform:             hostedPlatform(),
			ImageKind:            status.imageKind,
			Ready:                true,
			Schedulable:          status.schedulable,
			Degraded:             false,
			AvailableCPUVCPUs:    cpus,
			AvailableMemoryBytes: memory,
			// Stat this project's own data tree. Its parent belongs to the pool
			// container's root filesystem and would report the wrong backing
			// filesystem.
			AvailableStorageBytes: poolFreeStorageBytes(root.ProjectData(bootstrap.ProjectID)),
			Conditions:            conditions,
		})
	}
	if err := report(ctx); err != nil {
		return err
	}
	logger.Info("pool marked ready", "poolID", bootstrap.PoolID)

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// A failed beat is transient by assumption: the next tick
				// re-reports, and that retry is exactly what recovers a pool
				// whose readiness was cleared while the control plane was down.
				if err := report(ctx); err != nil && ctx.Err() == nil {
					logger.Warn("report pool status", "poolID", bootstrap.PoolID, "error", err)
				}
			}
		}
	}()
	return nil
}

// startResolveTokenRefresher mints the scoped token the proxy unit uses and
// writes it, with the control-plane URL, to this pool's own resolve-context
// file, refreshing it before expiry.
//
// It carries three scopes because the proxy unit does three things on a
// sandbox's behalf: resolve sentinels it sees in traffic, broker agent
// credential requests (ADR 0031), and forward the sandbox's calls to the
// discobox API (ADR 0140). They stay separate scopes so a future split of those
// roles across processes is a change of who holds which token, not a change of
// what a token means.
func startResolveTokenRefresher(ctx context.Context, logger *slog.Logger, root layout.Root, bootstrap Bootstrap, identity *Identity) {
	write := func() error {
		token, err := poolauth.CreateTokenWithTTL(identity.PrivateKey, poolauth.Claims{
			ProjectID: bootstrap.ProjectID,
			PoolID:    bootstrap.PoolID,
			Scopes:    []string{poolauth.ScopeSecretResolve, poolauth.ScopeCredentialBroker, poolauth.ScopeSandboxForward},
		}, resolveTokenTTL)
		if err != nil {
			return err
		}
		return proxyagent.WriteResolveContext(root, bootstrap.ProjectID, bootstrap.PoolID, bootstrap.ControlPlaneURL, token)
	}
	if err := write(); err != nil {
		logger.Warn("write proxy resolve token", "error", err)
	}
	go func() {
		ticker := time.NewTicker(resolveTokenRefresh)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := write(); err != nil {
					logger.Warn("refresh proxy resolve token", "error", err)
				}
			}
		}
	}()
}

// ExecSystemdChildIfRequested starts the child systemd process when requested by the systemd helper.
func ExecSystemdChildIfRequested() error {
	return agentsystemd.ExecSystemdChildIfRequested()
}

// Serve runs the pool's sandbox runtime loops and serves the pool-agent HTTP
// endpoints, over pool state under root, until ctx ends. It knows the runtime
// only as sandboxruntime.Runtime; what kind it is, and anything only that kind
// needs, is the caller's.
//
// A nil reporter runs no loop that reports to the control plane: no state
// channel, tree reaper, status poll or resource report. A nil assets serves
// no assets.discobox.internal.
func Serve(ctx context.Context, logger *slog.Logger, root layout.Root, bootstrap Bootstrap, identity *Identity, runtime sandboxruntime.Runtime, reporter SandboxStateClient, assets *assetcache.Cache) error {
	if reporter != nil {
		if identity == nil {
			return errors.New("the pool's identity is required for sandbox state reporting")
		}
		// One boot ID per agent process, and a sequence within it. The control
		// plane uses the pair to drop a delayed delta that would otherwise
		// overwrite a newer complete sync (ADR 0017 §10).
		bootID := id.NewString(id.PrefixPoolAgentBoot)
		var sequence atomic.Int64
		go runtime.WatchSandboxStates(ctx, logger, func(reportCtx context.Context, batch sandboxruntime.SandboxStateBatch) error {
			states := make([]SandboxState, 0, len(batch.States))
			for _, observed := range batch.States {
				states = append(states, SandboxState{
					SandboxID: observed.SandboxID,
					State:     observed.State,
					Error:     observed.Error,
				})
			}
			return reporter.ReportSandboxStates(reportCtx, SandboxStateRequest{
				ControlPlaneURL: bootstrap.ControlPlaneURL,
				ProjectID:       bootstrap.ProjectID,
				PoolID:          bootstrap.PoolID,
				PrivateKey:      identity.PrivateKey,
				BootID:          bootID,
				Sequence:        sequence.Add(1),
				ReportedAt:      batch.ReportedAt,
				Complete:        batch.Complete,
				States:          states,
			})
		})
		// Provisioning progress rides the same channel, reported by whoever is
		// doing the work rather than derived from what the runtime observes, so it
		// is a sink to hold rather than a stream to watch (ADR 0039). It shares
		// the boot id and sequence, so the control plane orders progress and
		// state against each other exactly as it already orders state.
		go runtime.WatchSandboxProgress(ctx, func(reportCtx context.Context, observed sandboxruntime.SandboxProgressObservation) error {
			progress := SandboxProgress{SandboxID: observed.SandboxID, Phase: observed.Phase}
			if observed.Pull != nil {
				progress.Pull = &SandboxPullProgress{
					Image:          observed.Pull.Image,
					Layers:         observed.Pull.Layers,
					LayersComplete: observed.Pull.LayersComplete,
					Current:        observed.Pull.Current,
					Total:          observed.Pull.Total,
					Done:           observed.Pull.Done,
				}
			}
			if observed.Clone != nil {
				progress.Clone = &SandboxCloneProgress{
					Source:       observed.Clone.Source,
					Stage:        observed.Clone.Stage,
					Objects:      observed.Clone.Objects,
					ObjectsTotal: observed.Clone.ObjectsTotal,
					Bytes:        observed.Clone.Bytes,
				}
			}
			return reporter.ReportSandboxStates(reportCtx, SandboxStateRequest{
				ControlPlaneURL: bootstrap.ControlPlaneURL,
				ProjectID:       bootstrap.ProjectID,
				PoolID:          bootstrap.PoolID,
				PrivateKey:      identity.PrivateKey,
				BootID:          bootID,
				Sequence:        sequence.Add(1),
				ReportedAt:      time.Now().UTC(),
				// A progress report carries no state observation, so States is
				// empty and Complete is false: a complete sync's "every sandbox
				// I host" claim is about States, and marking this one complete
				// would read as "this pool hosts nothing".
				States:   []SandboxState{},
				Progress: []SandboxProgress{progress},
			})
		})
		// The control plane, not this pool, decides which sandboxes exist, so the
		// tree reaper runs only where it can be asked (ADR 26-10-01-876). An agent
		// with no channel to it reaps no sandbox tree.
		go runtime.WatchSandboxVolumes(ctx, logger, func(listCtx context.Context) ([]string, error) {
			return reporter.ListHeldSandboxes(listCtx, HeldSandboxesRequest{
				ControlPlaneURL: bootstrap.ControlPlaneURL,
				ProjectID:       bootstrap.ProjectID,
				PoolID:          bootstrap.PoolID,
				PrivateKey:      identity.PrivateKey,
			})
		})
		// reporter's concrete client (HTTPClient in production) also implements
		// SandboxAgentStatusClient; test doubles that only implement
		// SandboxStateClient simply skip starting the poller.
		if statusClient, ok := reporter.(SandboxAgentStatusClient); ok {
			poller := startSandboxAgentStatusPoller(ctx, logger, bootstrap, identity, runtime, statusClient)
			// The resource reporter reads the counters that poll already
			// collected rather than polling every sandbox a second time, so it
			// only runs where the poller does (ADR 0071 resource accounting §2).
			if resourceClient, ok := reporter.(PoolResourceClient); ok && poller != nil {
				startPoolResourceReporter(ctx, logger, root, bootstrap, identity, runtime, poller, resourceClient)
			}
		}
	}
	go runtime.WatchProxyMaterial(ctx, logger)
	// The listen URL's scheme selects the transport, so a VSOCK-only or
	// socket-only pool needs no special case here.
	listener, err := wire.Listen(bootstrap.AgentListenURL)
	if err != nil {
		return fmt.Errorf("listen on pool-agent endpoint %q: %w", bootstrap.AgentListenURL, err)
	}
	defer func() { _ = listener.Close() }()
	// Audit stays a nil interface when the key cannot be read, so the relay
	// answers 503 rather than the agent failing to serve anything at all.
	var audit poolserver.AuditReader
	if auditClient, err := proxyagent.NewAuditClient(root, bootstrap.ProjectID, bootstrap.PoolID); err != nil {
		logger.Warn("proxy audit relay disabled", "error", err)
	} else {
		audit = auditClient
	}
	// The git-origins route verifies the tokens this pool issues its sandboxes
	// against its own identity key (ADR 0126 §4).
	var sandboxTokenKey ed25519.PublicKey
	if identity != nil {
		// An ed25519.PrivateKey's public half is always an ed25519.PublicKey.
		sandboxTokenKey, _ = identity.PrivateKey.Public().(ed25519.PublicKey)
	}
	// The sandboxes' own fetches of their origins arrive through the pool
	// proxy, on loopback (ADR 26-10-08-561). Without the pool's key there is
	// no token to accept, so nothing listens.
	var originListener net.Listener
	if sandboxTokenKey != nil {
		var listenConfig net.ListenConfig
		originListener, err = listenConfig.Listen(ctx, "tcp", proxyagent.OriginsListenAddress)
		if err != nil {
			return fmt.Errorf("listen for sandbox origins on %s: %w", proxyagent.OriginsListenAddress, err)
		}
		defer func() { _ = originListener.Close() }()
	}
	// The sandboxes' fetches of their bundles arrive through the pool proxy
	// too, on a socket of their own that no box can name (ADR 26-10-16-423
	// §13, proxyagent.AssetsHost).
	var assetListener net.Listener
	var assetHandler http.Handler
	if assets != nil {
		socket := root.ProxyAssetsSocket(bootstrap.ProjectID, bootstrap.PoolID)
		if assetListener, err = listenProxySocket(socket); err != nil {
			return fmt.Errorf("listen for sandbox assets on %s: %w", socket, err)
		}
		defer func() { _ = assetListener.Close() }()
		assetHandler = assets.Handler()
		assets.StartSweeping(ctx, logger)
	}
	return poolserver.Serve(ctx, logger, poolserver.Config{
		Identity: poolserver.Identity{
			ProjectID: bootstrap.ProjectID,
			PoolID:    bootstrap.PoolID,
		},
		Registration:          serverRegistration(identity),
		Runtime:               runtime,
		Audit:                 audit,
		ControlPlanePublicKey: bootstrap.ControlPlaneKey,
		SandboxTokenKey:       sandboxTokenKey,
		Listener:              listener,
		OriginListener:        originListener,
		AssetListener:         assetListener,
		Assets:                assetHandler,
	})
}

func serverRegistration(identity *Identity) *poolserver.Registration {
	if identity == nil {
		return nil
	}
	return &poolserver.Registration{PublicKey: identity.PublicKey}
}

func availableCPUVCPUs() float64 {
	return float64(runtime.NumCPU())
}

// poolFreeStorageBytes is what the scheduler places against: the space an
// unprivileged writer can still use on the filesystem behind a path. What the
// pool actually holds, broken down by sandbox, rides the resource report
// instead (ADR 0071 resource accounting) — this one number is a scheduling input, not accounting.
func poolFreeStorageBytes(path string) int64 {
	usage, ok := filesystemUsage(path)
	if !ok {
		return 0
	}
	return usage.FreeBytes
}

// totalMemoryBytes is how much memory the host has, which is what a usage
// figure is read against. It is the denominator to availableMemoryBytes's
// remainder.
func totalMemoryBytes() int64 {
	return meminfoBytes("MemTotal:")
}

// sandboxSharedMemoryBytes is the /dev/shm every sandbox gets: half the memory
// the pool can use, which is what the kernel sizes a tmpfs at by default and so
// what /dev/shm is on an ordinary Linux machine. What the pool can use is its
// container's cgroup limit when it has one, and the host's memory otherwise. The
// size is a cap on the tmpfs, not memory set aside, so every sandbox gets the
// same half rather than a share of it. Zero when neither is known, which leaves
// Docker's default.
func sandboxSharedMemoryBytes(cgroupRoot string) int64 {
	memory := totalMemoryBytes()
	if limit, ok := readCgroupInt(filepath.Join(cgroupRoot, "memory.max")); ok && limit > 0 && (memory <= 0 || limit < memory) {
		memory = limit
	}
	if memory <= 0 {
		return 0
	}
	return memory / 2
}

func availableMemoryBytes() int64 {
	return meminfoBytes("MemAvailable:")
}

func meminfoBytes(key string) int64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == key {
			kib, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0
			}
			return kib * 1024
		}
	}
	return 0
}

// RunBuildkitMediator serves the pool's BuildKit mediator, and the registry
// front beside it. It is the entrypoint for the mediator systemd unit inside
// the pool container.
//
// The mediator is the only route from a sandbox to buildkitd, and it
// terminates mTLS with the same certificate bundle the proxy uses, so a build
// is attributed to the sandbox whose client certificate opened the connection
// (ADR 0044 decision 2). The front is the only route from a sandbox to the
// registry those builds push to, as the sandbox the proxy names (ADR
// 26-10-29-160 §2); the two are the builder's, and stand and fall with it.
func RunBuildkitMediator(ctx context.Context, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	projectID := strings.TrimSpace(os.Getenv("DISCOBOX_PROJECT_ID"))
	poolID := strings.TrimSpace(os.Getenv("DISCOBOX_POOL_ID"))
	if projectID == "" || poolID == "" {
		return fmt.Errorf("mediator unit environment names no pool")
	}
	bundle, err := proxyagent.PrepareBundle(layout.Container(), projectID, poolID)
	if err != nil {
		return fmt.Errorf("prepare mediator certificates: %w", err)
	}
	tlsConfig, err := buildkitagent.ClientTLSConfig(bundle.ServerCertPath, bundle.ServerKeyPath, bundle.MTLSCAPath)
	if err != nil {
		return err
	}
	mediator, err := buildkitagent.NewMediator(logger)
	if err != nil {
		return err
	}
	defer mediator.Close()

	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", buildkitagent.MediatorListen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", buildkitagent.MediatorListen, err)
	}
	socket := layout.Container().ProxyRegistrySocket(projectID, poolID)
	frontListener, err := listenProxySocket(socket)
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("listen for the registry front on %s: %w", socket, err)
	}
	registryTLS, err := buildkitagent.RegistryClientTLS(layout.Container(), projectID, poolID)
	if err != nil {
		_ = listener.Close()
		_ = frontListener.Close()
		return err
	}
	front := &http.Server{
		Handler: buildkitagent.NewRegistryFront(&url.URL{Scheme: "https", Host: buildkitagent.RegistryHost},
			&http.Transport{Proxy: nil, TLSClientConfig: registryTLS, ForceAttemptHTTP2: false}, logger),
		ReadHeaderTimeout: 30 * time.Second,
	}
	frontDone := make(chan error, 1)
	go func() {
		logger.Info("registry front serving", "socket", socket)
		frontDone <- front.Serve(frontListener)
	}()
	mediatorDone := make(chan error, 1)
	go func() {
		logger.Info("buildkit mediator serving", "addr", buildkitagent.MediatorListen)
		mediatorDone <- mediator.Serve(ctx, listener, tlsConfig)
	}()
	select {
	case err := <-mediatorDone:
		_ = front.Close()
		return err
	case err := <-frontDone:
		// The mediator drains on ctx alone, so a front that stopped first takes
		// the unit down hard, and systemd brings both back.
		_ = listener.Close()
		return fmt.Errorf("registry front stopped: %w", err)
	}
}

// ensurePoolName names the pool's own host by its server name
// (proxyagent.ServerName) in hostsFile, unless a line there already does: a
// build step's forwarder dials the pool proxy by that name, which listens on
// every interface of this host. It never asks DNS: a resolver that answered
// the name would be handed every build's egress, with its sandbox's
// certificate. A pool machine's network has no alias for it; a Docker pool's
// container has one, and loopback reaches the same services.
func ensurePoolName(hostsFile string) error {
	data, err := os.ReadFile(hostsFile) //nolint:gosec // G304: the host's /etc/hosts
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("name the pool in %s: %w", hostsFile, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(strings.SplitN(line, "#", 2)[0]); len(fields) > 1 && slices.Contains(fields[1:], proxyagent.ServerName) {
			return nil
		}
	}
	f, err := os.OpenFile(hostsFile, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644) //nolint:gosec // G302: /etc/hosts is world-readable by design
	if err != nil {
		return fmt.Errorf("name the pool in %s: %w", hostsFile, err)
	}
	_, werr := fmt.Fprintf(f, "\n# The pool's own services, by the name they serve under (discobox).\n127.0.0.1 %s\n", proxyagent.ServerName)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("name the pool in %s: %w", hostsFile, werr)
	}
	return nil
}

// listenProxySocket binds a socket the pool proxy forwards a pool host to (the
// asset cache's, the registry front's), replacing one an earlier process left,
// readable and writable by root alone: the proxy unit is root, and nothing else
// in the pool has a reason to reach it.
func listenProxySocket(socket string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		return nil, err
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	listener, err := wire.Listen(wire.UnixURL(socket))
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}
