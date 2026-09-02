package vz

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// The console log is read from the pool's state directory, not from a running
// VM: the boot an operator needs to see is usually the one that ended.
func TestPoolLogsReadsTheGuestConsole(t *testing.T) {
	stateDir := t.TempDir()
	driver := &Driver{stateDir: stateDir}
	if err := os.MkdirAll(filepath.Join(stateDir, "pool-1"), 0o700); err != nil {
		t.Fatalf("create pool state dir: %v", err)
	}
	path := filepath.Join(stateDir, "pool-1", consoleLogName)
	if err := os.WriteFile(path, []byte("[    0.0] Linux version 6.6\nEXT4-fs error\n"), 0o600); err != nil {
		t.Fatalf("write console log: %v", err)
	}

	stream, err := driver.PoolLogs(context.Background(), "pool-1", sandbox.PoolLogOptions{Tail: 1})
	if err != nil {
		t.Fatalf("PoolLogs: %v", err)
	}
	defer stream.Close()
	if !strings.Contains(stream.Source, "console") {
		t.Fatalf("source = %q, want the console named", stream.Source)
	}
	got, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "EXT4-fs error\n" {
		t.Fatalf("tail = %q", got)
	}
}

// A pool that has never run on this host has no console log, and saying so is
// more useful than a bare "no such file".
func TestPoolLogsExplainsAMissingConsole(t *testing.T) {
	driver := &Driver{stateDir: t.TempDir()}
	_, err := driver.PoolLogs(context.Background(), "pool-1", sandbox.PoolLogOptions{})
	if err == nil {
		t.Fatal("PoolLogs on a pool with no VM succeeded")
	}
	if !strings.Contains(err.Error(), "has not been started on this host") {
		t.Fatalf("error = %v", err)
	}
}

// Pool IDs reach this as path segments, so the same validation the rest of the
// driver applies has to run before one is joined onto the state directory.
func TestPoolLogsRejectsUnusablePoolIDs(t *testing.T) {
	driver := &Driver{stateDir: t.TempDir()}
	if _, err := driver.PoolLogs(context.Background(), "../escape", sandbox.PoolLogOptions{}); err == nil {
		t.Fatal("PoolLogs accepted a pool ID that escapes the state directory")
	}
}
