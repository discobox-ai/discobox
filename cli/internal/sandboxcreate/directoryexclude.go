package sandboxcreate

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/adrg/xdg"
	"github.com/discobox-ai/x/gitutil"
)

// directoryStatePaths is a conservative exclusion set, not a choice of where
// to store state. It includes XDG's current defaults, the CLI's separate state
// convention, overrides, and old defaults left after an override was set.
func directoryStatePaths() []string {
	home, _ := os.UserHomeDir()
	state := filepath.Join(home, ".local", "state")
	if runtime.GOOS == "windows" {
		state = os.Getenv("LOCALAPPDATA")
	}
	paths := []string{filepath.Join(xdg.DataHome, "discobox"), filepath.Join(xdg.StateHome, "discobox"), filepath.Join(xdg.CacheHome, "discobox"), filepath.Join(state, "discobox")}
	if home != "" {
		paths = append(paths, filepath.Join(home, ".local", "share", "discobox"), filepath.Join(home, ".local", "state", "discobox"))
	}
	for _, name := range []string{"DISCOBOX_DATA_DIR", "DISCOBOX_STATE_DIR", "DISCOBOX_CACHE_DIR", "DISCOBOX_IMAGE_CACHE_DIR"} {
		if dir := strings.TrimSpace(os.Getenv(name)); dir != "" {
			paths = append(paths, dir)
		}
	}
	if dir := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); dir != "" {
		paths = append(paths, filepath.Join(dir, "discobox"))
	}
	var resolved []string
	for _, path := range paths {
		absolute, err := filepath.Abs(path)
		if err != nil {
			continue
		}
		path = absolute
		resolved = append(resolved, filepath.Clean(path))
		if canonical, err := filepath.EvalSymlinks(path); err == nil {
			resolved = append(resolved, canonical)
		}
	}
	return resolved
}

// directoryWorkspaceTree indexes only copyable paths in the disposable
// repository. Exclude pathspecs cannot be undone by a source's .gitignore.
func directoryWorkspaceTree(ctx context.Context, dir, repo string) (gitutil.WorkspaceTree, error) {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return gitutil.WorkspaceTree{}, err
	}
	if canonical, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = canonical
	}
	args := []string{"add", "--all", "--", "."}
	for _, state := range directoryStatePaths() {
		if pathInsideDirectory(state, absolute) {
			args = append(args, ":(exclude)**")
			continue
		}
		rel, err := filepath.Rel(absolute, state)
		if err != nil || !pathInsideDirectory(absolute, state) {
			continue
		}
		args = append(args, ":(top,exclude,literal)"+filepath.ToSlash(rel))
	}
	if _, err := gitutil.Output(ctx, repo, nil, nil, args...); err != nil {
		return gitutil.WorkspaceTree{}, err
	}
	tree, err := gitutil.Output(ctx, repo, nil, nil, "write-tree")
	if err != nil {
		return gitutil.WorkspaceTree{}, err
	}
	base, err := gitutil.ResolveCommit(ctx, repo, "HEAD")
	if err != nil {
		return gitutil.WorkspaceTree{}, err
	}
	empty, err := gitutil.EmptyTree(ctx, repo)
	if err != nil {
		return gitutil.WorkspaceTree{}, err
	}
	tree = strings.TrimSpace(tree)
	return gitutil.WorkspaceTree{BaseCommit: base, BaseTree: empty, Tree: tree, Dirty: tree != empty}, nil
}
