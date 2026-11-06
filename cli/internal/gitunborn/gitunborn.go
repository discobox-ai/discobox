// Package gitunborn holds what the rest of the client needs to work with a
// repository that has no commits — git's *unborn* HEAD, and what `git init`
// leaves behind.
//
// Such a repository breaks the assumption everything else in git tooling
// starts from: that HEAD resolves to a commit. There is no base to snapshot
// against at create (ADR 0083) and no HEAD to cherry-pick onto at apply
// (ADR 0084). Both sides need the same answer to whether HEAD is unborn.
package gitunborn

import (
	"context"

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
