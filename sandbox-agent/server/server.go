package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"github.com/go-chi/chi/v5"
	sandboxapi "github.com/obot-platform/discobox/api/sandboxgen"

	"github.com/obot-platform/discobox/sandbox-agent/config"
	"github.com/obot-platform/discobox/sandbox-agent/execs"
	agenthooks "github.com/obot-platform/discobox/sandbox-agent/hooks"
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
	ResolvedAgentConfig   *config.Agent
	AgentConfigs          []config.Agent
	Agents                []config.Agent
	UnitManager           terminal.UnitManager
	Installer             terminal.Installer
	ExecUnitManager       execs.UnitManager
	AuditRecorder         terminal.AuditRecorder
	ExecAuditRecorder     execs.AuditRecorder
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
		ResolvedAgentConfig:   cfg.ResolvedAgentConfig,
		AgentConfigs:          cfg.AgentConfigs,
		Agents:                cfg.Agents,
	}
}

func NewRouter(cfg Config) (*chi.Mux, error) {
	router, _, _, _, err := newRouterAndManager(cfg)
	return router, err
}

func newRouterAndManager(cfg Config) (*chi.Mux, *terminal.Manager, *execs.Manager, *agentstore.Store, error) {
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
	execAudit := cfg.ExecAuditRecorder
	if localStore == nil && audit == nil {
		st, err := agentstore.Open(context.Background(), cfg.DatabasePath)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		localStore = st
		audit = st
		execAudit = st
	}
	if audit == nil && localStore != nil {
		audit = localStore
	}
	if execAudit == nil && localStore != nil {
		execAudit = localStore
	}
	authenticator, err := NewSignedTokenAuthenticator(cfg.Identity, cfg.ControlPlanePublicKey)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	manager, err := terminal.NewManager(terminal.ManagerConfig{
		ResolvedAgentConfig: cfg.ResolvedAgentConfig,
		AgentConfigs:        cfg.AgentConfigs,
		Agents:              cfg.Agents,
		WorkingRoot:         cfg.WorkingRoot,
		RuntimeDir:          cfg.RuntimeDir,
		Units:               cfg.UnitManager,
		Installer:           cfg.Installer,
		Audit:               audit,
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	manager.SetHookSocketPath(agenthooks.SocketPath(cfg.RuntimeDir))
	execManager, err := execs.NewManager(cfg.WorkingRoot, filepath.Join(cfg.RuntimeDir, "execs"), cfg.ExecUnitManager, execAudit)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	handler := &handler{
		identity:          cfg.Identity,
		terminals:         manager,
		execs:             execManager,
		store:             localStore,
		resourceCollector: cfg.ResourceCollector,
		resourceInterval:  cfg.Resources.SampleInterval,
		resourceRetention: cfg.Resources.RetentionCount,
	}
	generated, err := sandboxapi.NewServer(handler)
	if err != nil {
		return nil, nil, nil, nil, err
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
		protected.Post("/api/projects/{projectId}/sandboxes/{sandboxId}/agent-terminals/{terminalId}/start", func(w http.ResponseWriter, r *http.Request) {
			handler.startTerminalHTTP(w, r, chi.URLParam(r, "terminalId"))
		})
		protected.Get("/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/attach", func(w http.ResponseWriter, r *http.Request) {
			handler.attachExecHTTP(w, r, chi.URLParam(r, "execId"))
		})
		protected.Post("/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/start", func(w http.ResponseWriter, r *http.Request) {
			handler.startExecHTTP(w, r, chi.URLParam(r, "execId"))
		})
		protected.Mount("/", generated)
	})
	return router, manager, execManager, localStore, nil
}

func Serve(ctx context.Context, logger *slog.Logger, cfg Config) error {
	if logger == nil {
		logger = slog.Default()
	}
	addr := cfg.ListenAddress
	if addr == "" {
		addr = ":3003"
	}
	router, manager, execManager, localStore, err := newRouterAndManager(cfg)
	if err != nil {
		return err
	}
	go reconcileLoop(ctx, logger, manager)
	go execReconcileLoop(ctx, logger, execManager)
	go func() {
		if err := agenthooks.Serve(ctx, agenthooks.SocketPath(cfg.RuntimeDir), localStore); err != nil && !errors.Is(err, context.Canceled) {
			logger.Debug("sandbox agent hook collector stopped", "error", err)
		}
	}()
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

func execReconcileLoop(ctx context.Context, logger *slog.Logger, manager *execs.Manager) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := manager.Reconcile(ctx); err != nil {
				logger.Debug("sandbox agent exec reconcile failed", "error", err)
			}
		}
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
