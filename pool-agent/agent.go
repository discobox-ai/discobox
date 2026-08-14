package poolagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/obot-platform/discobox/id"

	"github.com/obot-platform/discobox/layout"
	"github.com/obot-platform/discobox/pool-agent/buildkitagent"
	"github.com/obot-platform/discobox/pool-agent/endpoint"
	"github.com/obot-platform/discobox/pool-agent/poolauth"
	"github.com/obot-platform/discobox/pool-agent/proxyagent"
	"github.com/obot-platform/discobox/pool-agent/sandboxruntime"
	poolserver "github.com/obot-platform/discobox/pool-agent/server"
	agentsystemd "github.com/obot-platform/discobox/pool-agent/systemd"
)

// RunProxy runs the pool-scoped proxy server. It is the entrypoint for the
// proxy systemd unit inside the pool container.
func RunProxy(ctx context.Context, logger *slog.Logger) error {
	return proxyagent.RunProxy(ctx, logger)
}

// RunAgent registers the pool, marks it ready, and serves the pool-agent HTTP endpoints.
func RunAgent(ctx context.Context, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	bootstrap := FromEnv()
	// Publish the host-mount prefix for the proxy systemd unit, then prepare the
	// proxy certificate bundle before booting systemd so the proxy unit and
	// per-sandbox client certificates share a consistent CA without racing on
	// first-time generation.
	if err := proxyagent.WriteUnitEnvironment(bootstrap.HostMountPrefix, bootstrap.ControlPlaneURL, bootstrap.ProjectID, bootstrap.PoolID); err != nil {
		return err
	}
	bundle, err := proxyagent.PrepareBundle(bootstrap.ProjectID, bootstrap.PoolID)
	if err != nil {
		return err
	}
	// Render the shared builder's and its registry's configuration for the same
	// reason and at the same point: both run as systemd units with a clean
	// environment, and every path they own is pool-scoped (ADR 0039).
	if err := buildkitagent.Prepare(bootstrap.ProjectID, bootstrap.PoolID, bundle.MITMCAPath); err != nil {
		return err
	}
	systemd, err := agentsystemd.StartNamespace(ctx, logger)
	if err != nil {
		return err
	}
	stopReaper := agentsystemd.StartChildReaper(ctx, logger, agentsystemd.ManagedChildProcesses(systemd))
	defer stopReaper()
	defer agentsystemd.Stop(systemd)

	// The control plane URL's scheme selects the transport; nothing here knows
	// whether that is IP, VSOCK, or a Unix socket served by a guest helper.
	baseURL, controlPlaneHTTPClient, err := endpoint.HTTPClient(bootstrap.ControlPlaneURL, 0)
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
	registration, err := Run(ctx, Config{Bootstrap: bootstrap, Client: client})
	if err != nil {
		return err
	}
	logger.Info("pool registered", "poolID", bootstrap.PoolID)

	if err := startStatusReporter(ctx, logger, bootstrap, registration, client, statusReportInterval); err != nil {
		return err
	}

	startResolveTokenRefresher(ctx, logger, bootstrap, registration)

	return Serve(ctx, logger, bootstrap, registration, client)
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
func startStatusReporter(ctx context.Context, logger *slog.Logger, bootstrap Bootstrap, registration *Registration, client StatusClient, interval time.Duration) error {
	conditions := map[string]any{
		"agent": map[string]any{
			"version": "dev",
			"status":  "ready",
		},
	}
	report := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, statusReportTimeout)
		defer cancel()
		return client.UpdatePoolStatus(ctx, StatusRequest{
			ControlPlaneURL:      bootstrap.ControlPlaneURL,
			ProjectID:            bootstrap.ProjectID,
			PoolID:               bootstrap.PoolID,
			PrivateKey:           registration.PrivateKey,
			Ready:                true,
			Schedulable:          true,
			Degraded:             false,
			AvailableCPUVCPUs:    availableCPUVCPUs(),
			AvailableMemoryBytes: availableMemoryBytes(),
			// Stat this project's own data tree. Its parent belongs to the pool
			// container's root filesystem and would report the wrong backing
			// filesystem.
			AvailableStorageBytes: availableStorageBytes(layout.ProjectData(bootstrap.ProjectID)),
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

// startResolveTokenRefresher mints the scoped secret:resolve token the proxy
// unit uses and writes it, with the control-plane URL, to this pool's own
// resolve-context file, refreshing it before expiry.
func startResolveTokenRefresher(ctx context.Context, logger *slog.Logger, bootstrap Bootstrap, registration *Registration) {
	write := func() error {
		token, err := poolauth.CreateTokenWithTTL(registration.PrivateKey, poolauth.Claims{
			ProjectID: bootstrap.ProjectID,
			PoolID:    bootstrap.PoolID,
			Scopes:    []string{poolauth.ScopeSecretResolve},
		}, resolveTokenTTL)
		if err != nil {
			return err
		}
		return proxyagent.WriteResolveContext(bootstrap.ProjectID, bootstrap.PoolID, bootstrap.ControlPlaneURL, token)
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

// Serve starts the pool-agent HTTP server.
func Serve(ctx context.Context, logger *slog.Logger, bootstrap Bootstrap, registration *Registration, reporters ...SandboxStateClient) error {
	runtime, err := sandboxruntime.NewDockerSandboxRuntime(sandboxruntime.DockerSandboxRuntimeConfig{
		ProjectID:             bootstrap.ProjectID,
		PoolID:                bootstrap.PoolID,
		ControlPlanePublicKey: bootstrap.ControlPlaneKey,
		HostMountPrefix:       bootstrap.HostMountPrefix,
		HostStateRoot:         bootstrap.HostStateRoot,
	})
	if err != nil {
		return err
	}
	var reporter SandboxStateClient
	if len(reporters) > 0 {
		reporter = reporters[0]
	}
	if reporter != nil {
		if registration == nil {
			return errors.New("pool registration is required for sandbox state reporting")
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
				PrivateKey:      registration.PrivateKey,
				BootID:          bootID,
				Sequence:        sequence.Add(1),
				ReportedAt:      batch.ReportedAt,
				Complete:        batch.Complete,
				States:          states,
			})
		})
		// Provisioning progress rides the same channel, reported by whoever is
		// doing the work rather than derived from the Docker event stream, so it
		// is a sink to hold rather than a stream to watch (ADR 0039). It shares
		// the boot id and sequence, so the control plane orders progress and
		// state against each other exactly as it already orders state.
		go runtime.WatchSandboxProgress(ctx, func(reportCtx context.Context, observed sandboxruntime.SandboxProgressObservation) error {
			progress := SandboxProgress{SandboxID: observed.SandboxID}
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
			return reporter.ReportSandboxStates(reportCtx, SandboxStateRequest{
				ControlPlaneURL: bootstrap.ControlPlaneURL,
				ProjectID:       bootstrap.ProjectID,
				PoolID:          bootstrap.PoolID,
				PrivateKey:      registration.PrivateKey,
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
		// reporter's concrete client (HTTPClient in production) also implements
		// SandboxAgentStatusClient; test doubles that only implement
		// SandboxStateClient simply skip starting the poller.
		if statusClient, ok := reporter.(SandboxAgentStatusClient); ok {
			startSandboxAgentStatusPoller(ctx, logger, bootstrap, registration, runtime, statusClient)
		}
	}
	go runtime.WatchProxyMaterial(ctx, logger)
	go runtime.WatchImages(ctx, logger)
	return ServeWithRuntime(ctx, logger, bootstrap, registration, runtime)
}

// ServeWithRuntime starts the pool-agent HTTP server with an explicit sandbox runtime.
func ServeWithRuntime(ctx context.Context, logger *slog.Logger, bootstrap Bootstrap, registration *Registration, runtime sandboxruntime.Runtime) error {
	// The listen URL's scheme selects the transport, so a VSOCK-only or
	// socket-only pool needs no special case here.
	listener, err := endpoint.Listen(bootstrap.AgentListenURL)
	if err != nil {
		return fmt.Errorf("listen on pool-agent endpoint %q: %w", bootstrap.AgentListenURL, err)
	}
	defer func() { _ = listener.Close() }()
	return poolserver.Serve(ctx, logger, poolserver.Config{
		Identity: poolserver.Identity{
			ProjectID: bootstrap.ProjectID,
			PoolID:    bootstrap.PoolID,
		},
		Registration:          serverRegistration(registration),
		Runtime:               runtime,
		ControlPlanePublicKey: bootstrap.ControlPlaneKey,
		Listener:              listener,
	})
}

func serverRegistration(registration *Registration) *poolserver.Registration {
	if registration == nil {
		return nil
	}
	return &poolserver.Registration{PublicKey: registration.PublicKey}
}

func availableCPUVCPUs() float64 {
	return float64(runtime.NumCPU())
}

func availableMemoryBytes() int64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "MemAvailable:" {
			kib, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0
			}
			return kib * 1024
		}
	}
	return 0
}

// RunBuildkitMediator serves the pool's BuildKit mediator. It is the entrypoint
// for the mediator systemd unit inside the pool container.
//
// It is the only route from a sandbox to buildkitd, and it terminates mTLS with
// the same certificate bundle the proxy uses, so a build is attributed to the
// sandbox whose client certificate opened the connection (ADR 0039 decision 2).
func RunBuildkitMediator(ctx context.Context, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	projectID := strings.TrimSpace(os.Getenv("DISCOBOX_PROJECT_ID"))
	poolID := strings.TrimSpace(os.Getenv("DISCOBOX_POOL_ID"))
	if projectID == "" || poolID == "" {
		return fmt.Errorf("mediator unit environment names no pool")
	}
	bundle, err := proxyagent.PrepareBundle(projectID, poolID)
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
	logger.Info("buildkit mediator serving", "addr", buildkitagent.MediatorListen)
	return mediator.Serve(ctx, listener, tlsConfig)
}
