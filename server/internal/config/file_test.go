package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/adrg/xdg"

	"github.com/discobox-ai/discobox/controlplane"
)

// writeConfigFile writes a configuration file and points Load at it.
func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
	t.Setenv(ConfigFileVar, path)
	return path
}

func TestFileConfiguresTheServer(t *testing.T) {
	clearConfigEnv(t)
	writeConfigFile(t, `
port: 9999
dataDir: /srv/discobox
dispatcherPollInterval: 5s
sandboxReconcileJobConcurrency: 9
iroh:
  relayUrls:
    - https://relay.one.example
    - https://relay.two.example
`)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Port != 9999 {
		t.Fatalf("Port = %d, want 9999", cfg.Port)
	}
	if cfg.DataDir != "/srv/discobox" {
		t.Fatalf("DataDir = %q, want /srv/discobox", cfg.DataDir)
	}
	if cfg.DispatcherPollInterval != 5*time.Second {
		t.Fatalf("DispatcherPollInterval = %s, want 5s", cfg.DispatcherPollInterval)
	}
	if cfg.SandboxReconcileJobConcurrency != 9 {
		t.Fatalf("SandboxReconcileJobConcurrency = %d, want 9", cfg.SandboxReconcileJobConcurrency)
	}
	want := []string{"https://relay.one.example", "https://relay.two.example"}
	if !reflect.DeepEqual(cfg.Iroh.RelayURLs, want) {
		t.Fatalf("Iroh.RelayURLs = %v, want %v", cfg.Iroh.RelayURLs, want)
	}
	// A setting the file did not mention still gets its default.
	if cfg.DatabaseDSN != defaultDatabaseDSN("/srv/discobox") {
		t.Fatalf("DatabaseDSN = %q, want the default derived from dataDir", cfg.DatabaseDSN)
	}
}

// The environment wins, which is what keeps an existing container or systemd
// unit configuring the server exactly as it did before (ADR 0096 §4, configuration file).
func TestEnvironmentOverridesTheFile(t *testing.T) {
	clearConfigEnv(t)
	writeConfigFile(t, "dataDir: /from/file\nport: 1111\n")
	t.Setenv("DISCOBOX_DATA_DIR", "/from/env")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.DataDir != "/from/env" {
		t.Fatalf("DataDir = %q, want the environment to win", cfg.DataDir)
	}
	// The file still supplies what the environment did not.
	if cfg.Port != 1111 {
		t.Fatalf("Port = %d, want 1111 from the file", cfg.Port)
	}
}

// The reason the file earns its place over the environment: a key nothing
// defines is a failure that names it, rather than a silent default
// (ADR 0096 §3, configuration file).
func TestUnknownKeyFailsAndNamesItself(t *testing.T) {
	clearConfigEnv(t)
	path := writeConfigFile(t, "dataDirr: /typo\n")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() accepted a key nothing defines")
	}
	if !strings.Contains(err.Error(), "dataDirr") {
		t.Fatalf("error %q does not name the offending key", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error %q does not name the file", err)
	}
}

// A server that has never seen this file keeps working, which is the whole
// compatibility story.
//
// That is about the *default* path being absent. A path the operator named and
// got wrong is a different thing; see TestNamedConfigFileMustExist.
func TestMissingFileIsNotAnError(t *testing.T) {
	clearConfigEnv(t)
	useTempConfigHome(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v with no file, want the environment alone to configure a server", err)
	}
	if cfg.Port != controlplane.DefaultPort {
		t.Fatalf("Port = %d, want the default", cfg.Port)
	}
}

// A file the operator wrote and this server cannot parse is a failure, not
// something to shrug at: it means their intent is not being applied.
func TestUnparsableFileFails(t *testing.T) {
	clearConfigEnv(t)
	writeConfigFile(t, "port: [this is not a port\n")

	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted a file it could not parse")
	}
}

// A retention set to zero is not the same as a retention nobody set: the first
// purges on sight, the second means the package default stands.
func TestZeroRetentionInTheFileIsRejected(t *testing.T) {
	clearConfigEnv(t)
	writeConfigFile(t, "archiveRetention: 0s\n")

	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted archiveRetention: 0s")
	}

	clearConfigEnv(t)
	writeConfigFile(t, "dataDir: /srv/discobox\n")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ArchiveRetention != 0 {
		t.Fatalf("ArchiveRetention = %s, want 0 when nobody set it", cfg.ArchiveRetention)
	}
}

// The settings the packages that use them would otherwise read themselves are
// ordinary fields, which is what makes "all valid configuration is in the file"
// true (ADR 0096 §5, configuration file).
func TestFileCarriesTheSettingsPackagesUsedToReadThemselves(t *testing.T) {
	clearConfigEnv(t)
	writeConfigFile(t, `
archiveRetention: 15m
imageRetention: 2h
dockerPoolImage: pool:test
wslcCommand: C:\wslc.exe
devImageSync: false
`)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ArchiveRetention != 15*time.Minute {
		t.Fatalf("ArchiveRetention = %s, want 15m", cfg.ArchiveRetention)
	}
	if cfg.ImageRetention != 2*time.Hour {
		t.Fatalf("ImageRetention = %s, want 2h", cfg.ImageRetention)
	}
	if cfg.DockerPoolImage != "pool:test" {
		t.Fatalf("DockerPoolImage = %q, want pool:test", cfg.DockerPoolImage)
	}
	if cfg.WSLCCommand != `C:\wslc.exe` {
		t.Fatalf("WSLCCommand = %q", cfg.WSLCCommand)
	}
}

// The OpenTelemetry variables keep their specified spellings — an exporter
// name and a count of milliseconds — while the file spells both the way every
// other setting here is spelled.
func TestOpenTelemetrySettingsKeepTheirSpecifiedSpelling(t *testing.T) {
	clearConfigEnv(t)
	writeConfigFile(t, "otelMetricsEnabled: true\notelMetricExportInterval: 30s\n")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.OTelMetricsEnabled || cfg.OTelMetricExportInterval != 30*time.Second {
		t.Fatalf("from the file: enabled=%v interval=%s", cfg.OTelMetricsEnabled, cfg.OTelMetricExportInterval)
	}

	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "5000")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.OTelMetricsEnabled || cfg.OTelMetricExportInterval != 5*time.Second {
		t.Fatalf("from the environment: enabled=%v interval=%s", cfg.OTelMetricsEnabled, cfg.OTelMetricExportInterval)
	}
}

// The port default lives in a struct tag, where a reader of the schema can see
// it. This is what stops it drifting from the constant it is meant to be.
func TestPortDefaultTagMatchesTheConstant(t *testing.T) {
	field, ok := reflect.TypeOf(Config{}).FieldByName("Port")
	if !ok {
		t.Fatal("Config has no Port field")
	}
	if got := field.Tag.Get("default"); got != strconv.Itoa(controlplane.DefaultPort) {
		t.Fatalf("port default tag = %q, want %d", got, controlplane.DefaultPort)
	}
}

// A named path that does not exist is a misconfiguration, not a default.
//
// It is the one setting no schema can check — it is what finds the schema — so
// silently starting on defaults there is exactly the failure ADR 0096 §3 (configuration file) is
// written against. A missing file at the *default* path stays fine: that is
// the ordinary case of a server configured entirely by environment.
func TestNamedConfigFileMustExist(t *testing.T) {
	clearConfigEnv(t)
	missing := filepath.Join(t.TempDir(), "typo.yaml")
	t.Setenv(ConfigFileVar, missing)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() accepted a DISCOBOX_CONFIG_FILE naming a file that does not exist")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("error %q does not name the path", err)
	}

	// The default path being absent is still not an error.
	clearConfigEnv(t)
	useTempConfigHome(t)
	if _, err := Load(); err != nil {
		t.Fatalf("Load() with no file at the default path: %v", err)
	}
}

// useTempConfigHome points the default configuration path at a temp directory,
// and is the only safe way to test what happens when there is no file.
//
// t.Setenv("XDG_CONFIG_HOME", …) alone does nothing: adrg/xdg resolves its
// directories once in init() and caches them, so ConfigFilePath keeps naming
// the developer's real ~/.config/discobox/server.yaml — a test asserting
// "there is no file" would quietly read theirs. CI never sees it, because a
// fresh checkout has no such file. Same pattern as internal/hostid's tests.
func useTempConfigHome(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	os.Unsetenv(ConfigFileVar)
	xdg.Reload()
	t.Cleanup(xdg.Reload)
}
