package poolagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/obot-platform/discobox/layout"
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
	if _, err := proxyagent.PrepareBundle(bootstrap.ProjectID, bootstrap.PoolID); err != nil {
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
func Serve(ctx context.Context, logger *slog.Logger, bootstrap Bootstrap, registration *Registration, reporters ...SandboxRemovalClient) error {
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
	var reporter SandboxRemovalClient
	if len(reporters) > 0 {
		reporter = reporters[0]
	}
	if reporter != nil && registration == nil {
		return errors.New("pool registration is required for sandbox removal reporting")
	}
	go runtime.WatchSandboxRemovals(ctx, logger, func(reportCtx context.Context, sandboxID, containerID string) error {
		if reporter == nil {
			return nil
		}
		return reporter.ReportSandboxRemoved(reportCtx, SandboxRemovalRequest{
			ControlPlaneURL: bootstrap.ControlPlaneURL,
			ProjectID:       bootstrap.ProjectID,
			PoolID:          bootstrap.PoolID,
			PrivateKey:      registration.PrivateKey,
			SandboxID:       sandboxID,
			ContainerID:     containerID,
		})
	})
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
