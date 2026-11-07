//go:build !windows

package cli

import "testing"

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
