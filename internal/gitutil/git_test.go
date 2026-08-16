package gitutil

import (
	"context"
	"errors"
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

func TestRootReportsADirectoryOutsideAnyRepositoryDistinctly(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	_, err := Root(ctx, dir)
	if !errors.Is(err, ErrNotARepository) {
		t.Fatalf("Root outside a repository: err = %v, want ErrNotARepository", err)
	}

	gitRun(t, dir, "init", "--initial-branch=main")
	root, err := Root(ctx, dir)
	if err != nil {
		t.Fatalf("Root inside a repository: %v", err)
	}
	if resolved, symErr := filepath.EvalSymlinks(dir); symErr == nil && root != resolved {
		t.Fatalf("root = %q, want %q", root, resolved)
	}
}

func TestInitOverWorkTreeLeavesTheWorkTreeUntouched(t *testing.T) {
	ctx := context.Background()
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "sub", "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An ignored file stays ignored: the repository reads the working tree's
	// own rules, exactly as one living inside it would.
	if err := os.WriteFile(filepath.Join(work, ".gitignore"), []byte("skipped\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "skipped"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	repo, cleanup, err := InitOverWorkTree(ctx, work)
	if err != nil {
		t.Fatalf("InitOverWorkTree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, ".git")); !os.IsNotExist(err) {
		t.Fatalf("stat %s/.git = %v, want the work tree to have no repository in it", work, err)
	}

	changes, err := StatusChanges(ctx, repo)
	if err != nil {
		t.Fatalf("StatusChanges: %v", err)
	}
	var paths []string
	for _, change := range changes {
		paths = append(paths, change.Path)
	}
	want := []string{".gitignore", "a.txt", "sub/b.txt"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("status paths = %v, want %v", paths, want)
	}

	// The empty tree plus a root commit is a usable HEAD, which is what makes
	// the working tree snapshottable as a change against nothing.
	empty, err := EmptyTree(ctx, repo)
	if err != nil {
		t.Fatalf("EmptyTree: %v", err)
	}
	base, err := CommitTree(ctx, repo, empty, "", "base\n")
	if err != nil {
		t.Fatalf("CommitTree: %v", err)
	}
	branch, ok := CurrentBranch(ctx, repo)
	if !ok {
		t.Fatal("a fresh repository reported no branch")
	}
	if err := UpdateRef(ctx, repo, "refs/heads/"+branch, base); err != nil {
		t.Fatalf("UpdateRef: %v", err)
	}
	tree, treeCleanup, err := CurrentWorkspaceTree(ctx, repo)
	if err != nil {
		t.Fatalf("CurrentWorkspaceTree: %v", err)
	}
	defer treeCleanup()
	if !tree.Dirty || tree.BaseCommit != base || tree.BaseTree != empty {
		t.Fatalf("tree = %+v, want the work tree dirty against the empty root commit %s", tree, base)
	}

	cleanup()
	if _, err := os.Stat(repo); !os.IsNotExist(err) {
		t.Fatalf("stat %s = %v, want the repository removed", repo, err)
	}
	if _, err := os.Stat(filepath.Join(work, "a.txt")); err != nil {
		t.Fatalf("the work tree did not survive its repository: %v", err)
	}
}
