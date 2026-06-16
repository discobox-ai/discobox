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

	"github.com/obot-platform/discobox/gormdb"
)

const appName = "discobox"

// Config holds all configuration for discobox-server.
type Config struct {
	// Server settings.
	Port int

	// XDG-backed application directories.
	DataDir   string
	ConfigDir string
	CacheDir  string
	StateDir  string

	// Tenant settings.
	TenantID string

	// Database settings.
	DatabaseDSN     string
	DatabaseReadDSN string
	DatabaseDriver  gormdb.Driver

	// Job queue settings.
	JobMaxAttempts int

	// Secret encryption settings.
	EncryptionKey string

	// Dispatcher settings.
	DispatcherEnabled              bool
	DispatcherPollInterval         time.Duration
	DispatcherJobTimeout           time.Duration
	DispatcherStaleJobTimeout      time.Duration
	DispatcherImmediateExecution   bool
	DispatcherDefaultConcurrency   int
	SandboxReconcileJobConcurrency int
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	cfg := &Config{}

	cfg.Port = getEnvInt("PORT", 8080)

	cfg.DataDir = getEnv("DISCOBOX_DATA_DIR", filepath.Join(xdg.DataHome, appName))
	cfg.ConfigDir = getEnv("DISCOBOX_CONFIG_DIR", filepath.Join(xdg.ConfigHome, appName))
	cfg.CacheDir = getEnv("DISCOBOX_CACHE_DIR", filepath.Join(xdg.CacheHome, appName))
	cfg.StateDir = getEnv("DISCOBOX_STATE_DIR", filepath.Join(xdg.StateHome, appName))
	cfg.TenantID = getEnv("DISCOBOX_TENANT_ID", "")

	cfg.DatabaseDSN = getEnv("DATABASE_DSN", defaultDatabaseDSN(cfg.DataDir))
	cfg.DatabaseReadDSN = getEnv("DATABASE_READ_DSN", "")
	cfg.DatabaseDriver = getEnvDriver("DATABASE_DRIVER", gormdb.DetectDriver(cfg.DatabaseDSN))

	cfg.JobMaxAttempts = getEnvInt("JOB_MAX_ATTEMPTS", 3)
	cfg.EncryptionKey = getEnv("DISCOBOX_ENCRYPTION_KEY", "")

	cfg.DispatcherEnabled = getEnvBool("DISPATCHER_ENABLED", true)
	cfg.DispatcherPollInterval = getEnvDuration("DISPATCHER_POLL_INTERVAL", time.Second)
	cfg.DispatcherJobTimeout = getEnvDuration("DISPATCHER_JOB_TIMEOUT", time.Minute)
	cfg.DispatcherStaleJobTimeout = getEnvDuration("DISPATCHER_STALE_JOB_TIMEOUT", 5*time.Minute)
	cfg.DispatcherImmediateExecution = getEnvBool("DISPATCHER_IMMEDIATE_EXECUTION", true)
	cfg.DispatcherDefaultConcurrency = getEnvInt("DISPATCHER_DEFAULT_CONCURRENCY", 1)
	cfg.SandboxReconcileJobConcurrency = getEnvInt("SANDBOX_RECONCILE_JOB_CONCURRENCY", 4)

	if cfg.Port <= 0 || cfg.Port > 65535 {
		return nil, fmt.Errorf("PORT must be between 1 and 65535")
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
	if cfg.JobMaxAttempts < 1 {
		return nil, fmt.Errorf("JOB_MAX_ATTEMPTS must be at least 1")
	}
	if cfg.DispatcherPollInterval <= 0 {
		return nil, fmt.Errorf("DISPATCHER_POLL_INTERVAL must be greater than 0")
	}
	if cfg.DispatcherJobTimeout <= 0 {
		return nil, fmt.Errorf("DISPATCHER_JOB_TIMEOUT must be greater than 0")
	}
	if cfg.DispatcherStaleJobTimeout <= 0 {
		return nil, fmt.Errorf("DISPATCHER_STALE_JOB_TIMEOUT must be greater than 0")
	}
	if cfg.DispatcherDefaultConcurrency < 1 {
		return nil, fmt.Errorf("DISPATCHER_DEFAULT_CONCURRENCY must be at least 1")
	}
	if cfg.SandboxReconcileJobConcurrency < 1 {
		return nil, fmt.Errorf("SANDBOX_RECONCILE_JOB_CONCURRENCY must be at least 1")
	}

	return cfg, nil
}

func defaultDatabaseDSN(dataDir string) string {
	return "sqlite3://" + filepath.Join(dataDir, "discobox.db")
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

func getEnvBool(key string, defaultValue bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.ParseBool(value)
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
