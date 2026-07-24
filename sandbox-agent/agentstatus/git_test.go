package agentstatus

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/obot-platform/discobox/sandboxconfig"
)

func runGitCommand(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // args are fixed, test-authored git subcommands, not external input.
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func TestComputeGitStatusCleanRepo(t *testing.T) {
	dir := t.TempDir()
	runGitCommand(t, dir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, dir, "add", "README.md")
	runGitCommand(t, dir, "commit", "-m", "initial")

	statuses := ComputeGitStatus(context.Background(), []sandboxconfig.Source{{Slug: "primary", Target: dir}})
	if len(statuses) != 1 {
		t.Fatalf("statuses = %d, want 1", len(statuses))
	}
	got := statuses[0]
	if got.Error != "" {
		t.Fatalf("error = %q, want none", got.Error)
	}
	if !got.Clean {
		t.Fatalf("clean = false, want true; porcelain=%q", got.Porcelain)
	}
	if got.Branch != "main" {
		t.Fatalf("branch = %q, want main", got.Branch)
	}
	if got.HeadCommit == "" {
		t.Fatal("headCommit is empty")
	}
	if got.Slug != "primary" || got.Target != dir {
		t.Fatalf("slug/target = %q/%q, want primary/%q", got.Slug, got.Target, dir)
	}
}

func TestComputeGitStatusDirtyRepo(t *testing.T) {
	dir := t.TempDir()
	runGitCommand(t, dir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, dir, "add", "README.md")
	runGitCommand(t, dir, "commit", "-m", "initial")
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	statuses := ComputeGitStatus(context.Background(), []sandboxconfig.Source{{Slug: "primary", Target: dir}})
	got := statuses[0]
	if got.Clean {
		t.Fatalf("clean = true, want false; porcelain=%q", got.Porcelain)
	}
	if !strings.Contains(got.Porcelain, "untracked.txt") {
		t.Fatalf("porcelain = %q, want it to mention untracked.txt", got.Porcelain)
	}
}

func TestComputeGitStatusNonRepoDirectory(t *testing.T) {
	dir := t.TempDir()
	statuses := ComputeGitStatus(context.Background(), []sandboxconfig.Source{{Slug: "primary", Target: dir}})
	if len(statuses) != 1 {
		t.Fatalf("statuses = %d, want 1", len(statuses))
	}
	if statuses[0].Error == "" {
		t.Fatal("error is empty, want a non-repo error")
	}
}

func TestComputeGitStatusMissingDirectory(t *testing.T) {
	statuses := ComputeGitStatus(context.Background(), []sandboxconfig.Source{{Slug: "primary", Target: "/does/not/exist"}})
	if len(statuses) != 1 {
		t.Fatalf("statuses = %d, want 1", len(statuses))
	}
	if statuses[0].Error == "" {
		t.Fatal("error is empty, want a missing-directory error")
	}
}

func TestComputeGitStatusOneBadSourceDoesNotFailOthers(t *testing.T) {
	goodDir := t.TempDir()
	runGitCommand(t, goodDir, "init", "-b", "main")
	runGitCommand(t, goodDir, "commit", "--allow-empty", "-m", "initial")

	statuses := ComputeGitStatus(context.Background(), []sandboxconfig.Source{
		{Slug: "bad", Target: "/does/not/exist"},
		{Slug: "good", Target: goodDir},
	})
	if len(statuses) != 2 {
		t.Fatalf("statuses = %d, want 2", len(statuses))
	}
	if statuses[0].Error == "" {
		t.Fatal("bad source: error is empty")
	}
	if statuses[1].Error != "" || !statuses[1].Clean {
		t.Fatalf("good source = %+v, want clean with no error", statuses[1])
	}
}

func TestLimitedBufferTruncates(t *testing.T) {
	buf := &limitedBuffer{max: 4}
	if _, err := buf.Write([]byte("hello world")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if buf.buf.String() != "hell" {
		t.Fatalf("captured = %q, want %q", buf.buf.String(), "hell")
	}
	if !buf.truncated {
		t.Fatal("truncated = false, want true")
	}
}
