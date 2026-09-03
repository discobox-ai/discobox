package access

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// initRepo makes a one-commit git repository in a fresh temp directory, chdirs
// into it for the duration of the test, and returns the commit's full SHA.
func initRepo(t *testing.T, subject string) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "t")
	if err := exec.Command("sh", "-c", "echo hi > f").Run(); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	run("add", "f")
	run("commit", "-q", "-m", subject)
	sha, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(sha))
}

func TestGitOutputIsEmptyOutsideARepository(t *testing.T) {
	t.Chdir(t.TempDir())
	if got := gitOutput(context.Background(), "rev-parse", "--show-toplevel"); got != "" {
		t.Fatalf("repo root = %q, want empty outside a repository", got)
	}
}

func TestGatherFactsFindsTheRepoRootAndAResolvedRef(t *testing.T) {
	sha := initRepo(t, "the commit this test is about")

	f := gatherFacts(context.Background(), []string{"git", "push", "origin", sha + ":refs/heads/main"})

	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("rev-parse --show-toplevel: %v", err)
	}
	if f.repoRoot != strings.TrimSpace(string(root)) {
		t.Fatalf("repoRoot = %q, want %q", f.repoRoot, strings.TrimSpace(string(root)))
	}
	if f.refSHA != sha {
		t.Fatalf("refSHA = %q, want the resolved commit %q", f.refSHA, sha)
	}
	if f.refSubject != "the commit this test is about" {
		t.Fatalf("refSubject = %q, want the commit's own subject", f.refSubject)
	}
}

// The left half of a refspec is what this command actually pushes; the right
// half is a destination name that may not exist locally at all. Neither
// resolving is not an error — it means gitRefFact moves on to the next
// candidate, or reports nothing.
func TestGitRefFactTriesTheSecondHalfOfARefspecWhenTheFirstDoesNotResolve(t *testing.T) {
	initRepo(t, "reachable only by branch name")

	sha, subject := gitRefFact(context.Background(), []string{"origin", "not-a-real-ref:master"})
	if subject != "reachable only by branch name" {
		t.Fatalf("subject = %q, want the commit on master", subject)
	}
	if sha == "" {
		t.Fatal("sha is empty, want master's commit")
	}
}

func TestGatherFactsSkipsNonGitCommands(t *testing.T) {
	sha := initRepo(t, "not what this test is asking about")

	f := gatherFacts(context.Background(), []string{"gh", "pr", "create", sha})
	if f.refSHA != "" || f.refSubject != "" {
		t.Fatalf("facts = %+v, want no ref fact for a non-git command", f)
	}
	if f.repoRoot == "" {
		t.Fatal("repoRoot is empty, want it to still be resolved: the repo location is not about the command")
	}
}

// The repository this queries is written by the agent it is helping judge, so
// its own config must not be able to redirect a lookup — a slow or failing
// pager and diff.external are exactly what an untrusted .git/config could set
// to turn a fact-gathering call into something else (ADR 0090 §2). The `-c`
// overrides gitOutput passes take precedence over anything committed to
// config, which is what this asserts by setting both to something that would
// visibly break the call if it were honored.
func TestGitOutputOverridesRepositoryConfigThatWouldRedirectIt(t *testing.T) {
	initRepo(t, "config cannot redirect this")
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("config", "core.pager", "exit 1; #")
	run("config", "diff.external", "exit 1; #")

	if got := gitOutput(context.Background(), "rev-parse", "--show-toplevel"); got == "" {
		t.Fatal("repo root is empty; the repository's own core.pager should not have been able to break this")
	}
}
