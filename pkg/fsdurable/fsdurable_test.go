package fsdurable

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// WriteFile replaces what path held with data, whole and at exactly the mode
// it is given, and leaves nothing of its own beside it.
func TestWriteFileReplacesWholeAndLeavesNothingBeside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte(`{"new":true}`), 0o640); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != `{"new":true}` {
		t.Fatalf("read back %q, %v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); runtime.GOOS != "windows" && mode != 0o640 {
		t.Errorf("mode %v, want 0640", mode)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the file", len(entries))
	}
}

// Remove of a path already gone is done, not an error.
func TestRemoveOfAPathAlreadyGone(t *testing.T) {
	if err := Remove(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatal(err)
	}
}

// SyncIn syncs a file or directory in a root, and refuses one a link leads
// out of it to. Windows has nothing to sync, so nothing to refuse.
func TestSyncInStaysInTheRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SyncIn is a no-op on Windows")
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "in"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "in", "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "out")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, name := range []string{".", "in", "in/file"} {
		if err := SyncIn(root, name); err != nil {
			t.Errorf("SyncIn(%q): %v", name, err)
		}
	}
	if err := SyncIn(root, "out"); err == nil {
		t.Error("SyncIn followed a link out of its root")
	}
}
