package config

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/obot-platform/disco2/gormdb"
)

func TestLoadDefaults(t *testing.T) {
	clearConfigEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Port != 8080 {
		t.Fatalf("Port = %d, want 8080", cfg.Port)
	}
	if cfg.DatabaseDSN == "" {
		t.Fatalf("DatabaseDSN is empty")
	}
	if cfg.DataDir == "" || cfg.ConfigDir == "" || cfg.CacheDir == "" || cfg.StateDir == "" {
		t.Fatalf("expected all XDG directories to be set")
	}
	if cfg.DatabaseDSN != "sqlite3://"+filepath.Join(cfg.DataDir, "disco2.db") {
		t.Fatalf("DatabaseDSN = %q, want sqlite database under DataDir %q", cfg.DatabaseDSN, cfg.DataDir)
	}
	if cfg.DatabaseDriver != gormdb.DriverSQLite {
		t.Fatalf("DatabaseDriver = %q, want %q", cfg.DatabaseDriver, gormdb.DriverSQLite)
	}
	if cfg.JobMaxAttempts != 3 {
		t.Fatalf("JobMaxAttempts = %d, want 3", cfg.JobMaxAttempts)
	}
	if !cfg.DispatcherEnabled {
		t.Fatalf("DispatcherEnabled = false, want true")
	}
	if cfg.DispatcherPollInterval != time.Second {
		t.Fatalf("DispatcherPollInterval = %s, want 1s", cfg.DispatcherPollInterval)
	}
	if cfg.SandboxReconcileJobConcurrency != 4 {
		t.Fatalf("SandboxReconcileJobConcurrency = %d, want 4", cfg.SandboxReconcileJobConcurrency)
	}
}

func TestLoadEnvironmentOverrides(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("PORT", "9090")
	t.Setenv("DISCO2_DATA_DIR", "/tmp/disco2/data")
	t.Setenv("DISCO2_CONFIG_DIR", "/tmp/disco2/config")
	t.Setenv("DISCO2_CACHE_DIR", "/tmp/disco2/cache")
	t.Setenv("DISCO2_STATE_DIR", "/tmp/disco2/state")
	t.Setenv("DATABASE_DSN", "postgres://user:pass@localhost/disco2")
	t.Setenv("DATABASE_READ_DSN", "postgres://user:pass@localhost/disco2_read")
	t.Setenv("DATABASE_DRIVER", "postgres")
	t.Setenv("JOB_MAX_ATTEMPTS", "7")
	t.Setenv("DISPATCHER_ENABLED", "false")
	t.Setenv("DISPATCHER_POLL_INTERVAL", "250ms")
	t.Setenv("DISPATCHER_JOB_TIMEOUT", "2m")
	t.Setenv("DISPATCHER_STALE_JOB_TIMEOUT", "10m")
	t.Setenv("DISPATCHER_IMMEDIATE_EXECUTION", "false")
	t.Setenv("DISPATCHER_DEFAULT_CONCURRENCY", "3")
	t.Setenv("SANDBOX_RECONCILE_JOB_CONCURRENCY", "9")
	t.Setenv("DISCO2_ENCRYPTION_KEY", "key")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Port != 9090 {
		t.Fatalf("Port = %d, want 9090", cfg.Port)
	}
	if cfg.DataDir != "/tmp/disco2/data" {
		t.Fatalf("DataDir = %q", cfg.DataDir)
	}
	if cfg.ConfigDir != "/tmp/disco2/config" {
		t.Fatalf("ConfigDir = %q", cfg.ConfigDir)
	}
	if cfg.CacheDir != "/tmp/disco2/cache" {
		t.Fatalf("CacheDir = %q", cfg.CacheDir)
	}
	if cfg.StateDir != "/tmp/disco2/state" {
		t.Fatalf("StateDir = %q", cfg.StateDir)
	}
	if cfg.DatabaseDSN != "postgres://user:pass@localhost/disco2" {
		t.Fatalf("DatabaseDSN = %q", cfg.DatabaseDSN)
	}
	if cfg.DatabaseDriver != gormdb.DriverPostgres {
		t.Fatalf("DatabaseDriver = %q, want %q", cfg.DatabaseDriver, gormdb.DriverPostgres)
	}
	if cfg.DatabaseReadDSN != "postgres://user:pass@localhost/disco2_read" {
		t.Fatalf("DatabaseReadDSN = %q", cfg.DatabaseReadDSN)
	}
	if cfg.JobMaxAttempts != 7 {
		t.Fatalf("JobMaxAttempts = %d, want 7", cfg.JobMaxAttempts)
	}
	if cfg.DispatcherEnabled {
		t.Fatalf("DispatcherEnabled = true, want false")
	}
	if cfg.DispatcherPollInterval != 250*time.Millisecond {
		t.Fatalf("DispatcherPollInterval = %s, want 250ms", cfg.DispatcherPollInterval)
	}
	if cfg.DispatcherJobTimeout != 2*time.Minute {
		t.Fatalf("DispatcherJobTimeout = %s, want 2m", cfg.DispatcherJobTimeout)
	}
	if cfg.DispatcherStaleJobTimeout != 10*time.Minute {
		t.Fatalf("DispatcherStaleJobTimeout = %s, want 10m", cfg.DispatcherStaleJobTimeout)
	}
	if cfg.DispatcherImmediateExecution {
		t.Fatalf("DispatcherImmediateExecution = true, want false")
	}
	if cfg.DispatcherDefaultConcurrency != 3 {
		t.Fatalf("DispatcherDefaultConcurrency = %d, want 3", cfg.DispatcherDefaultConcurrency)
	}
	if cfg.SandboxReconcileJobConcurrency != 9 {
		t.Fatalf("SandboxReconcileJobConcurrency = %d, want 9", cfg.SandboxReconcileJobConcurrency)
	}
	if cfg.EncryptionKey != "key" {
		t.Fatalf("EncryptionKey = %q, want key", cfg.EncryptionKey)
	}
}

func TestLoadUsesDataDirForDefaultDatabaseDSN(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DISCO2_DATA_DIR", "/tmp/disco2-data")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	want := "sqlite3://" + filepath.Join("/tmp/disco2-data", "disco2.db")
	if cfg.DatabaseDSN != want {
		t.Fatalf("DatabaseDSN = %q, want %q", cfg.DatabaseDSN, want)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		key  string
		val  string
	}{
		{name: "port", key: "PORT", val: "0"},
		{name: "driver", key: "DATABASE_DRIVER", val: "mysql"},
		{name: "attempts", key: "JOB_MAX_ATTEMPTS", val: "0"},
		{name: "poll interval", key: "DISPATCHER_POLL_INTERVAL", val: "-1s"},
		{name: "job timeout", key: "DISPATCHER_JOB_TIMEOUT", val: "0s"},
		{name: "stale timeout", key: "DISPATCHER_STALE_JOB_TIMEOUT", val: "0s"},
		{name: "dispatcher concurrency", key: "DISPATCHER_DEFAULT_CONCURRENCY", val: "0"},
		{name: "sandbox concurrency", key: "SANDBOX_RECONCILE_JOB_CONCURRENCY", val: "0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv(tt.key, tt.val)

			if _, err := Load(); err == nil {
				t.Fatalf("Load() error = nil, want error")
			}
		})
	}
}

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"PORT",
		"DISCO2_DATA_DIR",
		"DISCO2_CONFIG_DIR",
		"DISCO2_CACHE_DIR",
		"DISCO2_STATE_DIR",
		"DATABASE_DSN",
		"DATABASE_READ_DSN",
		"DATABASE_DRIVER",
		"JOB_MAX_ATTEMPTS",
		"DISPATCHER_ENABLED",
		"DISPATCHER_POLL_INTERVAL",
		"DISPATCHER_JOB_TIMEOUT",
		"DISPATCHER_STALE_JOB_TIMEOUT",
		"DISPATCHER_IMMEDIATE_EXECUTION",
		"DISPATCHER_DEFAULT_CONCURRENCY",
		"SANDBOX_RECONCILE_JOB_CONCURRENCY",
		"DISCO2_ENCRYPTION_KEY",
	} {
		t.Setenv(key, "")
	}
}
