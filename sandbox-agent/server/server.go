package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	sandboxapi "github.com/obot-platform/discobox/api/sandboxgen"

	"github.com/obot-platform/discobox/sandbox-agent/config"
	"github.com/obot-platform/discobox/sandbox-agent/resources"
	agentstore "github.com/obot-platform/discobox/sandbox-agent/store"
	"github.com/obot-platform/discobox/sandbox-agent/terminal"
)

type Identity struct {
	ProjectID string
	SandboxID string
	WorkerID  string
}

type Config struct {
	Identity              Identity
	ControlPlanePublicKey string
	ListenAddress         string
	WorkingRoot           string
	RuntimeDir            string
	DatabasePath          string
	Resources             config.ResourceConfig
	Agents                []config.Agent
	UnitManager           terminal.UnitManager
	AuditRecorder         terminal.AuditRecorder
	Store                 *agentstore.Store
	ResourceCollector     resources.Collector
}

func ConfigFromAgentConfig(cfg config.Config) Config {
	return Config{
		Identity: Identity{
			ProjectID: cfg.Identity.ProjectID,
			SandboxID: cfg.Identity.SandboxID,
			WorkerID:  cfg.Identity.WorkerID,
		},
		ControlPlanePublicKey: cfg.ControlPlanePublicKey,
		ListenAddress:         cfg.ListenAddress,
		WorkingRoot:           cfg.WorkingRoot,
		RuntimeDir:            cfg.RuntimeDir,
		DatabasePath:          cfg.DatabasePath,
		Resources:             cfg.Resources,
		Agents:                cfg.Agents,
	}
}

func NewRouter(cfg Config) (*chi.Mux, error) {
	router, _, err := newRouterAndManager(cfg)
	return router, err
}

func newRouterAndManager(cfg Config) (*chi.Mux, *terminal.Manager, error) {
	if cfg.WorkingRoot == "" {
		cfg.WorkingRoot = "/workspace"
	}
	if cfg.RuntimeDir == "" {
		cfg.RuntimeDir = "/run/discobox/agent-terminals"
	}
	if cfg.Resources.SampleInterval <= 0 {
		cfg.Resources.SampleInterval = time.Second
	}
	if cfg.Resources.RetentionCount <= 0 {
		cfg.Resources.RetentionCount = 300
	}
	localStore := cfg.Store
	audit := cfg.AuditRecorder
	if localStore == nil && audit == nil {
		st, err := agentstore.Open(context.Background(), cfg.DatabasePath)
		if err != nil {
			return nil, nil, err
		}
		localStore = st
		audit = st
	}
	if audit == nil && localStore != nil {
		audit = localStore
	}
	authenticator, err := NewSignedTokenAuthenticator(cfg.Identity, cfg.ControlPlanePublicKey)
	if err != nil {
		return nil, nil, err
	}
	manager, err := terminal.NewManager(cfg.Agents, cfg.WorkingRoot, cfg.RuntimeDir, cfg.UnitManager, audit)
	if err != nil {
		return nil, nil, err
	}
	handler := &handler{
		identity:          cfg.Identity,
		terminals:         manager,
		store:             localStore,
		resourceCollector: cfg.ResourceCollector,
		resourceInterval:  cfg.Resources.SampleInterval,
		resourceRetention: cfg.Resources.RetentionCount,
	}
	generated, err := sandboxapi.NewServer(handler)
	if err != nil {
		return nil, nil, err
	}

	router := chi.NewRouter()
	router.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	router.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ready": true, "schedulable": true})
	})
	router.HandleFunc("/metadata", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"projectId": cfg.Identity.ProjectID,
			"sandboxId": cfg.Identity.SandboxID,
			"workerId":  cfg.Identity.WorkerID,
		})
	})
	router.Group(func(protected chi.Router) {
		protected.Use(authenticator.Middleware)
		protected.Post("/api/projects/{projectId}/sandboxes/{sandboxId}/agent-terminals/{terminalId}/attach", func(w http.ResponseWriter, r *http.Request) {
			handler.attachHTTP(w, r, chi.URLParam(r, "terminalId"))
		})
		protected.Mount("/", generated)
	})
	return router, manager, nil
}

func Serve(ctx context.Context, logger *slog.Logger, cfg Config) error {
	if logger == nil {
		logger = slog.Default()
	}
	addr := cfg.ListenAddress
	if addr == "" {
		addr = ":3003"
	}
	router, manager, err := newRouterAndManager(cfg)
	if err != nil {
		return err
	}
	go reconcileLoop(ctx, logger, manager)
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("sandbox agent serving", "addr", httpServer.Addr)
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

func reconcileLoop(ctx context.Context, logger *slog.Logger, manager *terminal.Manager) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := manager.Reconcile(ctx); err != nil {
				logger.Debug("sandbox agent terminal reconcile failed", "error", err)
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type statusError struct {
	status  int
	message string
}

func (e statusError) Error() string { return e.message }

func (e statusError) StatusCode() int {
	if e.status == 0 {
		return http.StatusInternalServerError
	}
	return e.status
}

func notImplemented(message string) error {
	return statusError{status: http.StatusNotImplemented, message: fmt.Sprintf("%s is not implemented yet", message)}
}

func errorStatus(status int, message string) *sandboxapi.ErrorResponseStatusCode {
	return &sandboxapi.ErrorResponseStatusCode{
		StatusCode: status,
		Response:   sandboxapi.ErrorResponse{Error: message},
	}
}
