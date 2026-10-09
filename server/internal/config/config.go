// Package config loads discobox-server runtime configuration from the
// environment.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/adrg/xdg"
	"github.com/joho/godotenv"

	"github.com/discobox-ai/discobox/configfile"
	"github.com/discobox-ai/discobox/devimage"
	"github.com/discobox-ai/discobox/endpoint"
	"github.com/discobox-ai/discobox/internal/hostid"
	"github.com/discobox-ai/discobox/releasemanifest"
	"github.com/discobox-ai/discobox/server/internal/auth/discobot"
	"github.com/discobox-ai/discobox/server/internal/harnessdefs"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/x/gormdb"
)

const appName = "discobox"

// The OpenTelemetry specification's own variables. One names an exporter and
// the other counts milliseconds, so neither binds to its field the way every
// other setting does; Load reads them by hand.
const (
	otelExporterEnv       = "OTEL_METRICS_EXPORTER"
	otelExportIntervalEnv = "OTEL_METRIC_EXPORT_INTERVAL"
)

// EnvFile is the env file LoadEnvFile reads by default, and EnvFileVar names a
// different one.
//
// Deliberately not `.env`. A server that loads whatever `.env` sits in the
// current directory silently reconfigures itself from an unrelated project's
// file, which is how a hand-built binary run from a source tree ends up
// pointing at a development image or database nobody asked for. Development
// still wants that file, so the dev loop names it — `.wnb.yaml` runs the server
// with `DISCOBOX_ENV_FILE=.env` — rather than the server guessing.
const (
	EnvFile    = ".discobox-server.env"
	EnvFileVar = "DISCOBOX_ENV_FILE"
)

// LoadEnvFile reads EnvFile, or the file EnvFileVar names, into the process
// environment. Set EnvFileVar to the empty string to load nothing.
//
// A missing file is not an error: the file is an optional convenience, and
// every setting it carries can be set in the environment directly. The
// environment wins — godotenv never replaces a variable that is already set —
// so an explicit `DISCOBOX_...=x discobox-server` still means what it says.
//
// Call it before Load: Load reads the environment as it finds it.
func LoadEnvFile() {
	name, ok := os.LookupEnv(EnvFileVar)
	if !ok {
		name = EnvFile
	}
	if name == "" {
		return
	}
	_ = godotenv.Load(name)
}

// Config holds all configuration for discobox-server.
//
// The struct tags are the source of truth for the configuration file and its
// JSON Schema (ADR 0096 §2, configuration file): `yaml` is the key, `env` the variable that
// overrides it, `default` the literal default rendered into the schema, and
// `doc` the description an operator reads in their editor, and `example` a
// sample value in YAML, shown beside it ("-" for a setting that deliberately
// has none). A field tagged
// `yaml:"-"` is derived rather than configured, and appears in neither.
type Config struct {
	ReleaseManifest string                    `yaml:"releaseManifest" env:"DISCOBOX_RELEASE_MANIFEST" doc:"Path to a release manifest supplying the runtime image set, including built-in harnesses. Overrides individual image settings and disables development image synchronization." example:"/opt/discobox/release.json"`
	Release         *releasemanifest.Manifest `yaml:"-"`
	HarnessImages   map[string]string         `yaml:"-"`

	// ConfigFile is the configuration file Load looked for, and ConfigFileRead
	// whether it found one there. Derived: the path comes from the environment
	// alone (ADR 0096 §1), and an empty ConfigFile means ConfigFileVar was set
	// empty and nothing was looked for.
	ConfigFile     string `yaml:"-"`
	ConfigFileRead bool   `yaml:"-"`

	// Server settings.
	Port   int      `yaml:"port" env:"PORT" default:"18080" doc:"TCP port for an http:// listen endpoint that does not name one."`
	Listen []string `yaml:"listen" env:"DISCOBOX_SERVER_LISTEN" doc:"Endpoints to listen on, from unix:// (npipe:// on Windows), http://<host>:<port> and iroh://. Bare unix://, npipe:// and iroh:// mean this machine's default socket, pipe and iroh identity; an iroh listener logs the discobox:// address clients use. Local IPC is added when none is named, so the CLI can always reach the server. The environment variable takes a comma-separated list." example:"[unix://, iroh://, http://127.0.0.1:18080]"`
	// Name is what this server calls itself, and what a client offers as the
	// name to register it under (ADR 0116 §2). It identifies nothing: two
	// servers may share one, and nothing but a client's default choice reads it.
	Name string `yaml:"name" env:"DISCOBOX_SERVER_NAME" doc:"What this server calls itself: the name a client registering it is offered. Defaults to this machine's hostname." example:"build-server"`

	// XDG-backed application directories. Their defaults are derived from the
	// platform's base directories rather than being literals, so they are
	// described here and computed in Load.
	DataDir   string `yaml:"dataDir" env:"DISCOBOX_DATA_DIR" doc:"Durable server state: the database, the SSH host key, the iroh endpoint key, authorized_keys and authorized_ids. Defaults to <XDG data home>/discobox." example:"/var/lib/discobox"`
	ConfigDir string `yaml:"configDir" env:"DISCOBOX_CONFIG_DIR" doc:"Operator-edited configuration. Defaults to <XDG config home>/discobox. It cannot relocate the configuration file itself, which is found from the environment." example:"/etc/discobox"`
	CacheDir  string `yaml:"cacheDir" env:"DISCOBOX_CACHE_DIR" doc:"Reproducible data that may be deleted. Defaults to <XDG cache home>/discobox." example:"/var/cache/discobox"`
	StateDir  string `yaml:"stateDir" env:"DISCOBOX_STATE_DIR" doc:"State that should survive a restart but is not precious. Defaults to <XDG state home>/discobox." example:"/var/lib/discobox/state"`

	// HostID identifies the machine this server runs on, resolved the same way
	// a CLI on this machine resolves it. A create request whose origin reports
	// this host ID came from this filesystem, which is what makes binding a
	// client's local source directory possible.
	//
	// Derived, never configured: an operator who set it would be claiming to
	// be a different machine.
	HostID string `yaml:"-"`

	// Database settings.
	DatabaseDSN     string        `yaml:"databaseDsn" env:"DATABASE_DSN" doc:"Database DSN: postgres:// or postgresql:// for PostgreSQL, sqlite3:///<path> for SQLite. Defaults to a SQLite file under dataDir." example:"postgres://discobox:password@db.internal:5432/discobox?sslmode=require"`
	DatabaseReadDSN string        `yaml:"databaseReadDsn" env:"DATABASE_READ_DSN" doc:"Optional separate DSN for reads, for a deployment with a read replica. Same forms as databaseDsn." example:"postgres://discobox:password@db-replica.internal:5432/discobox?sslmode=require"`
	DatabaseDriver  gormdb.Driver `yaml:"databaseDriver" env:"DATABASE_DRIVER" enum:"sqlite,postgres" doc:"Database driver. Detected from databaseDsn when unset." example:"postgres"`

	// Secret encryption settings.
	EncryptionKey string `yaml:"encryptionKey" env:"DISCOBOX_ENCRYPTION_KEY" doc:"Base64 of the 32-byte AES key that seals stored secrets; generate one with: openssl rand -base64 32. Secrets are stored unsealed when empty. Keep it: secrets sealed with a key cannot be read without it." example:"-"`

	// discobot, the team-facing control plane in front of this server (ADR
	// 26-10-07-005).
	AuthRequired      bool   `yaml:"authRequired" env:"DISCOBOX_AUTH_REQUIRED" doc:"Never serve the default user, who holds every scope, on any listener. Every request must then authenticate: a discobot assertion, an enrolled iroh peer, or a pool agent's or sandbox's own credential. The local CLI over local IPC or HTTP is refused, and so SSH is reachable only over iroh. Health, the API spec, and docs still answer. An enrolled iroh peer authenticates as the operator's own client, so the CLI still reaches the server over iroh. Requires discobotPublicKey or an iroh endpoint in listen."`
	DiscobotPublicKey string `yaml:"discobotPublicKey" env:"DISCOBOX_DISCOBOT_PUBLIC_KEY" doc:"Base64 of discobot's Ed25519 public key. When set, a request carrying a discobot assertion acts as the person it names, in the project it names; discobox keeps no users of its own. Unset, every assertion is refused." example:"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="`

	// Reconcile engine settings.
	DispatcherPollInterval         time.Duration `yaml:"dispatcherPollInterval" env:"DISPATCHER_POLL_INTERVAL" default:"1s" doc:"How often the job dispatcher polls for work."`
	SandboxReconcileJobConcurrency int           `yaml:"sandboxReconcileJobConcurrency" env:"SANDBOX_RECONCILE_JOB_CONCURRENCY" default:"4" doc:"How many sandbox reconcile jobs run at once."`

	// Sandbox settings.
	DefaultSandboxImage string `yaml:"defaultSandboxImage" env:"DISCOBOX_DEFAULT_SANDBOX_IMAGE" doc:"Image for sandboxes that name no harness. Defaults to the built-in sandbox agent image." example:"ghcr.io/discobox-ai/discobox-sandbox-agent:v0.8.0"`
	// DefaultSandboxImageDigest is the identity behind DefaultSandboxImage.
	// Sandboxes with no harness config run the default image, and the tag alone
	// cannot say which build that is — dev workflows rebuild tags in place — so
	// the digest is what lets such a sandbox report and take an upgrade
	// (ADR 0016 §1's "a digest and not just a tag"). Empty when unknown, which
	// simply means those sandboxes report no upgrade.
	DefaultSandboxImageDigest string `yaml:"defaultSandboxImageDigest" env:"DISCOBOX_DEFAULT_SANDBOX_IMAGE_DIGEST" doc:"Digest identifying the build behind defaultSandboxImage. Those sandboxes report no upgrade when it is unknown." example:"sha256:4f2a9c1e7b3d8a6f0e5c2b9d7a1f3e8c6b4d2a0f9e7c5b3a1d8f6e4c2b0a9d7e"`

	// JudgeCommands has the judge decide whether the command
	// `discobox-access run` declares carries out the use it names, before the
	// pool mints a credential for it, and whether a discobox may hand a
	// credential on (ADR 26-09-22-838 §3, ADR 26-10-02-054).
	//
	// It is on by default. A command is judged once, before anything is
	// minted, so it costs one verdict per command an agent chose to run. Off,
	// the pool mints without asking and records no verdict, and a discobox
	// hands nothing on.
	JudgeCommands bool `yaml:"judgeCommands" env:"DISCOBOX_JUDGE_COMMANDS" default:"true" doc:"Have each project's judge decide whether a command discobox-access runs carries out the approved use it names, before a credential is issued for it, and whether a discobox may hand a credential on. On by default. A project whose default harness runs no model then runs no credentialed command unless the server judges with Jev. Off, credentials are issued for whatever command asks, with no verdict on record, and no discobox hands one on."`

	// JudgeCredentials has the judge decide whether each credential-bearing
	// request the proxy observes is part of what its use was approved for
	// (ADR 26-09-22-838 §4).
	//
	// It is off by default and opted into: it puts a model in front of every
	// such request, holding its connection open, and a busy discobox sends
	// many per command. Its key predates JudgeCommands, which is why it does
	// not say requests (ADR 26-10-02-054 §1).
	JudgeCredentials bool `yaml:"judgeCredentials" env:"DISCOBOX_JUDGE_CREDENTIALS" doc:"Have each project's judge decide whether each credential-bearing request is part of what the credential's approved use allows. Off by default. While it is off credentials are resolved for whatever request carries them, held only to the host and grant they were approved for. Commands are judged by judgeCommands."`

	// JudgeBackend is what answers when judgeCommands or judgeCredentials is on: each
	// project's judge discobox, or TypeSafe's Jev, asked by this server
	// directly (ADR 26-10-01-324). It is the server's choice, like judging
	// itself. auto is resolved by Load, so after it this is harness or jev:
	// a server given a Jev key judges with Jev without being told twice.
	JudgeBackend string `yaml:"judgeBackend" env:"DISCOBOX_JUDGE_BACKEND" enum:"auto,harness,jev" default:"auto" doc:"What judges credential use when judgeCommands or judgeCredentials is on: auto, jev when jevApiKey is set and harness otherwise; harness, a judge discobox per project running the project's default harness; or jev, TypeSafe's Jev, asked by this server directly, which requires jevApiKey."`
	// JevAPIKey is required when JudgeBackend is jev, and is what auto
	// chooses Jev by.
	JevAPIKey string `yaml:"jevApiKey" env:"DISCOBOX_JEV_API_KEY" doc:"TypeSafe API key the server asks Jev with. Setting it makes judgeBackend auto choose jev; judgeBackend jev requires it." example:"-"`
	// JevModel is pinned to a version by default, because the thresholds the
	// judge decides Jev's probabilities against were tuned on one.
	JevModel string `yaml:"jevModel" env:"DISCOBOX_JEV_MODEL" default:"jev-1.13.0" doc:"The Jev model asked when judgeBackend is jev. A version rather than the jev-latest alias, which moves when TypeSafe ships a new one."`
	// JevRefusal is what happens to a job Jev does not allow: put to the
	// project's judge discobox, which each project then keeps, or refused.
	// Put by default (ADR 26-10-07-937): Jev's refusals included requests
	// and commands a correct judge allows at every score, and the judge
	// discobox is there to settle them. The key's name, jevUnsure, is older
	// than that: it once sent on only the refusals Jev was unsure of, and
	// servers' configuration files still spell it that way.
	JevRefusal string `yaml:"jevUnsure" env:"DISCOBOX_JEV_UNSURE" enum:"harness,refuse" default:"harness" doc:"What a jev judge does with a command, request or delegation it does not allow: harness, ask the project's judge discobox (as judgeBackend harness runs one), which then decides, so every project keeps a judge discobox for them and Jev decides only its allows; or refuse it, and run no judge discobox. An ask to be shown a request's body is not a refusal and goes back to be answered either way; with harness, the rounds after the judge discobox asked for a body are its to decide too."`

	// ArchiveRetention is how long an archived sandbox is kept before it is
	// purged, for projects that have not set their own retention. Zero means
	// nothing configured it and sandboxes.DefaultArchiveRetention applies; a
	// project's own setting always wins over both.
	ArchiveRetention time.Duration `yaml:"archiveRetention" env:"DISCOBOX_ARCHIVE_RETENTION" doc:"How long an archived sandbox is kept before purging, for projects with no retention of their own, as a Go duration. Defaults to 24h." example:"168h"`

	// ImageRetention is how long an unused Discobox image is kept on the host
	// daemon. Zero means nothing configured it, and that distinction is
	// load-bearing: the engine records a configured value in each pool
	// container's configuration, so materializing a default here would change
	// every pool's revision and recreate it for no reason. This field is where
	// that distinction lives now; imagereap keeps its own unexported copy for
	// the pool agent's side of the same wire.
	ImageRetention time.Duration `yaml:"imageRetention" env:"DISCOBOX_IMAGE_RETENTION" doc:"How long an unused Discobox image is kept on the host Docker daemon. The configured value is propagated into pool containers, so one setting governs both. A Go duration; defaults to 24h." example:"72h"`

	// DockerPoolImage overrides the pool agent image the Docker provider runs.
	DockerPoolImage string `yaml:"dockerPoolImage" env:"DISCOBOX_DOCKER_POOL_IMAGE" doc:"Pool agent image the Docker provider runs. Defaults to the released image for this build." example:"ghcr.io/discobox-ai/discobox-pool-agent:v0.8.0"`

	// ImageCacheDir is the image store (ADR 0113): an OCI layout a CLI stages a
	// release's images into before it starts this server, which a provider
	// fetches a VM guest through and a pool on this machine loads a container
	// image from before pulling one. The CLI names its own on every server it
	// launches; a server started otherwise keeps one under its cache.
	ImageCacheDir string `yaml:"imageCacheDir" env:"DISCOBOX_IMAGE_CACHE_DIR" doc:"OCI image layout that VM guest images are fetched through, and that a pool on this machine loads images from before pulling them. The CLI names the one it stages a release's images into. Defaults to <cacheDir>/images." example:"/var/cache/discobox/images"`

	// OverlayDir is where the overlays of templates with no image are staged
	// (ADR 0145 §2), and the one directory a harness's file:// manifest
	// reference may name a file in. A harness config is registered over the
	// API, so a reference anywhere else would have this server read whatever
	// path a caller named and answer with what it found.
	OverlayDir string `yaml:"overlayDir" env:"DISCOBOX_OVERLAY_DIR" doc:"Directory the overlays of non-Linux sandbox templates are staged in, and the only one a harness's file:// manifest reference may name a file in. Defaults to <cacheDir>/overlays." example:"/var/cache/discobox/overlays"`

	// WSLCCommand overrides the WSL Containers program the Windows host is
	// checked for at startup, for a host that keeps it somewhere this check
	// would not look. It accepts a full path.
	WSLCCommand string `yaml:"wslcCommand" env:"DISCOBOX_WSLC_COMMAND" doc:"The WSL Containers program to look for on a Windows host, as a name on PATH or a full path. Empty looks for the component's own name, wslc. Windows only." example:"'C:\\tools\\wslc.exe'"`

	// DevImageSync converges the watcher-built images onto each Docker daemon
	// before it hosts a development pool, and DevImageManifest names the file
	// listing them.
	DevImageSync     bool   `yaml:"devImageSync" env:"DISCOBOX_DEV_DOCKER_IMAGE_SYNC" doc:"Converge locally built images onto each Docker daemon before it hosts a development pool."`
	DevImageManifest string `yaml:"devImageManifest" env:"DISCOBOX_DEV_DOCKER_IMAGE_MANIFEST" doc:"Path to the manifest listing the images devImageSync converges. Required when devImageSync is set." example:"/tmp/discobox-dev-images.json"`

	// DevelopmentImages is the image set read from DevImageManifest. Derived,
	// because it is the contents of a file rather than a setting.
	DevelopmentImages []devimage.Image `yaml:"-"`

	// Iroh groups the settings of the iroh transport (ADR 0052).
	Iroh IrohSettings `yaml:"iroh" doc:"Settings for the iroh transport, which is how a server is reached from another network without a port, a certificate, or a name."`

	// OpenTelemetry metrics settings.
	// OTelMetricsEnabled has no env tag because OTEL_METRICS_EXPORTER names an
	// exporter rather than carrying a boolean, and that name is fixed by the
	// OpenTelemetry environment specification rather than by us. Load applies
	// it by hand; the file spells the same choice as a boolean.
	OTelMetricsEnabled bool `yaml:"otelMetricsEnabled" doc:"Export OpenTelemetry metrics over OTLP/HTTP, to the collector the standard OTEL_EXPORTER_OTLP_ENDPOINT environment variable names, such as http://otel-collector:4318. The OTEL_METRICS_EXPORTER=otlp environment variable sets this too."`
	// OTelMetricExportInterval has no env tag for the same reason
	// OTelMetricsEnabled has none: the OpenTelemetry specification defines
	// OTEL_METRIC_EXPORT_INTERVAL as a number of milliseconds, not a Go
	// duration, and that spelling is not ours to change. The file spells it as
	// a duration like every other interval here.
	OTelMetricExportInterval time.Duration `yaml:"otelMetricExportInterval" default:"1s" doc:"How often metrics are exported. The OTEL_METRIC_EXPORT_INTERVAL environment variable sets this too, in milliseconds, as the OpenTelemetry specification defines it."`
}

// IrohSettings configures the iroh transport's reach.
type IrohSettings struct {
	// RelayURLs replaces n0's public relays with a deployment's own
	// (ADR 0096 §6, configuration file). Empty keeps n0's, which are free but rate-limited,
	// shared, and carry no uptime guarantee.
	//
	// Clients need the same list. An address carries a peer ID and nothing
	// else, so a server that moves off n0's relays does not move its clients
	// with it.
	RelayURLs []string `yaml:"relayUrls" env:"DISCOBOX_IROH_RELAY_URLS" doc:"Relay servers to use instead of n0's public ones, which are free and need no configuration but rate-limit traffic, are shared with every other iroh deployment, and carry no uptime guarantee. Running your own means running iroh-relay and listing it here. Clients need the same list (discobox --iroh-relay): an address does not carry the server's relay, so a half-configured pair fails when it tries to connect. Address discovery is separate, still uses n0's public service, and is not configurable yet." example:"[https://relay.example.com]"`

	// LogLevel turns on the transport's own account of itself. An iroh
	// connection fails in layers — the socket, the relay, the handshake, the
	// admission check — and the error a client is handed names only the top
	// one, so a server that cannot say what it did at each layer can only be
	// diagnosed from the client side.
	LogLevel string `yaml:"logLevel" env:"DISCOBOX_IROH_LOG" doc:"Log the iroh transport as it binds, accepts and refuses: off (the default), error, warn, info, debug, or trace. It also sets the verbosity of iroh's own tracing, which is written to this server's log. The matching client-side setting is discobox --iroh-log." example:"debug"`
}

// ConfigFileVar names the configuration file, and DefaultConfigFileName is
// what Load reads inside the platform's config directory when it does not.
//
// The path comes from the environment and nowhere else. `configDir` is itself
// a setting, so letting it relocate the file that declares it is a loop with no
// fixed point (ADR 0096 §1, configuration file).
const (
	ConfigFileVar         = "DISCOBOX_CONFIG_FILE"
	DefaultConfigFileName = "server.yaml"
)

// ConfigFilePath is the file Load reads: ConfigFileVar when set, otherwise
// DefaultConfigFileName under the platform's config directory. An empty
// ConfigFileVar means read nothing.
func ConfigFilePath() string {
	if name, ok := os.LookupEnv(ConfigFileVar); ok {
		return strings.TrimSpace(name)
	}
	return filepath.Join(xdg.ConfigHome, appName, DefaultConfigFileName)
}

// Load resolves configuration from defaults, then the configuration file, then
// the environment — in that order, so the environment wins (ADR 0096 §4, configuration file).
//
// A missing file is not an error: the environment alone configures a server
// completely, which is what keeps a deployment that has never seen this file
// working exactly as before. A file that exists and cannot be parsed, or that
// names a key nothing defines, is an error.
func Load() (*Config, error) {
	cfg := &Config{}
	if err := configfile.ApplyDefaults(cfg); err != nil {
		return nil, err
	}

	path := ConfigFilePath()
	_, named := os.LookupEnv(ConfigFileVar)
	fromFile := map[string]bool{}
	cfg.ConfigFile = path
	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if fromFile, err = configfile.Decode(cfg, data, path); err != nil {
				return nil, err
			}
			cfg.ConfigFileRead = true
		case os.IsNotExist(err) && !named:
			// Nothing at the default path, which is the ordinary case: the
			// environment alone configures a server completely.
		case os.IsNotExist(err):
			// A path the operator named explicitly is different. Reading
			// nothing there and starting on defaults is the silent
			// misconfiguration this file exists to prevent (ADR 0096 §3, configuration file), and
			// it is the one setting no schema can check, because it is what
			// finds the schema.
			return nil, fmt.Errorf("%s names %s, which does not exist", ConfigFileVar, path)
		default:
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
	}

	fromEnv, err := configfile.ApplyEnv(cfg, os.LookupEnv)
	if err != nil {
		return nil, err
	}
	if exporter, ok := os.LookupEnv(otelExporterEnv); ok && strings.TrimSpace(exporter) != "" {
		cfg.OTelMetricsEnabled = strings.EqualFold(strings.TrimSpace(exporter), "otlp")
		fromEnv["otelMetricsEnabled"] = true
	}
	if raw, ok := os.LookupEnv(otelExportIntervalEnv); ok && strings.TrimSpace(raw) != "" {
		milliseconds, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not a number of milliseconds", otelExportIntervalEnv, raw)
		}
		cfg.OTelMetricExportInterval = time.Duration(milliseconds) * time.Millisecond
		fromEnv["otelMetricExportInterval"] = true
	}
	configured := func(path string) bool { return fromFile[path] || fromEnv[path] }

	// Defaults that are computed rather than literal, applied only where
	// nothing claimed the field.
	if !configured("dataDir") {
		cfg.DataDir = filepath.Join(xdg.DataHome, appName)
	}
	if !configured("configDir") {
		cfg.ConfigDir = filepath.Join(xdg.ConfigHome, appName)
	}
	if !configured("cacheDir") {
		cfg.CacheDir = filepath.Join(xdg.CacheHome, appName)
	}
	if !configured("stateDir") {
		cfg.StateDir = filepath.Join(xdg.StateHome, appName)
	}
	// A key written empty counts as absent here, unlike most: the store is not
	// optional (ADR 0113 §3), and a server without one cannot fetch a VM guest
	// at all — so one blank line in a configuration file would take every VM
	// pool on the machine down.
	if !configured("imageCacheDir") || strings.TrimSpace(cfg.ImageCacheDir) == "" {
		cfg.ImageCacheDir = filepath.Join(cfg.CacheDir, "images")
	}
	// Empty counts as absent for the reason the image store's does: a blank
	// line would otherwise leave no directory at all, and every manifest file
	// refused.
	if !configured("overlayDir") || strings.TrimSpace(cfg.OverlayDir) == "" {
		cfg.OverlayDir = filepath.Join(cfg.CacheDir, "overlays")
	}
	if !configured("databaseDsn") {
		cfg.DatabaseDSN = defaultDatabaseDSN(cfg.DataDir)
	}
	// auto is a choice made from the key: Jev when there is one to ask it
	// with, and the judge discobox otherwise.
	if cfg.JudgeBackend == JudgeBackendAuto {
		cfg.JudgeBackend = JudgeBackendHarness
		if strings.TrimSpace(cfg.JevAPIKey) != "" {
			cfg.JudgeBackend = JudgeBackendJev
		}
	}
	if !configured("databaseDriver") {
		cfg.DatabaseDriver = gormdb.DetectDriver(cfg.DatabaseDSN)
	}
	if !configured("defaultSandboxImage") {
		cfg.DefaultSandboxImage = sandbox.DefaultSandboxImageName
	}
	if !configured("name") {
		cfg.Name = defaultName()
	}
	cfg.Name = strings.TrimSpace(cfg.Name)
	// The listen list always carries local IPC, whether it came from the file,
	// the environment, or nowhere: a running server no one can reach is worse
	// than one that opened a socket nobody asked for.
	cfg.Listen = requireLocalListenEndpoint(cfg.Listen)

	hostID, err := hostid.Get()
	if err != nil {
		return nil, fmt.Errorf("resolve host ID: %w", err)
	}
	cfg.HostID = hostID

	cfg.HarnessImages = harnessdefs.ImageOverridesFromEnv(os.Getenv)
	if cfg.ReleaseManifest != "" {
		manifest, err := releasemanifest.Read(cfg.ReleaseManifest)
		if err != nil {
			return nil, err
		}
		for slug := range manifest.Images.Harnesses {
			if err := harnessdefs.ValidateSlug(slug); err != nil {
				return nil, fmt.Errorf("release manifest harness: %w", err)
			}
		}
		cfg.Release = &manifest
		cfg.DockerPoolImage = manifest.Images.PoolAgent
		cfg.DefaultSandboxImage = manifest.Images.SandboxAgent
		cfg.DefaultSandboxImageDigest = ""
		cfg.HarnessImages = manifest.Images.Harnesses
		cfg.DevImageSync = false
		cfg.DevImageManifest = ""
	}
	if cfg.DevImageSync {
		if cfg.DevImageManifest == "" {
			return nil, fmt.Errorf("devImageManifest is required when devImageSync is set")
		}
		manifest, err := devimage.Read(cfg.DevImageManifest)
		if err != nil {
			return nil, err
		}
		cfg.DevelopmentImages = manifest.Images
	}

	if err := cfg.validate(configured); err != nil {
		return nil, err
	}
	return cfg, nil
}

// HasIrohEndpoint reports whether listen names an iroh endpoint, the one
// transport whose peers prove an enrolled identity.
func HasIrohEndpoint(listen []string) bool {
	for _, raw := range listen {
		if parsed, err := endpoint.Parse(raw); err == nil && parsed.Scheme == "iroh" {
			return true
		}
	}
	return false
}

// validate rejects a configuration the server cannot run on. Messages name the
// YAML key rather than the environment variable, because the key is what the
// schema, the editor, and this package all agree on; the variable that set it
// is one of several ways in.
func (c *Config) validate(configured func(string) bool) error {
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	if len(c.Listen) == 0 {
		return fmt.Errorf("listen must include at least one endpoint")
	}
	for _, raw := range c.Listen {
		if _, err := endpoint.Parse(raw); err != nil {
			return fmt.Errorf("listen: %w", err)
		}
	}
	for _, dir := range []struct{ key, value string }{
		{"dataDir", c.DataDir},
		{"configDir", c.ConfigDir},
		{"cacheDir", c.CacheDir},
		{"stateDir", c.StateDir},
	} {
		if dir.value == "" {
			return fmt.Errorf("%s is required", dir.key)
		}
	}
	if c.DatabaseDSN == "" {
		return fmt.Errorf("databaseDsn is required")
	}
	switch c.DatabaseDriver {
	case gormdb.DriverSQLite, gormdb.DriverPostgres:
	default:
		return fmt.Errorf("databaseDriver must be one of: sqlite, postgres")
	}
	if c.DiscobotPublicKey != "" {
		if _, err := discobot.ParsePublicKey(c.DiscobotPublicKey); err != nil {
			return fmt.Errorf("discobotPublicKey: %w", err)
		}
	}
	if c.AuthRequired && c.DiscobotPublicKey == "" && !HasIrohEndpoint(c.Listen) {
		return fmt.Errorf("authRequired requires discobotPublicKey or an iroh endpoint in listen: without either nobody can authenticate")
	}
	if c.DispatcherPollInterval <= 0 {
		return fmt.Errorf("dispatcherPollInterval must be greater than 0")
	}
	if c.SandboxReconcileJobConcurrency < 1 {
		return fmt.Errorf("sandboxReconcileJobConcurrency must be at least 1")
	}
	// Zero means unconfigured for both retentions, which is why they are
	// checked against what was actually set rather than against the value: a
	// window somebody wrote as 0 would purge on sight, and the two ways to get
	// a retention wrong are destroying data that was still wanted and never
	// reclaiming any. Neither should happen quietly.
	for _, retention := range []struct {
		key   string
		value time.Duration
	}{
		{"archiveRetention", c.ArchiveRetention},
		{"imageRetention", c.ImageRetention},
	} {
		if configured(retention.key) && retention.value <= 0 {
			return fmt.Errorf("%s must be greater than 0", retention.key)
		}
	}
	if c.OTelMetricsEnabled && c.OTelMetricExportInterval <= 0 {
		return fmt.Errorf("otelMetricExportInterval must be greater than 0")
	}
	switch c.JudgeBackend {
	case JudgeBackendAuto:
		// Load resolves it; a Config built some other way has not been.
		return fmt.Errorf("judgeBackend auto was not resolved")
	case JudgeBackendHarness:
	case JudgeBackendJev:
		// Refused at startup whether or not judging is on: a server told to
		// judge with Jev and given no key would otherwise start, and refuse
		// every credential the moment judging is turned on.
		if strings.TrimSpace(c.JevAPIKey) == "" {
			return fmt.Errorf("jevApiKey is required when judgeBackend is jev")
		}
		if strings.TrimSpace(c.JevModel) == "" {
			return fmt.Errorf("jevModel is required when judgeBackend is jev")
		}
		if c.JevRefusal != JevRefusalRefuse && c.JevRefusal != JevRefusalHarness {
			return fmt.Errorf("jevUnsure must be one of: %s, %s", JevRefusalRefuse, JevRefusalHarness)
		}
	default:
		return fmt.Errorf("judgeBackend must be one of: %s, %s, %s", JudgeBackendAuto, JudgeBackendHarness, JudgeBackendJev)
	}
	return nil
}

// The judge backends judgeBackend chooses between.
const (
	JudgeBackendAuto    = "auto"
	JudgeBackendHarness = "harness"
	JudgeBackendJev     = "jev"
)

// What JevRefusal (jevUnsure) does with a job Jev does not allow.
const (
	JevRefusalRefuse  = "refuse"
	JevRefusalHarness = "harness"
)

func defaultDatabaseDSN(dataDir string) string {
	return "sqlite3://" + filepath.Join(dataDir, "discobox.db")
}

// requireLocalListenEndpoint adds the local IPC endpoint the server always
// needs but the operator may not have named. It is how the CLI reaches the
// server, and losing it silently would leave a running server no one can talk
// to.
//
// Nothing else is implied. The server opens no TCP listener unless
// DISCOBOX_SERVER_LISTEN names one: a TCP port is a machine-wide surface — and
// on Windows a firewall prompt — that most deployments never need. No pool
// backend requires it either. libkrun dials over VSOCK, wslc over its guest
// relay's socket, and the Docker provider binds this socket into its pool
// containers whenever its daemon is local. HTTP is for the cases that genuinely
// reach the control plane over IP: a remote Docker daemon, or a cloud backend.
func requireLocalListenEndpoint(endpoints []string) []string {
	for _, raw := range endpoints {
		parsed, err := endpoint.Parse(raw)
		if err != nil {
			continue
		}
		if parsed.Scheme == "unix" || parsed.Scheme == "npipe" {
			return endpoints
		}
	}
	return append([]string{endpoint.DefaultEndpoint()}, endpoints...)
}

// defaultName is the name a server takes when nothing gives it one: this
// machine's hostname. A hostname that cannot be read leaves the name empty
// rather than failing startup — a client names an unnamed server after its
// address, which is what it does for one that predates names altogether.
func defaultName() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return name
}
