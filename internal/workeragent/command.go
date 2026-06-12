package workeragent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// RunCommand registers the worker, marks it ready, and serves the worker-agent HTTP endpoints.
func RunCommand(ctx context.Context, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	systemd, err := startSystemdNamespace(ctx, logger)
	if err != nil {
		return err
	}
	stopReaper := startChildReaper(ctx, logger, managedChildProcesses(systemd))
	defer stopReaper()
	defer stopSystemd(systemd)

	bootstrap := FromEnv()
	registration, err := Run(ctx, Config{Bootstrap: bootstrap})
	if err != nil {
		return err
	}
	logger.Info("worker registered", "workerID", bootstrap.WorkerID, "tenantID", bootstrap.TenantID)

	client := NewHTTPClient(bootstrap.ControlPlaneURL)
	conditions := map[string]any{
		"agent": map[string]any{
			"version": "dev",
			"status":  "ready",
		},
	}
	if err := client.UpdateWorkerStatus(ctx, StatusRequest{
		ControlPlaneURL:       bootstrap.ControlPlaneURL,
		TenantID:              bootstrap.TenantID,
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

func stopSystemd(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
}

// Serve starts the worker-agent HTTP server.
func Serve(ctx context.Context, logger *slog.Logger, bootstrap Bootstrap, registration *Registration) error {
	if logger == nil {
		logger = slog.Default()
	}
	port := bootstrap.AgentPort
	if port == 0 {
		port = envInt("PORT", 3002)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ready": true, "schedulable": true})
	})
	mux.HandleFunc("/metadata", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		metadata := map[string]any{
			"tenantId":  bootstrap.TenantID,
			"projectId": bootstrap.ProjectID,
			"sandboxId": bootstrap.SandboxID,
			"workerId":  bootstrap.WorkerID,
		}
		if registration != nil {
			metadata["publicKey"] = registration.PublicKey
		}
		_ = json.NewEncoder(w).Encode(metadata)
	})

	server := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: mux}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("worker agent serving", "addr", server.Addr)
		errCh <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		return ctx.Err()
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
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

func availableStorageBytes(path string) int64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0
	}
	return int64(stat.Bavail) * stat.Bsize
}
