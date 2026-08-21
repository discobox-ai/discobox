package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-faster/jx"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"

	"github.com/discobox-ai/discobox/sandbox-agent/config"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
	harnesshooks "github.com/discobox-ai/discobox/sandbox-agent/hooks"
	"github.com/discobox-ai/discobox/sandbox-agent/ports"
	"github.com/discobox-ai/discobox/sandbox-agent/resources"
	"github.com/discobox-ai/discobox/sandbox-agent/secretswatch"
	"github.com/discobox-ai/discobox/sandbox-agent/services"
	"github.com/discobox-ai/discobox/sandbox-agent/sourcesready"
	agentstore "github.com/discobox-ai/discobox/sandbox-agent/store"
	"github.com/discobox-ai/discobox/sandbox-agent/terminal"
	"github.com/discobox-ai/discobox/sandboxconfig"
	"github.com/discobox-ai/discobox/sandboxuser"
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
	built, err := newRouterAndManager(cfg)
	return built.router, err
}

// agentRuntime is everything newRouterAndManager builds. Serve owns the pieces
// that need a context to run under; NewRouter takes only the router.
type agentRuntime struct {
	router     *chi.Mux
	terminals  *terminal.Service
	execs      *execs.Manager
	services   *services.Manager
	store      *agentstore.Store
	portsWatch *ports.Watcher
	listenAddr string
}

// defaultListenAddress is where the agent serves when the manifest names no
// address. It is also what the port watcher excludes from what it reports.
const defaultListenAddress = ":3003"

func newRouterAndManager(cfg Config) (agentRuntime, error) {
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
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = defaultListenAddress
	}
	localStore := cfg.Store
	execAudit := cfg.ExecAuditRecorder
	if localStore == nil && execAudit == nil {
		st, err := agentstore.Open(context.Background(), cfg.DatabasePath)
		if err != nil {
			return agentRuntime{}, err
		}
		localStore = st
		execAudit = st
	}
	if execAudit == nil && localStore != nil {
		execAudit = localStore
	}
	authenticator, err := NewSignedTokenAuthenticator(cfg.Identity, cfg.ControlPlanePublicKey)
	if err != nil {
		return agentRuntime{}, err
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
		return agentRuntime{}, err
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
		// nil for every sandbox whose sources were in place before its
		// container was created, which is all of them but a push-delivered one.
		AwaitSources: sourcesready.Gate(cfg.Sources, "", slog.Default()),
	})
	if err != nil {
		return agentRuntime{}, err
	}
	manager.SetHookSocketPath(harnesshooks.SocketPath(cfg.RuntimeDir))
	// Services are declared in the primary source's working tree, which is the
	// directory an exec that names no workdir starts in. Asking the exec
	// manager rather than reading the config keeps the two from disagreeing
	// about where that is (ADR 0068 §1).
	serviceManager, err := newServiceManager(execManager)
	if err != nil {
		// A sandbox whose default workdir does not resolve is broken for every
		// exec, not just for services, and it has to come up far enough to be
		// diagnosed. The service routes then answer "not available here"
		// rather than reporting an empty listing, which would read as "this
		// repository declares no services".
		slog.Default().Warn("sandbox agent services disabled", "error", err)
		serviceManager = nil
	}
	portsWatch, err := newPortsWatcher(cfg, execManager)
	if err != nil {
		// Telemetry must not be what keeps a sandbox from booting. A sandbox
		// whose run identity does not resolve is broken for execs too, and it
		// has to come up far enough to be diagnosed — so this reports no ports
		// rather than substituting a guess for the uid or failing startup.
		slog.Default().Warn("sandbox agent listening port watcher disabled", "error", err)
		portsWatch = nil
	}
	handler := &handler{
		identity:          cfg.Identity,
		terminals:         manager,
		execs:             execManager,
		services:          serviceManager,
		store:             localStore,
		resourceCollector: cfg.ResourceCollector,
		resourceInterval:  cfg.Resources.SampleInterval,
		resourceRetention: cfg.Resources.RetentionCount,
		sources:           cfg.Sources,
		harnessTypeID:     cfg.Harness.TypeID,
		execUser:          execManager.DefaultUser(),
		ports:             portsWatch,
	}
	generated, err := sandboxapi.NewServer(handler)
	if err != nil {
		return agentRuntime{}, err
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
	return agentRuntime{
		router:     router,
		terminals:  manager,
		execs:      execManager,
		services:   serviceManager,
		store:      localStore,
		portsWatch: portsWatch,
		listenAddr: cfg.ListenAddress,
	}, nil
}

// newServiceManager resolves where this sandbox's service declarations live
// and wires the service layer to the exec primitive that runs them.
func newServiceManager(execManager *execs.Manager) (*services.Manager, error) {
	root, err := execManager.DefaultWorkdir()
	if err != nil {
		return nil, err
	}
	return services.NewManager(services.ManagerConfig{Execs: execManager, Root: root})
}

// newPortsWatcher wires the listening-port watcher to the identity the sandbox
// actually runs processes as. That identity is asked of the exec manager rather
// than rebuilt from the manifest (see REVIEW.md), and a manifest that names
// nobody means an exec inherits this process's own identity (ADR 0025 §5), so
// that is the uid whose sockets count.
//
// The agent's own listener is excluded by port. The uid filter already excludes
// it in the normal case, where the agent is root and the sandbox user is not,
// but a sandbox whose run user *is* root would otherwise report the control
// port as one of its own services.
func newPortsWatcher(cfg Config, execManager *execs.Manager) (*ports.Watcher, error) {
	user, err := execManager.ResolveUser(execs.CreateRequest{})
	if err != nil {
		return nil, err
	}
	uid := int64(os.Getuid())
	if user != nil && user.UID != nil {
		uid = *user.UID
	}
	return ports.New(ports.Config{
		UID:          uid,
		ExcludePorts: listenPorts(cfg.ListenAddress),
	}), nil
}

// listenPorts is the agent's own listen port, or nothing when the address does
// not name one it could collide with.
func listenPorts(address string) []int {
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 {
		return nil
	}
	return []int{port}
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
	if cfg.SecretEnv == nil {
		watcher := secretswatch.Watch(ctx, "", func(err error) {
			logger.Warn("sandbox agent secrets watch", "error", err)
		})
		cfg.SecretEnv = watcher.Env
	}
	built, err := newRouterAndManager(cfg)
	if err != nil {
		return err
	}
	manager, execManager, localStore := built.terminals, built.execs, built.store
	if cfg.HarnessMode == "config" {
		// Before anything can be seeded: the configure command runs as the
		// sandbox user, and this is the only part of root-owned /run/discobox
		// it may write.
		if err := ensureConfigureDir(execManager.DefaultUser()); err != nil {
			return err
		}
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
	// The repository's declared services come up alongside the primary
	// terminal (ADR 0068 §5). Like the primary launch this runs from a
	// goroutine started before the server serves, so a slow service script
	// cannot hold up the agent answering — and a config-mode sandbox starts
	// none, since it exists to run one setup command and end, not to be worked
	// in.
	if cfg.HarnessMode != "config" && built.services != nil {
		go func() {
			switch err := built.services.EnsureStarted(ctx, logger); {
			case err == nil, errors.Is(err, context.Canceled):
			default:
				logger.Error("start sandbox services", "error", err)
			}
		}()
	}
	go execReconcileLoop(ctx, logger, execManager)
	// A harness that reads its credential from a file can clear that file on an
	// upstream 401 it did nothing to cause, and cannot put it back: the refresh
	// token it holds is a placeholder. This restores the sentinel so the next
	// launch is signed in. See terminal/secretfiles.go.
	go manager.WatchSecretFiles(ctx, logger)
	// The listening-port watcher is the one status component with a standing
	// loop behind it, because classifying a port means connecting to whatever
	// is behind it (ADR 0046).
	go built.portsWatch.Run(ctx)
	go func() {
		if err := harnesshooks.Serve(ctx, harnesshooks.SocketPath(cfg.RuntimeDir), localStore); err != nil && !errors.Is(err, context.Canceled) {
			logger.Debug("sandbox agent hook collector stopped", "error", err)
		}
	}()
	httpServer := &http.Server{
		Addr:              built.listenAddr,
		Handler:           built.router,
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
