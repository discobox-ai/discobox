//go:build !windows

package layout

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// The trees a pool's state hangs from are made at their modes, and ones a
// writer made first at another mode — a key's private directory making the
// state root, a pool machine provisioned before this — are put back at theirs.
func TestPreparePoolTreesSetsEveryTreesMode(t *testing.T) {
	root := ContainerAt(t.TempDir())
	// What an earlier writer left: the state root and a pool's tree private,
	// and the identity tree open.
	for _, dir := range []string{root.PoolData("proj_a", "pool_a"), root.identityTree()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(root.identityTree(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := root.PreparePoolTrees("proj_a", "pool_a"); err != nil {
		t.Fatal(err)
	}
	want := map[string]fs.FileMode{
		root.Dir():                             0o755,
		root.dataTree():                        0o755,
		root.cacheTree():                       0o755,
		root.proxyTree():                       0o755,
		root.identityTree():                    0o700,
		root.ProjectData("proj_a"):             0o755,
		root.ProjectPools("proj_a"):            0o755,
		root.PoolData("proj_a", "pool_a"):      0o755,
		root.PoolSandboxes("proj_a", "pool_a"): 0o755,
	}
	for dir, mode := range want {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Errorf("%s mode = %o, want %o", dir, info.Mode().Perm(), mode)
		}
	}
	// Prepared again, nothing changes and nothing fails.
	if err := root.PreparePoolTrees("proj_a", "pool_a"); err != nil {
		t.Fatal(err)
	}
}

// A tree that is not a directory is refused, not followed or replaced.
func TestPreparePoolTreesRefusesALink(t *testing.T) {
	root := ContainerAt(t.TempDir())
	if err := os.MkdirAll(root.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root.Dir(), "identity")); err != nil {
		t.Fatal(err)
	}
	if err := root.PreparePoolTrees("proj_a", "pool_a"); err == nil {
		t.Fatal("a linked identity tree was taken")
	}
}
