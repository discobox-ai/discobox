//go:build windows

package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/discobox-ai/discobox/internal/privatefs"
)

// State goes where Windows keeps state a program derives — %LOCALAPPDATA%, the
// one that does not roam — rather than into an XDG path built by hand out of
// the home directory.
func TestStateDirIsLocalAppData(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("LOCALAPPDATA", `C:\Users\someone\AppData\Local`)
	if got, want := cliStateDir(), `C:\Users\someone\AppData\Local\discobox\cli`; got != want {
		t.Fatalf("state dir = %q, want %q", got, want)
	}
	// Honored here too, because nothing on Windows sets it by accident and a
	// test or a portable install may want it.
	t.Setenv("XDG_STATE_HOME", `D:\state`)
	if got, want := cliStateDir(), `D:\state\discobox\cli`; got != want {
		t.Fatalf("with XDG_STATE_HOME the state dir = %q, want %q", got, want)
	}
}

// The directory a state file lands in is private, and so is the file: a key
// written into a profile that grants a group access inherits that grant, which
// is what ssh refuses.
func TestEnsureStateDirRestrictsWhatItCreates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "ssh")
	if err := ensureStateDir(dir); err != nil {
		t.Fatalf("ensureStateDir: %v", err)
	}
	if err := privatefs.Check(dir); err != nil {
		t.Fatal(err)
	}

	file := filepath.Join(dir, "identity")
	if err := os.WriteFile(file, []byte("key"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Inherited from the directory above, so it is already private — and
	// restricting it again is what repairs a file an older run left behind.
	if err := privatefs.Check(file); err != nil {
		t.Fatal(err)
	}
	if err := privatefs.Restrict(file); err != nil {
		t.Fatalf("privatefs.Restrict: %v", err)
	}
	if err := privatefs.Check(file); err != nil {
		t.Fatal(err)
	}
}
