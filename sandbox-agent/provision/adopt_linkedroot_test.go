package provision

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// linkedRoot is a box root reached through a link, as every temporary
// directory on macOS is (/var is /private/var): the copies under it resolve
// to paths that do not start with the root as it was named.
func linkedRoot(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the runner provisions a POSIX box")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink("real", link); err != nil {
		t.Fatal(err)
	}
	return link
}

func writeExecutable(t *testing.T, path, script string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// A usable copy is the box's own however its root is reached.
func TestASystemCommandUnderALinkedRootIsInTheBox(t *testing.T) {
	root := linkedRoot(t)
	writeExecutable(t, filepath.Join(root, "usr/bin/node"), "#!/bin/sh\necho v24.9.0\n")
	r := &Runner{Root: root}
	got, err := r.systemCommand("node")
	if err != nil {
		t.Fatalf("systemCommand: %v", err)
	}
	if want := filepath.Join(root, "usr/bin/node"); got != want {
		t.Fatalf("systemCommand = %q, want %q", got, want)
	}
}

// A placeholder is never the box's copy, however the root is reached.
func TestAPlaceholderUnderALinkedRootIsNotTheBoxsCopy(t *testing.T) {
	root := linkedRoot(t)
	placeholder := filepath.Join(root, filepath.FromSlash(PlaceholderDir), "node")
	writeExecutable(t, placeholder, "#!/bin/sh\necho v24.9.0\n")
	if err := os.MkdirAll(filepath.Join(root, "usr/local/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(placeholder, filepath.Join(root, "usr/local/bin/node")); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Root: root, Placeholders: PlaceholderDir}
	if got, err := r.systemCommand("node"); err == nil {
		t.Fatalf("systemCommand = %q, want the placeholder passed over", got)
	}
}
