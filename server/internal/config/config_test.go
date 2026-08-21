package config

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/controlplane"
	"github.com/discobox-ai/discobox/devimage"
	"github.com/discobox-ai/discobox/endpoint"
	"github.com/discobox-ai/discobox/gormdb"
)

const (
	//nolint:gosec // Test fixture DSN verifies config parsing; it is not a real credential.
	testDatabaseDSN = "postgres://user:pass@localhost/discobox"
	//nolint:gosec // Test fixture DSN verifies config parsing; it is not a real credential.
	testDatabaseReadDSN = "postgres://user:pass@localhost/discobox_read"
)

// defaultListen is what an unconfigured server listens on: local IPC and
// nothing else. HTTP is opt-in on every platform.
func defaultListen() []string {
	return []string{endpoint.DefaultEndpoint()}
}

func TestLoadDefaults(t *testing.T) {
	clearConfigEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Port != controlplane.DefaultPort {
		t.Fatalf("Port = %d, want %d", cfg.Port, controlplane.DefaultPort)
	}
	if !reflect.DeepEqual(cfg.Listen, defaultListen()) {
		t.Fatalf("Listen = %#v, want %#v", cfg.Listen, defaultListen())
	}
	if cfg.AutoShutdownTimeout != 0 {
		t.Fatalf("AutoShutdownTimeout = %s, want 0", cfg.AutoShutdownTimeout)
	}
	if cfg.DatabaseDSN == "" {
		t.Fatalf("DatabaseDSN is empty")
	}
	if cfg.DataDir == "" || cfg.ConfigDir == "" || cfg.CacheDir == "" || cfg.StateDir == "" {
		t.Fatalf("expected all XDG directories to be set")
	}
	if cfg.DatabaseDSN != "sqlite3://"+filepath.Join(cfg.DataDir, "discobox.db") {
		t.Fatalf("DatabaseDSN = %q, want sqlite database under DataDir %q", cfg.DatabaseDSN, cfg.DataDir)
	}
	if cfg.DatabaseDriver != gormdb.DriverSQLite {
		t.Fatalf("DatabaseDriver = %q, want %q", cfg.DatabaseDriver, gormdb.DriverSQLite)
	}
	if cfg.DispatcherPollInterval != time.Second {
		t.Fatalf("DispatcherPollInterval = %s, want 1s", cfg.DispatcherPollInterval)
	}
	if cfg.SandboxReconcileJobConcurrency != 4 {
		t.Fatalf("SandboxReconcileJobConcurrency = %d, want 4", cfg.SandboxReconcileJobConcurrency)
	}
	if cfg.DefaultSandboxImage != "discobox-sandbox-agent:local" {
		t.Fatalf("DefaultSandboxImage = %q, want local sandbox image", cfg.DefaultSandboxImage)
	}
	if cfg.OTelMetricsEnabled {
		t.Fatalf("OTelMetricsEnabled = true, want false")
	}
	if cfg.OTelMetricExportInterval != time.Second {
		t.Fatalf("OTelMetricExportInterval = %s, want 1s", cfg.OTelMetricExportInterval)
	}
}

func TestLoadEnvironmentOverrides(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("PORT", "9090")
	t.Setenv("DISCOBOX_DATA_DIR", "/tmp/discobox/data")
	t.Setenv("DISCOBOX_SERVER_IDLE_TIMEOUT", "5m")
	t.Setenv("DISCOBOX_CONFIG_DIR", "/tmp/discobox/config")
	t.Setenv("DISCOBOX_CACHE_DIR", "/tmp/discobox/cache")
	t.Setenv("DISCOBOX_STATE_DIR", "/tmp/discobox/state")
	t.Setenv("DATABASE_DSN", testDatabaseDSN)
	t.Setenv("DATABASE_READ_DSN", testDatabaseReadDSN)
	t.Setenv("DATABASE_DRIVER", "postgres")
	t.Setenv("DISPATCHER_POLL_INTERVAL", "250ms")
	t.Setenv("SANDBOX_RECONCILE_JOB_CONCURRENCY", "9")
	t.Setenv("DISCOBOX_DEFAULT_SANDBOX_IMAGE", "discobox-sandbox-agent:test")
	t.Setenv("DISCOBOX_ENCRYPTION_KEY", "key")
	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "5000")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Port != 9090 {
		t.Fatalf("Port = %d, want 9090", cfg.Port)
	}
	// PORT no longer implies a listener: it is only the port an explicitly
	// configured HTTP endpoint defaults to.
	if !reflect.DeepEqual(cfg.Listen, defaultListen()) {
		t.Fatalf("Listen = %#v, want %#v", cfg.Listen, defaultListen())
	}
	if cfg.AutoShutdownTimeout != 5*time.Minute {
		t.Fatalf("AutoShutdownTimeout = %s, want 5m", cfg.AutoShutdownTimeout)
	}
	if cfg.DataDir != "/tmp/discobox/data" {
		t.Fatalf("DataDir = %q", cfg.DataDir)
	}
	if cfg.ConfigDir != "/tmp/discobox/config" {
		t.Fatalf("ConfigDir = %q", cfg.ConfigDir)
	}
	if cfg.CacheDir != "/tmp/discobox/cache" {
		t.Fatalf("CacheDir = %q", cfg.CacheDir)
	}
	if cfg.StateDir != "/tmp/discobox/state" {
		t.Fatalf("StateDir = %q", cfg.StateDir)
	}
	if cfg.DatabaseDSN != testDatabaseDSN {
		t.Fatalf("DatabaseDSN = %q", cfg.DatabaseDSN)
	}
	if cfg.DatabaseDriver != gormdb.DriverPostgres {
		t.Fatalf("DatabaseDriver = %q, want %q", cfg.DatabaseDriver, gormdb.DriverPostgres)
	}
	if cfg.DatabaseReadDSN != testDatabaseReadDSN {
		t.Fatalf("DatabaseReadDSN = %q", cfg.DatabaseReadDSN)
	}
	if cfg.DispatcherPollInterval != 250*time.Millisecond {
		t.Fatalf("DispatcherPollInterval = %s, want 250ms", cfg.DispatcherPollInterval)
	}
	if cfg.SandboxReconcileJobConcurrency != 9 {
		t.Fatalf("SandboxReconcileJobConcurrency = %d, want 9", cfg.SandboxReconcileJobConcurrency)
	}
	if cfg.DefaultSandboxImage != "discobox-sandbox-agent:test" {
		t.Fatalf("DefaultSandboxImage = %q, want test image", cfg.DefaultSandboxImage)
	}
	if cfg.EncryptionKey != "key" {
		t.Fatalf("EncryptionKey = %q, want key", cfg.EncryptionKey)
	}
	if !cfg.OTelMetricsEnabled {
		t.Fatalf("OTelMetricsEnabled = false, want true")
	}
	if cfg.OTelMetricExportInterval != 5*time.Second {
		t.Fatalf("OTelMetricExportInterval = %s, want 5s", cfg.OTelMetricExportInterval)
	}
}

func TestLoadDevelopmentImageManifest(t *testing.T) {
	clearConfigEnv(t)
	path := filepath.Join(t.TempDir(), "dev-images.json")
	manifest, err := devimage.NewManifest([]devimage.Image{
		{Reference: "discobox-pool-agent:dev-test", ID: "sha256:test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := devimage.WriteAtomic(path, manifest); err != nil {
		t.Fatal(err)
	}
	t.Setenv(devimage.SyncEnv, "true")
	t.Setenv(devimage.ManifestEnv, path)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.DevelopmentImages, manifest.Images) {
		t.Fatalf("DevelopmentImages = %#v, want %#v", cfg.DevelopmentImages, manifest.Images)
	}
}

func TestLoadDevelopmentImageSyncRequiresManifest(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv(devimage.SyncEnv, "true")
	if _, err := Load(); err == nil {
		t.Fatal("Load() succeeded without a development image manifest")
	}
}

func TestLoadDevelopmentImageSyncRejectsInvalidBoolean(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv(devimage.SyncEnv, "sometimes")
	if _, err := Load(); err == nil {
		t.Fatal("Load() succeeded with an invalid development image sync flag")
	}
}

// Naming the endpoints explicitly yields exactly those. An implied TCP listener
// would be one the operator never asked for, which on Windows also costs a
// firewall prompt; configuring HTTP remains possible, it is just not automatic.
func TestLoadServerEndpointDoesNotImplyHTTP(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DISCOBOX_SERVER_LISTEN", "unix:///tmp/discobox/server.sock")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	want := []string{"unix:///tmp/discobox/server.sock"}
	if !reflect.DeepEqual(cfg.Listen, want) {
		t.Fatalf("Listen = %#v, want %#v", cfg.Listen, want)
	}
}

func TestLoadServerEndpointListOverride(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DISCOBOX_SERVER_LISTEN", "unix:///tmp/discobox/server.sock,http://0.0.0.0:18080")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	want := []string{"unix:///tmp/discobox/server.sock", "http://0.0.0.0:18080"}
	if !reflect.DeepEqual(cfg.Listen, want) {
		t.Fatalf("Listen = %#v, want %#v", cfg.Listen, want)
	}
}

func TestLoadServerEndpointAddsDefaultLocalIPC(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DISCOBOX_SERVER_LISTEN", "http://127.0.0.1:19090")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	want := []string{endpoint.DefaultEndpoint(), "http://127.0.0.1:19090"}
	if !reflect.DeepEqual(cfg.Listen, want) {
		t.Fatalf("Listen = %#v, want %#v", cfg.Listen, want)
	}
}

func TestLoadServerClientEndpointDoesNotOverrideListen(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DISCOBOX_SERVER", "unix:///tmp/discobox/server.sock")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// DISCOBOX_SERVER is where the client dials, not what the server binds.
	want := defaultListen()
	if !reflect.DeepEqual(cfg.Listen, want) {
		t.Fatalf("Listen = %#v, want %#v", cfg.Listen, want)
	}
}

func TestLoadUsesDataDirForDefaultDatabaseDSN(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DISCOBOX_DATA_DIR", "/tmp/discobox-data")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	want := "sqlite3://" + filepath.Join("/tmp/discobox-data", "discobox.db")
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
		{name: "idle timeout", key: "DISCOBOX_SERVER_IDLE_TIMEOUT", val: "-1s"},
		{name: "driver", key: "DATABASE_DRIVER", val: "mysql"},
		{name: "poll interval", key: "DISPATCHER_POLL_INTERVAL", val: "-1s"},
		{name: "sandbox concurrency", key: "SANDBOX_RECONCILE_JOB_CONCURRENCY", val: "0"},
		{name: "otel metric export interval", key: "OTEL_METRIC_EXPORT_INTERVAL", val: "0s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
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
		"DISCOBOX_SERVER",
		"DISCOBOX_SERVER_LISTEN",
		"DISCOBOX_SERVER_IDLE_TIMEOUT",
		"DISCOBOX_DATA_DIR",
		"DISCOBOX_CONFIG_DIR",
		"DISCOBOX_CACHE_DIR",
		"DISCOBOX_STATE_DIR",
		"DATABASE_DSN",
		"DATABASE_READ_DSN",
		"DATABASE_DRIVER",
		"DISPATCHER_POLL_INTERVAL",
		"SANDBOX_RECONCILE_JOB_CONCURRENCY",
		"DISCOBOX_DEFAULT_SANDBOX_IMAGE",
		devimage.SyncEnv,
		devimage.ManifestEnv,
		"DISCOBOX_ENCRYPTION_KEY",
		"OTEL_METRICS_EXPORTER",
		"OTEL_METRIC_EXPORT_INTERVAL",
	} {
		t.Setenv(key, "")
	}
}
