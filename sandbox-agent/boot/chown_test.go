//go:build linux

package boot

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The directories a volume nested in home invents on the way to its
// mountpoint -- ~/go and ~/go/pkg above ~/go/pkg/mod -- are the user's, because
// nothing walks home to fix them up later (#234). What it invents above home
// stays root's: home's parent is not the user's to have.
func TestMkdirAllOwnedGivesAwayOnlyWhatItCreatedUnderHome(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("giving a directory to another user requires root")
	}
	base := t.TempDir()
	home := filepath.Join(base, "home", "sandbox")
	mountpoint := filepath.Join(home, "go", "pkg", "mod")

	if err := mkdirAllOwned(mountpoint, home, sandboxUID, sandboxGID); err != nil {
		t.Fatalf("mkdirAllOwned: %v", err)
	}

	for _, p := range []string{home, filepath.Join(home, "go"), filepath.Join(home, "go", "pkg"), mountpoint} {
		if uid, gid := ownerOf(t, p); uid != sandboxUID || gid != sandboxGID {
			t.Errorf("%s owned by %d:%d, want %d:%d", p, uid, gid, sandboxUID, sandboxGID)
		}
	}
	if uid, gid := ownerOf(t, filepath.Join(base, "home")); uid != 0 || gid != 0 {
		t.Errorf("home's parent owned by %d:%d, want it left at 0:0", uid, gid)
	}
}

// The bug this pins (#234): on a Docker pool the home data volume and the cache
// volumes under it are binds from one host filesystem, so they share home's
// device number and a walk bounded by device descends into all of them -- ~1.3M
// inodes of a per-uid cache, 20-35s of every boot. A home that is already there
// is not walked at all: what is mounted under it, and what the user gave to
// somebody else, keep their owners.
//
// The mount is a bind from the same filesystem, not a tmpfs: a tmpfs always
// has its own device, which is exactly the case the old walk handled and the
// pool's never was.
func TestSeedHomeDoesNotWalkAHomeAlreadyThere(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("bind mounts and giving files away require root")
	}
	base := t.TempDir()
	home := filepath.Join(base, "home")
	backing := filepath.Join(base, "cache", ".users", "12345", "home", ".cache")
	mountpoint := filepath.Join(home, ".cache")
	for _, d := range []string{backing, mountpoint} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Mount(backing, mountpoint, "", syscall.MS_BIND, ""); err != nil {
		t.Skipf("bind mount: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Unmount(mountpoint, 0) })
	if deviceOf(t, mountpoint) != deviceOf(t, home) {
		t.Fatal("the bind reports its own device; this test needs the same-filesystem case")
	}

	const otherUID, otherGID = 4321, 8765
	cached := filepath.Join(mountpoint, "go-build", "entry")
	given := filepath.Join(home, "shared-with-someone")
	for _, f := range []string{cached, given} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Lchown(f, otherUID, otherGID); err != nil {
			t.Fatal(err)
		}
	}

	id := identity{uid: sandboxUID, gid: sandboxGID, name: "sandbox", home: home}
	if err := newBooter().seedHome(id); err != nil {
		t.Fatalf("seedHome: %v", err)
	}

	if uid, gid := ownerOf(t, home); uid != sandboxUID || gid != sandboxGID {
		t.Errorf("home owned by %d:%d, want %d:%d", uid, gid, sandboxUID, sandboxGID)
	}
	for _, f := range []string{cached, filepath.Dir(cached), given} {
		if uid, gid := ownerOf(t, f); uid == sandboxUID || gid == sandboxGID {
			t.Errorf("%s owned by %d:%d: seedHome walked a home that was already there", f, uid, gid)
		}
	}
}

// A fresh home is the one thing seedHome fills, as root, so the skeleton it
// copies is what it gives to the user.
func TestSeedHomeGivesTheSkeletonCopyToTheUser(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("giving files to another user requires root")
	}
	entries, err := os.ReadDir("/etc/skel")
	if err != nil || len(entries) == 0 {
		t.Skip("no /etc/skel to copy")
	}
	home := filepath.Join(t.TempDir(), "home")
	id := identity{uid: sandboxUID, gid: sandboxGID, name: "sandbox", home: home}
	if err := newBooter().seedHome(id); err != nil {
		t.Fatalf("seedHome: %v", err)
	}
	copied := 0
	if err := filepath.WalkDir(home, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		copied++
		if uid, gid := ownerOf(t, p); uid != sandboxUID || gid != sandboxGID {
			t.Errorf("%s owned by %d:%d, want %d:%d", p, uid, gid, sandboxUID, sandboxGID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if copied < 2 {
		t.Fatalf("home holds %d entries, want the skeleton copied into it", copied)
	}
}

// A dangling symlink is the reason the walk uses Lchown: dereferencing it would
// fail outright, and dereferencing a live one would chown something outside the
// tree.
func TestChownTreeDoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("/nonexistent/target", filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := chownTree(root, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("chownTree: %v", err)
	}
}

func ownerOf(t *testing.T, path string) (int, int) {
	t.Helper()
	st := statOf(t, path)
	return int(st.Uid), int(st.Gid)
}

func deviceOf(t *testing.T, path string) uint64 {
	t.Helper()
	return statOf(t, path).Dev
}

func statOf(t *testing.T, path string) *syscall.Stat_t {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no stat information for %s", path)
	}
	return st
}
