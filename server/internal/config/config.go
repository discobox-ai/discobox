// Package config loads discobox-server runtime configuration from the
// environment.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/adrg/xdg"

	"github.com/obot-platform/discobox/controlplane"
	"github.com/obot-platform/discobox/devimage"
	"github.com/obot-platform/discobox/gormdb"
	"github.com/obot-platform/discobox/internal/hostid"
	"github.com/obot-platform/discobox/localipc"
	"github.com/obot-platform/discobox/pool-agent/imagereap"
	"github.com/obot-platform/discobox/server/internal/harnessdefs"
	"github.com/obot-platform/discobox/server/internal/sandbox"
)

const appName = "discobox"

// Config holds all configuration for discobox-server.
type Config struct {
	// Server settings.
	Port                int
	Listen              []string
	AutoShutdownTimeout time.Duration

	// SSHListen is the address the SSH control-plane ingress (ADR 0024) binds,
	// e.g. ":3222". Empty disables it: unlike the local-IPC HTTP endpoint, an
	// SSH listener is a machine-wide TCP surface that is opted into, never
	// implied (server/DESIGN.md "Listen Endpoints").
	SSHListen string

	// SSHAdvertiseAddress is the host:port SSH clients should dial, which is
	// not the same thing as SSHListen: a bind address is frequently not
	// dialable at all (":3222" names no host, "0.0.0.0:3222" is not an
	// address a client can use), and the reachable endpoint may be a load
	// balancer, a NAT, or a tunnel in front of this process. It is served by
	// GET /ssh so clients discover it instead of hard-coding a port. Empty
	// when SSH is disabled; otherwise resolved from
	// DISCOBOX_SSH_ADVERTISE_ADDRESS, falling back to SSHListen's own host and
	// port with a wildcard host replaced by loopback.
	SSHAdvertiseAddress string

	// XDG-backed application directories.
	DataDir   string
	ConfigDir string
	CacheDir  string
	StateDir  string

	// HostID identifies the machine this server runs on, resolved the same way
	// a CLI on this machine resolves it. A create request whose origin reports
	// this host ID came from this filesystem, which is what makes binding a
	// client's local source directory possible.
	HostID string

	// Database settings.
	DatabaseDSN     string
	DatabaseReadDSN string
	DatabaseDriver  gormdb.Driver

	// Secret encryption settings.
	EncryptionKey string

	// Reconcile engine settings.
	DispatcherPollInterval         time.Duration
	SandboxReconcileJobConcurrency int

	// Sandbox settings.
	DefaultSandboxImage string
	// DefaultSandboxImageDigest is the identity behind DefaultSandboxImage.
	// Sandboxes with no harness config run the default image, and the tag alone
	// cannot say which build that is — dev workflows rebuild tags in place — so
	// the digest is what lets such a sandbox report and take an upgrade
	// (ADR 0016 §1's "a digest and not just a tag"). Empty when unknown, which
	// simply means those sandboxes report no upgrade.
	DefaultSandboxImageDigest string

	// HarnessImages overrides built-in harness definition images, keyed by
	// definition ID. Dev builds populate this from DISCOBOX_HARNESS_<ID>_IMAGE
	// so freshly tagged harness images flow through on server restart.
	HarnessImages map[string]string
	// DevelopmentImages is the watcher-built image set to converge onto each
	// Docker daemon before it hosts development pools.
	DevelopmentImages []devimage.Image

	// OpenTelemetry metrics settings.
	OTelMetricsEnabled       bool
	OTelMetricExportInterval time.Duration
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	cfg := &Config{}

	cfg.Port = getEnvInt("PORT", controlplane.DefaultPort)
	cfg.Listen = listenEndpoints()
	cfg.AutoShutdownTimeout = getEnvDuration("DISCOBOX_SERVER_IDLE_TIMEOUT", 0)
	cfg.SSHListen = getEnv("DISCOBOX_SSH_LISTEN", "")
	cfg.SSHAdvertiseAddress = getEnv("DISCOBOX_SSH_ADVERTISE_ADDRESS", "")

	cfg.DataDir = getEnv("DISCOBOX_DATA_DIR", filepath.Join(xdg.DataHome, appName))
	cfg.ConfigDir = getEnv("DISCOBOX_CONFIG_DIR", filepath.Join(xdg.ConfigHome, appName))
	cfg.CacheDir = getEnv("DISCOBOX_CACHE_DIR", filepath.Join(xdg.CacheHome, appName))
	cfg.StateDir = getEnv("DISCOBOX_STATE_DIR", filepath.Join(xdg.StateHome, appName))
	hostID, err := hostid.Get()
	if err != nil {
		return nil, fmt.Errorf("resolve host ID: %w", err)
	}
	cfg.HostID = hostID
	cfg.DatabaseDSN = getEnv("DATABASE_DSN", defaultDatabaseDSN(cfg.DataDir))
	cfg.DatabaseReadDSN = getEnv("DATABASE_READ_DSN", "")
	cfg.DatabaseDriver = getEnvDriver("DATABASE_DRIVER", gormdb.DetectDriver(cfg.DatabaseDSN))

	cfg.EncryptionKey = getEnv("DISCOBOX_ENCRYPTION_KEY", "")

	cfg.DispatcherPollInterval = getEnvDuration("DISPATCHER_POLL_INTERVAL", time.Second)
	cfg.SandboxReconcileJobConcurrency = getEnvInt("SANDBOX_RECONCILE_JOB_CONCURRENCY", 4)
	cfg.DefaultSandboxImage = getEnv("DISCOBOX_DEFAULT_SANDBOX_IMAGE", sandbox.DefaultSandboxImageName)
	cfg.DefaultSandboxImageDigest = getEnv("DISCOBOX_DEFAULT_SANDBOX_IMAGE_DIGEST", "")
	cfg.HarnessImages = harnessdefs.ImageOverridesFromEnv(os.Getenv)
	developmentImageSync, err := getEnvBool(devimage.SyncEnv, false)
	if err != nil {
		return nil, err
	}
	if developmentImageSync {
		manifestPath := getEnv(devimage.ManifestEnv, "")
		if manifestPath == "" {
			return nil, fmt.Errorf("%s is required when %s is enabled", devimage.ManifestEnv, devimage.SyncEnv)
		}
		manifest, err := devimage.Read(manifestPath)
		if err != nil {
			return nil, err
		}
		cfg.DevelopmentImages = manifest.Images
	}
	// Validated here, though it is read where it is used (dockerworker.New, and
	// the pool agent for its own daemon), so a typo fails startup with a clear
	// message instead of quietly taking one provider down with it.
	if _, err := imagereap.ConfiguredRetention(); err != nil {
		return nil, err
	}
	cfg.OTelMetricsEnabled = strings.EqualFold(getEnv("OTEL_METRICS_EXPORTER", "none"), "otlp")
	cfg.OTelMetricExportInterval = getEnvMillisecondsDuration("OTEL_METRIC_EXPORT_INTERVAL", time.Second)

	if cfg.Port <= 0 || cfg.Port > 65535 {
		return nil, fmt.Errorf("PORT must be between 1 and 65535")
	}
	if len(cfg.Listen) == 0 {
		return nil, fmt.Errorf("DISCOBOX_SERVER_LISTEN must include at least one endpoint")
	}
	for _, endpoint := range cfg.Listen {
		if _, err := localipc.Parse(endpoint); err != nil {
			return nil, fmt.Errorf("DISCOBOX_SERVER_LISTEN: %w", err)
		}
	}
	if cfg.AutoShutdownTimeout < 0 {
		return nil, fmt.Errorf("DISCOBOX_SERVER_IDLE_TIMEOUT must be greater than or equal to 0")
	}
	if cfg.SSHListen != "" {
		if _, _, err := net.SplitHostPort(cfg.SSHListen); err != nil {
			return nil, fmt.Errorf("DISCOBOX_SSH_LISTEN must be a host:port address: %w", err)
		}
	}
	if cfg.SSHAdvertiseAddress != "" {
		if _, _, err := net.SplitHostPort(cfg.SSHAdvertiseAddress); err != nil {
			return nil, fmt.Errorf("DISCOBOX_SSH_ADVERTISE_ADDRESS must be a host:port address: %w", err)
		}
		if cfg.SSHListen == "" {
			return nil, fmt.Errorf("DISCOBOX_SSH_ADVERTISE_ADDRESS is set but DISCOBOX_SSH_LISTEN is not: there is no SSH ingress to advertise")
		}
	}
	cfg.SSHAdvertiseAddress = resolveSSHAdvertiseAddress(cfg.SSHListen, cfg.SSHAdvertiseAddress)
	if cfg.DataDir == "" {
		return nil, fmt.Errorf("DISCOBOX_DATA_DIR is required")
	}
	if cfg.ConfigDir == "" {
		return nil, fmt.Errorf("DISCOBOX_CONFIG_DIR is required")
	}
	if cfg.CacheDir == "" {
		return nil, fmt.Errorf("DISCOBOX_CACHE_DIR is required")
	}
	if cfg.StateDir == "" {
		return nil, fmt.Errorf("DISCOBOX_STATE_DIR is required")
	}
	if cfg.DatabaseDSN == "" {
		return nil, fmt.Errorf("DATABASE_DSN is required")
	}
	switch cfg.DatabaseDriver {
	case gormdb.DriverSQLite, gormdb.DriverPostgres:
	default:
		return nil, fmt.Errorf("DATABASE_DRIVER must be one of: sqlite, postgres")
	}
	if cfg.DispatcherPollInterval <= 0 {
		return nil, fmt.Errorf("DISPATCHER_POLL_INTERVAL must be greater than 0")
	}
	if cfg.SandboxReconcileJobConcurrency < 1 {
		return nil, fmt.Errorf("SANDBOX_RECONCILE_JOB_CONCURRENCY must be at least 1")
	}
	if cfg.OTelMetricsEnabled && cfg.OTelMetricExportInterval <= 0 {
		return nil, fmt.Errorf("OTEL_METRIC_EXPORT_INTERVAL must be greater than 0")
	}

	return cfg, nil
}

func defaultDatabaseDSN(dataDir string) string {
	return "sqlite3://" + filepath.Join(dataDir, "discobox.db")
}

func listenEndpoints() []string {
	return requireLocalListenEndpoint(splitListenEndpoints(getEnv("DISCOBOX_SERVER_LISTEN", "")))
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
	for _, endpoint := range endpoints {
		parsed, err := localipc.Parse(endpoint)
		if err != nil {
			continue
		}
		if parsed.Scheme == "unix" || parsed.Scheme == "npipe" {
			return endpoints
		}
	}
	return append([]string{localipc.DefaultEndpoint()}, endpoints...)
}

func splitListenEndpoints(value string) []string {
	var endpoints []string
	for _, endpoint := range strings.Split(value, ",") {
		endpoint = strings.TrimSpace(endpoint)
		if endpoint != "" {
			endpoints = append(endpoints, endpoint)
		}
	}
	return endpoints
}

func getEnv(key, defaultValue string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue
	}
	return value
}

func getEnvInt(key string, defaultValue int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return defaultValue
	}
	return parsed
}

func getEnvBool(key string, defaultValue bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", key, err)
	}
	return parsed, nil
}

func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return defaultValue
	}
	return parsed
}

func getEnvMillisecondsDuration(key string, defaultValue time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue
	}
	milliseconds, err := strconv.Atoi(value)
	if err == nil {
		return time.Duration(milliseconds) * time.Millisecond
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return defaultValue
	}
	return parsed
}

func getEnvDriver(key string, defaultValue gormdb.Driver) gormdb.Driver {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if value == "" {
		return defaultValue
	}
	switch value {
	case "sqlite", "sqlite3":
		return gormdb.DriverSQLite
	case "postgres", "postgresql":
		return gormdb.DriverPostgres
	default:
		return gormdb.Driver(value)
	}
}

// resolveSSHAdvertiseAddress returns the host:port SSH clients should dial.
//
// An explicit advertised address always wins: it is the only way to express an
// endpoint that differs from what this process bound, which is the normal case
// behind a load balancer, a port mapping, or a tunnel. Otherwise the address is
// derived from the bind address, whose host is replaced by loopback when it
// names no host or a wildcard — ":3222", "0.0.0.0:3222", and "[::]:3222" are
// bind-side spellings of "every interface", not addresses a client can dial.
func resolveSSHAdvertiseAddress(listen, advertised string) string {
	if listen == "" {
		return ""
	}
	if advertised != "" {
		return advertised
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return ""
	}
	switch strings.TrimSpace(host) {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
