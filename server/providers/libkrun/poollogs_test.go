package libkrun

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// The console log lives in the pool's runtime directory, where the launcher
// appends it, and is readable whether or not the VM is still running: a guest
// that never brought Docker up has nothing else to say for itself.
func TestPoolLogsReadsTheGuestConsole(t *testing.T) {
	runtimeDir := t.TempDir()
	driver := &Driver{runtimeDir: runtimeDir}
	if err := os.MkdirAll(filepath.Join(runtimeDir, "pool-1"), 0o700); err != nil {
		t.Fatalf("create pool runtime dir: %v", err)
	}
	path := filepath.Join(runtimeDir, "pool-1", consoleLogName)
	if err := os.WriteFile(path, []byte("[    0.0] Linux version 6.6\nfailed to start dockerd\n"), 0o600); err != nil {
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
	if string(got) != "failed to start dockerd\n" {
		t.Fatalf("tail = %q", got)
	}
}

// A pool with no VM on this host gets an explanation rather than a bare "no
// such file".
func TestPoolLogsExplainsAMissingConsole(t *testing.T) {
	driver := &Driver{runtimeDir: t.TempDir()}
	_, err := driver.PoolLogs(context.Background(), "pool-1", sandbox.PoolLogOptions{})
	if err == nil {
		t.Fatal("PoolLogs on a pool with no VM succeeded")
	}
	if !strings.Contains(err.Error(), "has not been started on this host") {
		t.Fatalf("error = %v", err)
	}
}

// Pool IDs become path segments here, so they are validated before being joined
// onto the runtime directory.
func TestPoolLogsRejectsUnusablePoolIDs(t *testing.T) {
	driver := &Driver{runtimeDir: t.TempDir()}
	if _, err := driver.PoolLogs(context.Background(), "../escape", sandbox.PoolLogOptions{}); err == nil {
		t.Fatal("PoolLogs accepted a pool ID that escapes the runtime directory")
	}
}
