package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// The install script is the one piece of this that runs somewhere else, in a
// shell, against paths only the sandbox knows — so it is exercised here for
// real rather than reasoned about.
func runInstallScript(t *testing.T, home, workdir, dest, content string) error {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sh", "-c", installToolFileScript, "sh", dest, content)
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(), "HOME="+home, "PWD="+workdir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("script output: %s", out)
	}
	return err
}

func TestInstallScriptWritesOnlyWhenAbsent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the script runs inside the discobox, which is never Windows")
	}
	home := t.TempDir()
	work := t.TempDir()

	if err := runInstallScript(t, home, work, ".config/fresh/config.json", "first\n"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	landed := filepath.Join(home, ".config/fresh/config.json")
	if got, err := os.ReadFile(landed); err != nil || string(got) != "first\n" {
		t.Fatalf("read back %q, %v; want the content written", got, err)
	}

	// A second run must leave it alone and still succeed — the `set -e` trap
	// that an `[ -e ] && exit 0` would spring.
	if err := runInstallScript(t, home, work, ".config/fresh/config.json", "second\n"); err != nil {
		t.Fatalf("second write should succeed and do nothing: %v", err)
	}
	if got, _ := os.ReadFile(landed); string(got) != "first\n" {
		t.Fatalf("content = %q, want the discobox's own copy kept", got)
	}
}

// Content has to survive verbatim: it is JSON with comments, dollars, percents
// and backticks in it, passed as an argv element precisely so no shell gets to
// interpret any of it.
func TestInstallScriptKeepsContentVerbatim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the script runs inside the discobox, which is never Windows")
	}
	home := t.TempDir()
	work := t.TempDir()
	content := "// vim: set ft=jsonc :\n{\n  // a $dollar, `backticks`, 100% literal, \"quotes\", 'single',\n  \"k\": false,\n}\n"

	if err := runInstallScript(t, home, work, "x/y.json", content); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(home, "x/y.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Fatalf("content came through as\n%q\nwant\n%q", got, content)
	}
}
