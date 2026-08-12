package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-faster/jx"

	sandboxapi "github.com/obot-platform/discobox/api/sandboxgen"

	"github.com/obot-platform/discobox/sandbox-agent/config"
	"github.com/obot-platform/discobox/sandbox-agent/execs"
	harnesshooks "github.com/obot-platform/discobox/sandbox-agent/hooks"
	"github.com/obot-platform/discobox/sandbox-agent/resources"
	"github.com/obot-platform/discobox/sandbox-agent/secretswatch"
	agentstore "github.com/obot-platform/discobox/sandbox-agent/store"
	"github.com/obot-platform/discobox/sandbox-agent/terminal"
	"github.com/obot-platform/discobox/sandboxconfig"
	"github.com/obot-platform/discobox/sandboxuser"
)

type Identity struct {
	ProjectID string
	SandboxID string
	PoolID    string
}

type Config struct {
	Identity              Identity
	ControlPlanePublicKey string
	ListenAddress         string
	WorkingRoot           string
	ExecDefaults          config.ExecDefaults
	RuntimeDir            string
	DatabasePath          string
	Env                   map[string]string
	Prompt                []string
	HarnessMode           string
	Resources             config.ResourceConfig
	Harness               config.Harness
	Sources               []sandboxconfig.Source
	SandboxConfig         map[string]any
	Installer             terminal.Installer
	ExecUnitManager       execs.UnitManager
	ExecAuditRecorder     execs.AuditRecorder
	Store                 *agentstore.Store
	ResourceCollector     resources.Collector
	// SecretEnv returns the sandbox's current secret-bound env->sentinel map.
	// Serve wires this to a live secretswatch.Watcher; callers that build a
	// router directly (e.g. tests) may leave it nil.
	SecretEnv func() map[string]string
}

func ConfigFromHarnessConfig(cfg config.Config) Config {
	return Config{
		Identity: Identity{
			ProjectID: cfg.Identity.ProjectID,
			SandboxID: cfg.Identity.SandboxID,
			PoolID:    cfg.Identity.PoolID,
		},
		ControlPlanePublicKey: cfg.ControlPlanePublicKey,
		ListenAddress:         cfg.ListenAddress,
		WorkingRoot:           cfg.WorkingRoot,
		ExecDefaults:          cfg.ExecDefaults,
		RuntimeDir:            cfg.RuntimeDir,
		DatabasePath:          cfg.DatabasePath,
		Env:                   cfg.Env,
		Prompt:                cfg.Prompt,
		HarnessMode:           cfg.HarnessMode,
		Resources:             cfg.Resources,
		Harness:               cfg.Harness,
		Sources:               cfg.Sources,
		SandboxConfig:         cfg.SandboxConfig,
	}
}

func NewRouter(cfg Config) (*chi.Mux, error) {
	router, _, _, _, err := newRouterAndManager(cfg)
	return router, err
}

func newRouterAndManager(cfg Config) (*chi.Mux, *terminal.Service, *execs.Manager, *agentstore.Store, error) {
	if cfg.WorkingRoot == "" {
		cfg.WorkingRoot = "/workspace"
	}
	if cfg.RuntimeDir == "" {
		cfg.RuntimeDir = "/run/discobox/harness-terminals"
	}
	if cfg.Resources.SampleInterval <= 0 {
		cfg.Resources.SampleInterval = time.Second
	}
	if cfg.Resources.RetentionCount <= 0 {
		cfg.Resources.RetentionCount = 300
	}
	localStore := cfg.Store
	execAudit := cfg.ExecAuditRecorder
	if localStore == nil && execAudit == nil {
		st, err := agentstore.Open(context.Background(), cfg.DatabasePath)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		localStore = st
		execAudit = st
	}
	if execAudit == nil && localStore != nil {
		execAudit = localStore
	}
	authenticator, err := NewSignedTokenAuthenticator(cfg.Identity, cfg.ControlPlanePublicKey)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	// One exec runtime backs both plain execs and terminals.
	execManager, err := execs.NewManagerWithConfig(execs.ManagerConfig{
		WorkingRoot:    cfg.WorkingRoot,
		DefaultWorkdir: cfg.ExecDefaults.Workdir,
		DefaultUser:    execDefaultUser(cfg.ExecDefaults),
		RuntimeDir:     cfg.RuntimeDir,
		DatabasePath:   cfg.DatabasePath,
		Env:            cfg.Env,
		Units:          cfg.ExecUnitManager,
		Audit:          execAudit,
		Logs:           localStore,
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	manager, err := terminal.NewService(terminal.ServiceConfig{
		Execs:         execManager,
		Harness:       cfg.Harness,
		SandboxConfig: cfg.SandboxConfig,
		WorkingRoot:   cfg.WorkingRoot,
		RuntimeDir:    cfg.RuntimeDir,
		Env:           cfg.Env,
		SecretEnv:     cfg.SecretEnv,
		ExecDefaults:  cfg.ExecDefaults,
		Units:         cfg.ExecUnitManager,
		Installer:     cfg.Installer,
		PrimaryState:  localStore,
		HarnessMode:   cfg.HarnessMode,
		Prompt:        cfg.Prompt,
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	manager.SetHookSocketPath(harnesshooks.SocketPath(cfg.RuntimeDir))
	handler := &handler{
		identity:          cfg.Identity,
		terminals:         manager,
		execs:             execManager,
		store:             localStore,
		resourceCollector: cfg.ResourceCollector,
		resourceInterval:  cfg.Resources.SampleInterval,
		resourceRetention: cfg.Resources.RetentionCount,
		sources:           cfg.Sources,
		harnessTypeID:     cfg.Harness.TypeID,
		prompt:            cfg.Prompt,
		execUser:          execManager.DefaultUser(),
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
			"poolId":    cfg.Identity.PoolID,
		})
	})
	router.Group(func(protected chi.Router) {
		protected.Use(authenticator.Middleware)
		protected.Get("/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/attach", func(w http.ResponseWriter, r *http.Request) {
			handler.attachExecHTTP(w, r, chi.URLParam(r, "execId"))
		})
		// POST to the same path is the one-shot form: the body is the exec's
		// stdin and the response is its output, for callers that want plain
		// request/response instead of a duplex stream. GET remains the websocket
		// attach.
		protected.Post("/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/attach", func(w http.ResponseWriter, r *http.Request) {
			handler.oneShotExecHTTP(w, r, chi.URLParam(r, "execId"))
		})
		protected.Post("/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/start", func(w http.ResponseWriter, r *http.Request) {
			handler.startExecHTTP(w, r, chi.URLParam(r, "execId"))
		})
		protected.Get("/api/projects/{projectId}/sandboxes/{sandboxId}/tcp/attach", handler.attachTCPTunnelHTTP)
		protected.Mount("/", generated)
	})
	return router, manager, execManager, localStore, nil
}

// execDefaultUser is the manifest layer: the sandbox's declared user, as the
// exec defaults carry it. Whether it names anybody is asked of sandboxuser.Named
// rather than re-tested here -- that predicate having been written per-site is
// what let an exec naming only a group fall through the gap between two copies
// of it (ADR 0033 §1).
func execDefaultUser(defaults config.ExecDefaults) *execs.User {
	user := &execs.User{
		Name:             defaults.Username,
		UID:              cloneInt64(defaults.UID),
		GID:              cloneInt64(defaults.GID),
		GroupName:        defaults.GroupName,
		HomeDirectory:    defaults.HomeDirectory,
		AdditionalGroups: append([]string(nil), defaults.AdditionalGroups...),
	}
	if !sandboxuser.Named(user) {
		return nil
	}
	return user
}

func cloneInt64(in *int64) *int64 {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func Serve(ctx context.Context, logger *slog.Logger, cfg Config) error {
	if logger == nil {
		logger = slog.Default()
	}
	addr := cfg.ListenAddress
	if addr == "" {
		addr = ":3003"
	}
	if cfg.SecretEnv == nil {
		watcher := secretswatch.Watch(ctx, "", func(err error) {
			logger.Warn("sandbox agent secrets watch", "error", err)
		})
		cfg.SecretEnv = watcher.Env
	}
	router, manager, execManager, localStore, err := newRouterAndManager(cfg)
	if err != nil {
		return err
	}
	// Config mode defers the primary terminal to the first attach. The configure
	// command is interactive and reads inputs seeded into the sandbox after it is
	// running, so launching it at boot would race that seeding. Attaching to the
	// virtual primary exec id launches it (see terminal.ResolvePrimary).
	if cfg.HarnessMode != "config" {
		go func() {
			switch err := manager.EnsurePrimary(ctx, cfg.Prompt); {
			case err == nil, errors.Is(err, context.Canceled):
			default:
				logger.Error("launch primary terminal", "error", err)
			}
		}()
	}
	go execReconcileLoop(ctx, logger, execManager)
	go func() {
		if err := harnesshooks.Serve(ctx, harnesshooks.SocketPath(cfg.RuntimeDir), localStore); err != nil && !errors.Is(err, context.Canceled) {
			logger.Debug("sandbox agent hook collector stopped", "error", err)
		}
	}()
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		// No ReadTimeout/WriteTimeout: those set absolute per-request conn
		// deadlines that survive the websocket hijack in exec attach
		// (coder/websocket Accept keeps them) and cut long-lived attaches off
		// mid-flight. Liveness comes from ReadHeaderTimeout, IdleTimeout, and
		// websocket keepalive pings on attach tunnels.
		IdleTimeout: 120 * time.Second,
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

func writeJSON(w http.ResponseWriter, status int, value any) {
	var body []byte
	if enc, ok := value.(interface{ Encode(*jx.Encoder) }); ok {
		// ogen types must be encoded via jx: encoding/json mis-serializes their
		// optional fields (an unset OptString's MarshalJSON returns empty bytes,
		// which fails json.Marshal with "unexpected end of JSON input").
		var e jx.Encoder
		enc.Encode(&e)
		body = e.Bytes()
	} else {
		var err error
		body, err = json.Marshal(value)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
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

func errorStatus(status int, message string) *sandboxapi.ErrorResponseStatusCode {
	return &sandboxapi.ErrorResponseStatusCode{
		StatusCode: status,
		Response:   sandboxapi.ErrorResponse{Error: message},
	}
}
