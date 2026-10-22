package endpoint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The log belongs with the server's state, not with the socket: the socket
// lives in a runtime directory the system clears, and a log that is gone by the
// next login is not a log. DISCOBOX_STATE_DIR is what the server itself reads,
// so the two stay in the same place when it is set.
func TestServerLogPathFollowsTheServerState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DISCOBOX_STATE_DIR", dir)
	if got, want := ServerLogPath(), filepath.Join(dir, "server.log"); got != want {
		t.Fatalf("ServerLogPath() = %q, want %q", got, want)
	}
	t.Setenv("DISCOBOX_STATE_DIR", "")
	if got := ServerLogPath(); !strings.HasSuffix(got, filepath.Join("discobox", "server.log")) {
		t.Fatalf("ServerLogPath() = %q, want a path under the state home", got)
	}
}

// Appending forever fills a disk. One rotation keeps the run before the current
// one and drops everything older.
func TestOpenServerLogRotatesOnceItIsTooBig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	if err := os.WriteFile(path, make([]byte, serverLogRotateBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := openServerLog(path, "/usr/local/bin/discobox", []string{"admin", "server"})
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() >= serverLogRotateBytes {
		t.Fatalf("current log is %d bytes; it was not rotated", info.Size())
	}
	previous, err := os.Stat(PreviousServerLogPath(path))
	if err != nil {
		t.Fatalf("previous log: %v", err)
	}
	if previous.Size() != serverLogRotateBytes {
		t.Fatalf("previous log is %d bytes, want the whole rotated log", previous.Size())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The banner says what was started, so a log holding several runs is read
	// back as several runs.
	if !strings.Contains(string(data), serverLogBanner) || !strings.Contains(string(data), "admin server") {
		t.Fatalf("log %q does not open with a banner naming the launch", data)
	}
}
