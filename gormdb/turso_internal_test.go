package gormdb

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestParseTursoDSNExtractsRemoteURL(t *testing.T) {
	remoteURL := "https://example.turso.io"
	gotLocal, gotRemote, err := parseTursoDSN("turso://app.db?_busy_timeout=1000&remote_url=" + url.QueryEscape(remoteURL))
	if err != nil {
		t.Fatalf("parse turso DSN: %v", err)
	}
	if gotRemote != remoteURL {
		t.Fatalf("remote URL = %q, want %q", gotRemote, remoteURL)
	}
	if gotLocal != "app.db?_busy_timeout=1000" {
		t.Fatalf("local DSN = %q, want app.db?_busy_timeout=1000", gotLocal)
	}
}

func TestParseTursoDSNWithoutRemoteURLKeepsLocalQuery(t *testing.T) {
	gotLocal, gotRemote, err := parseTursoDSN("turso:/tmp/app.db?_busy_timeout=1000")
	if err != nil {
		t.Fatalf("parse turso DSN: %v", err)
	}
	if gotRemote != "" {
		t.Fatalf("remote URL = %q, want empty", gotRemote)
	}
	if gotLocal != "/tmp/app.db?_busy_timeout=1000" {
		t.Fatalf("local DSN = %q, want /tmp/app.db?_busy_timeout=1000", gotLocal)
	}
}

func TestPrepareSyncedTursoPathCreatesNestedDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "db")
	dsn := filepath.Join(dir, "test.db") + "?_busy_timeout=1000"

	got, err := prepareSyncedTursoPath(dsn)
	if err != nil {
		t.Fatalf("prepare synced turso path: %v", err)
	}
	if got != dsn {
		t.Fatalf("path = %q, want %q", got, dsn)
	}
	if !dirExists(dir) {
		t.Fatalf("expected directory %s to exist", dir)
	}
}

func TestOpenSyncedTursoCreatesNestedDirectoryBeforeOpening(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "synced", "nested")
	_, err := openSyncedTurso(filepath.Join(dir, "test.db"), Config{
		TursoDatabaseURL: "http://127.0.0.1:1",
	})
	if err == nil {
		t.Fatal("expected fake remote to fail synced turso open")
	}
	if !dirExists(dir) {
		t.Fatalf("expected directory %s to exist", dir)
	}
}

func TestPrepareSyncedTursoPathSkipsMemoryDatabase(t *testing.T) {
	got, err := prepareSyncedTursoPath(":memory:")
	if err != nil {
		t.Fatalf("prepare memory synced turso path: %v", err)
	}
	if got != ":memory:" {
		t.Fatalf("path = %q, want :memory:", got)
	}
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
