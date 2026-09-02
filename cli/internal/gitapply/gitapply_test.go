package gitapply

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	if !reflect.DeepEqual(result, Result{}) {
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

// newUnbornHostAndSandbox builds the pair ADR 0083 creates: a local repository
// with no commits whose working tree holds files, and a discobox-side history
// of an empty base commit plus commits on top. Both live in the one repository,
// the way they do after `discobox apply` has fetched the discobox's tip.
//
// It returns the repository root, the empty base commit, the discobox tip, and
// the tree the discobox was created from.
func newUnbornHostAndSandbox(t *testing.T, files map[string]string) (string, string, string, string) {
	t.Helper()
	host := t.TempDir()
	run(t, host, "init", "--initial-branch=main")
	run(t, host, "config", "user.email", "test@example.com")
	run(t, host, "config", "user.name", "test")
	for name, content := range files {
		writeFile(t, host, name, content)
	}

	// The empty base and the snapshot of the working tree, as create writes
	// them: objects and refs in the user's own repository, no branch moved.
	emptyTree := run(t, host, "hash-object", "-t", "tree", "-w", "--stdin")
	base := commitTree(t, host, emptyTree, "", "discobox run empty base")
	run(t, host, "update-ref", "refs/discobox/run/base", base)
	wantTree := emptyTree
	if len(files) > 0 {
		wantTree = workspaceTree(t, host)
		run(t, host, "update-ref", "refs/discobox/run/snap", commitTree(t, host, wantTree, base, "snapshot"))
	}

	// The discobox's own commits, built in a worktree so the host's unborn HEAD
	// and untracked files are left exactly as they are.
	work := filepath.Join(t.TempDir(), "sandbox")
	run(t, host, "worktree", "add", "--detach", work, base)
	for name, content := range files {
		writeFile(t, work, name, content)
	}
	// Always something of the discobox's own in the first commit: a discobox
	// created from an empty repository has nothing else to put in it.
	writeFile(t, work, "FIRST.md", "written in the discobox\n")
	run(t, work, "add", "-A")
	run(t, work, "commit", "-m", "first commit")
	writeFile(t, work, "NEW.md", "made in the discobox\n")
	run(t, work, "add", "-A")
	run(t, work, "commit", "-m", "second commit")
	tip := run(t, work, "rev-parse", "HEAD")
	run(t, host, "worktree", "remove", "--force", work)
	return host, base, tip, wantTree
}

func commitTree(t *testing.T, repoRoot, tree, parent, message string) string {
	t.Helper()
	args := []string{"commit-tree", tree, "-m", message}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	return run(t, repoRoot, args...)
}

func workspaceTree(t *testing.T, repoRoot string) string {
	t.Helper()
	index := filepath.Join(t.TempDir(), "index")
	cmd := exec.CommandContext(context.Background(), "git", "add", "--all", "--", ".")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+index)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.CommandContext(context.Background(), "git", "write-tree")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+index)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git write-tree: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestAttemptRootGivesAnUnbornRepositoryItsHistory(t *testing.T) {
	setGitIdentity(t)
	host, base, tip, wantTree := newUnbornHostAndSandbox(t, map[string]string{"README.md": "hello\n"})

	result, err := AttemptRoot(context.Background(), host, base, tip, wantTree)
	if err != nil {
		t.Fatalf("AttemptRoot: %v", err)
	}
	if !result.Landed || result.HostTip == "" {
		t.Fatalf("result = %+v, want the commits landed", result)
	}
	// The branch HEAD already named is what was born, at the applied commits.
	if head := run(t, host, "rev-parse", "refs/heads/main"); head != result.HostTip {
		t.Fatalf("main = %s, want the applied tip %s", head, result.HostTip)
	}
	// The discobox's empty base is replayed away: the user's own first commit
	// is the root of their history, and nothing they did not write is in it.
	roots := run(t, host, "rev-list", "--max-parents=0", "HEAD")
	if strings.Contains(roots, base) {
		t.Fatalf("history root = %s, want the discobox's empty base %s left out of it", roots, base)
	}
	subjects := run(t, host, "log", "--reverse", "--format=%s")
	if subjects != "first commit\nsecond commit" {
		t.Fatalf("history = %q, want only the discobox's own commits", subjects)
	}
	// The working tree is what the discobox made of it, and it is clean.
	if status := run(t, host, "status", "--porcelain"); status != "" {
		t.Fatalf("status = %q, want a clean tree after the apply", status)
	}
	if _, err := os.Stat(filepath.Join(host, "NEW.md")); err != nil {
		t.Fatalf("the discobox's new file did not land: %v", err)
	}
	// No scratch branch outlives the worktree it was made in.
	if branches := run(t, host, "for-each-ref", "--format=%(refname:short)", "refs/heads"); branches != "main" {
		t.Fatalf("branches = %q, want only main left behind", branches)
	}
}

func TestAttemptRootLandsIntoAnEmptyRepository(t *testing.T) {
	setGitIdentity(t)
	host, base, tip, wantTree := newUnbornHostAndSandbox(t, nil)

	result, err := AttemptRoot(context.Background(), host, base, tip, wantTree)
	if err != nil {
		t.Fatalf("AttemptRoot: %v", err)
	}
	if !result.Landed {
		t.Fatalf("result = %+v, want the commits landed", result)
	}
	if subjects := run(t, host, "log", "--reverse", "--format=%s"); subjects != "first commit\nsecond commit" {
		t.Fatalf("history = %q, want the discobox's commits rooted at the first", subjects)
	}
}

func TestAttemptRootRefusesWhenTheWorkingTreeMovedOn(t *testing.T) {
	setGitIdentity(t)
	host, base, tip, wantTree := newUnbornHostAndSandbox(t, map[string]string{"README.md": "hello\n"})
	// Work done here since the discobox was created, which has no commits to
	// survive in and would be replaced by what the discobox made of the file.
	writeFile(t, host, "README.md", "hello, and something only local\n")

	result, err := AttemptRoot(context.Background(), host, base, tip, wantTree)
	if err != nil {
		t.Fatalf("AttemptRoot: %v", err)
	}
	if result.Landed {
		t.Fatal("AttemptRoot landed over local changes it should have refused")
	}
	if len(result.ChangedPaths) != 1 || !strings.Contains(result.ChangedPaths[0], "README.md") {
		t.Fatalf("changed paths = %v, want the file that moved on named", result.ChangedPaths)
	}
	// Nothing at all happened: no branch, and the local edit is still there.
	if refs := run(t, host, "for-each-ref", "--format=%(refname)", "refs/heads"); refs != "" {
		t.Fatalf("refs/heads = %q, want the repository still without commits", refs)
	}
	content, err := os.ReadFile(filepath.Join(host, "README.md"))
	if err != nil || string(content) != "hello, and something only local\n" {
		t.Fatalf("README.md = %q (%v), want the local change untouched", content, err)
	}
}
