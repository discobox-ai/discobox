package gitapply

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRepo creates a repo with one commit on main, returning its root and that
// commit's SHA.
func newRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "init", "--initial-branch=main")
	// Configure the identity in the repo itself, not just in the environment
	// this helper passes to its own git calls. Attempt shells out to git on its
	// own, and cherry-pick needs a committer: without this the test passes only
	// on machines that happen to have a global user.name/user.email, and fails
	// with "Committer identity unknown" everywhere else.
	run(t, dir, "config", "user.email", "test@example.com")
	run(t, dir, "config", "user.name", "test")
	writeFile(t, dir, "README.md", "base\n")
	run(t, dir, "add", "README.md")
	run(t, dir, "commit", "-m", "base")
	commit := run(t, dir, "rev-parse", "HEAD")
	return dir, commit
}

// setGitIdentity gives the test process itself a git identity, so the git
// commands Attempt runs internally (which, unlike run(), do not carry their
// own explicit identity env) do not depend on the host running the test
// already having ~/.gitconfig or /etc/gitconfig configured.
func setGitIdentity(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// cloneWithCommits returns a bare-ish second working copy of repoRoot with
// extraCommits new commits on top of base, standing in for a sandbox's
// repository whose tip a caller has already fetched into repoRoot.
func addCommits(t *testing.T, repoRoot string, files ...string) string {
	t.Helper()
	for i, name := range files {
		writeFile(t, repoRoot, name, "sandbox change\n")
		run(t, repoRoot, "add", name)
		run(t, repoRoot, "commit", "-m", "sandbox commit "+string(rune('a'+i)))
	}
	return run(t, repoRoot, "rev-parse", "HEAD")
}

func TestAttemptLandsCleanRangeAndFastForwards(t *testing.T) {
	ctx := context.Background()
	setGitIdentity(t)
	host, base := newRepo(t)

	// Simulate a fetched sandbox tip by branching off base in the same repo
	// (Attempt only needs base and tipRef to already be reachable locally,
	// which is exactly what a real fetch would have arranged).
	run(t, host, "checkout", "-q", "-b", "sandbox-tip", base)
	tip := addCommits(t, host, "a.txt", "b.txt")
	run(t, host, "checkout", "-q", "main")

	result, err := Attempt(ctx, host, base, tip)
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if !result.Landed {
		t.Fatalf("expected a clean range to land, got %+v", result)
	}
	if result.HostTip == "" {
		t.Fatal("landed result has no HostTip")
	}
	// HostBase is what the caller reports the branch moved from, so it has to
	// be the pre-apply tip, not the post-apply one.
	if result.HostBase != base {
		t.Fatalf("HostBase = %s, want the pre-apply host tip %s", result.HostBase, base)
	}
	headNow := run(t, host, "rev-parse", "HEAD")
	if headNow != result.HostTip {
		t.Fatalf("host HEAD = %s, want fast-forwarded to %s", headNow, result.HostTip)
	}
	// Two sandbox commits in, preserved individually rather than squashed.
	count := run(t, host, "rev-list", "--count", base+".."+headNow)
	if count != "2" {
		t.Fatalf("landed commit count = %s, want 2", count)
	}
	// No leftover scratch worktree.
	worktrees := run(t, host, "worktree", "list", "--porcelain")
	if strings.Count(worktrees, "worktree ") != 1 {
		t.Fatalf("expected only the main worktree to remain, got:\n%s", worktrees)
	}
}

func TestAttemptConflictLeavesHostUntouched(t *testing.T) {
	ctx := context.Background()
	setGitIdentity(t)
	host, base := newRepo(t)

	// A host-side change and a conflicting sandbox-side change to the same
	// file, both starting from base.
	writeFile(t, host, "README.md", "host change\n")
	run(t, host, "commit", "-am", "host edits README")
	hostHead := run(t, host, "rev-parse", "HEAD")

	run(t, host, "checkout", "-q", "-b", "sandbox-tip", base)
	writeFile(t, host, "README.md", "sandbox change\n")
	run(t, host, "commit", "-am", "sandbox edits README")
	tip := run(t, host, "rev-parse", "HEAD")
	run(t, host, "checkout", "-q", "main")

	result, err := Attempt(ctx, host, base, tip)
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if result.Landed {
		t.Fatalf("expected a conflicting range to fail, got %+v", result)
	}
	if result.ConflictCommit != tip {
		t.Fatalf("conflictCommit = %s, want %s", result.ConflictCommit, tip)
	}
	// A conflict still reports where the host branch stands, since that is
	// what the caller tells the user is unchanged.
	if result.HostBase != hostHead {
		t.Fatalf("HostBase = %s, want %s", result.HostBase, hostHead)
	}
	headNow := run(t, host, "rev-parse", "HEAD")
	if headNow != hostHead {
		t.Fatalf("host HEAD moved from %s to %s after a conflicting attempt", hostHead, headNow)
	}
	status := run(t, host, "status", "--porcelain")
	if status != "" {
		t.Fatalf("host working tree is dirty after a conflicting attempt: %q", status)
	}
	worktrees := run(t, host, "worktree", "list", "--porcelain")
	if strings.Count(worktrees, "worktree ") != 1 {
		t.Fatalf("expected only the main worktree to remain, got:\n%s", worktrees)
	}
}

// TestAttemptNonConflictFailureReturnsError covers a cherry-pick that fails
// for a reason other than conflicting content — no identity to attribute the
// resulting commit to, in this case. That must surface as an error, not be
// misreported as a conflict on some commit: nothing about it is a content
// conflict a caller could ask the user to resolve.
func TestAttemptNonConflictFailureReturnsError(t *testing.T) {
	ctx := context.Background()
	setGitIdentity(t)
	host, base := newRepo(t)

	run(t, host, "checkout", "-q", "-b", "sandbox-tip", base)
	tip := addCommits(t, host, "a.txt")
	run(t, host, "checkout", "-q", "main")
	hostHead := run(t, host, "rev-parse", "HEAD")

	// Explicit (even empty) identity env vars override any discovered
	// ~/.gitconfig, so this deterministically fails the same way regardless
	// of what git identity the host running the test happens to have.
	t.Setenv("GIT_AUTHOR_NAME", "")
	t.Setenv("GIT_AUTHOR_EMAIL", "")
	t.Setenv("GIT_COMMITTER_NAME", "")
	t.Setenv("GIT_COMMITTER_EMAIL", "")

	result, err := Attempt(ctx, host, base, tip)
	if err == nil {
		t.Fatalf("expected a non-conflict cherry-pick failure to return an error, got result %+v", result)
	}
	if result != (Result{}) {
		t.Fatalf("expected a zero Result alongside the error, got %+v", result)
	}
	headNow := run(t, host, "rev-parse", "HEAD")
	if headNow != hostHead {
		t.Fatalf("host HEAD moved from %s to %s after a failed attempt", hostHead, headNow)
	}
	worktrees := run(t, host, "worktree", "list", "--porcelain")
	if strings.Count(worktrees, "worktree ") != 1 {
		t.Fatalf("expected only the main worktree to remain, got:\n%s", worktrees)
	}
}

func TestMergeBase(t *testing.T) {
	ctx := context.Background()
	host, base := newRepo(t)
	run(t, host, "checkout", "-q", "-b", "sandbox-tip", base)
	tip := addCommits(t, host, "a.txt")
	run(t, host, "checkout", "-q", "main")

	got, err := MergeBase(ctx, host, tip)
	if err != nil {
		t.Fatalf("merge base: %v", err)
	}
	if got != base {
		t.Fatalf("merge base = %s, want %s", got, base)
	}
}
