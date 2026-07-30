// Package gitapply lands a range of fetched commits onto a host repository's
// checked-out branch by cherry-pick, per ADR 0014.
//
// Git has no dry-run for "will this range of commits cherry-pick cleanly" —
// the only way to find out is to attempt the three-way merges. Attempt does
// so for real, but never against the caller's actual checked-out branch: it
// runs the cherry-pick in a disposable linked worktree first, and only
// fast-forwards the real branch onto the result once the whole range has
// landed cleanly. A conflict at any commit aborts and removes the scratch
// worktree, leaving the repository exactly as it was — so a caller never
// records a result for a range that only partially applied.
package gitapply

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/obot-platform/discobox/internal/gitutil"
)

// Result is the outcome of an Attempt.
type Result struct {
	// HostBase is the commit repoRoot's branch was on before the attempt.
	// Always set: on a conflict it is where the branch still is, and on a
	// clean landing it is the "from" half of the range the apply added.
	HostBase string
	// Landed is true when the commits were fast-forwarded into repoRoot.
	Landed bool
	// HostTip is the resulting host-side commit SHA, set when Landed.
	HostTip string
	// ConflictCommit is the sandbox-side commit that failed to cherry-pick,
	// set when !Landed.
	ConflictCommit string
}

// MergeBase returns the common ancestor of ref and repoRoot's current HEAD.
func MergeBase(ctx context.Context, repoRoot, ref string) (string, error) {
	out, err := gitutil.Output(ctx, repoRoot, nil, nil, "merge-base", ref, "HEAD")
	if err != nil {
		return "", fmt.Errorf("compute merge base: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// Attempt cherry-picks the commits in (base, tipRef] onto repoRoot's current
// HEAD. base and tipRef must both already be reachable in repoRoot (e.g. via
// a prior fetch). Callers should not call Attempt when base and the commit
// tipRef resolves to are the same — there is nothing to cherry-pick, and an
// empty range fails git cherry-pick outright.
func Attempt(ctx context.Context, repoRoot, base, tipRef string) (Result, error) {
	head, err := gitutil.Output(ctx, repoRoot, nil, nil, "rev-parse", "HEAD")
	if err != nil {
		return Result{}, fmt.Errorf("resolve host HEAD: %w", err)
	}
	head = strings.TrimSpace(head)

	scratch, err := os.MkdirTemp("", "disco-apply-*")
	if err != nil {
		return Result{}, fmt.Errorf("create scratch worktree directory: %w", err)
	}
	removed := false
	cleanup := func() {
		if removed {
			return
		}
		removed = true
		_, _ = gitutil.Output(ctx, repoRoot, nil, nil, "worktree", "remove", "--force", scratch)
		_ = os.RemoveAll(scratch)
	}
	defer cleanup()

	if _, err := gitutil.Output(ctx, repoRoot, nil, nil, "worktree", "add", "--detach", scratch, head); err != nil {
		return Result{}, fmt.Errorf("create scratch worktree: %w", err)
	}

	rangeSpec := base + ".." + tipRef
	if _, err := gitutil.Output(ctx, scratch, nil, nil, "cherry-pick", rangeSpec); err != nil {
		conflict, _ := gitutil.Output(ctx, scratch, nil, nil, "rev-parse", "CHERRY_PICK_HEAD")
		_, _ = gitutil.Output(ctx, scratch, nil, nil, "cherry-pick", "--abort")
		return Result{HostBase: head, ConflictCommit: strings.TrimSpace(conflict)}, nil
	}

	scratchTip, err := gitutil.Output(ctx, scratch, nil, nil, "rev-parse", "HEAD")
	if err != nil {
		return Result{}, fmt.Errorf("resolve scratch worktree tip: %w", err)
	}
	scratchTip = strings.TrimSpace(scratchTip)
	cleanup()

	if _, err := gitutil.Output(ctx, repoRoot, nil, nil, "merge", "--ff-only", scratchTip); err != nil {
		return Result{}, fmt.Errorf("fast-forward host branch onto the applied commits: %w", err)
	}

	return Result{HostBase: head, Landed: true, HostTip: scratchTip}, nil
}
