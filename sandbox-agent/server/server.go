package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-faster/jx"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/platform"
	"github.com/discobox-ai/discobox/pkg/sandboxpath"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/pkg/sandboxuser"
	sandboxtools "github.com/discobox-ai/discobox/pkg/tools"
	"github.com/discobox-ai/discobox/sandbox-agent/agenttls"
	"github.com/discobox-ai/discobox/sandbox-agent/autostop"
	"github.com/discobox-ai/discobox/sandbox-agent/config"
	"github.com/discobox-ai/discobox/sandbox-agent/credentials"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
	"github.com/discobox-ai/discobox/sandbox-agent/files"
	harnesshooks "github.com/discobox-ai/discobox/sandbox-agent/hooks"
	"github.com/discobox-ai/discobox/sandbox-agent/intake"
	"github.com/discobox-ai/discobox/sandbox-agent/meta"
	"github.com/discobox-ai/discobox/sandbox-agent/monitors"
	"github.com/discobox-ai/discobox/sandbox-agent/ports"
	"github.com/discobox-ai/discobox/sandbox-agent/provision"
	"github.com/discobox-ai/discobox/sandbox-agent/rawsecrets"
	"github.com/discobox-ai/discobox/sandbox-agent/resources"
	"github.com/discobox-ai/discobox/sandbox-agent/secretswatch"
	"github.com/discobox-ai/discobox/sandbox-agent/services"
	"github.com/discobox-ai/discobox/sandbox-agent/sourceconverge"
	"github.com/discobox-ai/discobox/sandbox-agent/sourcesready"
	agentstore "github.com/discobox-ai/discobox/sandbox-agent/store"
	"github.com/discobox-ai/discobox/sandbox-agent/terminal"
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
	// Description seeds the meta file when the sandbox has none (ADR 0136).
	Description string
	HarnessMode string
	Resources   config.ResourceConfig
	// PoolPublicKey verifies the tokens the pool signs to deliver runtime
	// config, and nothing else (ADR 26-10-08-127 §3). Empty accepts none.
	PoolPublicKey string
	// AwaitsRuntimeConfig holds the first harness launch and the declared
	// services until an applied runtime-config document grants readiness
	// (ADR 26-10-08-127 §6).
	AwaitsRuntimeConfig bool
	Harness             config.Harness
	Sources             []sandboxconfig.Source
	Skills              sandboxconfig.Skills
	SkillsSources       []sandboxconfig.SkillsSource
	SandboxConfig       map[string]any
	Installer           terminal.Installer
	ExecUnitManager     execs.UnitManager
	ExecAuditRecorder   execs.AuditRecorder
	Store               *agentstore.Store
	// SecretEnv returns the sandbox's current secret-bound env->sentinel map.
	// Serve wires this to a live secretswatch.Watcher; callers that build a
	// router directly (e.g. tests) may leave it nil.
	SecretEnv func() map[string]string
	// RawSecrets holds the sandbox's raw secrets (ADR 26-10-27-840). Serve
	// builds one and keeps it filled from the pool; callers that build a
	// router directly may leave it nil, which holds none.
	RawSecrets *rawsecrets.Holder
	// CredentialsBridgePath overrides where the agent credentials relay reads
	// the pool-staged mTLS material and endpoint. Empty uses the documented
	// mount path.
	CredentialsBridgePath string
	// RuntimeConfig is the runtime-config intake the pool delivers to (ADR
	// 0126 §3). The binary opens it before serving, so the document it kept is
	// applied before anything reads its files, and the idle timeout that
	// document carries is the one the idle stop starts with; nil leaves the
	// routes answering that there is no intake.
	RuntimeConfig *intake.Intake
	// Provision is the box's provisioning this boot, which the binary runs
	// beside the server (ADR 26-10-16-423 §5): the primary terminal and the
	// declared services wait for their provisioning dependencies, and the status report
	// carries its document. nil waits for nothing and reports none.
	Provision *provision.Tracker
	// ClearCaches empties the sandbox's declared cache paths in place and
	// returns them (boot.ClearCaches). nil answers the cache-clear route that
	// the agent clears nothing.
	ClearCaches func() ([]string, error)
	// TLS is the keypair the agent serves its API over TLS alone with, as its
	// pool reaches it (ADR 26-10-29-481 §3). nil serves plain HTTP alone.
	TLS *agenttls.Keypair
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
		Description:           cfg.Description,
		HarnessMode:           cfg.HarnessMode,
		Resources:             cfg.Resources,
		PoolPublicKey:         cfg.PoolPublicKey,
		AwaitsRuntimeConfig:   cfg.AwaitsRuntimeConfig,
		Harness:               cfg.Harness,
		Sources:               cfg.Sources,
		Skills:                cfg.Skills,
		SkillsSources:         cfg.SkillsSources,
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
	autostop   *autostop.Policy
	listenAddr string
	// awaitSources is the readiness gate — a push-delivered sandbox's working
	// tree, or the first runtime-config document a pool delivers — nil when
	// there is nothing to wait for. Built once and shared, because
	// everything that reads the tree at boot has to be behind the same answer:
	// a second, independently constructed copy of "wait for delivery" is how
	// one of them goes stale.
	awaitSources func(context.Context) error
	// sourceConverger clones the sources the runtime-config document names
	// and answers their origins' credential helper (ADR 0126 §4).
	sourceConverger *sourceconverge.Converger
}

// defaultListenAddress is where the agent serves when the manifest names no
// address. It is also what the port watcher excludes from what it reports.
const defaultListenAddress = ":3003"

func newRouterAndManager(cfg Config) (agentRuntime, error) {
	if cfg.WorkingRoot == "" {
		// The agent runs inside the sandbox, so the sandbox's platform is
		// the one it was built for.
		cfg.WorkingRoot = sandboxpath.For(platform.Current()).WorkingRoot()
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
	authenticator, err := NewSignedTokenAuthenticator(cfg.Identity, cfg.ControlPlanePublicKey, cfg.PoolPublicKey)
	if err != nil {
		return agentRuntime{}, err
	}
	// One exec runtime backs both plain execs and terminals.
	execManager, err := execs.NewManagerWithConfig(execs.ManagerConfig{
		WorkingRoot:     cfg.WorkingRoot,
		DefaultWorkdir:  cfg.ExecDefaults.Workdir,
		SourceRoot:      cfg.ExecDefaults.SourceRoot,
		DefaultUser:     execDefaultUser(cfg.ExecDefaults),
		RuntimeDir:      cfg.RuntimeDir,
		DatabasePath:    cfg.DatabasePath,
		Env:             cfg.Env,
		SecretEnv:       cfg.SecretEnv,
		FileSecrets:     cfg.Harness.FileSecrets,
		RawSecrets:      rawSecretDeliveries(cfg.RawSecrets),
		RawSecretPath:   rawSecretPath(cfg.RawSecrets),
		AwaitRawSecrets: awaitRawSecrets(cfg.RawSecrets, cfg.RuntimeConfig),
		Units:           cfg.ExecUnitManager,
		Audit:           execAudit,
		Logs:            localStore,
	})
	if err != nil {
		return agentRuntime{}, err
	}
	// nil only for a sandbox no pool delivers runtime config to and whose
	// sources were in place before its container was created.
	awaitSources := sourcesready.Gate(cfg.Sources, cfg.AwaitsRuntimeConfig, "", slog.Default())
	var awaitProvision func(context.Context) error
	if cfg.Provision != nil {
		awaitProvision = cfg.Provision.AwaitReady
	}
	manager, err := terminal.NewService(terminal.ServiceConfig{
		CaptureStateDir:    terminal.CaptureStateDirectory,
		CaptureRecoveryDir: config.CaptureRecoveryDir(cfg.DatabasePath),
		Execs:              execManager,
		Harness:            cfg.Harness,
		SandboxConfig:      cfg.SandboxConfig,
		WorkingRoot:        cfg.WorkingRoot,
		RuntimeDir:         cfg.RuntimeDir,
		SecretEnv:          cfg.SecretEnv,
		ExecDefaults:       cfg.ExecDefaults,
		Units:              cfg.ExecUnitManager,
		Installer:          cfg.Installer,
		PrimaryState:       localStore,
		HarnessMode:        cfg.HarnessMode,
		Prompt:             cfg.Prompt,
		Skills:             cfg.Skills,
		SkillsSources:      cfg.SkillsSources,
		AwaitSources:       awaitSources,
		AwaitProvision:     awaitProvision,
	})
	if err != nil {
		return agentRuntime{}, err
	}
	manager.SetHookSocketPath(harnesshooks.SocketPath(cfg.RuntimeDir))
	// Services are declared in the primary source's working tree, which is the
	// directory an exec that names no workdir starts in. Asking the exec
	// manager rather than reading the config keeps the two from disagreeing
	// about where that is (ADR 0070 §1).
	serviceManager, err := newServiceManager(execManager, cfg.Provision)
	if err != nil {
		// A sandbox whose default workdir does not resolve is broken for every
		// exec, not just for services, and it has to come up far enough to be
		// diagnosed. The service routes then answer "not available here"
		// rather than reporting an empty listing, which would read as "this
		// repository declares no services".
		slog.Default().Warn("sandbox agent services disabled", "error", err)
		serviceManager = nil
	}
	// Tools are declared beside services, in the same working tree, so they
	// are rooted at the same source root (ADR 0125 §7).
	var declaredTools toolDirs
	if root, err := execManager.SourceRoot(); err == nil {
		declaredTools = toolDirs{image: sandboxtools.ImageDir, source: filepath.Join(root, sandboxtools.SourceDirName)}
	} else {
		slog.Default().Warn("sandbox agent tools disabled", "error", err)
	}
	portsWatch, err := newPortsWatcher(cfg, execManager, serviceManager)
	if err != nil {
		// Telemetry must not be what keeps a sandbox from booting. A sandbox
		// whose run identity does not resolve is broken for execs too, and it
		// has to come up far enough to be diagnosed — so this reports no ports
		// rather than substituting a guess for the uid or failing startup.
		slog.Default().Warn("sandbox agent listening port watcher disabled", "error", err)
		portsWatch = nil
	}
	// The idle stop reads every exec's shim-reported activity — screen
	// changes, attachers, last access — fresh on each evaluation (ADR 0108,
	// ADR 0124).
	//
	// Its timeout is the pool's, from the runtime-config document the sandbox
	// holds, and a later delivery changes it in place (ADR 26-10-08-127 §5).
	//
	// Wakes live in the run user's home, beside meta.json, because they have
	// to outlive the stop they ask to come back from (ADR 26-10-16-337 §2). A
	// sandbox whose run user does not resolve reads none and still stops.
	appliedAgent := appliedRuntimeAgent(cfg.RuntimeConfig)
	idleStopConfig := autostop.Config{Execs: execManager.List, IdleTimeout: appliedAgent.IdleTimeoutDuration()}
	if home, owner, err := runUserHome(execManager); err != nil {
		slog.Default().Warn("sandbox agent wakes disabled", "error", err)
	} else {
		idleStopConfig.WakeDir = filepath.Join(home, ".discobox", "wake")
		if owner.UID != nil && owner.GID != nil {
			idleStopConfig.WakeOwner = &autostop.Owner{UID: int(*owner.UID), GID: int(*owner.GID)}
		}
	}
	idleStop := autostop.New(idleStopConfig)
	idleStop.SetControlPlanePower(appliedAgent.ControlPlanePower)
	metaFile, err := newMetaFile(execManager)
	if err != nil {
		// Like the port watcher: meta is not worth failing a boot over, and a
		// sandbox whose run identity does not resolve has to come up far enough
		// to be diagnosed. Its status then says why it reports no meta.
		slog.Default().Warn("sandbox agent meta disabled", "error", err)
		metaFile = nil
	} else if err := metaFile.Seed(cfg.Description); err != nil {
		slog.Default().Warn("seed sandbox meta description", "path", metaFile.Path(), "error", err)
	}
	// git runs with the sandbox's own environment, so a clone takes the route
	// off the box a user's fetch does, and as whoever owns each checkout; the
	// default user is who that normally is.
	sourceConverger := sourceconverge.New(sourceconverge.Config{
		Manifest:         cfg.Sources,
		Env:              cfg.Env,
		User:             execManager.DefaultUser(),
		CredentialHelper: gitCredentialHelper(cfg.RuntimeDir),
	})
	fileService, err := files.New()
	if err != nil {
		slog.Default().Warn("sandbox agent files API disabled", "error", err)
		fileService = nil
	}
	handler := &handler{
		identity:          cfg.Identity,
		terminals:         manager,
		execs:             execManager,
		services:          serviceManager,
		tools:             declaredTools,
		store:             localStore,
		resourceSampler:   resources.NewSampler(),
		resourceInterval:  cfg.Resources.SampleInterval,
		resourceRetention: cfg.Resources.RetentionCount,
		sources:           cfg.Sources,
		execUser:          execManager.DefaultUser(),
		ports:             portsWatch,
		autostop:          idleStop,
		meta:              metaFile,
		runtimeConfig:     cfg.RuntimeConfig,
		rawSecrets:        cfg.RawSecrets,
		sourceConverger:   sourceConverger,
		provision:         cfg.Provision,
		files:             fileService,
		clearCaches:       cfg.ClearCaches,
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
		protected.Get("/api/projects/{projectId}/sandboxes/{sandboxId}/udp/attach", handler.attachUDPTunnelHTTP)
		protected.Handle("/api/projects/{projectId}/sandboxes/{sandboxId}/git-repositories/*", http.HandlerFunc(handler.serveGitRepositoryHTTP))
		protected.Mount("/", generated)
	})
	return agentRuntime{
		router:       router,
		terminals:    manager,
		execs:        execManager,
		services:     serviceManager,
		store:        localStore,
		portsWatch:   portsWatch,
		autostop:     idleStop,
		listenAddr:   cfg.ListenAddress,
		awaitSources: awaitSources,

		sourceConverger: sourceConverger,
	}, nil
}

// gitCredentialHelper is the credential helper each source's origin is
// configured with: this binary's git-credential mode, against the socket the
// converger answers on. Without a path to this binary there is no helper to
// name, and origins are configured without one.
func gitCredentialHelper(runtimeDir string) string {
	executable, err := os.Executable()
	if err != nil {
		slog.Default().Warn("no git credential helper for source origins", "error", err)
		return ""
	}
	return sourceconverge.HelperCommand(executable, sourceconverge.SocketPath(runtimeDir))
}

// newServiceManager resolves where this sandbox's service declarations live
// and wires the service layer to the exec primitive that runs them, and to
// the features provisioning turned off.
func newServiceManager(execManager *execs.Manager, tracker *provision.Tracker) (*services.Manager, error) {
	root, err := execManager.SourceRoot()
	if err != nil {
		return nil, err
	}
	cfg := services.ManagerConfig{Execs: execManager, Root: root}
	if tracker != nil {
		cfg.FeatureOff = func(feature string) bool { return tracker.Status().Off(feature) }
		cfg.AwaitFeatures = tracker.AwaitFeatures
	}
	return services.NewManager(cfg)
}

// newPortsWatcher wires the listening-port watcher to the identity the sandbox
// actually runs processes as, and to the declarations that name ports it cannot
// discover. That identity is asked of the exec manager rather than rebuilt from
// the manifest (see REVIEW.md), and a manifest that names nobody means an exec
// inherits this process's own identity (ADR 0025 §5), so that is the uid whose
// sockets count.
//
// Discobox's own listeners are excluded by port (agentListenPorts): the agent,
// the credentials endpoint, and the forwarders the pool stages. The uid filter
// already excludes them in the normal case, where they run as root and the
// sandbox user does not, but a sandbox whose run user *is* root would otherwise
// report them as its own services — and probe the forwarders, sending the probe
// out of the sandbox.
//
// The declared set is read through the service manager on every tick, so a
// declaration added while the sandbox is up is honored without a restart
// (ADR 0076). A sandbox whose services are unavailable — the workdir above did
// not resolve — declares nothing, which is the same answer as a repository with
// no declarations.
func newPortsWatcher(cfg Config, execManager *execs.Manager, serviceManager *services.Manager) (*ports.Watcher, error) {
	user, err := execManager.ResolveUser(execs.CreateRequest{})
	if err != nil {
		return nil, err
	}
	uid := int64(os.Getuid())
	if user != nil && user.UID != nil {
		uid = *user.UID
	}
	watcher := ports.Config{
		UID:             uid,
		ExcludeTCPPorts: agentListenPorts(cfg.ListenAddress),
	}
	// A configure sandbox's sign-in may listen on a callback port the user's
	// browser is about to be redirected to, which the CLI can forward only once
	// it is reported (ADR 26-10-09-867). The sandbox is short-lived and runs
	// little else, so it scans far more often than a working one.
	if cfg.HarnessMode == config.HarnessModeConfig {
		watcher.Interval = ports.ConfigureInterval
	}
	// One seam, two directories behind it: the image's declarations and the
	// repository's, which services.Discover already merges. A declaration that
	// states a protocol keeps its port out of the probe queue entirely, which
	// is what stops a classification probe from socket-activating the desktop
	// (ADR 26-09-05-409, image-declared services).
	if serviceManager != nil {
		watcher.Declared = serviceManager.Declarations
	}
	return ports.New(watcher), nil
}

// newMetaFile is the sandbox's meta file, in the home of the identity the
// sandbox runs processes as and owned by it, so the agent working in the
// sandbox can edit what an API write leaves there (ADR 0136). That identity is
// asked of the exec manager, as the port watcher asks it (see REVIEW.md). A
// manifest that names nobody means an exec inherits this process's identity
// (ADR 0025 §5), so the home is this process's own and nothing is chowned.
func newMetaFile(execManager *execs.Manager) (*meta.File, error) {
	home, owner, err := runUserHome(execManager)
	if err != nil {
		return nil, err
	}
	return meta.New(home, owner), nil
}

// runUserHome is the run user's home and who owns what the agent writes in
// it: this process's own home, owned by nobody in particular, when the
// sandbox runs as whoever started the agent.
func runUserHome(execManager *execs.Manager) (string, meta.Owner, error) {
	user, err := execManager.ResolveUser(execs.CreateRequest{})
	if err != nil {
		return "", meta.Owner{}, err
	}
	if user == nil {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", meta.Owner{}, err
		}
		return home, meta.Owner{}, nil
	}
	home := strings.TrimSpace(user.HomeDirectory)
	if home == "" {
		return "", meta.Owner{}, errors.New("the sandbox user has no home directory")
	}
	return home, meta.Owner{UID: user.UID, GID: user.GID}, nil
}

// agentListenPorts are the TCP ports Discobox's own plumbing listens on inside
// the sandbox, which are never a discobox's ports: the agent itself, the
// credentials endpoint, and the forwarders (egress, BuildKit, the DNS stub).
// The uid filter does not keep them out when the sandbox user is root. They
// must not be probed either: a forwarder carries what it is sent out of the
// sandbox, so a classification probe of one is traffic nobody meant.
//
// The forwarders' addresses are the sandbox's own (ADR 26-10-08-127 §4), so
// they are excluded whether or not the pool has delivered the material that
// starts them yet.
func agentListenPorts(listenAddress string) []int {
	var out []int
	for _, address := range []string{
		listenAddress,
		credentials.ListenAddress,
		sandboxconfig.SandboxEgressListenAddress,
		sandboxconfig.SandboxBuildKitListenAddress,
		sandboxconfig.SandboxDNSListenAddress,
	} {
		out = append(out, listenPorts(address)...)
	}
	return out
}

// appliedRuntimeAgent is the agent section of the runtime-config document the
// sandbox holds — its idle timeout, and whether the control plane holds its
// power — and the zero section, the idle stop's defaults, when it holds none.
func appliedRuntimeAgent(runtimeConfig *intake.Intake) sandboxconfig.RuntimeAgent {
	if runtimeConfig == nil {
		return sandboxconfig.RuntimeAgent{}
	}
	applied, ok := runtimeConfig.Applied()
	if !ok {
		return sandboxconfig.RuntimeAgent{}
	}
	return applied.Agent
}

// listenPorts is the port a listen address binds, or nothing when the address
// does not name one a discobox's port could collide with.
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

// rawSecretDeliveries and rawSecretPath are the exec manager's view of the
// raw secrets held: none without a holder.
func rawSecretDeliveries(holder *rawsecrets.Holder) func() map[string]string {
	if holder == nil {
		return nil
	}
	return holder.Deliveries
}

// awaitRawSecrets holds a process's start until the pool has answered for the
// raw secrets the applied document names (rawsecrets.Holder.AwaitFetched). It
// tells the holder that document first, so a start that the document's
// readiness let through before its delivery reached the holder still waits.
func awaitRawSecrets(holder *rawsecrets.Holder, runtimeConfig *intake.Intake) func(context.Context) error {
	if holder == nil {
		return nil
	}
	return func(ctx context.Context) error {
		if runtimeConfig != nil {
			if applied, ok := runtimeConfig.Applied(); ok {
				holder.SetApplied(applied.RawSecrets)
			}
		}
		return holder.AwaitFetched(ctx)
	}
}

func rawSecretPath(holder *rawsecrets.Holder) func(string) string {
	if holder == nil {
		return nil
	}
	return holder.Path
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
	if cfg.RawSecrets == nil {
		var owner rawsecrets.Owner
		if user := execDefaultUser(cfg.ExecDefaults); user != nil {
			owner = rawsecrets.Owner{UID: user.UID, GID: user.GID}
		}
		cfg.RawSecrets = rawsecrets.New(rawsecrets.Config{Owner: owner, Logger: logger})
	}
	// Before anything can start a process: values a previous run kept are not
	// trusted to still stand, and are asked for again (ADR 26-10-27-840 §3).
	if err := cfg.RawSecrets.Sweep(); err != nil {
		logger.Warn("remove the raw secrets a previous run kept", "error", err)
	}
	built, err := newRouterAndManager(cfg)
	if err != nil {
		return err
	}
	manager, execManager, localStore := built.terminals, built.execs, built.store
	if cfg.HarnessMode == config.HarnessModeConfig {
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
	if config.WorkedIn(cfg.HarnessMode) {
		// The wakes that fell due while the sandbox was down are handed to the
		// harness this boot launches, and nowhere else (ADR 26-10-16-337 §7).
		// They are taken before the launch starts, so an attach that beats
		// boot to the primary carries them as well as boot would.
		if paths, err := built.autostop.TakeDueWakes(); err != nil {
			logger.Warn("hand due wakes to the primary terminal", "error", err)
		} else if len(paths) > 0 {
			logger.Info("sandbox woken", "wakes", paths)
			manager.HandWake(paths)
		}
		go func() {
			switch err := manager.EnsurePrimary(ctx, cfg.Prompt); {
			case err == nil, errors.Is(err, context.Canceled):
			default:
				logger.Error("launch primary terminal", "error", err)
			}
		}()
	}
	// The repository's declared services come up alongside the primary
	// terminal (ADR 0070 §5). Like the primary launch this runs from a
	// goroutine started before the server serves, so a slow service script
	// cannot hold up the agent answering — and a config-mode sandbox starts
	// none, since it exists to run one setup command and end, not to be worked
	// in.
	if config.WorkedIn(cfg.HarnessMode) && built.services != nil {
		go startDeclaredServices(ctx, logger, built.awaitSources, built.services.EnsureStarted)
	}
	// The monitors an agent enabled resume with the box (ADR 26-10-31-073
	// §5), once the working tree a monitor runs in is there and the whole
	// provisioning run, async features included, is over. A configure sandbox
	// is not worked in, and runs none; nor does a Windows one, which cannot run
	// a monitor's script (monitors.Supported).
	if config.WorkedIn(cfg.HarnessMode) && monitors.Supported {
		if reconciler, err := newMonitorReconciler(execManager, logger); err != nil {
			logger.Warn("sandbox agent monitors disabled", "error", err)
		} else {
			go runMonitors(ctx, logger, built.awaitSources, cfg.Provision, reconciler)
		}
	}
	// Exec state converges on notifications rather than a poll (ADR 0115): the
	// shim's own runtime write for an ordinary exit, and systemd's unit signals
	// for an end it could not write.
	go execs.NewWatcher(execs.WatcherConfig{Manager: execManager, Logger: logger}).Run(ctx)
	// A harness that reads its credential from a file can clear that file on an
	// upstream 401 it did nothing to cause, and cannot put it back: the refresh
	// token it holds is a placeholder. This restores the sentinel so the next
	// launch is signed in. See terminal/secretfiles.go.
	go manager.WatchSecretFiles(ctx, logger)
	go manager.WatchHarnessConfig(ctx)
	// The listening-port watcher is the one status component with a standing
	// loop behind it, because classifying a port means connecting to whatever
	// is behind it (ADR 0046).
	go built.portsWatch.Run(ctx)
	// The sandbox powers itself off once nothing has happened in it for the
	// idle timeout, and the pool agent's auto-start brings it back (ADR 0108).
	// A configure-mode sandbox runs one setup command for a flow the control
	// plane drives, so its lifetime belongs to that flow and not to this.
	if config.WorkedIn(cfg.HarnessMode) {
		go built.autostop.Run(ctx)
	}
	go func() {
		if err := harnesshooks.Serve(ctx, harnesshooks.SocketPath(cfg.RuntimeDir), localStore); err != nil && !errors.Is(err, context.Canceled) {
			logger.Debug("sandbox agent hook collector stopped", "error", err)
		}
	}()
	go serveCredentials(ctx, logger, cfg.CredentialsBridgePath, cfg.RawSecrets)
	// The sources converge on the document the intake holds: the one it
	// restored at start, here, and each one delivered after, from the
	// handler (ADR 0126 §4).
	go func() {
		if err := built.sourceConverger.ServeCredentials(ctx, sourceconverge.SocketPath(cfg.RuntimeDir)); err != nil && !errors.Is(err, context.Canceled) {
			logger.Warn("sandbox agent git credential socket stopped", "error", err)
		}
	}()
	go built.sourceConverger.Run(ctx)
	if cfg.RuntimeConfig != nil {
		if applied, ok := cfg.RuntimeConfig.Applied(); ok {
			built.sourceConverger.Converge(applied)
			cfg.RawSecrets.SetApplied(applied.RawSecrets)
		}
	}
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
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", httpServer.Addr)
	if err != nil {
		return err
	}
	// As its pool reaches it: TLS alone or plain HTTP alone (ADR 26-10-29-481
	// §3).
	if cfg.TLS != nil {
		listener = tls.NewListener(listener, agenttls.Config(cfg.TLS))
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("sandbox agent serving", "addr", httpServer.Addr, "tls", cfg.TLS != nil)
		errCh <- httpServer.Serve(listener)
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

// serveCredentials runs the in-sandbox agent credentials endpoint (ADR 0031).
//
// The endpoint relays with the sandbox's proxy identity, which arrives in the
// pool's runtime-config document once the agent is up (ADR 26-10-08-127 §1),
// so it waits for the bridge config that names it rather than looking once at
// start. A sandbox no pool delivers proxy material to waits for as long as it
// runs, which is the same as the endpoint never coming up: the CLI reports a
// refused connection, which says more than an endpoint answering "denied" to
// everything.
//
// The raw secrets are asked for over the same relay (ADR 26-10-27-840 §3),
// from when it is up until ctx ends; the relay's endpoint does not serve them.
func serveCredentials(ctx context.Context, logger *slog.Logger, bridgePath string, raw *rawsecrets.Holder) {
	if bridgePath == "" {
		bridgePath = credentials.DefaultBridgeConfigPath
	}
	if err := sourcesready.Wait(ctx, bridgePath, logger); err != nil {
		return
	}
	relay, err := credentials.New(bridgePath)
	if err != nil {
		logger.Info("sandbox credentials endpoint not started", "reason", err)
		return
	}
	if raw != nil {
		go raw.Run(ctx, relay.RawSecrets)
	}
	if err := credentials.Serve(ctx, logger, relay, ""); err != nil && !errors.Is(err, context.Canceled) {
		logger.Warn("sandbox credentials endpoint stopped", "error", err)
	}
}

// startDeclaredServices waits for the sandbox's sources and then starts what
// the repository declares.
//
// The wait is the point. Services are declared *inside* the working tree, so a
// push-delivered sandbox — whose tree is materialized after its container is
// already running (ADR 0001) — has nothing to discover at the moment the agent
// boots. Discovery is a one-shot at boot and an absent directory is not an
// error, since most repositories declare none, so starting before delivery did
// not fail: it found nothing, said nothing, and never looked again. The same
// gate the primary terminal's first launch clears is what this clears.
func startDeclaredServices(ctx context.Context, logger *slog.Logger, await func(context.Context) error, start func(context.Context, *slog.Logger) error) {
	if await != nil {
		switch err := await(ctx); {
		case err == nil:
		case errors.Is(err, context.Canceled):
			return
		default:
			logger.Error("wait for the sandbox to be ready before starting its services", "error", err)
			return
		}
	}
	switch err := start(ctx, logger); {
	case err == nil, errors.Is(err, context.Canceled):
	default:
		logger.Error("start sandbox services", "error", err)
	}
}

// newMonitorReconciler wires the monitors' reconciler to the run user's home,
// where instances and their events live, and to the agent at the path that
// outlives an upgrade, which is what an instance's exec runs: an exec record
// outlives the box's stop, and the layer os.Executable names may not.
func newMonitorReconciler(execManager *execs.Manager, logger *slog.Logger) (*monitors.Reconciler, error) {
	home, owner, err := runUserHome(execManager)
	if err != nil {
		return nil, err
	}
	executable, err := stableAgentPath()
	if err != nil {
		return nil, err
	}
	return &monitors.Reconciler{
		Execs:      execManager,
		Dirs:       monitors.HomeDirs(home),
		Owner:      monitors.Owner{UID: owner.UID, GID: owner.GID},
		Executable: executable,
		Home:       home,
		Logger:     logger,
	}, nil
}

// stableAgentPath is this agent at a path that outlives an upgrade, for a
// command an exec records and runs again: agentlayer.AgentPath where the box
// runs its agent from the bundle store, whose old layers are swept; elsewhere
// — a sandbox whose agent is not placed as layers, a macOS or Windows guest —
// there is no layer to sweep, and the executable's own path is the stable one.
func stableAgentPath() (string, error) {
	if _, err := os.Stat(agentlayer.AgentPath); err == nil {
		return agentlayer.AgentPath, nil
	}
	return os.Executable()
}

// runMonitors starts the reconciler once the sources are in place and the
// whole provisioning run is over, async features included, as a service waits
// by default: the image's monitors and the gh and jq they run on come from an
// async feature. A provisioning failure is logged and does not hold monitors
// back: one that needs the failed feature fails on its own, where the agent
// sees it.
func runMonitors(ctx context.Context, logger *slog.Logger, awaitSources func(context.Context) error, tracker *provision.Tracker, reconciler *monitors.Reconciler) {
	if awaitSources != nil {
		switch err := awaitSources(ctx); {
		case err == nil:
		case errors.Is(err, context.Canceled):
			return
		default:
			logger.Error("wait for the sandbox to be ready before starting its monitors", "error", err)
			return
		}
	}
	if tracker != nil {
		if err := tracker.AwaitFeatures(ctx, nil); err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Warn("monitors start with provisioning failed", "error", err)
		}
	}
	reconciler.Run(ctx)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	var body []byte
	if enc, ok := jxEncoder(value); ok {
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

// jxEncoder finds the generated encoder for value, including when value is a
// struct rather than a pointer to one.
//
// The generated Encode has a pointer receiver, so a value never satisfies the
// interface and would fall through to encoding/json — which is the thing that
// cannot encode an unset optional field. A caller that writes
// `writeJSON(w, status, sandboxapi.ErrorResponse{...})` is not making a
// mistake it could see, so the copy is taken here rather than asked for at
// every call site.
func jxEncoder(value any) (interface{ Encode(*jx.Encoder) }, bool) {
	if enc, ok := value.(interface{ Encode(*jx.Encoder) }); ok {
		return enc, true
	}
	held := reflect.ValueOf(value)
	if held.Kind() != reflect.Struct {
		return nil, false
	}
	addressable := reflect.New(held.Type())
	addressable.Elem().Set(held)
	enc, ok := addressable.Interface().(interface{ Encode(*jx.Encoder) })
	return enc, ok
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
