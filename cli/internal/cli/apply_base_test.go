package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveApplyBaseFallsBackToMergeBaseAfterSandboxRebase(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(name, content, subject string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", name)
		git("commit", "-m", subject)
		return git("rev-parse", "HEAD")
	}

	git("init", "--initial-branch=main")
	base := commit("base.txt", "base\n", "base")

	// The first sandbox commit was applied onto a host that had moved on, so
	// its sandbox SHA is deliberately different from the host-side cherry-pick.
	git("checkout", "-b", "old-sandbox", base)
	lastApplied := commit("applied.txt", "sandbox work\n", "sandbox work")
	git("checkout", "main")
	commit("host.txt", "host work\n", "host work")
	git("cherry-pick", lastApplied)
	hostTip := git("rev-parse", "HEAD")

	// Rebase the sandbox onto the updated host and add two more commits. The
	// recorded sandbox SHA still exists as an object, but not in this history.
	git("checkout", "-b", "rebased-sandbox", hostTip)
	commit("one.txt", "one\n", "one")
	tip := commit("two.txt", "two\n", "two")
	git("checkout", "main")

	got, origin, err := resolveApplyBase(ctx, repo, tip, lastApplied, "")
	if err != nil {
		t.Fatalf("resolveApplyBase: %v", err)
	}
	if got != hostTip || origin != baseOriginMergeBase {
		t.Fatalf("base = %s (%s), want current merge base %s (%s)", got, origin, hostTip, baseOriginMergeBase)
	}
	if count := git("rev-list", "--count", got+".."+tip); count != "2" {
		t.Fatalf("selected range has %s commits, want the two made after the rebase", count)
	}
}

func TestResolveApplyBaseRetainsAncestralLastAppliedCursor(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(name string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", name)
		git("commit", "-m", name)
		return git("rev-parse", "HEAD")
	}

	git("init", "--initial-branch=main")
	commit("base")
	lastApplied := commit("already-applied")
	tip := commit("new-work")

	got, origin, err := resolveApplyBase(ctx, repo, tip, lastApplied, "")
	if err != nil {
		t.Fatalf("resolveApplyBase: %v", err)
	}
	if got != lastApplied || origin != baseOriginLastApplied {
		t.Fatalf("base = %s (%s), want last applied %s (%s)", got, origin, lastApplied, baseOriginLastApplied)
	}
}
