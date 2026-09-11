package boot

import (
	"os"
	"path/filepath"
	"testing"
)

// An overlay's upperdir decides what the merged root looks like, so it has to
// start out looking like the directory the image shipped. The case that matters
// is a setgid, group-writable tree: that is how an image hands a path to a
// group when it cannot know the uid the sandbox will run as, and a root:root
// 0755 upper would take the whole arrangement away at the top level.
func TestAdoptDirIdentityKeepsModeAndSetgid(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "image")
	dst := filepath.Join(root, "upper")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	// What the Dockerfile leaves behind: group-writable and setgid.
	if err := os.Chmod(src, 0o775|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	// What wireVolume creates for the upperdir.
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := adoptDirIdentity(src, dst); err != nil {
		t.Fatalf("adoptDirIdentity: %v", err)
	}

	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fi.Mode().Perm(), os.FileMode(0o775); got != want {
		t.Errorf("upperdir permissions = %o, want %o", got, want)
	}
	if fi.Mode()&os.ModeSetgid == 0 {
		t.Error("upperdir lost the setgid bit, so the merged root would not hand new entries to the group")
	}
}

// A path the image left at its default has nothing special to carry, and must
// not acquire anything either.
func TestAdoptDirIdentityPlainDirectory(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "image"), filepath.Join(root, "upper")
	for _, d := range []string{src, dst} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := adoptDirIdentity(src, dst); err != nil {
		t.Fatalf("adoptDirIdentity: %v", err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode(); got != os.FileMode(0o755)|os.ModeDir {
		t.Errorf("upperdir mode = %v, want drwxr-xr-x", got)
	}
}

func TestAdoptDirIdentityMissingSource(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "upper")
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := adoptDirIdentity(filepath.Join(root, "absent"), dst); err == nil {
		t.Fatal("adoptDirIdentity on a missing source returned nil")
	}
}
