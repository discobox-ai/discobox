package sandboxcreate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/discobox-ai/x/gitutil"
)

// recoveryExclusion is shared by measurement and capture. Existing symlinks
// are resolved because a source may name the same directory through an alias.
func recoveryExclusion(dir string) (string, error) {
	root, err := pendingRoot()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	if pathInsideDirectory(root, dir) {
		return ".", nil
	}
	if !pathInsideDirectory(dir, root) {
		return "", nil
	}
	return filepath.Rel(dir, root)
}

// captureWorkspaceTree snapshots work on top of the selected base without
// touching the person's index. Explicit exclusion pathspecs keep a source's
// .gitignore negations from adding another source's retained private data.
func captureWorkspaceTree(ctx context.Context, dir, repo, base string) (gitutil.WorkspaceTree, error) {
	exclude, err := recoveryExclusion(dir)
	if err != nil {
		return gitutil.WorkspaceTree{}, err
	}
	baseTree, err := gitutil.Output(ctx, repo, nil, nil, "rev-parse", base+"^{tree}")
	if err != nil {
		return gitutil.WorkspaceTree{}, err
	}
	temp, err := os.MkdirTemp("", "discobox-source-index-*")
	if err != nil {
		return gitutil.WorkspaceTree{}, err
	}
	defer func() { _ = os.RemoveAll(temp) }()
	env := map[string]string{"GIT_INDEX_FILE": filepath.Join(temp, "index")}
	if _, err = gitutil.Output(ctx, repo, nil, env, "read-tree", base); err != nil {
		return gitutil.WorkspaceTree{}, err
	}
	args := []string{"add", "-A", "--", "."}
	if exclude != "" {
		args = append(args, ":(top,literal,exclude)"+filepath.ToSlash(exclude))
	}
	if exclude != "." {
		if _, err = gitutil.Output(ctx, repo, nil, env, args...); err != nil {
			return gitutil.WorkspaceTree{}, fmt.Errorf("capture directory: %w", err)
		}
	}
	tree, err := gitutil.Output(ctx, repo, nil, env, "write-tree")
	if err != nil {
		return gitutil.WorkspaceTree{}, err
	}
	tree, baseTree = strings.TrimSpace(tree), strings.TrimSpace(baseTree)
	return gitutil.WorkspaceTree{BaseCommit: base, BaseTree: baseTree, Tree: tree, Dirty: tree != baseTree}, nil
}
