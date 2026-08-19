//go:build linux

package boot

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// chownTreeOnOwnFilesystem chowns to ids this process may actually assign, so
// the walk is exercised rather than failing on the first Lchown. Only root may
// give a file away, which is the same constraint selfIdentity works around.
func TestChownTreeOnOwnFilesystemCoversNestedDirectories(t *testing.T) {
	root := t.TempDir()
	// The shape that motivates the recursion: the intermediate directories boot
	// creates as root on the way to a mountpoint, which nothing else chowns.
	nested := filepath.Join(root, ".cargo", "registry")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "go", "pkg", "mod", "marker")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	uid, gid := os.Getuid(), os.Getgid()
	if err := chownTreeOnOwnFilesystem(root, uid, gid); err != nil {
		t.Fatalf("chownTreeOnOwnFilesystem: %v", err)
	}

	for _, p := range []string{root, filepath.Join(root, ".cargo"), nested, file} {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		if int(st.Uid) != uid || int(st.Gid) != gid {
			t.Errorf("%s owned by %d:%d, want %d:%d", p, st.Uid, st.Gid, uid, gid)
		}
	}
}

// A dangling symlink is the reason the walk uses Lchown: dereferencing it would
// fail outright, and dereferencing a live one would chown something outside the
// tree.
func TestChownTreeOnOwnFilesystemDoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("/nonexistent/target", filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := chownTreeOnOwnFilesystem(root, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("chownTreeOnOwnFilesystem: %v", err)
	}
}

// The behavior the whole change is for: a volume mounted under home keeps the
// ownership it was given and is never walked. Needs a real mount, so it needs
// root; the ownership left behind is what proves the subtree was skipped rather
// than merely chowned to the same value.
func TestChownTreeOnOwnFilesystemSkipsMountedVolumes(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("mounting a filesystem requires root")
	}
	root := t.TempDir()
	mountpoint := filepath.Join(root, ".cache")
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mount("tmpfs", mountpoint, "tmpfs", 0, ""); err != nil {
		t.Skipf("mount tmpfs: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Unmount(mountpoint, 0) })

	inside := filepath.Join(mountpoint, "cached")
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	const otherUID, otherGID = 12345, 12345
	if err := os.Lchown(inside, otherUID, otherGID); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "own")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := chownTreeOnOwnFilesystem(root, 0, 0); err != nil {
		t.Fatalf("chownTreeOnOwnFilesystem: %v", err)
	}

	if uid, gid := ownerOf(t, inside); uid != otherUID || gid != otherGID {
		t.Errorf("mounted volume content owned by %d:%d, want it left at %d:%d", uid, gid, otherUID, otherGID)
	}
	if uid, gid := ownerOf(t, outside); uid != 0 || gid != 0 {
		t.Errorf("home's own file owned by %d:%d, want 0:0", uid, gid)
	}
}

func ownerOf(t *testing.T, path string) (int, int) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no stat information for %s", path)
	}
	return int(st.Uid), int(st.Gid)
}
