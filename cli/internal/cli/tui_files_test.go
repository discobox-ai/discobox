package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A download never replaces a file already there: it is numbered beside it,
// the way a browser names a second download of the same file.
func TestFreeLocalNameNumbersBesideWhatIsThere(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"notes.txt", "notes (2).txt", ".env", "src"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range map[string]string{
		"fresh.go":  "fresh.go",
		"notes.txt": "notes (3).txt",
		".env":      ".env (2)",
		"src":       "src (2)",
	} {
		got, err := freeLocalName(dir, name)
		if err != nil {
			t.Fatal(err)
		}
		if got != filepath.Join(dir, want) {
			t.Errorf("freeLocalName(%q) = %q, want %q", name, filepath.Base(got), want)
		}
	}
}

// A command run for the window answers with what it wrote and how it exited,
// and leaves no session behind in the discobox.
func TestRunCommandReadsTheOutputAndRemovesTheSession(t *testing.T) {
	ds, created, deleted := execServer(t, func(command []string) (string, int64) {
		if strings.Join(command, " ") == "git rev-parse --show-toplevel" {
			return "/src\n", 0
		}
		return "", 128
	})
	got, err := ds.RunCommand(t.Context(), "sbx_1", "/src", []string{"git", "rev-parse", "--show-toplevel"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Stdout != "/src\n" || got.ExitCode != 0 {
		t.Fatalf("RunCommand = %+v, want the top level and exit 0", got)
	}
	if got, err := ds.RunCommand(t.Context(), "sbx_1", "", []string{"false"}); err != nil || got.ExitCode != 128 {
		t.Fatalf("RunCommand(false) = %+v, %v; want its exit code", got, err)
	}
	if n, d := len(created()), deleted(); n != 2 || len(d) != 2 {
		t.Fatalf("%d execs created, deleted %q; want each removed after", n, d)
	}
}
