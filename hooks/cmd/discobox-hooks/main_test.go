package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"text/tabwriter"
	"time"

	hooks "github.com/discobox-ai/discobox/hooks"
	hookapi "github.com/discobox-ai/discobox/hooks/api"
	"github.com/discobox-ai/discobox/hooks/client"
	"github.com/discobox-ai/discobox/hooks/models"
	"github.com/discobox-ai/discobox/internal/gitutil"
	"github.com/spf13/cobra"
)

func writeTestJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func TestRootCommandRejectsUnknownOutput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newRootCommand(cliOptions{stdout: &stdout, stderr: &stderr})
	cmd.SetArgs([]string{"-o", "yaml", "list"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "unsupported output format") {
		t.Fatalf("expected unsupported output error, got %v", err)
	}
}

func TestCommandLayout(t *testing.T) {
	cmd := newRootCommand(cliOptions{})
	for _, args := range [][]string{
		{"daemon"},
		{"daemon", "status"},
		{"daemon", "shutdown"},
		{"ls"},
		{"list"},
		{"run"},
		{"check"},
		{"runs"},
		{"changes"},
		{"snapshots"},
		{"snapshots", "stat"},
		{"snapshots", "diff"},
		{"snapshots", "apply"},
		{"snapshots", "reset"},
		{"queue"},
	} {
		if found, _, err := cmd.Find(args); err != nil || found == nil {
			t.Fatalf("Find(%v) = cmd %#v err %v", args, found, err)
		}
	}
	for _, args := range [][]string{
		{"db"},
		{"rerun"},
		{"status"},
		{"shutdown"},
	} {
		if found, _, err := cmd.Find(args); err == nil && found != cmd {
			t.Fatalf("Find(%v) unexpectedly found %q", args, found.CommandPath())
		}
	}
}

func TestChangesLimitZeroSendsExplicitLimit(t *testing.T) {
	temp, err := os.MkdirTemp("", "h")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(temp) })
	t.Setenv("XDG_STATE_HOME", filepath.Join(temp, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(temp, "run"))
	repoRoot := filepath.Join(temp, "repo")
	if err := os.MkdirAll(repoRoot, 0755); err != nil {
		t.Fatal(err)
	}
	paths := computeSessionPaths(repoRoot, "test-session")
	if err := os.MkdirAll(paths.RuntimeDir, 0755); err != nil {
		t.Fatal(err)
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", paths.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	seen := make(chan string, 1)
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/changes" {
			http.NotFound(w, r)
			return
		}
		seen <- r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		writeTestJSON(t, w, client.ChangesResponse{Changes: []client.ObservedFileChange{}})
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Shutdown(context.Background())

	var stdout, stderr bytes.Buffer
	cmd := newRootCommand(cliOptions{
		repoRoot:  repoRoot,
		sessionID: "test-session",
		noStart:   true,
		stdout:    &stdout,
		stderr:    &stderr,
	})
	cmd.SetArgs([]string{"changes", "--limit", "0"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("changes --limit 0: %v\nstderr: %s", err, stderr.String())
	}
	select {
	case got := <-seen:
		if got != "limit=0" {
			t.Fatalf("expected explicit limit=0 query, got %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for changes request")
	}
}

func TestWriteHooksTable(t *testing.T) {
	a := &app{opts: cliOptions{output: "table"}}
	cmd, out := testOutputCommand()
	hooksList := []client.HookStatus{{
		Hook:      hooks.Hook{ID: "lint", Name: "Lint", Type: hooks.HookTypeFile, Engine: hooks.HookEngineScript, Pattern: "**/*.go"},
		Status:    "success",
		RunCount:  2,
		FailCount: 1,
	}}
	if err := a.writeHooks(cmd, hooksList); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"ID", "NAME", "TYPE", "ENGINE", "STATUS", "lint", "Lint", "**/*.go"} {
		if !strings.Contains(got, want) {
			t.Fatalf("table output missing %q: %s", want, got)
		}
	}
}

func TestWriteHooksJSON(t *testing.T) {
	a := &app{opts: cliOptions{output: "json"}}
	cmd, out := testOutputCommand()
	if err := a.writeHooks(cmd, []client.HookStatus{{Hook: hooks.Hook{ID: "lint"}}}); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Hooks []client.HookStatus `json:"hooks"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, out.String())
	}
	if len(decoded.Hooks) != 1 || decoded.Hooks[0].Hook.ID != "lint" {
		t.Fatalf("unexpected json: %#v", decoded)
	}
}

func TestFilterRunTargetsSelectsPhaseScope(t *testing.T) {
	statuses := []client.HookStatus{
		{Hook: hooks.Hook{ID: "lint"}, Status: models.StatusQueued},
		{Hook: hooks.Hook{ID: "review", Phase: "review"}, Status: models.StatusQueued},
		{Hook: hooks.Hook{ID: "deploy", Phase: "deploy"}, Status: models.StatusQueued},
		{Hook: hooks.Hook{ID: "success"}, Status: models.StatusSuccess},
		{Hook: hooks.Hook{ID: "failed"}, Status: models.StatusFailure},
	}
	got := filterRunTargets(statuses, runTargetOptions{})
	if strings.Join(got, ",") != "lint,success,failed" {
		t.Fatalf("unexpected unphased targets: %v", got)
	}

	got = filterRunTargets(statuses, runTargetOptions{Phases: []string{"review"}})
	if strings.Join(got, ",") != "review" {
		t.Fatalf("unexpected review targets: %v", got)
	}

	got = filterRunTargets(statuses, runTargetOptions{Phases: []string{"review", "deploy"}})
	if strings.Join(got, ",") != "review,deploy" {
		t.Fatalf("unexpected multi-phase targets: %v", got)
	}

	got = filterRunTargets(statuses, runTargetOptions{AllPhases: true})
	if strings.Join(got, ",") != "lint,review,deploy,success,failed" {
		t.Fatalf("unexpected all-phase targets: %v", got)
	}
}

func TestNormalizePhaseSelector(t *testing.T) {
	phases, all := normalizePhaseSelector([]string{" Review ", "deploy", "review", ""})
	if all || strings.Join(phases, ",") != "review,deploy" {
		t.Fatalf("unexpected selector: phases=%v all=%t", phases, all)
	}
	phases, all = normalizePhaseSelector([]string{"ALL", "review"})
	if !all || strings.Join(phases, ",") != "review" {
		t.Fatalf("unexpected all selector: phases=%v all=%t", phases, all)
	}
}

func TestSplitRunArgs(t *testing.T) {
	ids, all := splitRunArgs([]string{"lint", "All", "lint", "review"})
	if !all || strings.Join(ids, ",") != "lint,review" {
		t.Fatalf("unexpected split: ids=%v all=%t", ids, all)
	}
	ids, all = splitRunArgs(nil)
	if all || len(ids) != 0 {
		t.Fatalf("unexpected empty split: ids=%v all=%t", ids, all)
	}
}

func TestWriteStatusTable(t *testing.T) {
	a := &app{opts: cliOptions{output: "table"}}
	cmd, out := testOutputCommand()
	if err := a.writeStatus(cmd, &client.StatusResponse{SessionID: "s1", RepoRoot: "/repo", Queued: 3, Hooks: []client.HookStatus{{}}}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"SESSION", "REPO", "QUEUED", "s1", "/repo", "3"} {
		if !strings.Contains(got, want) {
			t.Fatalf("status table missing %q: %s", want, got)
		}
	}
}

func TestWriteDatabaseInspectionTables(t *testing.T) {
	a := &app{opts: cliOptions{output: "table"}}
	for name, tt := range map[string]struct {
		want string
		fn   func(*cobra.Command) error
	}{
		"runs": {want: "STATUS", fn: func(cmd *cobra.Command) error {
			return a.writeRuns(cmd, []client.Run{{ID: "run-1", HookID: "lint", Status: models.StatusFailure, ExitCode: 1, Error: "boom"}})
		},
		},
		"changes": {want: "DIFF_BYTES", fn: func(cmd *cobra.Command) error {
			return a.writeObservedChanges(cmd, []client.ObservedFileChange{{ID: "change-1", Path: "main.go", Kind: "modified", Diff: "diff"}})
		},
		},
		"snapshots": {want: "PATCH_BYTES", fn: func(cmd *cobra.Command) error {
			return a.writeSnapshots(cmd, []client.WorkspaceSnapshot{{ID: "snapshot-1", ParentID: "parent-1", BaseCommit: "0123456789abcdef", TreeHash: "tree-1", PatchBytes: 42, ChangedFiles: []client.ChangedFile{{Path: "main.go"}}}})
		},
		},
		"queue": {want: "POSITION", fn: func(cmd *cobra.Command) error {
			return a.writeQueue(cmd, []client.QueuedHook{{HookID: "lint", Position: 7, ChangedFiles: []client.ChangedFile{{Path: "main.go"}}}})
		},
		},
	} {
		cmd, out := testOutputCommand()
		if err := tt.fn(cmd); err != nil {
			t.Fatalf("%s writer: %v", name, err)
		}
		if got := out.String(); !strings.Contains(got, tt.want) {
			t.Fatalf("%s table did not include header %q: %s", name, tt.want, got)
		} else if name == "snapshots" {
			for _, notWant := range []string{"PARENT", "TREE", "parent-1", "tree-1", "0123456789abcdef"} {
				if strings.Contains(got, notWant) {
					t.Fatalf("snapshot table included %q unexpectedly: %s", notWant, got)
				}
			}
			if !strings.Contains(got, "0123456789ab") {
				t.Fatalf("snapshot table missing short base commit: %s", got)
			}
		}
	}
}

func TestTableWritersSortByCreatedAtAscending(t *testing.T) {
	a := &app{opts: cliOptions{output: "table"}}
	base := time.Date(2026, 6, 17, 4, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		first string
		last  string
		fn    func(*cobra.Command) error
	}{
		{
			name:  "events",
			first: "event-old",
			last:  "event-new",
			fn: func(cmd *cobra.Command) error {
				return a.writeEvents(cmd, []client.Event{
					{ID: "event-new", CreatedAt: base.Add(time.Minute), Type: "hook.run.finished"},
					{ID: "event-old", CreatedAt: base, Type: "hook.run.started"},
				})
			},
		},
		{
			name:  "runs",
			first: "run-old",
			last:  "run-new",
			fn: func(cmd *cobra.Command) error {
				return a.writeRuns(cmd, []client.Run{
					{ID: "run-new", StartedAt: base.Add(time.Minute)},
					{ID: "run-old", StartedAt: base},
				})
			},
		},
		{
			name:  "changes",
			first: "change-old",
			last:  "change-new",
			fn: func(cmd *cobra.Command) error {
				return a.writeObservedChanges(cmd, []client.ObservedFileChange{
					{ID: "change-new", CreatedAt: base.Add(time.Minute), Path: "new.go"},
					{ID: "change-old", CreatedAt: base, Path: "old.go"},
				})
			},
		},
		{
			name:  "snapshots",
			first: "shot-old",
			last:  "shot-new",
			fn: func(cmd *cobra.Command) error {
				return a.writeSnapshots(cmd, []client.WorkspaceSnapshot{
					{ID: "snapshot-new", CreatedAt: base.Add(time.Minute)},
					{ID: "snapshot-old", CreatedAt: base},
				})
			},
		},
		{
			name:  "queue",
			first: "hook-old",
			last:  "hook-new",
			fn: func(cmd *cobra.Command) error {
				return a.writeQueue(cmd, []client.QueuedHook{
					{HookID: "hook-new", CreatedAt: base.Add(time.Minute), Position: 2},
					{HookID: "hook-old", CreatedAt: base, Position: 1},
				})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, out := testOutputCommand()
			if err := tt.fn(cmd); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			firstIndex := strings.Index(got, tt.first)
			lastIndex := strings.Index(got, tt.last)
			if firstIndex < 0 || lastIndex < 0 || firstIndex > lastIndex {
				t.Fatalf("expected %q before %q in table:\n%s", tt.first, tt.last, got)
			}
		})
	}
}

func TestGitDiffColorArgHonorsEnvironment(t *testing.T) {
	var out bytes.Buffer
	t.Setenv("CLICOLOR_FORCE", "1")
	t.Setenv("NO_COLOR", "")
	if got := gitDiffColorArg(&out, true); got != "--color=never" {
		t.Fatalf("NO_COLOR should disable color, got %q", got)
	}

	t.Setenv("NO_COLOR", "")
	_ = os.Unsetenv("NO_COLOR")
	t.Setenv("CLICOLOR", "0")
	if got := gitDiffColorArg(&out, true); got != "--color=never" {
		t.Fatalf("CLICOLOR=0 should disable color, got %q", got)
	}

	t.Setenv("CLICOLOR", "")
	_ = os.Unsetenv("CLICOLOR")
	t.Setenv("CLICOLOR_FORCE", "1")
	if got := gitDiffColorArg(&out, true); got != "--color=always" {
		t.Fatalf("CLICOLOR_FORCE should force color, got %q", got)
	}
	if got := gitDiffColorArg(&out, false); got != "--color=never" {
		t.Fatalf("disabled color flag should win for JSON output, got %q", got)
	}
}

func TestSnapshotApplyAndResetUseUnstagedWorkspaceChanges(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	runTestGit(t, repo, "init")
	runTestGit(t, repo, "config", "user.email", "test@example.com")
	runTestGit(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repo, "add", "tracked.txt")
	runTestGit(t, repo, "commit", "-m", "base")
	baseCommit := strings.TrimSpace(runTestGit(t, repo, "rev-parse", "HEAD"))

	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("snapshot\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshotTree, cleanup, err := gitutil.CurrentWorkspaceTree(ctx, repo)
	if err != nil {
		t.Fatalf("snapshot tree: %v", err)
	}
	cleanup()
	snapshotPatch := runTestGit(t, repo, "diff", "--binary", baseCommit, snapshotTree.Tree)
	runTestGit(t, repo, "restore", "--worktree", "tracked.txt")
	if err := os.Remove(filepath.Join(repo, "new.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("current\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	snapshot := client.WorkspaceSnapshot{ID: "snapshot-1", BaseCommit: baseCommit, TreeHash: snapshotTree.Tree, Patch: snapshotPatch}
	diff, err := snapshotDiff(ctx, repo, snapshot, "--color=never")
	if err != nil {
		t.Fatalf("snapshot diff: %v", err)
	}
	if !strings.Contains(diff, "-base") || !strings.Contains(diff, "+snapshot") || strings.Contains(diff, "current") {
		t.Fatalf("snapshot diff should compare base commit to snapshot, got:\n%s", diff)
	}
	changed, err := applySnapshotDiff(ctx, repo, snapshot)
	if err != nil {
		t.Fatalf("apply snapshot: %v", err)
	}
	if !changed {
		t.Fatal("expected snapshot apply to change workspace")
	}
	if got := string(mustReadFile(t, filepath.Join(repo, "tracked.txt"))); got != "snapshot\n" {
		t.Fatalf("tracked content after apply = %q", got)
	}
	if got := string(mustReadFile(t, filepath.Join(repo, "new.txt"))); got != "new\n" {
		t.Fatalf("new file after apply = %q", got)
	}
	if status := runTestGit(t, repo, "status", "--porcelain"); !strings.Contains(status, " M tracked.txt") || !strings.Contains(status, "?? new.txt") {
		t.Fatalf("snapshot apply should leave unstaged changes, status:\n%s", status)
	}

	changed, err = resetSnapshotDiff(ctx, repo, snapshot)
	if err != nil {
		t.Fatalf("reset snapshot: %v", err)
	}
	if !changed {
		t.Fatal("expected snapshot reset to change workspace")
	}
	if got := string(mustReadFile(t, filepath.Join(repo, "tracked.txt"))); got != "base\n" {
		t.Fatalf("tracked content after reset = %q", got)
	}
	if _, err := os.Stat(filepath.Join(repo, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("new file should be removed after reset, err=%v", err)
	}
	if status := strings.TrimSpace(runTestGit(t, repo, "status", "--porcelain")); status != "" {
		t.Fatalf("snapshot reset should restore clean worktree, status:\n%s", status)
	}
}

func TestSnapshotRangeDiffSupportsSnapshotPairs(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	runTestGit(t, repo, "init")
	runTestGit(t, repo, "config", "user.email", "test@example.com")
	runTestGit(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repo, "add", "tracked.txt")
	runTestGit(t, repo, "commit", "-m", "base")
	baseCommit := strings.TrimSpace(runTestGit(t, repo, "rev-parse", "HEAD"))

	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	firstTree, firstCleanup, err := gitutil.CurrentWorkspaceTree(ctx, repo)
	if err != nil {
		t.Fatalf("first tree: %v", err)
	}
	firstCleanup()
	firstPatch := runTestGit(t, repo, "diff", "--binary", baseCommit, firstTree.Tree)

	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	secondTree, secondCleanup, err := gitutil.CurrentWorkspaceTree(ctx, repo)
	if err != nil {
		t.Fatalf("second tree: %v", err)
	}
	secondCleanup()
	secondPatch := runTestGit(t, repo, "diff", "--binary", baseCommit, secondTree.Tree)

	first := client.WorkspaceSnapshot{ID: "snapshot-first", BaseCommit: baseCommit, TreeHash: firstTree.Tree, Patch: firstPatch}
	second := client.WorkspaceSnapshot{ID: "snapshot-second", ParentID: first.ID, BaseCommit: baseCommit, TreeHash: secondTree.Tree, Patch: secondPatch}
	diff, err := snapshotRangeDiff(ctx, repo, snapshotDiffRange{From: &first, To: second}, "--color=never")
	if err != nil {
		t.Fatalf("snapshot pair diff: %v", err)
	}
	if !strings.Contains(diff, "-first") || !strings.Contains(diff, "+second") {
		t.Fatalf("pair diff did not compare first to second:\n%s", diff)
	}
}

func TestLatestSnapshotSelectsNewestCreatedAt(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	snapshots := []client.WorkspaceSnapshot{
		{ID: "newer-b", CreatedAt: base.Add(time.Minute)},
		{ID: "older", CreatedAt: base},
		{ID: "newer-a", CreatedAt: base.Add(time.Minute)},
	}
	if got := latestSnapshot(snapshots); got.ID != "newer-b" {
		t.Fatalf("latest snapshot = %s, want newer-b", got.ID)
	}
}

func TestWriteCheckResultReportsNonSuccessfulOutputs(t *testing.T) {
	var out bytes.Buffer
	resp := &client.WaitResponse{Settled: true}
	outputs := []checkHookOutput{{
		HookID:      "lint",
		Name:        "Lint",
		Description: "Checks Go formatting and lint issues.",
		Type:        "file",
		Pattern:     "**/*.go",
		Path:        ".discobox/hooks/lint.sh",
		Status:      models.StatusFailure,
		Output:      "lint failed\n",
	}}
	if err := writeCheckResult(&out, resp, outputs); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"=== FAILED HOOK lint ===", "--- metadata ---", "name: Lint", "description: Checks Go formatting", "type: file", "pattern: **/*.go", "hook_file: .discobox/hooks/lint.sh", "--- output ---", "lint failed", "--- end output ---", "=== END FAILED HOOK lint ==="} {
		if !strings.Contains(got, want) {
			t.Fatalf("check result missing %q: %s", want, got)
		}
	}
}

func TestWriteCheckProgressReportsPendingWork(t *testing.T) {
	var out bytes.Buffer
	resp := &client.StatusResponse{
		Queued: 2,
		Paused: true,
		Hooks: []client.HookStatus{
			{Hook: hooks.Hook{ID: "z-queued"}, Status: models.StatusQueued},
			{Hook: hooks.Hook{ID: "running"}, Status: models.StatusRunning},
			{Hook: hooks.Hook{ID: "a-queued"}, Status: models.StatusQueued},
		},
	}
	writeCheckProgress(&out, resp)
	got := out.String()
	for _, want := range []string{"waiting for hooks:", "running=running", "queued=a-queued,z-queued", "paused=true"} {
		if !strings.Contains(got, want) {
			t.Fatalf("check progress missing %q: %s", want, got)
		}
	}
}

func TestRetryableCheckWaitErrorIncludesDaemonDisconnects(t *testing.T) {
	for _, err := range []error{
		io.EOF,
		io.ErrUnexpectedEOF,
		errors.Join(client.ErrNotRunning, io.EOF),
		errors.New("net/http: HTTP/1.x transport connection broken: unexpected EOF"),
		errors.New("net/http: HTTP/1.x transport connection broken: malformed HTTP response"),
		errors.New("stream reset: protocol error"),
	} {
		if !isRetryableCheckWaitError(err) {
			t.Fatalf("isRetryableCheckWaitError(%v) = false, want true", err)
		}
	}
	if isRetryableCheckWaitError(errors.New("daemon returned 500: hook failed")) {
		t.Fatal("non-disconnect error should not be retryable")
	}
}

func TestHookIDsWithStatusUsesDashForEmpty(t *testing.T) {
	got := hookIDsWithStatus([]client.HookStatus{{Hook: hooks.Hook{ID: "lint"}, Status: models.StatusSuccess}}, models.StatusRunning)
	if got != "-" {
		t.Fatalf("hook ids = %q, want dash", got)
	}
}

func TestCheckProgressFallsBackToAggregateState(t *testing.T) {
	resp := &client.StatusResponse{Running: true, Queued: 3}
	if got := checkRunningSummary(resp); got != "yes" {
		t.Fatalf("running summary = %q, want yes", got)
	}
	if got := checkQueuedSummary(resp); got != "3" {
		t.Fatalf("queued summary = %q, want queued count", got)
	}
}

func TestFailedHooksIgnoresIdleSuccessAndQueued(t *testing.T) {
	hooksList := []client.HookStatus{
		{Hook: hooks.Hook{ID: "idle"}, Status: models.StatusIdle},
		{Hook: hooks.Hook{ID: "success"}, Status: models.StatusSuccess},
		{Hook: hooks.Hook{ID: "failure"}, Status: models.StatusFailure},
		{Hook: hooks.Hook{ID: "queued"}, Status: models.StatusQueued},
	}
	got := failedHooks(hooksList)
	if len(got) != 1 || got[0].Hook.ID != "failure" {
		t.Fatalf("unexpected failed hooks: %#v", got)
	}
}

func TestWriteRunsJSON(t *testing.T) {
	a := &app{opts: cliOptions{output: "json"}}
	cmd, out := testOutputCommand()
	if err := a.writeRuns(cmd, []client.Run{{ID: "run-1", HookID: "lint"}}); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Runs []client.Run `json:"runs"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, out.String())
	}
	if len(decoded.Runs) != 1 || decoded.Runs[0].ID != "run-1" {
		t.Fatalf("unexpected runs json: %#v", decoded)
	}
}

func TestWriteEventsTable(t *testing.T) {
	a := &app{opts: cliOptions{output: "table"}}
	cmd, out := testOutputCommand()
	events := []client.Event{{ID: "event-1", Type: "hook.run.finished", HookID: "lint", RunID: "run-7", Message: "hook run finished"}}
	if err := a.writeEvents(cmd, events); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"ID", "TIME", "TYPE", "HOOK", "RUN", "MESSAGE", "hook.run.finished", "lint", "run-7"} {
		if !strings.Contains(got, want) {
			t.Fatalf("events table missing %q: %s", want, got)
		}
	}
}

func TestLiveEventTableRowUsesAbsoluteTimestamp(t *testing.T) {
	var out bytes.Buffer
	tw := tabwriter.NewWriter(&out, 0, 0, 2, ' ', 0)
	event := client.Event{
		ID:        "event-1",
		CreatedAt: time.Date(2026, 6, 20, 12, 34, 56, 0, time.FixedZone("MST", -7*60*60)),
		Type:      "hook.run.finished",
		HookID:    "lint",
		RunID:     "run-7",
		Message:   "hook run finished",
	}
	writeEventTableRow(tw, event, formatLiveEventTime)
	if err := tw.Flush(); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "2026-06-20T19:34:56Z") {
		t.Fatalf("live event row missing absolute timestamp: %s", got)
	}
	if strings.Contains(got, "ago") {
		t.Fatalf("live event row used relative timestamp: %s", got)
	}
}

func TestEventMessageAppendsErrorDetail(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event client.Event
		want  string
	}{
		{
			name: "appends error detail",
			event: client.Event{
				Message: "watch snapshot persist failed",
				Details: map[string]any{"files": 3, "error": "write checkpoint: disk full"},
			},
			want: "watch snapshot persist failed: write checkpoint: disk full",
		},
		{
			name: "collapses multiline error",
			event: client.Event{
				Message: "language server failed",
				Details: map[string]any{"error": "exit status 1\nmissing binary"},
			},
			want: "language server failed: exit status 1 missing binary",
		},
		{
			name: "keeps message when error already included",
			event: client.Event{
				Message: "language server file update failed: modified main.go: boom",
				Details: map[string]any{"error": "boom"},
			},
			want: "language server file update failed: modified main.go: boom",
		},
		{
			name: "keeps message when error detail is empty",
			event: client.Event{
				Message: "hook run finished",
				Details: map[string]any{"error": "", "exit_code": 0},
			},
			want: "hook run finished",
		},
		{
			name:  "no details",
			event: client.Event{Message: "hook enqueued from file changes"},
			want:  "hook enqueued from file changes",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := eventMessage(tc.event); got != tc.want {
				t.Fatalf("eventMessage = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWriteEventsJSONUsesJSONL(t *testing.T) {
	a := &app{opts: cliOptions{output: "json"}}
	cmd, out := testOutputCommand()
	events := []client.Event{
		{ID: "event-1", Type: "hook.run.started", HookID: "lint"},
		{ID: "event-2", Type: "hook.run.finished", HookID: "lint"},
	}
	if err := a.writeEvents(cmd, events); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected one JSON object per line, got %d lines: %q", len(lines), out.String())
	}
	for i, line := range lines {
		var decoded client.Event
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("line %d is invalid json: %v\n%s", i, err, line)
		}
		if decoded.ID != events[i].ID || decoded.Type != events[i].Type {
			t.Fatalf("unexpected event on line %d: %#v", i, decoded)
		}
	}
	var envelope struct {
		Events []client.Event `json:"events"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err == nil {
		t.Fatalf("expected JSONL, not a JSON event envelope: %#v", envelope)
	}
}

func TestWriteJSONLineIsCompact(t *testing.T) {
	var out bytes.Buffer
	event := client.Event{ID: "event-1", Type: "hook.run.finished", HookID: "lint"}
	if err := writeJSONLine(&out, event); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected compact single-line json, got %d lines: %q", len(lines), out.String())
	}
	var decoded client.Event
	if err := json.Unmarshal([]byte(lines[0]), &decoded); err != nil {
		t.Fatalf("invalid json line: %v\n%s", err, lines[0])
	}
	if decoded.ID != event.ID || decoded.Type != event.Type || decoded.HookID != event.HookID {
		t.Fatalf("unexpected decoded event: %#v", decoded)
	}
}

func TestFormatRelativeTime(t *testing.T) {
	now := time.Date(2026, 6, 17, 4, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value time.Time
		want  string
	}{
		{name: "second", value: now.Add(-1 * time.Second), want: "1 second ago"},
		{name: "seconds", value: now.Add(-12 * time.Second), want: "12 seconds ago"},
		{name: "minute", value: now.Add(-1 * time.Minute), want: "1 minute ago"},
		{name: "minutes", value: now.Add(-12 * time.Minute), want: "12 minutes ago"},
		{name: "hour", value: now.Add(-1 * time.Hour), want: "1 hour ago"},
		{name: "hours", value: now.Add(-12 * time.Hour), want: "12 hours ago"},
		{name: "day", value: now.Add(-24 * time.Hour), want: "1 day ago"},
		{name: "future", value: now.Add(2 * time.Minute), want: "2 minutes from now"},
		{name: "zero", value: time.Time{}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatRelativeTime(now, tt.value); got != tt.want {
				t.Fatalf("formatRelativeTime() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEventsListTypesTableDoesNotRequireDaemon(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newRootCommand(cliOptions{stdout: &stdout, stderr: &stderr, noStart: true})
	cmd.SetArgs([]string{"events", "--list-types"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("events --list-types: %v\nstderr=%s", err, stderr.String())
	}
	got := stdout.String()
	for _, want := range []string{"TYPE", "DESCRIPTION", "DETAILS", "daemon.started", "hook.run.finished", "file.change.observed", "daemon_session_id*"} {
		if !strings.Contains(got, want) {
			t.Fatalf("event type table missing %q: %s", want, got)
		}
	}
}

func TestEventsListTypesJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newRootCommand(cliOptions{stdout: &stdout, stderr: &stderr, output: "json"})
	cmd.SetArgs([]string{"events", "--list-types"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("events --list-types json: %v\nstderr=%s", err, stderr.String())
	}
	var decoded struct {
		EventTypes []hookapi.EventTypeInfo `json:"event_types"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, stdout.String())
	}
	if len(decoded.EventTypes) == 0 {
		t.Fatal("expected event types")
	}
	if decoded.EventTypes[0].Type == "" || decoded.EventTypes[0].Description == "" || len(decoded.EventTypes[0].Details) == 0 {
		t.Fatalf("expected type descriptions, got %#v", decoded.EventTypes[0])
	}
}

func TestEventsListTypesRejectsFollowAndHookIDs(t *testing.T) {
	for _, args := range [][]string{
		{"events", "--list-types", "--follow"},
		{"events", "--list-types", "lint"},
	} {
		var stdout, stderr bytes.Buffer
		cmd := newRootCommand(cliOptions{stdout: &stdout, stderr: &stderr})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("expected error for args %v", args)
		}
	}
}

func TestKnownEventTypesDocumentProductionEvents(t *testing.T) {
	known := map[string]bool{}
	for _, eventType := range hookapi.KnownEventTypes() {
		if eventType.Description == "" {
			t.Fatalf("event type %q has no description", eventType.Type)
		}
		if len(eventType.Details) == 0 {
			t.Fatalf("event type %q has no detail schema", eventType.Type)
		}
		for _, detail := range eventType.Details {
			if detail.Name == "" || detail.Description == "" {
				t.Fatalf("event type %q has incomplete detail schema: %#v", eventType.Type, detail)
			}
		}
		known[eventType.Type] = true
	}

	events := productionEventLiterals(t)
	for _, eventType := range events {
		if !known[eventType] {
			t.Fatalf("production event type %q is not documented in knownEventTypes", eventType)
		}
	}
}

func productionEventLiterals(t *testing.T) []string {
	t.Helper()
	root := hooksModuleRoot(t)
	recordEventRE := regexp.MustCompile(`\.recordEvent\(\s*"([a-z][a-z0-9]*(?:\.[a-z0-9]+)+)"`)
	managerRecordEventRE := regexp.MustCompile(`\.RecordEvent\([^,\n]+,\s*"([a-z][a-z0-9]*(?:\.[a-z0-9]+)+)"`)
	eventTypeRE := regexp.MustCompile(`Type:\s*"([a-z][a-z0-9]*(?:\.[a-z0-9]+)+)"`)
	events := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "gen" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || filepath.ToSlash(path) == filepath.ToSlash(filepath.Join(root, "api", "event_types.go")) {
			return nil
		}
		//nolint:gosec // Test scans repository source files discovered by WalkDir, not user-controlled paths.
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, match := range recordEventRE.FindAllSubmatch(content, -1) {
			events[string(match[1])] = true
		}
		for _, match := range managerRecordEventRE.FindAllSubmatch(content, -1) {
			events[string(match[1])] = true
		}
		for _, match := range eventTypeRE.FindAllSubmatch(content, -1) {
			events[string(match[1])] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan production events: %v", err)
	}
	return sortedKeys(events)
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func hooksModuleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			data, err := os.ReadFile(filepath.Join(wd, "go.mod"))
			if err != nil {
				t.Fatalf("read go.mod: %v", err)
			}
			if strings.Contains(string(data), "module github.com/discobox-ai/discobox/hooks") {
				return wd
			}
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			t.Fatal("could not find hooks module root")
		}
		wd = parent
	}
}

func testOutputCommand() (*cobra.Command, *bytes.Buffer) {
	out := &bytes.Buffer{}
	cmd := &cobra.Command{Use: "test"}
	cmd.SetOut(out)
	return cmd, out
}

func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, string(out))
	}
	return string(out)
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestWriteHookOutputsSingle(t *testing.T) {
	var out bytes.Buffer
	if err := writeHookOutputs(&out, []hookOutput{{HookID: "lint", Output: "ok"}}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "ok\n" {
		t.Fatalf("single output = %q", got)
	}
}

func TestWriteHookOutputsMultiple(t *testing.T) {
	var out bytes.Buffer
	if err := writeHookOutputs(&out, []hookOutput{{HookID: "lint", Output: "ok\n"}, {HookID: "test", Output: "pass"}}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"==> lint <==", "ok", "==> test <==", "pass"} {
		if !strings.Contains(got, want) {
			t.Fatalf("multi output missing %q: %s", want, got)
		}
	}
}

func TestUniqueStrings(t *testing.T) {
	got := uniqueStrings([]string{"lint", "", "test", "lint", " test "})
	want := []string{"lint", "test"}
	if len(got) != len(want) {
		t.Fatalf("uniqueStrings length = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("uniqueStrings[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFollowEventHookID(t *testing.T) {
	hookID, err := followEventHookID(nil)
	if err != nil || hookID != "" {
		t.Fatalf("all follow target = %q err=%v", hookID, err)
	}
	hookID, err = followEventHookID([]string{"lint"})
	if err != nil || hookID != "lint" {
		t.Fatalf("single follow target = %q err=%v", hookID, err)
	}
	if _, err := followEventHookID([]string{"lint", "test"}); err == nil {
		t.Fatal("expected error for multiple follow hook ids")
	}
	cmd := newRootCommand(cliOptions{})
	eventsCmd, _, err := cmd.Find([]string{"events"})
	if err != nil {
		t.Fatal(err)
	}
	if flag := eventsCmd.Flags().Lookup("follow"); flag == nil || flag.Shorthand != "f" {
		t.Fatalf("expected events -f/--follow flag, got %#v", flag)
	}
	if flag := eventsCmd.Flags().Lookup("list-types"); flag == nil {
		t.Fatalf("expected events --list-types flag")
	}
}

func TestComputeSessionPaths(t *testing.T) {
	stateHome := t.TempDir()
	runtimeHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeHome)

	repoRoot := filepath.Clean("/tmp/repo/")
	paths := computeSessionPaths(repoRoot, "s/1")
	repoKey := repoStateKey(repoRoot)
	wantState := filepath.Join(stateHome, "discobox", "session", "s-1", "hooks", repoKey)
	wantRuntime := filepath.Join(runtimeHome, "discobox", "session", "s-1", "hooks", repoKey)
	if paths.StateDir != wantState {
		t.Fatalf("StateDir = %q, want %q", paths.StateDir, wantState)
	}
	if paths.RuntimeDir != wantRuntime {
		t.Fatalf("RuntimeDir = %q, want %q", paths.RuntimeDir, wantRuntime)
	}
	checks := map[string]string{
		"Socket":  filepath.Join(wantRuntime, "daemon.sock"),
		"Lock":    filepath.Join(wantRuntime, "startup.lock"),
		"DB":      filepath.Join(wantState, "hooks.db"),
		"Runtime": filepath.Join(wantRuntime, "runtime.json"),
	}
	if paths.Socket != checks["Socket"] || paths.Lock != checks["Lock"] || paths.DB != checks["DB"] || paths.Runtime != checks["Runtime"] {
		t.Fatalf("unexpected paths: %#v", paths)
	}
}

func TestXDGPathFallbacks(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	if got := stateHome(); got == "" || got != defaultStateHome() {
		t.Fatalf("state home fallback = %q, want the platform's own %q", got, defaultStateHome())
	}
	if got := runtimeHome(); got == "" || filepath.Base(got) != "run" {
		t.Fatalf("runtime home fallback = %q", got)
	}
}

func TestResolveSessionID(t *testing.T) {
	old, had := os.LookupEnv("DISCOBOX_SESSION_ID")
	defer func() {
		if had {
			_ = os.Setenv("DISCOBOX_SESSION_ID", old)
		} else {
			_ = os.Unsetenv("DISCOBOX_SESSION_ID")
		}
	}()
	repoRoot := filepath.Clean("/tmp/repo")
	_ = os.Unsetenv("DISCOBOX_SESSION_ID")
	if got, want := resolveSessionID("", repoRoot), repoStateKey(repoRoot); got != want {
		t.Fatalf("default session = %q, want repo hash %q", got, want)
	}
	_ = os.Setenv("DISCOBOX_SESSION_ID", "env-session")
	if got := resolveSessionID("", repoRoot); got != "env-session" {
		t.Fatalf("env session = %q", got)
	}
	if got := resolveSessionID("flag-session", repoRoot); got != "flag-session" {
		t.Fatalf("explicit session = %q", got)
	}
}

func TestClientNewerThanDaemon(t *testing.T) {
	tests := []struct {
		name          string
		clientVersion int64
		daemonVersion int64
		want          bool
	}{
		{name: "client newer", clientVersion: 2, daemonVersion: 1, want: true},
		{name: "same version", clientVersion: 2, daemonVersion: 2},
		{name: "daemon newer", clientVersion: 1, daemonVersion: 2},
		{name: "zero daemon version", clientVersion: 1, daemonVersion: 0, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clientNewerThanDaemon(tt.clientVersion, tt.daemonVersion); got != tt.want {
				t.Fatalf("clientNewerThanDaemon(%d, %d) = %t, want %t", tt.clientVersion, tt.daemonVersion, got, tt.want)
			}
		})
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{in: "123", want: 123, ok: true},
		{in: " 456 ", want: 456, ok: true},
		{in: ""},
		{in: "abc"},
		{in: "0"},
		{in: "-1"},
	}
	for _, tt := range tests {
		got, ok := parseVersion(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Fatalf("parseVersion(%q) = (%d, %t), want (%d, %t)", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}
