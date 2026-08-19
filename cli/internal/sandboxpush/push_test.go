package sandboxpush

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/cli/internal/sandboxgit"
)

const (
	testProject = "proj_1"
	testSandbox = "sbx_1"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out))
}

// clientRepo is a local repository with one commit on branch, as a client that
// created a sandbox from it would have.
func clientRepo(t *testing.T, branch string) (root, commit string) {
	t.Helper()
	root = t.TempDir()
	git(t, root, "init", "-b", branch)
	git(t, root, "config", "user.email", "test@example.com")
	git(t, root, "config", "user.name", "Test")
	return root, commitFile(t, root, "README.md", "one\n", "one")
}

func commitFile(t *testing.T, root, name, content, message string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", name)
	git(t, root, "commit", "-m", message)
	return git(t, root, "rev-parse", "HEAD")
}

// originRepo stands in for the pool-side origin repository. The push runs
// against a local path rather than the proxy, which is the same receive-pack the
// proxy fronts.
func originRepo(t *testing.T, branch string) string {
	t.Helper()
	origin := filepath.Join(t.TempDir(), "primary.git")
	git(t, "", "init", "--bare", "-b", branch, origin)
	git(t, origin, "config", "receive.denyDeletes", "true")
	return origin
}

func pushDeliveredSource(branch, baseCommit string) apimodel.GitSource {
	source := apimodel.GitSource{
		Slug:           apiclientgen.NewOptString("primary"),
		Delivery:       apiclientgen.NewOptGitSourceDelivery(apiclientgen.GitSourceDeliveryPush),
		LocalDirectory: apiclientgen.NewOptString("/does/not/matter"),
		Checkout: apiclientgen.NewOptGitSourceCheckout(apimodel.GitSourceCheckout{
			RefName: apiclientgen.NewOptString(branch),
			RefType: apiclientgen.NewOptString("branch"),
			Commit:  apiclientgen.NewOptString(baseCommit),
		}),
	}
	return source
}

// push runs Push against a local origin path. serverURL is unused by the local
// form, so the test drives the same code by pointing the origin URL at the path
// through a stub: Push builds the URL itself, so the local test instead exercises
// pushTo, the seam every caller shares.
func push(t *testing.T, repoRoot, origin string, source apimodel.GitSource, opts Options) (Result, error) {
	t.Helper()
	return pushTo(context.Background(), repoRoot, origin, "", testSandbox, source, opts)
}

// The ordinary case: local commits since create, pushed to the origin, with the
// lease recorded so the next push has something to hold.
func TestPushSendsNewCommitsAndRecordsTheLease(t *testing.T) {
	root, base := clientRepo(t, "main")
	origin := originRepo(t, "main")
	source := pushDeliveredSource("main", base)

	first, err := push(t, root, origin, source, Options{})
	if err != nil {
		t.Fatalf("first push: %v", err)
	}
	if first.Commit != base || first.Branch != "main" {
		t.Fatalf("result = %#v, want the base commit on main", first)
	}
	if got := git(t, origin, "rev-parse", "refs/heads/main"); got != base {
		t.Fatalf("origin main = %q, want %q", got, base)
	}
	lease := sandboxgit.OriginLeaseRef(testSandbox, "primary", "main")
	if got := git(t, root, "rev-parse", lease); got != base {
		t.Fatalf("lease = %q, want %q", got, base)
	}

	// Nothing new: reported as such, and no push attempted.
	again, err := push(t, root, origin, source, Options{})
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if !again.UpToDate {
		t.Fatalf("result = %#v, want up to date", again)
	}

	next := commitFile(t, root, "README.md", "two\n", "two")
	third, err := push(t, root, origin, source, Options{})
	if err != nil {
		t.Fatalf("third push: %v", err)
	}
	if third.UpToDate || third.Commit != next || third.Lease != base {
		t.Fatalf("result = %#v, want %s pushed with %s leased", third, next, base)
	}
	if got := git(t, origin, "rev-parse", "refs/heads/main"); got != next {
		t.Fatalf("origin main = %q, want %q", got, next)
	}
}

// A local rebase or amend rewrites history, which is exactly what a sandbox is
// asked to rebase onto. The lease permits it because nothing else moved the
// origin.
func TestPushAcceptsARewrittenLocalHistoryUnderTheLease(t *testing.T) {
	root, base := clientRepo(t, "main")
	origin := originRepo(t, "main")
	source := pushDeliveredSource("main", base)
	if _, err := push(t, root, origin, source, Options{}); err != nil {
		t.Fatalf("first push: %v", err)
	}

	commitFile(t, root, "extra.txt", "x\n", "extra")
	git(t, root, "commit", "--amend", "-m", "extra amended")
	amended := git(t, root, "rev-parse", "HEAD")

	result, err := push(t, root, origin, source, Options{})
	if err != nil {
		t.Fatalf("push after amend: %v", err)
	}
	if result.Commit != amended {
		t.Fatalf("result = %#v, want the amended commit %s", result, amended)
	}
	if got := git(t, origin, "rev-parse", "refs/heads/main"); got != amended {
		t.Fatalf("origin main = %q, want %q", got, amended)
	}
}

// Someone else moved the origin since this client last pushed. Rewinding it now
// would drop commits this machine never saw, so the lease refuses until --force.
func TestPushRefusesWhenTheOriginMovedSinceTheLease(t *testing.T) {
	root, base := clientRepo(t, "main")
	origin := originRepo(t, "main")
	source := pushDeliveredSource("main", base)
	if _, err := push(t, root, origin, source, Options{}); err != nil {
		t.Fatalf("first push: %v", err)
	}

	// A second machine pushes a commit this one has no record of.
	other := t.TempDir()
	git(t, "", "clone", origin, other)
	git(t, other, "config", "user.email", "other@example.com")
	git(t, other, "config", "user.name", "Other")
	elsewhere := commitFile(t, other, "other.txt", "o\n", "from elsewhere")
	git(t, other, "push", origin, "main")

	// This machine rewrites its own history and pushes again.
	git(t, root, "commit", "--amend", "-m", "one amended")
	amended := git(t, root, "rev-parse", "HEAD")
	if _, err := push(t, root, origin, source, Options{}); err == nil {
		t.Fatal("push over another machine's commits: got nil error, want a refusal")
	}
	if got := git(t, origin, "rev-parse", "refs/heads/main"); got != elsewhere {
		t.Fatalf("origin main = %q, want the other machine's commit %q left alone", got, elsewhere)
	}

	forced, err := push(t, root, origin, source, Options{Force: true})
	if err != nil {
		t.Fatalf("forced push: %v", err)
	}
	if !forced.Forced || forced.Commit != amended {
		t.Fatalf("result = %#v, want a forced push of %s", forced, amended)
	}
	if got := git(t, origin, "rev-parse", "refs/heads/main"); got != amended {
		t.Fatalf("origin main = %q, want %q", got, amended)
	}
}

// With no lease — a machine that has never pushed to this sandbox — git's own
// fast-forward rule is the protection: adding commits works, rewinding does not
// until --force.
func TestPushWithoutALeaseIsFastForwardOnly(t *testing.T) {
	root, base := clientRepo(t, "main")
	origin := originRepo(t, "main")
	source := pushDeliveredSource("main", base)
	// The origin already holds the base commit, as it would after create, but
	// this client has no record of putting it there.
	git(t, root, "push", origin, "main")

	ahead := commitFile(t, root, "README.md", "two\n", "two")
	result, err := push(t, root, origin, source, Options{})
	if err != nil {
		t.Fatalf("fast-forward push without a lease: %v", err)
	}
	if result.Lease != "" || result.Commit != ahead {
		t.Fatalf("result = %#v, want %s pushed with no lease", result, ahead)
	}

	// Now rewind: without a lease this can only be a --force.
	git(t, root, "update-ref", "-d", sandboxgit.OriginLeaseRef(testSandbox, "primary", "main"))
	git(t, root, "reset", "--hard", base)
	rewound := commitFile(t, root, "README.md", "three\n", "three")
	if _, err := push(t, root, origin, source, Options{}); err == nil {
		t.Fatal("rewinding the origin without a lease: got nil error, want a refusal")
	}
	if _, err := push(t, root, origin, source, Options{Force: true}); err != nil {
		t.Fatalf("forced rewind: %v", err)
	}
	if got := git(t, origin, "rev-parse", "refs/heads/main"); got != rewound {
		t.Fatalf("origin main = %q, want %q", got, rewound)
	}
}

// A history with nothing in common with what the sandbox holds is refused: the
// sandbox could not rebase onto it. This is the general form of the throwaway
// repository ADR 0045 builds for a directory with no repository, whose every run
// mints an unrelated root commit.
func TestPushRefusesAnUnrelatedHistory(t *testing.T) {
	root, _ := clientRepo(t, "main")
	origin := originRepo(t, "main")

	// The sandbox was created from a commit in an unrelated repository, whose
	// objects this one happens to have. Its content differs from this
	// repository's: two identical root commits made in the same second are the
	// same commit, which would be related history rather than unrelated.
	unrelated := t.TempDir()
	git(t, unrelated, "init", "-b", "main")
	git(t, unrelated, "config", "user.email", "other@example.com")
	git(t, unrelated, "config", "user.name", "Other")
	unrelatedCommit := commitFile(t, unrelated, "OTHER.md", "elsewhere\n", "elsewhere")
	git(t, root, "fetch", unrelated, "main")
	source := pushDeliveredSource("main", unrelatedCommit)

	if _, err := push(t, root, origin, source, Options{}); err == nil {
		t.Fatal("push of an unrelated history: got nil error, want a refusal")
	}
	if _, err := push(t, root, origin, source, Options{Force: true}); err != nil {
		t.Fatalf("forced push of an unrelated history: %v", err)
	}
}

// A base commit this repository no longer has says nothing about relatedness, so
// it is skipped rather than treated as unrelated.
func TestPushAllowsAnUnknownBaseCommit(t *testing.T) {
	root, _ := clientRepo(t, "main")
	origin := originRepo(t, "main")
	source := pushDeliveredSource("main", "0000000000000000000000000000000000000000")
	if _, err := push(t, root, origin, source, Options{}); err != nil {
		t.Fatalf("push with an unknown base commit: %v", err)
	}
}

// Only commits are pushed. Uncommitted work is reported so it is clear what
// stayed behind.
func TestPushReportsUncommittedChangesWithoutSendingThem(t *testing.T) {
	root, base := clientRepo(t, "main")
	origin := originRepo(t, "main")
	source := pushDeliveredSource("main", base)
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := push(t, root, origin, source, Options{})
	if err != nil {
		t.Fatalf("push with a dirty tree: %v", err)
	}
	if result.DirtyFiles != 1 {
		t.Fatalf("result = %#v, want one uncommitted change reported", result)
	}
	if got := git(t, origin, "rev-parse", "refs/heads/main"); got != base {
		t.Fatalf("origin main = %q, want the committed state %q", got, base)
	}
}

// A source checked out at a bare commit or tag names no branch, so its origin's
// conventional push branch is what moves — the ref whose HEAD the sandbox tracks.
func TestPushOfADetachedSourceMovesThePushBranch(t *testing.T) {
	root, base := clientRepo(t, "main")
	origin := originRepo(t, sandboxgit.SourcePushBranch)
	source := pushDeliveredSource("main", base)
	source.Checkout = apiclientgen.NewOptGitSourceCheckout(apimodel.GitSourceCheckout{
		Commit:  apiclientgen.NewOptString(base),
		RefName: apiclientgen.NewOptString("v1.0.0"),
		RefType: apiclientgen.NewOptString("tag"),
	})

	result, err := push(t, root, origin, source, Options{})
	if err != nil {
		t.Fatalf("push of a detached source: %v", err)
	}
	if result.Branch != sandboxgit.SourcePushBranch || result.LocalRev != "HEAD" {
		t.Fatalf("result = %#v, want HEAD pushed to %s", result, sandboxgit.SourcePushBranch)
	}
	if got := git(t, origin, "rev-parse", "refs/heads/"+sandboxgit.SourcePushBranch); got != base {
		t.Fatalf("origin %s = %q, want %q", sandboxgit.SourcePushBranch, got, base)
	}
}

// --branch offers the sandbox another branch to rebase onto or cherry-pick from,
// under its own name, leaving the one the source tracks where it is.
func TestPushOfAnotherBranchLandsUnderItsOwnName(t *testing.T) {
	root, base := clientRepo(t, "main")
	origin := originRepo(t, "main")
	source := pushDeliveredSource("main", base)
	if _, err := push(t, root, origin, source, Options{}); err != nil {
		t.Fatalf("push main: %v", err)
	}
	git(t, root, "checkout", "-b", "spike")
	spike := commitFile(t, root, "spike.txt", "s\n", "spike")

	result, err := push(t, root, origin, source, Options{Branch: "spike"})
	if err != nil {
		t.Fatalf("push spike: %v", err)
	}
	if result.Branch != "spike" || result.Commit != spike {
		t.Fatalf("result = %#v, want spike at %s", result, spike)
	}
	if got := git(t, origin, "rev-parse", "refs/heads/main"); got != base {
		t.Fatalf("origin main = %q, want it left at %q", got, base)
	}
}

// A source the sandbox reaches on its own has no origin repository to push into,
// and the two ways that happens read differently.
func TestCheckPushDeliveredExplainsWhyThereIsNothingToPush(t *testing.T) {
	live := apimodel.GitSource{
		Slug:           apiclientgen.NewOptString("primary"),
		Delivery:       apiclientgen.NewOptGitSourceDelivery(apiclientgen.GitSourceDeliveryClone),
		LocalDirectory: apiclientgen.NewOptString("/home/me/project"),
	}
	err := CheckPushDelivered(live)
	if err == nil || !strings.Contains(err.Error(), "reads your directory live") {
		t.Fatalf("clone-delivered local source error = %v", err)
	}
	remote := apimodel.GitSource{
		Slug:     apiclientgen.NewOptString("primary"),
		Delivery: apiclientgen.NewOptGitSourceDelivery(apiclientgen.GitSourceDeliveryClone),
	}
	err = CheckPushDelivered(remote)
	if err == nil || !strings.Contains(err.Error(), "from a remote") {
		t.Fatalf("remote source error = %v", err)
	}
}
