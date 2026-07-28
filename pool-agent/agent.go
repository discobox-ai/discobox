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

	conditions := map[string]any{
		"agent": map[string]any{
			"version": "dev",
			"status":  "ready",
		},
	}
	if err := client.UpdatePoolStatus(ctx, StatusRequest{
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
	}); err != nil {
		return err
	}
	logger.Info("pool marked ready", "poolID", bootstrap.PoolID)

	startResolveTokenRefresher(ctx, logger, bootstrap, registration)

	return Serve(ctx, logger, bootstrap, registration, client)
}

const (
	resolveTokenTTL     = 30 * time.Minute
	resolveTokenRefresh = 20 * time.Minute
)

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
