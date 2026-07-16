package workeragent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/obot-platform/discobox/worker-agent/proxyagent"
	"github.com/obot-platform/discobox/worker-agent/sandboxruntime"
	workerserver "github.com/obot-platform/discobox/worker-agent/server"
	agentsystemd "github.com/obot-platform/discobox/worker-agent/systemd"
	"github.com/obot-platform/discobox/worker-agent/workerauth"
)

// RunProxy runs the worker-scoped proxy server. It is the entrypoint for the
// proxy systemd unit inside the worker container.
func RunProxy(ctx context.Context, logger *slog.Logger) error {
	return proxyagent.RunProxy(ctx, logger)
}

// RunAgent registers the worker, marks it ready, and serves the worker-agent HTTP endpoints.
func RunAgent(ctx context.Context, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	bootstrap := FromEnv()
	// Publish the host-mount prefix for the proxy systemd unit, then prepare the
	// proxy certificate bundle before booting systemd so the proxy unit and
	// per-sandbox client certificates share a consistent CA without racing on
	// first-time generation.
	if err := proxyagent.WriteUnitEnvironment(bootstrap.HostMountPrefix); err != nil {
		return err
	}
	if _, err := proxyagent.PrepareBundle(proxyagent.Resolver(bootstrap.HostMountPrefix)); err != nil {
		return err
	}
	systemd, err := agentsystemd.StartNamespace(ctx, logger)
	if err != nil {
		return err
	}
	stopReaper := agentsystemd.StartChildReaper(ctx, logger, agentsystemd.ManagedChildProcesses(systemd))
	defer stopReaper()
	defer agentsystemd.Stop(systemd)

	registration, err := Run(ctx, Config{Bootstrap: bootstrap})
	if err != nil {
		return err
	}
	logger.Info("worker registered", "workerID", bootstrap.WorkerID)

	client := NewHTTPClient(bootstrap.ControlPlaneURL)
	conditions := map[string]any{
		"agent": map[string]any{
			"version": "dev",
			"status":  "ready",
		},
	}
	if err := client.UpdateWorkerStatus(ctx, StatusRequest{
		ControlPlaneURL:       bootstrap.ControlPlaneURL,
		ProjectID:             bootstrap.ProjectID,
		WorkerID:              bootstrap.WorkerID,
		PrivateKey:            registration.PrivateKey,
		Ready:                 true,
		Schedulable:           true,
		Degraded:              false,
		AvailableCPUVCPUs:     availableCPUVCPUs(),
		AvailableMemoryBytes:  availableMemoryBytes(),
		AvailableStorageBytes: availableStorageBytes("/"),
		Conditions:            conditions,
	}); err != nil {
		return err
	}
	logger.Info("worker marked ready", "workerID", bootstrap.WorkerID)

	startResolveTokenRefresher(ctx, logger, bootstrap, registration)

	return Serve(ctx, logger, bootstrap, registration, client)
}

const (
	resolveTokenTTL     = 30 * time.Minute
	resolveTokenRefresh = 20 * time.Minute
)

// startResolveTokenRefresher mints the scoped secret:resolve token the proxy
// unit uses and writes it (with the control-plane URL and worker ID) to the
// shared resolve-context file, refreshing it before expiry.
func startResolveTokenRefresher(ctx context.Context, logger *slog.Logger, bootstrap Bootstrap, registration *Registration) {
	hostDirFor := proxyagent.Resolver(bootstrap.HostMountPrefix)
	write := func() error {
		token, err := workerauth.CreateTokenWithTTL(registration.PrivateKey, workerauth.Claims{
			ProjectID: bootstrap.ProjectID,
			WorkerID:  bootstrap.WorkerID,
			Scopes:    []string{workerauth.ScopeSecretResolve},
		}, resolveTokenTTL)
		if err != nil {
			return err
		}
		return proxyagent.WriteResolveContext(hostDirFor, bootstrap.ControlPlaneURL, bootstrap.WorkerID, token)
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

// Serve starts the worker-agent HTTP server.
func Serve(ctx context.Context, logger *slog.Logger, bootstrap Bootstrap, registration *Registration, reporters ...SandboxRemovalClient) error {
	runtime, err := sandboxruntime.NewDockerSandboxRuntime(sandboxruntime.DockerSandboxRuntimeConfig{
		ProjectID:             bootstrap.ProjectID,
		WorkerID:              bootstrap.WorkerID,
		ControlPlanePublicKey: bootstrap.ControlPlaneKey,
		HostMountPrefix:       bootstrap.HostMountPrefix,
	})
	if err != nil {
		return err
	}
	var reporter SandboxRemovalClient
	if len(reporters) > 0 {
		reporter = reporters[0]
	}
	if reporter != nil && registration == nil {
		return errors.New("worker registration is required for sandbox removal reporting")
	}
	go runtime.WatchSandboxRemovals(ctx, logger, func(reportCtx context.Context, sandboxID string) error {
		if reporter == nil {
			return nil
		}
		return reporter.ReportSandboxRemoved(reportCtx, SandboxRemovalRequest{
			ControlPlaneURL: bootstrap.ControlPlaneURL,
			ProjectID:       bootstrap.ProjectID,
			WorkerID:        bootstrap.WorkerID,
			PrivateKey:      registration.PrivateKey,
			SandboxID:       sandboxID,
		})
	})
	return ServeWithRuntime(ctx, logger, bootstrap, registration, runtime)
}

// ServeWithRuntime starts the worker-agent HTTP server with an explicit sandbox runtime.
func ServeWithRuntime(ctx context.Context, logger *slog.Logger, bootstrap Bootstrap, registration *Registration, runtime sandboxruntime.Runtime) error {
	return workerserver.Serve(ctx, logger, workerserver.Config{
		Identity: workerserver.Identity{
			ProjectID: bootstrap.ProjectID,
			SandboxID: bootstrap.SandboxID,
			WorkerID:  bootstrap.WorkerID,
		},
		Registration:          serverRegistration(registration),
		Runtime:               runtime,
		ControlPlanePublicKey: bootstrap.ControlPlaneKey,
		Port:                  bootstrap.AgentPort,
	})
}

func serverRegistration(registration *Registration) *workerserver.Registration {
	if registration == nil {
		return nil
	}
	return &workerserver.Registration{PublicKey: registration.PublicKey}
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
