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
//
// AttemptRoot is the same idea for a repository that has no commits at all,
// which has no branch to fast-forward and no HEAD to pick onto (ADR 0084).
package gitapply

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/discobox-ai/discobox/cli/internal/gitunborn"
	"github.com/discobox-ai/x/gitutil"
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
	// ChangedPaths is how AttemptRoot refuses: the host working tree is no
	// longer what the discobox was created from, and these are the paths that
	// differ. Nothing was attempted, and nothing changed.
	ChangedPaths []string
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

	scratch, err := os.MkdirTemp("", "discobox-apply-*")
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
	if _, cherryErr := gitutil.Output(ctx, scratch, nil, nil, "cherry-pick", rangeSpec); cherryErr != nil {
		conflicted, err := hasUnmergedPaths(ctx, scratch)
		if err != nil {
			_, _ = gitutil.Output(ctx, scratch, nil, nil, "cherry-pick", "--abort")
			return Result{}, fmt.Errorf("check cherry-pick conflict state: %w", err)
		}
		if !conflicted {
			// Cherry-pick failed for a reason other than conflicting content
			// (e.g. no git identity configured to create the commit) — that
			// is not something a caller should report as "commit X did not
			// apply cleanly", so surface it as a real error instead.
			_, _ = gitutil.Output(ctx, scratch, nil, nil, "cherry-pick", "--abort")
			return Result{}, fmt.Errorf("cherry-pick %s: %w", rangeSpec, cherryErr)
		}
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

// AttemptRoot lands the commits in (base, tipRef] into a repository that has no
// commits at all — the discobox this apply comes from was created from a `git
// init` and nothing since (ADR 0083), and this is the apply that gives that
// repository its history (ADR 0084).
//
// It differs from Attempt in the two ways that case demands.
//
// The cherry-pick runs onto an unborn HEAD of its own, so the discobox's empty
// base commit is replayed away rather than inherited: the first sandbox commit
// becomes the repository's root commit, authored by whoever wrote it, and no
// commit the user did not write ends up in their history.
//
// And there is no branch to fast-forward, only untracked files to replace, so
// the landing is guarded by wantTree — the tree the discobox was created from.
// The host working tree must still be exactly that, or this refuses with the
// paths that differ and changes nothing: those files are about to be replaced
// by what the discobox made of them, and anything the user has changed since
// would go with them. wantTree is the workspace snapshot's tree, or the empty
// tree for a discobox created from an empty repository.
func AttemptRoot(ctx context.Context, repoRoot, base, tipRef, wantTree string) (Result, error) {
	branch, ok := gitutil.CurrentBranch(ctx, repoRoot)
	if !ok {
		return Result{}, fmt.Errorf("%s has no commits and no branch checked out, so there is nothing to land on", repoRoot)
	}
	gotTree, cleanupTree, err := gitunborn.WorkspaceTree(ctx, repoRoot)
	if err != nil {
		return Result{}, err
	}
	defer cleanupTree()
	if gotTree != wantTree {
		changed, err := changedPaths(ctx, repoRoot, wantTree, gotTree)
		if err != nil {
			return Result{}, err
		}
		return Result{ChangedPaths: changed}, nil
	}

	scratch, err := os.MkdirTemp("", "discobox-apply-*")
	if err != nil {
		return Result{}, fmt.Errorf("create scratch worktree directory: %w", err)
	}
	// The orphan checkout below names a branch, which outlives the worktree it
	// was made in, so cleanup deletes it too. The scratch directory's own name
	// is unique and says where it came from, which is what a branch left behind
	// by a crash should say as well.
	scratchBranch := filepath.Base(scratch)
	removed := false
	cleanup := func() {
		if removed {
			return
		}
		removed = true
		_, _ = gitutil.Output(ctx, repoRoot, nil, nil, "worktree", "remove", "--force", scratch)
		_, _ = gitutil.Output(ctx, repoRoot, nil, nil, "branch", "-D", scratchBranch)
		_ = os.RemoveAll(scratch)
	}
	defer cleanup()

	// Detached at the empty base first, so the scratch working tree and index
	// start empty; --orphan then keeps them and drops only the history, which
	// is the whole point.
	if _, err := gitutil.Output(ctx, repoRoot, nil, nil, "worktree", "add", "--detach", scratch, base); err != nil {
		return Result{}, fmt.Errorf("create scratch worktree: %w", err)
	}
	if _, err := gitutil.Output(ctx, scratch, nil, nil, "checkout", "--orphan", scratchBranch); err != nil {
		return Result{}, fmt.Errorf("start the scratch worktree with no history: %w", err)
	}

	rangeSpec := base + ".." + tipRef
	if _, cherryErr := gitutil.Output(ctx, scratch, nil, nil, "cherry-pick", rangeSpec); cherryErr != nil {
		conflicted, err := hasUnmergedPaths(ctx, scratch)
		if err != nil {
			_, _ = gitutil.Output(ctx, scratch, nil, nil, "cherry-pick", "--abort")
			return Result{}, fmt.Errorf("check cherry-pick conflict state: %w", err)
		}
		if !conflicted {
			_, _ = gitutil.Output(ctx, scratch, nil, nil, "cherry-pick", "--abort")
			return Result{}, fmt.Errorf("cherry-pick %s: %w", rangeSpec, cherryErr)
		}
		conflict, _ := gitutil.Output(ctx, scratch, nil, nil, "rev-parse", "CHERRY_PICK_HEAD")
		_, _ = gitutil.Output(ctx, scratch, nil, nil, "cherry-pick", "--abort")
		return Result{ConflictCommit: strings.TrimSpace(conflict)}, nil
	}

	scratchTip, err := gitutil.Output(ctx, scratch, nil, nil, "rev-parse", "HEAD")
	if err != nil {
		return Result{}, fmt.Errorf("resolve scratch worktree tip: %w", err)
	}
	scratchTip = strings.TrimSpace(scratchTip)
	cleanup()

	// There is no merge to make here: HEAD already names the branch, and
	// creating it at the applied tip is what being born means. The reset then
	// fills the index and replaces the working tree files with what the
	// discobox made of them — safe because they are still byte for byte what it
	// was given, which is what wantTree checked. It removes nothing the
	// repository does not track, so anything ignored or never carried stays.
	if err := gitutil.UpdateRef(ctx, repoRoot, "refs/heads/"+branch, scratchTip); err != nil {
		return Result{}, fmt.Errorf("create branch %s at the applied commits: %w", branch, err)
	}
	if _, err := gitutil.Output(ctx, repoRoot, nil, nil, "reset", "--hard"); err != nil {
		return Result{}, fmt.Errorf("check out the applied commits in %s: %w", repoRoot, err)
	}

	return Result{Landed: true, HostTip: scratchTip}, nil
}

// changedPaths names what differs between the tree a discobox was created from
// and the one the working tree holds now, so a refusal says which files it is
// protecting rather than only that something moved.
func changedPaths(ctx context.Context, repoRoot, wantTree, gotTree string) ([]string, error) {
	out, err := gitutil.Output(ctx, repoRoot, nil, nil, "diff", "--name-status", wantTree, gotTree)
	if err != nil {
		return nil, fmt.Errorf("compare the working tree with what the discobox was created from: %w", err)
	}
	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			paths = append(paths, line)
		}
	}
	return paths, nil
}

// hasUnmergedPaths reports whether dir's index currently has unresolved
// conflict entries. A real cherry-pick conflict always leaves some; other
// cherry-pick failures (e.g. no git identity configured to create the
// resulting commit) leave CHERRY_PICK_HEAD set too but stage cleanly, so this
// is what actually distinguishes the two.
func hasUnmergedPaths(ctx context.Context, dir string) (bool, error) {
	out, err := gitutil.Output(ctx, dir, nil, nil, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return false, fmt.Errorf("list unmerged paths: %w", err)
	}
	return strings.TrimSpace(out) != "", nil
}
