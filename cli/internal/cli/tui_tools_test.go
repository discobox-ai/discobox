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
// real rather than reasoned about. A wrong workspace encoding is silent: the
// file lands somewhere nothing reads, and the tool comes up unconfigured with
// no error anywhere.
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

// {workspace} is fresh's own `encode_path_for_filename`, and the cases here are
// the ones that actually differ between a plausible guess and the real rule.
func TestInstallScriptEncodesTheWorkspacePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the script runs inside the discobox, which is never Windows")
	}
	for _, tc := range []struct{ dir, slug string }{
		// Observed in a real discobox.
		{"home/darren/src/disco2", "home_darren_src_disco2"},
		// Dots and dashes pass through untouched.
		{"tmp/probe-dot", "tmp_probe-dot"},
		{"workspace/repo.git", "workspace_repo.git"},
		// An underscore is *not* a separator, so it cannot stay one.
		{"home/u/my_repo", "home_u_my%5Frepo"},
		// Anything else is percent-encoded, per byte.
		{"home/u/my project", "home_u_my%20project"},
	} {
		t.Run(tc.slug, func(t *testing.T) {
			home := t.TempDir()
			work := filepath.Join(t.TempDir(), tc.dir)
			if err := os.MkdirAll(work, 0o700); err != nil {
				t.Fatal(err)
			}
			// The encoding runs on the absolute working directory, so assert on
			// the tail it produces rather than the whole of a temp path.
			if err := runInstallScript(t, home, work, ".data/{workspace}/trust.json", "{}"); err != nil {
				t.Fatalf("write: %v", err)
			}
			entries, err := os.ReadDir(filepath.Join(home, ".data"))
			if err != nil || len(entries) != 1 {
				t.Fatalf("read .data: %v entries=%v", err, entries)
			}
			if got := entries[0].Name(); !hasSuffixSlug(got, tc.slug) {
				t.Fatalf("encoded as %q, want it to end in %q", got, tc.slug)
			}
		})
	}
}

// hasSuffixSlug reports whether the encoded name ends with want at a separator
// boundary — the prefix is the temp directory the test happens to get.
func hasSuffixSlug(got, want string) bool {
	if len(got) < len(want) {
		return false
	}
	if got[len(got)-len(want):] != want {
		return false
	}
	return len(got) == len(want) || got[len(got)-len(want)-1] == '_'
}
