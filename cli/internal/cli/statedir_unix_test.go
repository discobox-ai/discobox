//go:build !windows

package cli

import (
	"os"
	"testing"
)

// assertPrivateToUser is the mode bits: 0600 for a file, 0700 for a directory.
func assertPrivateToUser(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	want := os.FileMode(0o600)
	if info.IsDir() {
		want = 0o700
	}
	if perm := info.Mode().Perm(); perm != want {
		t.Fatalf("%s permissions = %o, want %o (ssh refuses anything another user can reach)", path, perm, want)
	}
}

// The XDG default, and the override that names it elsewhere.
func TestStateDirIsTheXDGDefault(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/someone")
	if got, want := cliStateDir(), "/home/someone/.local/state/discobox/cli"; got != want {
		t.Fatalf("state dir = %q, want %q", got, want)
	}
	t.Setenv("XDG_STATE_HOME", "/var/state")
	if got, want := cliStateDir(), "/var/state/discobox/cli"; got != want {
		t.Fatalf("with XDG_STATE_HOME the state dir = %q, want %q", got, want)
	}
}
