package fsdurable

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// entries is the names in dir.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range list {
		names = append(names, entry.Name())
	}
	return names
}

// A staged file is invisible at its path until Commit, and is the path's
// content after it, with nothing left beside it.
func TestCommitPutsTheFileInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Create(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Discard() }()
	if _, err := f.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if !IsStaged(filepath.Base(f.Name())) {
		t.Errorf("staged as %s, which IsStaged does not know", f.Name())
	}
	if data, _ := os.ReadFile(path); string(data) != "old" {
		t.Fatalf("path holds %q before Commit", data)
	}
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := f.Discard(); err != nil {
		t.Errorf("Discard after Commit: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "new" {
		t.Fatalf("path holds %q after Commit", data)
	}
	if names := entries(t, dir); len(names) != 1 {
		t.Errorf("directory holds %v, want only the record", names)
	}
}

// Discard leaves the path as it was and removes what was staged.
func TestDiscardLeavesThePath(t *testing.T) {
	dir := t.TempDir()
	f, err := Create(filepath.Join(dir, "record"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := f.Discard(); err != nil {
		t.Fatal(err)
	}
	if names := entries(t, dir); len(names) != 0 {
		t.Errorf("directory holds %v after Discard", names)
	}
}

// CommitNew refuses a path another file holds, leaving that file and
// nothing of its own.
func TestCommitNewRefusesATakenPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record")
	if err := os.WriteFile(path, []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Create(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Discard() }()
	if _, err := f.Write([]byte("mine")); err != nil {
		t.Fatal(err)
	}
	if err := f.CommitNew(); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("CommitNew over a taken path: %v, want ErrExist", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "theirs" {
		t.Errorf("path holds %q", data)
	}
	if names := entries(t, dir); len(names) != 1 {
		t.Errorf("directory holds %v, want only the record", names)
	}
}

// CommitNew puts the file at a free path and leaves no staged name.
func TestCommitNewTakesAFreePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record")
	f, err := Create(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Discard() }()
	if _, err := f.Write([]byte("mine")); err != nil {
		t.Fatal(err)
	}
	if err := f.CommitNew(); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "mine" {
		t.Errorf("path holds %q", data)
	}
	if names := entries(t, dir); len(names) != 1 {
		t.Errorf("directory holds %v, want only the record", names)
	}
}

// A Commit whose rename failed can be called again once the cause is gone.
func TestCommitCanBeRetriedAfterARenameFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record")
	// A non-empty directory at the path refuses the rename.
	if err := os.MkdirAll(filepath.Join(path, "in"), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := Create(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Discard() }()
	if _, err := f.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := f.Commit(); err == nil {
		t.Fatal("Commit over a directory succeeded")
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "new" {
		t.Errorf("path holds %q", data)
	}
}

// CreateIn stages and commits a name in a root, and refuses a directory a
// link leads out of the root to.
func TestCreateInStaysInTheRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "in"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	f, err := CreateIn(root, "in/record", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Discard() }()
	if _, err := f.Write([]byte("rooted")); err != nil {
		t.Fatal(err)
	}
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "in", "record")); string(data) != "rooted" {
		t.Errorf("record holds %q", data)
	}
	if names := entries(t, filepath.Join(dir, "in")); len(names) != 1 {
		t.Errorf("directory holds %v, want only the record", names)
	}
	if runtime.GOOS == "windows" {
		return
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "out")); err != nil {
		t.Fatal(err)
	}
	if f, err := CreateIn(root, "out/record", 0o600); err == nil {
		_ = f.Discard()
		t.Error("CreateIn followed a link out of its root")
	}
}

// SyncTree and SyncDirs walk a tree of files, directories and links,
// following none of the links.
func TestSyncTreeWalksATree(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "b", "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink("/nonexistent", filepath.Join(dir, "a", "dangling")); err != nil {
			t.Fatal(err)
		}
	}
	if err := SyncTree(dir); err != nil {
		t.Errorf("SyncTree: %v", err)
	}
	if err := SyncDirs(dir); err != nil {
		t.Errorf("SyncDirs: %v", err)
	}
}

// Commit in a directory the caller may write but not read puts the file at
// its path and says so, though the directory cannot be opened to flush.
func TestCommitInADirectoryItCannotRead(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("needs a directory this user may write but not read")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "record")
	f, err := Create(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Discard() }()
	if _, err := f.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := f.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "x" {
		t.Errorf("path holds %q", data)
	}
}

// Symlink replaces whatever was at the path, a link included, without
// following it, and leaves nothing of its own beside it.
func TestSymlinkReplacesThePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need a privilege on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "link")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if err := Symlink("new-target", path); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(path); err != nil || got != "new-target" {
		t.Errorf("link is %q, %v", got, err)
	}
	if data, _ := os.ReadFile(outside); string(data) != "keep" {
		t.Errorf("the old link's target was written: %q", data)
	}
	if names := entries(t, dir); len(names) != 1 {
		t.Errorf("directory holds %v, want only the link", names)
	}
}
