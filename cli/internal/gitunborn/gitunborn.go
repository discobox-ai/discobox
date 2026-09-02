// Package gitunborn holds what the rest of the client needs to work with a
// repository that has no commits — git's *unborn* HEAD, and what `git init`
// leaves behind.
//
// Such a repository breaks the assumption everything else in git tooling
// starts from: that HEAD resolves to a commit. There is no base to snapshot
// against at create (ADR 0083) and no HEAD to cherry-pick onto at apply
// (ADR 0084), and both sides need the same two answers — whether HEAD is
// unborn, and what the working tree holds when there is no HEAD to read it
// against — so they live here rather than in either.
package gitunborn

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/discobox-ai/x/gitutil"
)

// HeadIsUnborn reports whether repoRoot's HEAD names a branch that does not
// exist yet. Such a repository has no commit to resolve a checkout against,
// and nothing in it to clone.
//
// A detached HEAD is never unborn: detaching is done at a commit. So a HEAD
// that names no branch at all is a repository that has commits.
func HeadIsUnborn(ctx context.Context, repoRoot string) bool {
	branch, ok := gitutil.CurrentBranch(ctx, repoRoot)
	if !ok {
		return false
	}
	_, err := gitutil.Output(ctx, repoRoot, nil, nil, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err != nil
}

// WorkspaceTree writes the tree of everything in an unborn repository's
// working tree into repoRoot and returns its ID, with a cleanup to call when
// the caller is done.
//
// gitutil.CurrentWorkspaceTree cannot answer this: it seeds its index from
// HEAD, which is the thing an unborn repository does not have. The index
// starts empty instead and `git add` fills it from the working tree, honoring
// .gitignore and skipping .git the way any add does. It is written into an
// index file of our own, so the repository's real index — which may already
// hold paths staged for a first commit — is left exactly as it was.
//
// An empty working tree gives the empty tree, which is what gitutil.EmptyTree
// returns for the same repository, so the two are directly comparable.
func WorkspaceTree(ctx context.Context, repoRoot string) (string, func(), error) {
	noop := func() {}
	tempDir, err := os.MkdirTemp("", "discobox-git-index-*")
	if err != nil {
		return "", noop, fmt.Errorf("create temporary git index directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tempDir) }
	env := map[string]string{"GIT_INDEX_FILE": filepath.Join(tempDir, "index")}
	if _, err := gitutil.Output(ctx, repoRoot, nil, env, "add", "--all", "--", "."); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("add working tree to temporary git index: %w", err)
	}
	out, err := gitutil.Output(ctx, repoRoot, nil, env, "write-tree")
	if err != nil {
		cleanup()
		return "", noop, fmt.Errorf("write current workspace tree: %w", err)
	}
	return strings.TrimSpace(out), cleanup, nil
}
