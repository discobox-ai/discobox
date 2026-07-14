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

	"github.com/obot-platform/discobox/controlplane"
	"github.com/obot-platform/discobox/gormdb"
	"github.com/obot-platform/discobox/localipc"
	"github.com/obot-platform/discobox/server/internal/sandbox"
)

const appName = "discobox"

// Config holds all configuration for discobox-server.
type Config struct {
	// Server settings.
	Port                int
	Listen              []string
	AutoShutdownTimeout time.Duration

	// XDG-backed application directories.
	DataDir   string
	ConfigDir string
	CacheDir  string
	StateDir  string

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

	// OpenTelemetry metrics settings.
	OTelMetricsEnabled       bool
	OTelMetricExportInterval time.Duration
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	cfg := &Config{}

	cfg.Port = getEnvInt("PORT", controlplane.DefaultPort)
	cfg.Listen = listenEndpoints(cfg.Port)
	cfg.AutoShutdownTimeout = getEnvDuration("DISCOBOX_SERVER_IDLE_TIMEOUT", 0)

	cfg.DataDir = getEnv("DISCOBOX_DATA_DIR", filepath.Join(xdg.DataHome, appName))
	cfg.ConfigDir = getEnv("DISCOBOX_CONFIG_DIR", filepath.Join(xdg.ConfigHome, appName))
	cfg.CacheDir = getEnv("DISCOBOX_CACHE_DIR", filepath.Join(xdg.CacheHome, appName))
	cfg.StateDir = getEnv("DISCOBOX_STATE_DIR", filepath.Join(xdg.StateHome, appName))
	cfg.DatabaseDSN = getEnv("DATABASE_DSN", defaultDatabaseDSN(cfg.DataDir))
	cfg.DatabaseReadDSN = getEnv("DATABASE_READ_DSN", "")
	cfg.DatabaseDriver = getEnvDriver("DATABASE_DRIVER", gormdb.DetectDriver(cfg.DatabaseDSN))

	cfg.EncryptionKey = getEnv("DISCOBOX_ENCRYPTION_KEY", "")

	cfg.DispatcherPollInterval = getEnvDuration("DISPATCHER_POLL_INTERVAL", time.Second)
	cfg.SandboxReconcileJobConcurrency = getEnvInt("SANDBOX_RECONCILE_JOB_CONCURRENCY", 4)
	cfg.DefaultSandboxImage = getEnv("DISCOBOX_DEFAULT_SANDBOX_IMAGE", sandbox.DefaultSandboxImageName)
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

func listenEndpoints(port int) []string {
	endpoints := splitListenEndpoints(getEnv("DISCOBOX_SERVER_LISTEN", ""))
	return requireLocalAndHTTPListenEndpoints(endpoints, port)
}

func requireLocalAndHTTPListenEndpoints(endpoints []string, port int) []string {
	hasLocal := false
	hasHTTP := false
	for _, endpoint := range endpoints {
		parsed, err := localipc.Parse(endpoint)
		if err != nil {
			continue
		}
		switch parsed.Scheme {
		case "unix", "npipe":
			hasLocal = true
		case "http":
			hasHTTP = true
		}
	}
	if !hasLocal {
		endpoints = append([]string{localipc.DefaultEndpoint()}, endpoints...)
	}
	if !hasHTTP {
		endpoints = append(endpoints, controlplane.DefaultListenEndpoint(port))
	}
	return endpoints
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
