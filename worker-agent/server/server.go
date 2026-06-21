package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	workerapi "github.com/obot-platform/discobox/worker-agent/api/gen"
	"github.com/obot-platform/discobox/worker-agent/sandboxruntime"
)

type Registration struct {
	PublicKey string
	AuthToken string
}

type Config struct {
	Identity     Identity
	Registration *Registration
	Runtime      sandboxruntime.Runtime
	AuthTokens   []string
	Port         int
}

func NewRouter(cfg Config) (*chi.Mux, error) {
	router := chi.NewRouter()
	router.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	router.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ready": true, "schedulable": true})
	})
	router.HandleFunc("/metadata", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		metadata := map[string]any{
			"projectId": cfg.Identity.ProjectID,
			"sandboxId": cfg.Identity.SandboxID,
			"workerId":  cfg.Identity.WorkerID,
		}
		if cfg.Registration != nil {
			metadata["publicKey"] = cfg.Registration.PublicKey
		}
		_ = json.NewEncoder(w).Encode(metadata)
	})
	handler := newSandboxService(cfg.Identity, cfg.Runtime, cfg.AuthTokens...)
	generated, err := workerapi.NewServer(handler, handler)
	if err != nil {
		return nil, err
	}
	router.Mount("/", generated)
	return router, nil
}

func Serve(ctx context.Context, logger *slog.Logger, cfg Config) error {
	if logger == nil {
		logger = slog.Default()
	}
	port := cfg.Port
	if port == 0 {
		port = envInt("PORT", 3002)
	}

	router, err := NewRouter(cfg)
	if err != nil {
		return err
	}
	httpServer := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: router}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("worker agent serving", "addr", httpServer.Addr)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
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
