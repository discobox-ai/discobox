package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo is a real repository to run the sandbox-side scripts against. They
// are shell and git, so nothing short of both actually tests them.
type gitRepo struct {
	t   *testing.T
	dir string
}

func newGitRepo(t *testing.T) *gitRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := &gitRepo{t: t, dir: t.TempDir()}
	repo.git("init", "-q", "-b", "main", ".")
	return repo
}

func (r *gitRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.CommandContext(r.t.Context(), "git", args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitErr runs git for its exit status, for the checks whose whole point is that
// the command is expected to fail.
func (r *gitRepo) gitErr(args ...string) error {
	r.t.Helper()
	cmd := exec.CommandContext(r.t.Context(), "git", args...)
	cmd.Dir = r.dir
	return cmd.Run()
}

func (r *gitRepo) write(name, content string) {
	r.t.Helper()
	path := filepath.Join(r.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *gitRepo) commit(name, content, message string) string {
	r.t.Helper()
	r.write(name, content)
	r.git("add", "-A")
	r.git("commit", "-qm", message)
	return r.git("rev-parse", "HEAD")
}

// run executes one of the sandbox-side commands in the repo, as the sandbox
// would, and returns its output.
func (r *gitRepo) run(command []string) (string, error) {
	r.t.Helper()
	//nolint:gosec // the command is this package's own script, not user input
	cmd := exec.CommandContext(r.t.Context(), command[0], command[1:]...)
	cmd.Dir = r.dir
	out, err := cmd.Output()
	return string(out), err
}

func (r *gitRepo) resolveBase(checkout, upstreamRef string) (string, string) {
	r.t.Helper()
	out, err := r.run(diffBaseCommand(checkout, upstreamRef))
	if err != nil {
		r.t.Fatalf("resolve base: %v", err)
	}
	commit, origin, ok := strings.Cut(strings.TrimSpace(out), "\t")
	if !ok {
		r.t.Fatalf("resolve base: unexpected output %q", out)
	}
	return commit, origin
}

// TestDiffBaseFallsBackToCheckout is the push-delivered sandbox: its repository
// is `git init` with no remote at all, so there is no upstream to find and the
// cloned commit is the only base that exists.
func TestDiffBaseFallsBackToCheckout(t *testing.T) {
	repo := newGitRepo(t)
	base := repo.commit("a.txt", "one\n", "base")
	repo.commit("a.txt", "two\n", "sandbox work")

	commit, origin := repo.resolveBase(base, "refs/remotes/origin/main")
	if origin != diffBaseCheckout || commit != base {
		t.Fatalf("got %s (%s), want %s (%s)", commit, origin, base, diffBaseCheckout)
	}
}

// TestDiffBaseIgnoresUpstreamNotPulled covers the ordinary sandbox: it has an
// upstream, but has only written its own commits. The merge base is the cloned
// commit, so nothing changes and the origin still says so.
func TestDiffBaseIgnoresUpstreamNotPulled(t *testing.T) {
	repo := newGitRepo(t)
	base := repo.commit("a.txt", "one\n", "base")
	// Upstream moved on, but the sandbox never fetched it: its tracking ref is
	// still where the clone left it.
	repo.git("update-ref", "refs/remotes/origin/main", base)
	repo.commit("a.txt", "two\n", "sandbox work")

	commit, origin := repo.resolveBase(base, "refs/remotes/origin/main")
	if origin != diffBaseCheckout || commit != base {
		t.Fatalf("got %s (%s), want %s (%s)", commit, origin, base, diffBaseCheckout)
	}
}

// TestDiffBaseUsesMergeBaseAfterPull is the case that started all this: the
// sandbox merged newer upstream, and those commits are not its work.
func TestDiffBaseUsesMergeBaseAfterPull(t *testing.T) {
	repo := newGitRepo(t)
	base := repo.commit("a.txt", "one\n", "base")

	// Upstream advances on its own branch, and the sandbox fetches and merges it.
	repo.git("checkout", "-q", "-b", "upstream")
	upstream := repo.commit("upstream.txt", "from upstream\n", "upstream work")
	repo.git("update-ref", "refs/remotes/origin/main", upstream)
	repo.git("checkout", "-q", "main")
	repo.commit("b.txt", "sandbox\n", "sandbox work")
	repo.git("merge", "-q", "--no-edit", "upstream")

	commit, origin := repo.resolveBase(base, "refs/remotes/origin/main")
	if origin != diffBaseMergeBase || commit != upstream {
		t.Fatalf("got %s (%s), want %s (%s)", commit, origin, upstream, diffBaseMergeBase)
	}
}

// TestDiffBaseKeepsCarriedInWork pins the rule the default exists for: a
// sandbox created from a dirty local tree still diffs against the commit it
// cloned, so the work handed to it is reported as present in the sandbox rather
// than silently excluded. --base snapshot is the other view.
func TestDiffBaseKeepsCarriedInWork(t *testing.T) {
	repo := newGitRepo(t)
	base := repo.commit("a.txt", "one\n", "base")
	repo.git("checkout", "-q", "-b", "snap")
	repo.commit("carried.txt", "handed to the sandbox\n", "workspace snapshot")
	repo.git("update-ref", "refs/discobox/run/snap_test", "HEAD")
	repo.git("checkout", "-q", "main")

	commit, origin := repo.resolveBase(base, "refs/remotes/origin/main")
	if origin != diffBaseCheckout || commit != base {
		t.Fatalf("got %s (%s), want %s (%s)", commit, origin, base, diffBaseCheckout)
	}
}

func TestDiffBaseDescribesWhereItCameFrom(t *testing.T) {
	for _, tc := range []struct {
		origin string
		want   string
	}{
		{diffBaseOverride, "--base"},
		{diffBaseMergeBase, "merge base with origin/main"},
		{diffBaseCheckout, "the commit the sandbox cloned"},
	} {
		got := diffBase{Origin: tc.origin}.describe("refs/remotes/origin/main")
		if got != tc.want {
			t.Fatalf("describe(%s): got %q, want %q", tc.origin, got, tc.want)
		}
	}
}

// TestDiffBaseNeverMovesBackwards covers an upstream branch that was rewritten:
// the merge base is then older than the cloned commit, and taking it would
// widen the diff with commits the sandbox never wrote.
func TestDiffBaseNeverMovesBackwards(t *testing.T) {
	repo := newGitRepo(t)
	older := repo.commit("a.txt", "one\n", "older")
	base := repo.commit("a.txt", "one\ntwo\n", "the commit the sandbox cloned")
	repo.commit("b.txt", "sandbox\n", "sandbox work")

	// Upstream drops the cloned commit and rebuilds the branch from the older
	// one, so HEAD and origin/main now share only that older commit.
	repo.git("branch", "-q", "rewritten", older)
	repo.git("checkout", "-q", "rewritten")
	repo.commit("a.txt", "one\nrewritten\n", "rewritten history")
	repo.git("update-ref", "refs/remotes/origin/main", "rewritten")
	repo.git("checkout", "-q", "main")

	commit, origin := repo.resolveBase(base, "refs/remotes/origin/main")
	if origin != diffBaseCheckout || commit != base {
		t.Fatalf("base moved backwards: got %s (%s), want %s (%s)", commit, origin, base, diffBaseCheckout)
	}
}
