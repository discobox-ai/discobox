package poolagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/obot-platform/discobox/pool-agent/poolauth"
	"github.com/obot-platform/discobox/pool-agent/proxyagent"
	"github.com/obot-platform/discobox/pool-agent/sandboxruntime"
	poolserver "github.com/obot-platform/discobox/pool-agent/server"
	agentsystemd "github.com/obot-platform/discobox/pool-agent/systemd"
	guestvsock "github.com/obot-platform/discobox/pool-agent/vsock"
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
	if err := proxyagent.WriteUnitEnvironment(bootstrap.HostMountPrefix, bootstrap.ControlPlaneVSOCKPort); err != nil {
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

	controlPlaneHTTPClient := http.DefaultClient
	if bootstrap.ControlPlaneVSOCKPort > 0 {
		controlPlaneHTTPClient = guestvsock.HTTPClient(bootstrap.ControlPlaneVSOCKPort, 0)
	}
	client := NewHTTPClient(bootstrap.ControlPlaneURL, WithHTTPClient(controlPlaneHTTPClient))
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
		// Stat the bind-mounted projects directory itself. Its parent beneath
		// /host belongs to the pool container's root filesystem and would
		// report the wrong backing filesystem.
		AvailableStorageBytes: availableStorageBytes(proxyagent.Resolver(bootstrap.HostMountPrefix)("/var/lib/discobox/projects")),
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
// unit uses and writes it (with the control-plane URL and pool ID) to the
// shared resolve-context file, refreshing it before expiry.
func startResolveTokenRefresher(ctx context.Context, logger *slog.Logger, bootstrap Bootstrap, registration *Registration) {
	hostDirFor := proxyagent.Resolver(bootstrap.HostMountPrefix)
	write := func() error {
		token, err := poolauth.CreateTokenWithTTL(registration.PrivateKey, poolauth.Claims{
			ProjectID: bootstrap.ProjectID,
			PoolID:    bootstrap.PoolID,
			Scopes:    []string{poolauth.ScopeSecretResolve},
		}, resolveTokenTTL)
		if err != nil {
			return err
		}
		return proxyagent.WriteResolveContext(hostDirFor, bootstrap.ControlPlaneURL, bootstrap.PoolID, token)
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
	var listener net.Listener
	if bootstrap.AgentVSOCKPort > 0 {
		var err error
		listener, err = guestvsock.Listen(bootstrap.AgentVSOCKPort)
		if err != nil {
			return fmt.Errorf("listen on pool-agent VSOCK port %d: %w", bootstrap.AgentVSOCKPort, err)
		}
		defer listener.Close()
	}
	return poolserver.Serve(ctx, logger, poolserver.Config{
		Identity: poolserver.Identity{
			ProjectID: bootstrap.ProjectID,
			PoolID:    bootstrap.PoolID,
		},
		Registration:          serverRegistration(registration),
		Runtime:               runtime,
		ControlPlanePublicKey: bootstrap.ControlPlaneKey,
		Port:                  bootstrap.AgentPort,
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
