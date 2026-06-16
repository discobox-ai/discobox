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

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/obot-platform/discobox/worker-agent/workeragent/sandboxruntime"
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

func NewRouter(cfg Config) (*chi.Mux, huma.API) {
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
	config := huma.DefaultConfig("Discobox Worker Agent", "0.1.0")
	config.DocsRenderer = huma.DocsRendererScalar
	config.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"workerBearerAuth": {
			Type:         "http",
			Scheme:       "bearer",
			BearerFormat: "worker-local API token",
		},
	}
	humaAPI := humachi.New(router, config)
	RegisterSandboxOperations(humaAPI, cfg.Identity, cfg.Runtime, cfg.AuthTokens...)
	return router, humaAPI
}

func Serve(ctx context.Context, logger *slog.Logger, cfg Config) error {
	if logger == nil {
		logger = slog.Default()
	}
	port := cfg.Port
	if port == 0 {
		port = envInt("PORT", 3002)
	}

	router, _ := NewRouter(cfg)
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
