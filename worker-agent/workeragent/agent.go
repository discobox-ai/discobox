package workeragent

import (
	"context"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/obot-platform/discobox/worker-agent/workeragent/sandboxruntime"
	workerserver "github.com/obot-platform/discobox/worker-agent/workeragent/server"
	agentsystemd "github.com/obot-platform/discobox/worker-agent/workeragent/systemd"
)

// RunAgent registers the worker, marks it ready, and serves the worker-agent HTTP endpoints.
func RunAgent(ctx context.Context, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	systemd, err := agentsystemd.StartNamespace(ctx, logger)
	if err != nil {
		return err
	}
	stopReaper := agentsystemd.StartChildReaper(ctx, logger, agentsystemd.ManagedChildProcesses(systemd))
	defer stopReaper()
	defer agentsystemd.Stop(systemd)

	bootstrap := FromEnv()
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
		WorkerID:              bootstrap.WorkerID,
		AuthToken:             registration.AuthToken,
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

	return Serve(ctx, logger, bootstrap, registration)
}

// ExecSystemdChildIfRequested starts the child systemd process when requested by the systemd helper.
func ExecSystemdChildIfRequested() error {
	return agentsystemd.ExecSystemdChildIfRequested()
}

// Serve starts the worker-agent HTTP server.
func Serve(ctx context.Context, logger *slog.Logger, bootstrap Bootstrap, registration *Registration) error {
	runtime, err := sandboxruntime.NewDockerSandboxRuntime(bootstrap.ProjectID, bootstrap.WorkerID)
	if err != nil {
		return err
	}
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
		Registration: serverRegistration(registration),
		Runtime:      runtime,
		AuthTokens:   workerSandboxAuthTokens(bootstrap, registration),
		Port:         bootstrap.AgentPort,
	})
}

func serverRegistration(registration *Registration) *workerserver.Registration {
	if registration == nil {
		return nil
	}
	return &workerserver.Registration{PublicKey: registration.PublicKey, AuthToken: registration.AuthToken}
}

func workerSandboxAuthTokens(bootstrap Bootstrap, registration *Registration) []string {
	tokens := []string{bootstrap.Token}
	if registration != nil {
		tokens = append(tokens, registration.AuthToken)
	}
	return tokens
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
