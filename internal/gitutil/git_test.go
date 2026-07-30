package gitutil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Ada Lovelace", "GIT_AUTHOR_EMAIL=ada@example.com",
		"GIT_COMMITTER_NAME=Ada Lovelace", "GIT_COMMITTER_EMAIL=ada@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestLogListsRangeOldestFirst(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	gitRun(t, dir, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "a.txt")
	gitRun(t, dir, "commit", "-m", "base")
	base := gitRun(t, dir, "rev-parse", "HEAD")

	// A subject with a tab in it: the record format has to survive whatever a
	// commit message contains.
	for _, subject := range []string{"first\tchange", "second change"} {
		if err := os.WriteFile(filepath.Join(dir, subject[:5]+".txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRun(t, dir, "add", "-A")
		gitRun(t, dir, "commit", "-m", subject)
	}

	commits, err := Log(ctx, dir, base+"..HEAD")
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	if len(commits) != 2 {
		t.Fatalf("got %d commits, want 2: %+v", len(commits), commits)
	}
	if commits[0].Subject != "first\tchange" || commits[1].Subject != "second change" {
		t.Fatalf("subjects out of order or mangled: %+v", commits)
	}
	if commits[0].Author != "Ada Lovelace" {
		t.Fatalf("author = %q, want Ada Lovelace", commits[0].Author)
	}
	if commits[0].Date.IsZero() {
		t.Fatal("commit date was not parsed")
	}
	if len(commits[0].SHA) != 40 {
		t.Fatalf("SHA = %q, want a full object name", commits[0].SHA)
	}
}

func TestLogEmptyRange(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	gitRun(t, dir, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "a.txt")
	gitRun(t, dir, "commit", "-m", "base")

	commits, err := Log(ctx, dir, "HEAD..HEAD")
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	if len(commits) != 0 {
		t.Fatalf("got %d commits for an empty range, want 0", len(commits))
	}
}

func TestTracerSeesCommandsWithCredentialsRedacted(t *testing.T) {
	dir := t.TempDir()
	gitRun(t, dir, "init", "--initial-branch=main")

	var traced [][]string
	ctx := WithTracer(context.Background(), func(_ string, args []string) {
		traced = append(traced, args)
	})
	_, _ = Output(ctx, dir, nil, nil,
		"-c", "http.extraHeader=Authorization: Bearer super-secret",
		"fetch", "https://user:hunter2@example.invalid/repo.git")

	if len(traced) != 1 {
		t.Fatalf("traced %d commands, want 1", len(traced))
	}
	line := strings.Join(traced[0], " ")
	if strings.Contains(line, "super-secret") || strings.Contains(line, "hunter2") {
		t.Fatalf("traced command leaked a credential: %s", line)
	}
	if !strings.Contains(line, "http.extraHeader=Authorization: [REDACTED]") {
		t.Fatalf("traced command lost the header name: %s", line)
	}
	if !strings.Contains(line, "https://user:[REDACTED]@example.invalid/repo.git") {
		t.Fatalf("traced command lost the URL: %s", line)
	}
}

func TestNoTracerIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	gitRun(t, dir, "init", "--initial-branch=main")
	if _, err := Output(context.Background(), dir, nil, nil, "rev-parse", "--is-inside-work-tree"); err != nil {
		t.Fatalf("output without a tracer: %v", err)
	}
}
