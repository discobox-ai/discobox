package layout

import (
	"fmt"
	"io/fs"
	"os"
)

// tree is one directory of the layout with the mode it is kept at.
type tree struct {
	path string
	mode fs.FileMode
}

// poolTrees are the directories a pool's own state hangs from, parents first,
// with the modes they are kept at. The state root and the trees down to each
// sandbox's are traversable, because a sandbox's origin is served from its
// tree as the sandbox's user (ADR 0126 §4); the identity tree alone is
// private, holding the credentials that authenticate a pool (ADR 0063). These
// are the modes a pool VM's storage unit gives the top-level trees
// (vm-image/discobox-mount-storage).
func (r Root) poolTrees(projectID, poolID string) []tree {
	return []tree{
		{r.Dir(), 0o755},
		{r.dataTree(), 0o755},
		{r.cacheTree(), 0o755},
		{r.proxyTree(), 0o755},
		{r.identityTree(), 0o700},
		{r.ProjectData(projectID), 0o755},
		{r.ProjectPools(projectID), 0o755},
		{r.PoolData(projectID, poolID), 0o755},
		{r.PoolSandboxes(projectID, poolID), 0o755},
	}
}

// PreparePoolTrees makes the state root and the trees one pool's state hangs
// from, and puts each at its mode whether it was made here or not.
//
// A pool container's trees are made by its driver, and a pool VM's by its
// storage unit, before the agent starts. A pool machine has neither: whichever
// writer came first would otherwise decide a shared parent's mode, and one
// that made it private (a key's directory, a cache's) would cut the sandbox
// user off from every origin below it. Setting the modes at every start is
// also what corrects a machine provisioned before they were set here.
func (r Root) PreparePoolTrees(projectID, poolID string) error {
	for _, t := range r.poolTrees(projectID, poolID) {
		if err := os.Mkdir(t.path, t.mode); err != nil && !os.IsExist(err) {
			return fmt.Errorf("make state tree %s: %w", t.path, err)
		}
		info, err := os.Lstat(t.path)
		if err != nil {
			return fmt.Errorf("state tree %s: %w", t.path, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("state tree %s is not a directory", t.path)
		}
		// Set rather than trusted: Mkdir is subject to the umask, and a tree
		// that already existed has whatever mode its first writer gave it.
		if info.Mode().Perm() != t.mode {
			if err := os.Chmod(t.path, t.mode); err != nil {
				return fmt.Errorf("set the mode of state tree %s: %w", t.path, err)
			}
		}
	}
	return nil
}
