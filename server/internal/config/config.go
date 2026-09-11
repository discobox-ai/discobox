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

	"github.com/discobox-ai/discobox/devimage"
	"github.com/discobox-ai/discobox/endpoint"
	"github.com/discobox-ai/discobox/internal/hostid"
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
// `doc` the description an operator reads in their editor. A field tagged
// `yaml:"-"` is derived rather than configured, and appears in neither.
type Config struct {
	// Server settings.
	Port   int      `yaml:"port" env:"PORT" default:"18080" doc:"TCP port for an http:// listen endpoint that does not name one."`
	Listen []string `yaml:"listen" env:"DISCOBOX_SERVER_LISTEN" doc:"Endpoints to listen on. Local IPC is added when none is named, so the CLI can always reach the server."`

	// XDG-backed application directories. Their defaults are derived from the
	// platform's base directories rather than being literals, so they are
	// described here and computed in Load.
	DataDir   string `yaml:"dataDir" env:"DISCOBOX_DATA_DIR" doc:"Durable server state: the database, the SSH host key, the iroh endpoint key, authorized_keys and authorized_ids. Defaults to <XDG data home>/discobox."`
	ConfigDir string `yaml:"configDir" env:"DISCOBOX_CONFIG_DIR" doc:"Operator-edited configuration. Defaults to <XDG config home>/discobox. It cannot relocate the configuration file itself, which is found from the environment (ADR 0096 §1)."`
	CacheDir  string `yaml:"cacheDir" env:"DISCOBOX_CACHE_DIR" doc:"Reproducible data that may be deleted. Defaults to <XDG cache home>/discobox."`
	StateDir  string `yaml:"stateDir" env:"DISCOBOX_STATE_DIR" doc:"State that should survive a restart but is not precious. Defaults to <XDG state home>/discobox."`

	// HostID identifies the machine this server runs on, resolved the same way
	// a CLI on this machine resolves it. A create request whose origin reports
	// this host ID came from this filesystem, which is what makes binding a
	// client's local source directory possible.
	//
	// Derived, never configured: an operator who set it would be claiming to
	// be a different machine.
	HostID string `yaml:"-"`

	// Database settings.
	DatabaseDSN     string        `yaml:"databaseDsn" env:"DATABASE_DSN" doc:"Database DSN. Defaults to a SQLite file under dataDir."`
	DatabaseReadDSN string        `yaml:"databaseReadDsn" env:"DATABASE_READ_DSN" doc:"Optional separate DSN for reads, for a deployment with a read replica."`
	DatabaseDriver  gormdb.Driver `yaml:"databaseDriver" env:"DATABASE_DRIVER" enum:"sqlite,postgres" doc:"Database driver. Detected from databaseDsn when unset."`

	// Secret encryption settings.
	EncryptionKey string `yaml:"encryptionKey" env:"DISCOBOX_ENCRYPTION_KEY" doc:"Base64 AES key that seals stored secrets. Secrets are stored unsealed when empty."`

	// Reconcile engine settings.
	DispatcherPollInterval         time.Duration `yaml:"dispatcherPollInterval" env:"DISPATCHER_POLL_INTERVAL" default:"1s" doc:"How often the job dispatcher polls for work."`
	SandboxReconcileJobConcurrency int           `yaml:"sandboxReconcileJobConcurrency" env:"SANDBOX_RECONCILE_JOB_CONCURRENCY" default:"4" doc:"How many sandbox reconcile jobs run at once."`

	// Sandbox settings.
	DefaultSandboxImage string `yaml:"defaultSandboxImage" env:"DISCOBOX_DEFAULT_SANDBOX_IMAGE" doc:"Image for sandboxes that name no harness. Defaults to the built-in sandbox agent image."`
	// DefaultSandboxImageDigest is the identity behind DefaultSandboxImage.
	// Sandboxes with no harness config run the default image, and the tag alone
	// cannot say which build that is — dev workflows rebuild tags in place — so
	// the digest is what lets such a sandbox report and take an upgrade
	// (ADR 0016 §1's "a digest and not just a tag"). Empty when unknown, which
	// simply means those sandboxes report no upgrade.
	DefaultSandboxImageDigest string `yaml:"defaultSandboxImageDigest" env:"DISCOBOX_DEFAULT_SANDBOX_IMAGE_DIGEST" doc:"Digest identifying the build behind defaultSandboxImage. Those sandboxes report no upgrade when it is unknown."`

	// ArchiveRetention is how long an archived sandbox is kept before it is
	// purged, for projects that have not set their own retention. Zero means
	// nothing configured it and sandboxes.DefaultArchiveRetention applies; a
	// project's own setting always wins over both.
	ArchiveRetention time.Duration `yaml:"archiveRetention" env:"DISCOBOX_ARCHIVE_RETENTION" doc:"How long an archived sandbox is kept before purging, for projects with no retention of their own."`

	// ImageRetention is how long an unused Discobox image is kept on the host
	// daemon. Zero means nothing configured it, and that distinction is
	// load-bearing: the engine records a configured value in each pool
	// container's configuration, so materializing a default here would change
	// every pool's revision and recreate it for no reason. This field is where
	// that distinction lives now; imagereap keeps its own unexported copy for
	// the pool agent's side of the same wire.
	ImageRetention time.Duration `yaml:"imageRetention" env:"DISCOBOX_IMAGE_RETENTION" doc:"How long an unused Discobox image is kept on the host Docker daemon. The configured value is propagated into pool containers, so one setting governs both."`

	// DockerPoolImage overrides the pool agent image the Docker provider runs.
	DockerPoolImage string `yaml:"dockerPoolImage" env:"DISCOBOX_DOCKER_POOL_IMAGE" doc:"Pool agent image the Docker provider runs. Defaults to the released image for this build."`

	// WSLCCommand overrides the WSL Containers program the Windows host is
	// checked for at startup, for a host that keeps it somewhere this check
	// would not look. It accepts a full path.
	WSLCCommand string `yaml:"wslcCommand" env:"DISCOBOX_WSLC_COMMAND" doc:"The WSL Containers program to look for on a Windows host, as a name on PATH or a full path. Empty looks for the component's own name, wslc. Windows only."`

	// DevImageSync converges the watcher-built images onto each Docker daemon
	// before it hosts a development pool, and DevImageManifest names the file
	// listing them.
	DevImageSync     bool   `yaml:"devImageSync" env:"DISCOBOX_DEV_DOCKER_IMAGE_SYNC" doc:"Converge locally built images onto each Docker daemon before it hosts a development pool."`
	DevImageManifest string `yaml:"devImageManifest" env:"DISCOBOX_DEV_DOCKER_IMAGE_MANIFEST" doc:"Path to the manifest listing the images devImageSync converges. Required when devImageSync is set."`

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
	OTelMetricsEnabled bool `yaml:"otelMetricsEnabled" doc:"Export OpenTelemetry metrics over OTLP. The OTEL_METRICS_EXPORTER=otlp environment variable sets this too."`
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
	RelayURLs []string `yaml:"relayUrls" env:"DISCOBOX_IROH_RELAY_URLS" doc:"Relay servers to use instead of n0's public ones, which are free and need no configuration but rate-limit traffic, are shared with every other iroh deployment, and carry no uptime guarantee. Running your own means running iroh-relay and listing it here. Clients need the same list (discobox --iroh-relay): an address does not carry the server's relay, so a half-configured pair fails when it tries to connect. Address discovery is separate, still uses n0's public service, and is not configurable yet."`

	// LogLevel turns on the transport's own account of itself. An iroh
	// connection fails in layers — the socket, the relay, the handshake, the
	// admission check — and the error a client is handed names only the top
	// one, so a server that cannot say what it did at each layer can only be
	// diagnosed from the client side.
	LogLevel string `yaml:"logLevel" env:"DISCOBOX_IROH_LOG" doc:"Log the iroh transport as it binds, accepts and refuses: off (the default), error, warn, info, debug, or trace. It also sets the verbosity of iroh's own tracing, which is written to this server's log. The matching client-side setting is discobox --iroh-log."`
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
	if err := applyDefaults(cfg); err != nil {
		return nil, err
	}

	path := ConfigFilePath()
	_, named := os.LookupEnv(ConfigFileVar)
	fromFile := map[string]bool{}
	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if fromFile, err = decodeFile(cfg, data, path); err != nil {
				return nil, err
			}
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

	fromEnv, err := applyEnv(cfg, os.LookupEnv)
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
	if !configured("databaseDsn") {
		cfg.DatabaseDSN = defaultDatabaseDSN(cfg.DataDir)
	}
	if !configured("databaseDriver") {
		cfg.DatabaseDriver = gormdb.DetectDriver(cfg.DatabaseDSN)
	}
	if !configured("defaultSandboxImage") {
		cfg.DefaultSandboxImage = sandbox.DefaultSandboxImageName
	}
	// The listen list always carries local IPC, whether it came from the file,
	// the environment, or nowhere: a running server no one can reach is worse
	// than one that opened a socket nobody asked for.
	cfg.Listen = requireLocalListenEndpoint(cfg.Listen)

	hostID, err := hostid.Get()
	if err != nil {
		return nil, fmt.Errorf("resolve host ID: %w", err)
	}
	cfg.HostID = hostID

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
	return nil
}

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
