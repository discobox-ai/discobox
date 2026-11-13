package fsdurable

import (
	"os"
	"path/filepath"
	"testing"
)

// WriteFile replaces what path held with data, whole and private to its
// owner, and leaves nothing of its own beside it.
func TestWriteFileReplacesWholeAndLeavesNothingBeside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte(`{"new":true}`)); err != nil {
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
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode %v, want 0600", mode)
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

// SyncRootDir syncs a directory in a root, and refuses one a link leads out
// of it to.
func TestSyncRootDirStaysInTheRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "in"), 0o755); err != nil {
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
	for _, name := range []string{".", "in"} {
		if err := SyncRootDir(root, name); err != nil {
			t.Errorf("SyncRootDir(%q): %v", name, err)
		}
	}
	if err := SyncRootDir(root, "out"); err == nil {
		t.Error("SyncRootDir followed a link out of its root")
	}
}
