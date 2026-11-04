//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestUninstallIncludesStableSocketAndShellRuntime(t *testing.T) {
	uninstallHome(t)
	t.Setenv("XDG_RUNTIME_DIR", "")
	locations := uninstallLocations()
	want := map[string]bool{
		filepath.Join("/tmp", "discobox-"+strconv.Itoa(os.Getuid()), "discobox"): false,
		discoboxRuntimeDir(): false,
	}
	for _, location := range locations {
		if _, ok := want[location.path]; ok {
			want[location.path] = true
		}
	}
	for path, found := range want {
		if !found {
			t.Errorf("uninstall omitted %q", path)
		}
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
