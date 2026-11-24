package agentstatus

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
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

	statuses := ComputeGitStatus(context.Background(), []sandboxconfig.Source{{Slug: "primary", Target: dir}}, nil)
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

// TestComputeGitStatusRunsAsGivenUser proves the git status shell-out
// actually applies the passed user's credential (via execs.AgentSysProcAttr)
// rather than silently ignoring it: sandbox-agent's server runs as root while
// sources are owned by the sandbox's resolved non-root user, so git status
// must run as that user or its dubious-ownership guard refuses every source.
func TestComputeGitStatusRunsAsGivenUser(t *testing.T) {
	// The credential this asserts on is a uid/gid pair, which only a Linux
	// sandbox has: elsewhere a sandbox has one account, the agent runs as it,
	// and execs.AgentSysProcAttr gives no credential at all (ADR 0145 §5).
	if runtime.GOOS != "linux" {
		t.Skip("a uid/gid credential is applied only in a Linux sandbox")
	}
	dir := t.TempDir()
	runGitCommand(t, dir, "init", "-b", "main")
	runGitCommand(t, dir, "commit", "--allow-empty", "-m", "initial")

	uid, gid := int64(os.Getuid()), int64(os.Getgid())
	statuses := ComputeGitStatus(context.Background(), []sandboxconfig.Source{{Slug: "primary", Target: dir}}, &execs.User{UID: &uid, GID: &gid})
	if len(statuses) != 1 {
		t.Fatalf("statuses = %d, want 1", len(statuses))
	}
	if got := statuses[0]; got.Error != "" {
		if strings.Contains(got.Error, "operation not permitted") {
			// Some sandboxes deny the setuid/setgid syscalls to compiled test
			// binaries specifically, even for a credential matching the
			// process's own uid/gid (a plain `go run` of the identical exec
			// call succeeds outside `go test`). That is an environment
			// restriction on this test's harness, not a defect in
			// ComputeGitStatus — sandbox-agent's real deployment runs as root
			// with CAP_SETUID and is covered by the e2e verification for this
			// feature (ADR 0030), not by this unit test.
			t.Skipf("setuid/setgid denied to this test binary in this environment: %s", got.Error)
		}
		t.Fatalf("error = %q, want none (user credential should match this process)", got.Error)
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

	statuses := ComputeGitStatus(context.Background(), []sandboxconfig.Source{{Slug: "primary", Target: dir}}, nil)
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
	statuses := ComputeGitStatus(context.Background(), []sandboxconfig.Source{{Slug: "primary", Target: dir}}, nil)
	if len(statuses) != 1 {
		t.Fatalf("statuses = %d, want 1", len(statuses))
	}
	if statuses[0].Error == "" {
		t.Fatal("error is empty, want a non-repo error")
	}
}

func TestComputeGitStatusMissingDirectory(t *testing.T) {
	statuses := ComputeGitStatus(context.Background(), []sandboxconfig.Source{{Slug: "primary", Target: "/does/not/exist"}}, nil)
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
	}, nil)
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

// The diff stat is measured against the manifest's base commit, spanning
// committed and uncommitted tracked changes both, and travels with its base:
// a source with no base commit, or a base the repository does not have,
// reports no stat at all.
func TestComputeGitStatusDiffStat(t *testing.T) {
	dir := t.TempDir()
	runGitCommand(t, dir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, dir, "add", "README.md")
	runGitCommand(t, dir, "commit", "-m", "initial")
	base := headCommit(t, dir)

	// One committed change and one uncommitted change, both against base.
	if err := os.WriteFile(filepath.Join(dir, "committed.txt"), []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, dir, "add", "committed.txt")
	runGitCommand(t, dir, "commit", "-m", "committed work")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	statuses := ComputeGitStatus(context.Background(), []sandboxconfig.Source{
		{Slug: "primary", Target: dir, BaseCommit: base},
	}, nil)
	got := statuses[0]
	if got.Error != "" {
		t.Fatalf("error = %q, want none", got.Error)
	}
	if got.DiffBase != base {
		t.Fatalf("diffBase = %q, want %q", got.DiffBase, base)
	}
	if got.DiffFiles != 2 || got.DiffAdded != 2 || got.DiffDeleted != 1 {
		t.Fatalf("diff stat = %d files +%d -%d, want 2 files +2 -1", got.DiffFiles, got.DiffAdded, got.DiffDeleted)
	}
}

func TestComputeGitStatusDiffStatUnknownBase(t *testing.T) {
	dir := t.TempDir()
	runGitCommand(t, dir, "init", "-b", "main")
	runGitCommand(t, dir, "commit", "--allow-empty", "-m", "initial")

	statuses := ComputeGitStatus(context.Background(), []sandboxconfig.Source{
		{Slug: "primary", Target: dir, BaseCommit: "4444444444444444444444444444444444444444"},
	}, nil)
	got := statuses[0]
	if got.Error != "" {
		t.Fatalf("error = %q, want none: a missing base drops the stat, not the source", got.Error)
	}
	if got.DiffBase != "" || got.DiffFiles != 0 {
		t.Fatalf("diff stat = %+v, want none", got)
	}
}

func TestParseShortstat(t *testing.T) {
	cases := []struct {
		out                   string
		files, added, deleted int
	}{
		{"", 0, 0, 0},
		{" 3 files changed, 61 insertions(+), 12 deletions(-)\n", 3, 61, 12},
		{" 1 file changed, 1 insertion(+)\n", 1, 1, 0},
		{" 2 files changed, 4 deletions(-)\n", 2, 0, 4},
	}
	for _, tc := range cases {
		files, added, deleted := parseShortstat(tc.out)
		if files != tc.files || added != tc.added || deleted != tc.deleted {
			t.Fatalf("parseShortstat(%q) = %d/%d/%d, want %d/%d/%d", tc.out, files, added, deleted, tc.files, tc.added, tc.deleted)
		}
	}
}

func headCommit(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// Once the sandbox has fetched, the diff base moves forward to the merge base
// with the upstream tracking ref, so commits it pulled rather than wrote stop
// counting as its changes (ADR 0018's rule, carried here by ADR 0037).
func TestComputeGitStatusDiffStatForwardsToMergeBase(t *testing.T) {
	upstream := t.TempDir()
	runGitCommand(t, upstream, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(upstream, "README.md"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, upstream, "add", "README.md")
	runGitCommand(t, upstream, "commit", "-m", "initial")

	parent := t.TempDir()
	clone := filepath.Join(parent, "clone")
	runGitCommand(t, parent, "clone", upstream, clone)
	base := headCommit(t, clone)

	// Upstream advances, and the sandbox pulls it: those lines are not the
	// sandbox's changes.
	if err := os.WriteFile(filepath.Join(upstream, "upstream.txt"), []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, upstream, "add", "upstream.txt")
	runGitCommand(t, upstream, "commit", "-m", "upstream work")
	runGitCommand(t, clone, "pull", "origin", "main")

	// One line of the sandbox's own on top.
	if err := os.WriteFile(filepath.Join(clone, "local.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, clone, "add", "local.txt")
	runGitCommand(t, clone, "commit", "-m", "local work")

	statuses := ComputeGitStatus(context.Background(), []sandboxconfig.Source{{
		Slug:        "primary",
		Target:      clone,
		BaseCommit:  base,
		UpstreamRef: "refs/remotes/origin/main",
	}}, nil)
	got := statuses[0]
	if got.Error != "" {
		t.Fatalf("error = %q, want none", got.Error)
	}
	if got.DiffBase == base {
		t.Fatalf("diffBase = spawn commit %s, want the merge base past the pulled commit", base)
	}
	if got.DiffFiles != 1 || got.DiffAdded != 1 || got.DiffDeleted != 0 {
		t.Fatalf("diff stat = %d files +%d -%d, want only the local change (1 file +1 -0)", got.DiffFiles, got.DiffAdded, got.DiffDeleted)
	}
}

// An upstream ref that does not resolve — a push-delivered source has no
// remote at all — leaves the base at the spawn commit.
func TestComputeGitStatusDiffStatUnresolvableUpstreamKeepsSpawnBase(t *testing.T) {
	dir := t.TempDir()
	runGitCommand(t, dir, "init", "-b", "main")
	runGitCommand(t, dir, "commit", "--allow-empty", "-m", "initial")
	base := headCommit(t, dir)

	statuses := ComputeGitStatus(context.Background(), []sandboxconfig.Source{{
		Slug:        "primary",
		Target:      dir,
		BaseCommit:  base,
		UpstreamRef: "refs/remotes/origin/HEAD",
	}}, nil)
	got := statuses[0]
	if got.DiffBase != base {
		t.Fatalf("diffBase = %q, want the spawn commit %q", got.DiffBase, base)
	}
}

// TestComputeGitStatusOverrunLeavesNoIndexLock pins that the status poll
// never leaves the sandbox's own repository locked. git status takes
// .git/index.lock after refreshing the index and holds it while it collects
// (diffs, submodules, the untracked scan) so it can write the refreshed index
// back. A poll that outruns its timeout is SIGKILLed, which git cannot clean
// up after, so the lock stays and the harness's next commit fails with
// "index.lock: File exists". A big freshly delivered checkout is what outruns
// it in a box; here a submodule whose own status runs a slow clean filter
// holds the parent's collect phase open, deterministically.
func TestComputeGitStatusOverrunLeavesNoIndexLock(t *testing.T) {
	dir := t.TempDir()
	runGitCommand(t, dir, "init", "-b", "main")
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, sub, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(sub, "f"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, sub, "add", "f")
	runGitCommand(t, sub, "commit", "-m", "sub")
	// The poll stops waiting at its deadline, but the filter runs on, with
	// its working directory in dir. Its marker says it has finished; the
	// cleanup below (run before dir's own) waits for it, so the test leaves
	// no process behind and Windows can remove dir.
	marker := filepath.Join(t.TempDir(), "filter-done")
	runGitCommand(t, sub, "config", "filter.slow.clean", "sleep 3; cat; : > '"+filepath.ToSlash(marker)+"'")
	t.Cleanup(func() {
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if _, err := os.Stat(marker); err == nil {
				return
			}
		}
		t.Log("the slow filter never reported finishing")
	})
	if err := os.WriteFile(filepath.Join(sub, ".git", "info", "attributes"), []byte("f filter=slow\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCommand(t, dir, "add", "sub")
	runGitCommand(t, dir, "commit", "-m", "initial")
	// Make the submodule's file stat-dirty, so its status must run the filter.
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(filepath.Join(sub, "f"), later, later); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	statuses := ComputeGitStatus(ctx, []sandboxconfig.Source{{Slug: "primary", Target: dir}}, nil)
	elapsed := time.Since(start)
	if len(statuses) != 1 || statuses[0].Error == "" {
		t.Fatalf("statuses = %+v, want one that timed out", statuses)
	}
	// The submodule's git and its filter hold git's output pipes open past
	// the kill; the poll still answers within its deadline and gitWaitDelay,
	// well before the filter's three seconds.
	if limit := 300*time.Millisecond + gitWaitDelay + time.Second; elapsed > limit {
		t.Fatalf("a timed-out status poll took %s, want at most %s: the deadline does not bound it", elapsed, limit)
	}
	lock := filepath.Join(dir, ".git", "index.lock")
	if _, err := os.Stat(lock); err == nil {
		t.Fatalf("a timed-out status poll left %s behind; the harness's next commit fails with \"File exists\"", lock)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
