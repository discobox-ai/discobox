package genfile_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/genfile"
)

func TestWriteFileLeavesIdenticalFileUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.go")
	if err := os.WriteFile(path, []byte("same"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := genfile.WriteFile(path, []byte("same"), 0o644); err != nil {
		t.Fatal(err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("mtime changed: %v -> %v", before.ModTime(), after.ModTime())
	}
	if !os.SameFile(before, after) {
		t.Fatal("an identical write replaced the file")
	}
}

func TestWriteFileReplacesChangedFileKeepingMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no executable mode bit to keep")
	}
	path := filepath.Join(t.TempDir(), "install.sh")
	if err := os.WriteFile(path, []byte("old"), 0o755); err != nil { //nolint:gosec // G306: the mode under test.
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := genfile.WriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("content = %q, want new", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want the existing 0755", info.Mode().Perm())
	}
	assertNoStaged(t, filepath.Dir(path))
}

func TestWriteFileCreatesNewFileAtPerm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.json")

	if err := genfile.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "{}" {
		t.Fatalf("content = %q", got)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o644 {
			t.Fatalf("mode = %v, want 0644", info.Mode().Perm())
		}
	}
}

func TestSyncDir(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write := func(dir, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(src, "oas_same_gen.go", "same")
	write(src, "oas_changed_gen.go", "new")
	write(src, "oas_added_gen.go", "added")
	write(dst, "oas_same_gen.go", "same")
	write(dst, "oas_changed_gen.go", "old")
	write(dst, "oas_stale_gen.go", "stale")
	write(dst, "iter.go", "hand-written")
	write(dst, "ogen.yml", "config")
	same, err := os.Stat(filepath.Join(dst, "oas_same_gen.go"))
	if err != nil {
		t.Fatal(err)
	}

	owns := func(name string) bool { return strings.HasPrefix(name, "oas_") }
	if err := genfile.SyncDir(src, dst, owns); err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"oas_same_gen.go":    "same",
		"oas_changed_gen.go": "new",
		"oas_added_gen.go":   "added",
		"iter.go":            "hand-written",
		"ogen.yml":           "config",
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(want) {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("dst holds %v, want %d files", names, len(want))
	}
	for name, content := range want {
		got, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(got) != content {
			t.Fatalf("%s = %q, want %q", name, got, content)
		}
	}
	after, err := os.Stat(filepath.Join(dst, "oas_same_gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(same, after) || !after.ModTime().Equal(same.ModTime()) {
		t.Fatal("an unchanged generated file was replaced")
	}
}

func TestSyncDirCreatesMissingTarget(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "gen")
	if err := os.WriteFile(filepath.Join(src, "oas_a_gen.go"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := genfile.SyncDir(src, dst, func(string) bool { return true }); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "oas_a_gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "a" {
		t.Fatalf("content = %q", got)
	}
}

func assertNoStaged(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("staged file left behind: %s", e.Name())
		}
	}
}
