package store

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	hooks "github.com/obot-platform/discobox/hooks"
	"github.com/obot-platform/discobox/hooks/models"
	"github.com/obot-platform/discobox/hooks/watcher"
)

func TestOpenMigratesExpectedTables(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)

	for _, table := range []string{"hook_definitions", "hook_statuses", "hook_runs", "pending_hooks", "daemon_states", "daemon_sessions", "hook_events", "hook_logs", "observed_file_changes", "workspace_snapshots", "watched_files"} {
		if !s.DB().Migrator().HasTable(table) {
			t.Fatalf("expected migrated table %s", table)
		}
	}
}

func TestWorkspaceSnapshotRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)

	first, err := s.RecordWorkspaceSnapshot(ctx, WorkspaceSnapshot{
		BaseCommit:   "base",
		TreeHash:     "tree-1",
		Patch:        []byte("diff --git a/file.txt b/file.txt\n"),
		ChangedFiles: []models.ChangedFile{{Path: "file.txt", Kind: watcher.Modified}},
		OmittedFiles: []SnapshotOmission{{Path: "big.bin", Kind: watcher.Created, Reason: "too_large", SizeBytes: 2 << 20, LimitBytes: 1 << 20}},
		MaxFileBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("record first snapshot: %v", err)
	}
	if first.ID == "" || first.PatchBytes == 0 || len(first.ChangedFiles) != 1 || len(first.OmittedFiles) != 1 {
		t.Fatalf("unexpected first snapshot: %#v", first)
	}

	second, err := s.RecordWorkspaceSnapshot(ctx, WorkspaceSnapshot{
		ParentID:     first.ID,
		BaseCommit:   "base",
		TreeHash:     "tree-2",
		Patch:        []byte("diff --git a/other.txt b/other.txt\n"),
		ChangedFiles: []models.ChangedFile{{Path: "other.txt", Kind: watcher.Created}},
		MaxFileBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("record second snapshot: %v", err)
	}
	latest, err := s.LatestWorkspaceSnapshot(ctx)
	if err != nil {
		t.Fatalf("latest snapshot: %v", err)
	}
	if latest == nil || latest.ID != second.ID || latest.ParentID != first.ID || latest.TreeHash != "tree-2" {
		t.Fatalf("unexpected latest snapshot: %#v", latest)
	}
	list, err := s.ListWorkspaceSnapshots(ctx, 10)
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	if len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Fatalf("unexpected snapshot list: %#v", list)
	}
}

func TestWatchedSnapshotRoundTripAndReplace(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)
	modTime := time.Unix(123, 456).UTC()

	if err := s.ReplaceWatchedSnapshot(ctx, map[string]watcher.Entry{
		"dir":          {Path: "dir", IsDir: true, Mode: fs.ModeDir | 0o755, ModTime: modTime},
		"dir/file.txt": {Path: "dir/file.txt", Size: 42, Mode: 0o644, ModTime: modTime},
	}); err != nil {
		t.Fatalf("replace snapshot: %v", err)
	}
	snapshot, err := s.LoadWatchedSnapshot(ctx)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if len(snapshot) != 2 {
		t.Fatalf("expected 2 snapshot entries, got %#v", snapshot)
	}
	if got := snapshot["dir/file.txt"]; got.Path != "dir/file.txt" || got.Size != 42 || got.Mode != 0o644 || !got.ModTime.Equal(modTime) {
		t.Fatalf("unexpected file snapshot: %#v", got)
	}

	if err := s.ReplaceWatchedSnapshot(ctx, map[string]watcher.Entry{
		"other.txt": {Path: "other.txt", Size: 1, Mode: 0o600, ModTime: modTime.Add(time.Second)},
	}); err != nil {
		t.Fatalf("replace snapshot again: %v", err)
	}
	snapshot, err = s.LoadWatchedSnapshot(ctx)
	if err != nil {
		t.Fatalf("load replaced snapshot: %v", err)
	}
	if len(snapshot) != 1 {
		t.Fatalf("expected 1 replaced snapshot entry, got %#v", snapshot)
	}
	if _, ok := snapshot["dir/file.txt"]; ok {
		t.Fatalf("old snapshot entry was not replaced: %#v", snapshot)
	}
	if got := snapshot["other.txt"]; got.Path != "other.txt" || got.Size != 1 {
		t.Fatalf("unexpected replaced snapshot: %#v", got)
	}
}

func TestUpdateWatchedPathsAppliesDiff(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)
	modTime := time.Unix(123, 456).UTC()

	if err := s.ReplaceWatchedSnapshot(ctx, map[string]watcher.Entry{
		"keep.txt":   {Path: "keep.txt", Size: 1, Mode: 0o644, ModTime: modTime},
		"edit.txt":   {Path: "edit.txt", Size: 2, Mode: 0o644, ModTime: modTime},
		"remove.txt": {Path: "remove.txt", Size: 3, Mode: 0o644, ModTime: modTime},
	}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	// The snapshot is the post-change state: edit.txt grew, remove.txt is gone,
	// and add.txt appeared. keep.txt is untouched and must not be rewritten.
	next := map[string]watcher.Entry{
		"keep.txt": {Path: "keep.txt", Size: 1, Mode: 0o644, ModTime: modTime},
		"edit.txt": {Path: "edit.txt", Size: 20, Mode: 0o600, ModTime: modTime.Add(time.Minute)},
		"add.txt":  {Path: "add.txt", Size: 4, Mode: 0o644, ModTime: modTime},
	}
	if err := s.UpdateWatchedPaths(ctx, next, []string{"edit.txt", "remove.txt", "add.txt"}); err != nil {
		t.Fatalf("update watched paths: %v", err)
	}

	snapshot, err := s.LoadWatchedSnapshot(ctx)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if len(snapshot) != 3 {
		t.Fatalf("expected 3 entries after diff, got %#v", snapshot)
	}
	if _, ok := snapshot["remove.txt"]; ok {
		t.Fatalf("deleted path still present: %#v", snapshot)
	}
	if got := snapshot["edit.txt"]; got.Size != 20 || got.Mode != 0o600 || !got.ModTime.Equal(modTime.Add(time.Minute)) {
		t.Fatalf("modified path not upserted: %#v", got)
	}
	if got := snapshot["add.txt"]; got.Size != 4 {
		t.Fatalf("created path not inserted: %#v", got)
	}
	if got := snapshot["keep.txt"]; got.Size != 1 || !got.ModTime.Equal(modTime) {
		t.Fatalf("untouched path was altered: %#v", got)
	}
}

// A diff whose changed paths exceed the SQLite host-parameter ceiling must be
// chunked on both the delete and the upsert side.
func TestUpdateWatchedPathsExceedingSQLVariableLimit(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)
	modTime := time.Unix(123, 456).UTC()

	const files = 12000
	seed := make(map[string]watcher.Entry, files)
	for i := range files {
		path := fmt.Sprintf("dir/file-%d.txt", i)
		seed[path] = watcher.Entry{Path: path, Size: int64(i), Mode: 0o644, ModTime: modTime}
	}
	if err := s.ReplaceWatchedSnapshot(ctx, seed); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	// Delete the first half and rewrite the second half in one diff.
	next := make(map[string]watcher.Entry, files/2)
	paths := make([]string, 0, files)
	for i := range files {
		path := fmt.Sprintf("dir/file-%d.txt", i)
		paths = append(paths, path)
		if i >= files/2 {
			next[path] = watcher.Entry{Path: path, Size: int64(i) * 2, Mode: 0o644, ModTime: modTime}
		}
	}
	if err := s.UpdateWatchedPaths(ctx, next, paths); err != nil {
		t.Fatalf("update watched paths: %v", err)
	}

	snapshot, err := s.LoadWatchedSnapshot(ctx)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if len(snapshot) != files/2 {
		t.Fatalf("expected %d entries after diff, got %d", files/2, len(snapshot))
	}
	if got := snapshot["dir/file-11999.txt"]; got.Size != 23998 {
		t.Fatalf("upserted entry not updated: %#v", got)
	}
	if _, ok := snapshot["dir/file-0.txt"]; ok {
		t.Fatalf("deleted entry still present")
	}
}

// Large repositories produce watcher snapshots with more entries than SQLite
// allows host parameters in one statement, so the insert must be chunked.
func TestWatchedSnapshotExceedingSQLVariableLimit(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)
	modTime := time.Unix(123, 456).UTC()

	const files = 20000
	snapshot := make(map[string]watcher.Entry, files)
	for i := range files {
		path := fmt.Sprintf("dir/file-%d.txt", i)
		snapshot[path] = watcher.Entry{Path: path, Size: int64(i), Mode: 0o644, ModTime: modTime}
	}
	if err := s.ReplaceWatchedSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("replace large snapshot: %v", err)
	}
	loaded, err := s.LoadWatchedSnapshot(ctx)
	if err != nil {
		t.Fatalf("load large snapshot: %v", err)
	}
	if len(loaded) != files {
		t.Fatalf("expected %d snapshot entries, got %d", files, len(loaded))
	}
	if got := loaded["dir/file-19999.txt"]; got.Size != 19999 {
		t.Fatalf("unexpected last snapshot entry: %#v", got)
	}
}

// Observed changes are inserted in one call per batch and must stay under the
// same SQLite host-parameter ceiling, with generated IDs returned for each row.
func TestRecordObservedChangesExceedingSQLVariableLimit(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)

	const count = 20000
	changes := make([]ObservedFileChange, 0, count)
	for i := range count {
		changes = append(changes, ObservedFileChange{Path: fmt.Sprintf("dir/file-%d.txt", i), Kind: watcher.Modified})
	}
	recorded, err := s.RecordObservedChanges(ctx, changes)
	if err != nil {
		t.Fatalf("record observed changes: %v", err)
	}
	if len(recorded) != count {
		t.Fatalf("expected %d recorded changes, got %d", count, len(recorded))
	}
	for _, change := range recorded {
		if change.ID == "" {
			t.Fatalf("recorded change missing id: %#v", change)
		}
	}
}

func TestRefreshDefinitionsAndListStatus(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)

	defs := []hooks.Hook{
		{ID: "lint", Name: "Lint", Type: hooks.HookTypeFile, Engine: hooks.HookEngineScript, Pattern: "**/*.go", Ignore: []string{"vendor/**"}, AbsPath: "/repo/.discobox/hooks/lint", RelPath: ".discobox/hooks/lint", Extensions: map[string]any{"x": "y"}},
		{ID: "session", Name: "Session", Type: hooks.HookTypeSession, Engine: hooks.HookEngineScript, Blocking: true, AbsPath: "/repo/.discobox/hooks/session", RelPath: ".discobox/hooks/session"},
	}
	if err := s.RefreshDefinitions(ctx, defs); err != nil {
		t.Fatalf("refresh definitions: %v", err)
	}

	statuses, err := s.ListStatus(ctx)
	if err != nil {
		t.Fatalf("list status: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("expected 2 statuses, got %d", len(statuses))
	}
	if statuses[0].Hook.ID != "lint" || statuses[0].Status != models.StatusIdle || statuses[0].Hook.Ignore[0] != "vendor/**" || statuses[0].ConfigHash == "" {
		t.Fatalf("unexpected lint status: %#v", statuses[0])
	}

	if err := s.RefreshDefinitions(ctx, defs[:1]); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	statuses, err = s.ListStatus(ctx)
	if err != nil {
		t.Fatalf("list status after stale removal: %v", err)
	}
	if len(statuses) != 1 || statuses[0].Hook.ID != "lint" {
		t.Fatalf("expected only lint after refresh, got %#v", statuses)
	}
}

func TestLSPHookReadyUsesCurrentDiagnostics(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)
	hook := hooks.Hook{ID: "go-lsp", Name: "Go LSP", Type: hooks.HookTypeFile, Engine: hooks.HookEngineLSP, Pattern: "**/*.go"}
	if err := s.RefreshDefinitions(ctx, []hooks.Hook{hook}); err != nil {
		t.Fatal(err)
	}

	if err := s.SetLSPHookRunning(ctx, hook.ID); err != nil {
		t.Fatalf("mark lsp running: %v", err)
	}
	if err := s.SetLSPHookReady(ctx, hook.ID); err != nil {
		t.Fatalf("mark lsp ready: %v", err)
	}
	statuses, err := s.ListStatus(ctx)
	if err != nil {
		t.Fatalf("list status: %v", err)
	}
	if len(statuses) != 1 || statuses[0].Status != models.StatusSuccess || statuses[0].LastError != "" {
		t.Fatalf("expected ready LSP with no diagnostics to be successful, got %#v", statuses)
	}

	if err := s.ReplaceDiagnosticsForURI(ctx, hook.ID, "file:///repo/main.go", "main.go", []Diagnostic{{
		HookID:  hook.ID,
		URI:     "file:///repo/main.go",
		Path:    "main.go",
		Message: "undefined: thing",
	}}); err != nil {
		t.Fatalf("replace diagnostics: %v", err)
	}
	if err := s.SetLSPHookReady(ctx, hook.ID); err != nil {
		t.Fatalf("mark lsp ready with diagnostics: %v", err)
	}
	statuses, err = s.ListStatus(ctx)
	if err != nil {
		t.Fatalf("list status after diagnostics: %v", err)
	}
	if len(statuses) != 1 || statuses[0].Status != models.StatusFailure || statuses[0].LastError != "1 diagnostics" {
		t.Fatalf("expected ready LSP with diagnostics to fail, got %#v", statuses)
	}
	if err := s.SetLSPHookRunning(ctx, hook.ID); err != nil {
		t.Fatalf("mark lsp running again: %v", err)
	}
	if err := s.SetLSPHookReady(ctx, hook.ID); err != nil {
		t.Fatalf("mark lsp ready after restart: %v", err)
	}
	diagnostics, err := s.ListDiagnostics(ctx, DiagnosticQuery{HookID: hook.ID})
	if err != nil {
		t.Fatalf("list diagnostics after lsp restart: %v", err)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("expected LSP restart to clear diagnostics, got %#v", diagnostics)
	}
	statuses, err = s.ListStatus(ctx)
	if err != nil {
		t.Fatalf("list status after lsp restart: %v", err)
	}
	if len(statuses) != 1 || statuses[0].Status != models.StatusSuccess || statuses[0].LastError != "" {
		t.Fatalf("expected restarted LSP with no diagnostics to be successful, got %#v", statuses)
	}

	hook.Pattern = "**/*.ts"
	if err := s.RefreshDefinitions(ctx, []hooks.Hook{hook}); err != nil {
		t.Fatalf("refresh changed lsp definition: %v", err)
	}
	diagnostics, err = s.ListDiagnostics(ctx, DiagnosticQuery{HookID: hook.ID})
	if err != nil {
		t.Fatalf("list diagnostics after changed lsp definition: %v", err)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("expected changed LSP definition to clear diagnostics, got %#v", diagnostics)
	}
	statuses, err = s.ListStatus(ctx)
	if err != nil {
		t.Fatalf("list status after changed lsp definition: %v", err)
	}
	if len(statuses) != 1 || statuses[0].Status != models.StatusSuccess || statuses[0].LastError != "" {
		t.Fatalf("expected changed LSP definition to reset status from diagnostics, got %#v", statuses)
	}

	if err := s.ReplaceDiagnosticsForURI(ctx, hook.ID, "file:///repo/main.go", "main.go", []Diagnostic{{
		HookID:  hook.ID,
		URI:     "file:///repo/main.go",
		Path:    "main.go",
		Message: "stale diagnostic",
	}}); err != nil {
		t.Fatalf("replace stale diagnostics: %v", err)
	}
	if err := s.RefreshDefinitions(ctx, []hooks.Hook{hook}); err != nil {
		t.Fatalf("refresh same lsp definition: %v", err)
	}
	diagnostics, err = s.ListDiagnostics(ctx, DiagnosticQuery{HookID: hook.ID})
	if err != nil {
		t.Fatalf("list diagnostics after same lsp definition: %v", err)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("expected same LSP definition to prune non-matching diagnostics, got %#v", diagnostics)
	}
	if err := s.EnqueueChanges(ctx, []string{hook.ID}, []watcher.Change{{Path: "main.go", Kind: watcher.Modified}}); err != nil {
		t.Fatalf("enqueue stale LSP pending row: %v", err)
	}
	if err := s.RefreshDefinitions(ctx, []hooks.Hook{hook}); err != nil {
		t.Fatalf("refresh lsp definition with stale pending row: %v", err)
	}
	if pending, err := s.NextPending(ctx); err != nil || pending != nil {
		t.Fatalf("expected LSP refresh to clear pending row, got %#v, %v", pending, err)
	}
}

func TestEnqueueMergesAndRunningFinishTransitions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)
	defs := []hooks.Hook{
		{ID: "a", Name: "A", Type: hooks.HookTypeFile, Engine: hooks.HookEngineScript, AbsPath: "/repo/a", RelPath: "a"},
		{ID: "b", Name: "B", Type: hooks.HookTypeFile, Engine: hooks.HookEngineScript, AbsPath: "/repo/b", RelPath: "b"},
	}
	if err := s.RefreshDefinitions(ctx, defs); err != nil {
		t.Fatal(err)
	}

	if err := s.EnqueueChanges(ctx, []string{"a", "b"}, []watcher.Change{{Path: "foo.go", Kind: watcher.Modified}}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	observed, err := s.RecordObservedChanges(ctx, []ObservedFileChange{{Path: "bar.go", Kind: watcher.Created, BaseCommit: "abc123", Diff: "diff --git a/bar.go b/bar.go"}})
	if err != nil {
		t.Fatalf("record observed changes: %v", err)
	}
	if len(observed) != 1 || observed[0].ID == "" || observed[0].BaseCommit != "abc123" || observed[0].Diff == "" {
		t.Fatalf("unexpected observed changes: %#v", observed)
	}
	if err := s.EnqueueWithChangeIDs(ctx, []string{"a"}, []models.ChangedFile{{Path: "bar.go", Kind: watcher.Created}, {Path: "foo.go", Kind: watcher.Modified}}, []string{observed[0].ID}); err != nil {
		t.Fatalf("merge enqueue: %v", err)
	}

	pending, err := s.NextPending(ctx)
	if err != nil {
		t.Fatalf("next pending: %v", err)
	}
	if pending == nil || pending.HookID != "a" || len(pending.ChangedFiles) != 2 || len(pending.ChangeIDs) != 1 || pending.ChangeIDs[0] != observed[0].ID {
		t.Fatalf("unexpected pending row: %#v", pending)
	}

	run, err := s.MarkRunning(ctx, "a", nil)
	if err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if run.HookID != "a" || run.Status != models.StatusRunning || run.InvocationID == "" || len(run.ChangedFiles) != 2 || len(run.ChangeIDs) != 1 || run.ChangeIDs[0] != observed[0].ID {
		t.Fatalf("unexpected run row: %#v", run)
	}
	var joinCount int64
	if err := s.DB().Model(&models.HookInvocationChange{}).Where("invocation_id = ? AND change_id = ?", run.InvocationID, observed[0].ID).Count(&joinCount).Error; err != nil {
		t.Fatalf("count invocation changes: %v", err)
	}
	if joinCount != 1 {
		t.Fatalf("expected invocation/change join, got %d", joinCount)
	}
	if err := s.FinishRun(ctx, run.ID, models.RunResult{Status: models.StatusFailure, ExitCode: 1, Error: "boom"}); err != nil {
		t.Fatalf("finish failure: %v", err)
	}
	if next, err := s.NextPending(ctx); err != nil || next != nil {
		t.Fatalf("expected failure to block later queued hook, next=%#v err=%v", next, err)
	}

	if err := s.Enqueue(ctx, []string{"a"}, []models.ChangedFile{{Path: "foo.go", Kind: watcher.Modified}}); err != nil {
		t.Fatalf("reenqueue failed hook: %v", err)
	}
	run, err = s.MarkRunning(ctx, "a", nil)
	if err != nil {
		t.Fatalf("mark second run: %v", err)
	}
	if err := s.FinishRun(ctx, run.ID, models.RunResult{Status: models.StatusSuccess}); err != nil {
		t.Fatalf("finish success: %v", err)
	}
	next, err := s.NextPending(ctx)
	if err != nil {
		t.Fatalf("next after success: %v", err)
	}
	if next == nil || next.HookID != "b" || next.Blocked {
		t.Fatalf("expected b unblocked after a success, got %#v", next)
	}

	statuses, err := s.ListStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	statusByID := map[string]StatusRow{}
	for _, st := range statuses {
		statusByID[st.Hook.ID] = st
	}
	if statusByID["a"].Status != models.StatusSuccess || statusByID["a"].RunCount != 2 || statusByID["a"].FailCount != 1 {
		t.Fatalf("unexpected status for a: %#v", statusByID["a"])
	}

	runs, err := s.ListRuns(ctx, "a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].Status != models.StatusSuccess || runs[1].Status != models.StatusFailure {
		t.Fatalf("unexpected runs: %#v", runs)
	}
}

func TestNextPendingSkipsPhaseHooksUntilActivated(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)
	if err := s.RefreshDefinitions(ctx, []hooks.Hook{
		{ID: "lint", Name: "Lint", Type: hooks.HookTypeFile, Engine: hooks.HookEngineScript},
		{ID: "review", Name: "Review", Type: hooks.HookTypeFile, Engine: hooks.HookEngineScript, Phase: "review"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue(ctx, []string{"review"}, []models.ChangedFile{{Path: "review.go", Kind: watcher.Modified}}); err != nil {
		t.Fatalf("enqueue review: %v", err)
	}
	if pending, err := s.NextPending(ctx); err != nil || pending != nil {
		t.Fatalf("expected gated review hook to stay pending, pending=%#v err=%v", pending, err)
	}
	if err := s.Enqueue(ctx, []string{"lint"}, []models.ChangedFile{{Path: "lint.go", Kind: watcher.Modified}}); err != nil {
		t.Fatalf("enqueue lint: %v", err)
	}
	pending, err := s.NextPending(ctx)
	if err != nil {
		t.Fatalf("next unphased pending: %v", err)
	}
	if pending == nil || pending.HookID != "lint" {
		t.Fatalf("expected unphased hook first, got %#v", pending)
	}
	run, err := s.MarkRunning(ctx, "lint", nil)
	if err != nil {
		t.Fatalf("mark lint running: %v", err)
	}
	if err := s.FinishRun(ctx, run.ID, models.RunResult{Status: models.StatusSuccess}); err != nil {
		t.Fatalf("finish lint: %v", err)
	}
	pending, err = s.NextPendingExcluding(ctx, []string{"review"}, nil)
	if err != nil {
		t.Fatalf("next review pending: %v", err)
	}
	if pending == nil || pending.HookID != "review" {
		t.Fatalf("expected activated review hook, got %#v", pending)
	}
}

func TestNextPendingExcludingSkipsRunningHookIDs(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)
	if err := s.RefreshDefinitions(ctx, []hooks.Hook{
		{ID: "a", Name: "A", Type: hooks.HookTypeFile, Engine: hooks.HookEngineScript},
		{ID: "b", Name: "B", Type: hooks.HookTypeFile, Engine: hooks.HookEngineScript},
		{ID: "review", Name: "Review", Type: hooks.HookTypeFile, Engine: hooks.HookEngineScript, Phase: "review"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue(ctx, []string{"a", "b", "review"}, []models.ChangedFile{{Path: "changed.go", Kind: watcher.Modified}}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	pending, err := s.NextPendingExcluding(ctx, []string{"review"}, []string{"a"})
	if err != nil {
		t.Fatalf("next excluding a: %v", err)
	}
	if pending == nil || pending.HookID != "b" {
		t.Fatalf("expected b after excluding a, got %#v", pending)
	}
	pending, err = s.NextPendingExcluding(ctx, []string{"review"}, []string{"a", "b"})
	if err != nil {
		t.Fatalf("next excluding a and b: %v", err)
	}
	if pending == nil || pending.HookID != "review" {
		t.Fatalf("expected phase hook after excluding unphased hooks, got %#v", pending)
	}
}

func TestListPendingReturnsQueueRowsInOrder(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)
	if err := s.RefreshDefinitions(ctx, []hooks.Hook{
		{ID: "a", Name: "A", Type: hooks.HookTypeFile, Engine: hooks.HookEngineScript},
		{ID: "b", Name: "B", Type: hooks.HookTypeFile, Engine: hooks.HookEngineScript},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue(ctx, []string{"b"}, []models.ChangedFile{{Path: "b.go", Kind: watcher.Modified}}); err != nil {
		t.Fatalf("enqueue b: %v", err)
	}
	if err := s.Enqueue(ctx, []string{"a"}, []models.ChangedFile{{Path: "a.go", Kind: watcher.Modified}}); err != nil {
		t.Fatalf("enqueue a: %v", err)
	}

	rows, err := s.ListPending(ctx, 1)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(rows) != 1 || rows[0].HookID != "b" || rows[0].Position != 1 || len(rows[0].ChangedFiles) != 1 {
		t.Fatalf("unexpected limited pending rows: %#v", rows)
	}
	rows, err = s.ListPending(ctx, 0)
	if err != nil {
		t.Fatalf("list all pending: %v", err)
	}
	if len(rows) != 2 || rows[0].HookID != "b" || rows[1].HookID != "a" {
		t.Fatalf("unexpected pending order: %#v", rows)
	}
}

func TestFailedRunInputsCarryForwardToLaterSchedule(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)
	if err := s.RefreshDefinitions(ctx, []hooks.Hook{{ID: "a", Name: "A", Type: hooks.HookTypeFile, Engine: hooks.HookEngineScript}}); err != nil {
		t.Fatal(err)
	}

	if err := s.Enqueue(ctx, []string{"a"}, []models.ChangedFile{{Path: "old.go", Kind: watcher.Modified}}); err != nil {
		t.Fatalf("enqueue old: %v", err)
	}
	run, err := s.MarkRunning(ctx, "a", nil)
	if err != nil {
		t.Fatalf("mark old running: %v", err)
	}
	if err := s.FinishRun(ctx, run.ID, models.RunResult{Status: models.StatusFailure, ExitCode: 1}); err != nil {
		t.Fatalf("finish failure: %v", err)
	}
	if pending, err := s.NextPending(ctx); err != nil || pending != nil {
		t.Fatalf("expected no pending retry without new changes, pending=%#v err=%v", pending, err)
	}

	if err := s.Enqueue(ctx, []string{"a"}, []models.ChangedFile{{Path: "new.go", Kind: watcher.Created}}); err != nil {
		t.Fatalf("enqueue new: %v", err)
	}
	pending, err := s.NextPending(ctx)
	if err != nil {
		t.Fatalf("next pending: %v", err)
	}
	if got := changedFilePaths(pending.ChangedFiles); got != "new.go,old.go" {
		t.Fatalf("expected failed input plus new input, got %q from %#v", got, pending)
	}
	run, err = s.MarkRunning(ctx, "a", nil)
	if err != nil {
		t.Fatalf("mark carry-forward running: %v", err)
	}
	if got := changedFilePaths(run.ChangedFiles); got != "new.go,old.go" {
		t.Fatalf("expected carried run inputs, got %q from %#v", got, run)
	}
	if err := s.FinishRun(ctx, run.ID, models.RunResult{Status: models.StatusSuccess}); err != nil {
		t.Fatalf("finish success: %v", err)
	}

	if err := s.Enqueue(ctx, []string{"a"}, []models.ChangedFile{{Path: "after-success.go", Kind: watcher.Modified}}); err != nil {
		t.Fatalf("enqueue after success: %v", err)
	}
	pending, err = s.NextPending(ctx)
	if err != nil {
		t.Fatalf("next after success: %v", err)
	}
	if got := changedFilePaths(pending.ChangedFiles); got != "after-success.go" {
		t.Fatalf("expected only new input after success, got %q from %#v", got, pending)
	}
}

func TestChangesDuringFailedRunMergeWithRunInputs(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)
	if err := s.RefreshDefinitions(ctx, []hooks.Hook{{ID: "a", Name: "A", Type: hooks.HookTypeFile, Engine: hooks.HookEngineScript}}); err != nil {
		t.Fatal(err)
	}

	if err := s.Enqueue(ctx, []string{"a"}, []models.ChangedFile{{Path: "started.go", Kind: watcher.Modified}}); err != nil {
		t.Fatalf("enqueue started: %v", err)
	}
	run, err := s.MarkRunning(ctx, "a", nil)
	if err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if err := s.Enqueue(ctx, []string{"a"}, []models.ChangedFile{{Path: "during.go", Kind: watcher.Created}}); err != nil {
		t.Fatalf("enqueue during run: %v", err)
	}
	if err := s.FinishRun(ctx, run.ID, models.RunResult{Status: models.StatusFailure, ExitCode: 1}); err != nil {
		t.Fatalf("finish failure: %v", err)
	}
	pending, err := s.NextPending(ctx)
	if err != nil {
		t.Fatalf("next pending: %v", err)
	}
	if pending == nil {
		t.Fatal("expected pending retry from change during failed run")
	}
	if got := changedFilePaths(pending.ChangedFiles); got != "during.go,started.go" {
		t.Fatalf("expected running inputs merged into pending changes, got %q from %#v", got, pending)
	}
}

func TestReconcileRunningRunsFailsAndRequeues(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)
	if err := s.RefreshDefinitions(ctx, []hooks.Hook{{ID: "a", Name: "A", Type: hooks.HookTypeFile, Engine: hooks.HookEngineScript}}); err != nil {
		t.Fatal(err)
	}

	if err := s.Enqueue(ctx, []string{"a"}, []models.ChangedFile{{Path: "first.go", Kind: watcher.Modified}}); err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	first, err := s.MarkRunning(ctx, "a", nil)
	if err != nil {
		t.Fatalf("mark first running: %v", err)
	}
	if err := s.Enqueue(ctx, []string{"a"}, []models.ChangedFile{{Path: "second.go", Kind: watcher.Created}}); err != nil {
		t.Fatalf("enqueue second: %v", err)
	}
	second, err := s.MarkRunning(ctx, "a", nil)
	if err != nil {
		t.Fatalf("mark second running: %v", err)
	}

	count, err := s.ReconcileRunningRuns(ctx, "daemon crashed")
	if err != nil {
		t.Fatalf("reconcile running: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 reconciled runs, got %d", count)
	}

	runs, err := s.ListRuns(ctx, "a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %#v", runs)
	}
	for _, run := range runs {
		if run.Status != models.StatusFailure || run.Error != "daemon crashed" || run.FinishedAt == nil {
			t.Fatalf("run was not failed by reconciliation: %#v", run)
		}
	}
	if runs[0].ID != second.ID || runs[1].ID != first.ID {
		t.Fatalf("unexpected run ordering: %#v", runs)
	}

	statuses, err := s.ListStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Status != models.StatusQueued || statuses[0].RunCount != 2 || statuses[0].FailCount != 2 || statuses[0].LastRunID != second.ID || statuses[0].LastError != "daemon crashed" {
		t.Fatalf("unexpected reconciled status: %#v", statuses)
	}

	pending, err := s.NextPending(ctx)
	if err != nil {
		t.Fatalf("next pending: %v", err)
	}
	if pending == nil || pending.HookID != "a" {
		t.Fatalf("expected requeued hook, got %#v", pending)
	}
	if got := changedFilePaths(pending.ChangedFiles); got != "first.go,second.go" {
		t.Fatalf("expected both stale run inputs requeued, got %q from %#v", got, pending)
	}
}

func TestPauseResumeAndDaemonState(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)
	if err := s.RefreshDefinitions(ctx, []hooks.Hook{{ID: "a", Name: "A", Type: hooks.HookTypeSession, Engine: hooks.HookEngineScript}}); err != nil {
		t.Fatal(err)
	}

	if err := s.PauseHook(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	statuses, err := s.ListStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !statuses[0].Paused {
		t.Fatalf("expected hook paused")
	}
	if err := s.ResumeHook(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	statuses, _ = s.ListStatus(ctx)
	if statuses[0].Paused {
		t.Fatalf("expected hook resumed")
	}

	if err := s.PauseGlobal(ctx); err != nil {
		t.Fatal(err)
	}
	v, ok, err := s.GetDaemonState(ctx, "paused")
	if err != nil || !ok || v != "true" {
		t.Fatalf("global paused state = %q %v %v", v, ok, err)
	}
	if err := s.ResumeGlobal(ctx); err != nil {
		t.Fatal(err)
	}
	v, ok, err = s.GetDaemonState(ctx, "paused")
	if err != nil || !ok || v != "false" {
		t.Fatalf("global resumed state = %q %v %v", v, ok, err)
	}
}

func TestDaemonSessionLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)

	first, err := s.StartDaemonSession(ctx, "session-1", "/repo", 7, 123)
	if err != nil {
		t.Fatalf("start first daemon session: %v", err)
	}
	if first.ID == "" || first.EndedAt != nil || first.LastHeartbeat.IsZero() {
		t.Fatalf("unexpected first daemon session: %#v", first)
	}
	if err := s.HeartbeatDaemonSession(ctx, first.ID); err != nil {
		t.Fatalf("heartbeat first daemon session: %v", err)
	}
	var heartbeated models.DaemonSession
	if err := s.read.WithContext(ctx).First(&heartbeated, "id = ?", first.ID).Error; err != nil {
		t.Fatalf("load heartbeated session: %v", err)
	}
	if heartbeated.LastHeartbeat.Before(first.LastHeartbeat) {
		t.Fatalf("heartbeat moved backwards: before=%s after=%s", first.LastHeartbeat, heartbeated.LastHeartbeat)
	}
	if err := s.EndDaemonSession(ctx, first.ID, "shutdown"); err != nil {
		t.Fatalf("end first daemon session: %v", err)
	}
	var ended models.DaemonSession
	if err := s.read.WithContext(ctx).First(&ended, "id = ?", first.ID).Error; err != nil {
		t.Fatalf("load ended session: %v", err)
	}
	if ended.EndedAt == nil || ended.EndReason != "shutdown" {
		t.Fatalf("expected graceful shutdown, got %#v", ended)
	}

	events, err := s.ListEvents(ctx, EventQuery{Limit: 10})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 2 || events[0].Type != "daemon.shutdown" || events[1].Type != "daemon.started" {
		t.Fatalf("unexpected daemon lifecycle events: %#v", events)
	}
	if events[0].Details["daemon_session_id"] != first.ID {
		t.Fatalf("shutdown event missing daemon_session_id: %#v", events[0].Details)
	}
}

func TestStartDaemonSessionTerminatesStaleSessions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)

	stale, err := s.StartDaemonSession(ctx, "session-1", "/repo", 1, 111)
	if err != nil {
		t.Fatalf("start stale daemon session: %v", err)
	}
	heartbeat := stale.StartedAt.Add(30 * time.Second)
	if err := s.write.WithContext(ctx).Model(&models.DaemonSession{}).Where("id = ?", stale.ID).Update("last_heartbeat", heartbeat).Error; err != nil {
		t.Fatalf("set stale heartbeat: %v", err)
	}
	next, err := s.StartDaemonSession(ctx, "session-1", "/repo", 2, 222)
	if err != nil {
		t.Fatalf("start replacement daemon session: %v", err)
	}
	if next.ID == stale.ID {
		t.Fatalf("expected new daemon session id")
	}

	var staleRow models.DaemonSession
	if err := s.read.WithContext(ctx).First(&staleRow, "id = ?", stale.ID).Error; err != nil {
		t.Fatalf("load stale session: %v", err)
	}
	if staleRow.EndedAt == nil || !staleRow.EndedAt.Equal(heartbeat) || staleRow.EndReason != "terminated" {
		t.Fatalf("expected stale session terminated at heartbeat, got %#v", staleRow)
	}
	var activeCount int64
	if err := s.read.WithContext(ctx).Model(&models.DaemonSession{}).Where("ended_at IS NULL").Count(&activeCount).Error; err != nil {
		t.Fatalf("count active sessions: %v", err)
	}
	if activeCount != 1 {
		t.Fatalf("expected one active session, got %d", activeCount)
	}
	events, err := s.ListEvents(ctx, EventQuery{Limit: 10})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 3 || countEvents(events, "daemon.started") != 2 || countEvents(events, "daemon.terminated") != 1 {
		t.Fatalf("unexpected stale termination events: %#v", events)
	}
	terminated := eventByType(events, "daemon.terminated")
	if terminated == nil || terminated.Details["daemon_session_id"] != stale.ID || terminated.Details["end_reason"] != "terminated" {
		t.Fatalf("termination event missing stale details: %#v", events)
	}
}

func TestRecordAndListEvents(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)

	first, err := s.RecordEvent(ctx, Event{Type: "daemon.started", Message: "daemon started", Details: map[string]any{"daemon_session_id": "daemon-1", "session_id": "s1", "repo_root": "/repo", "version": int64(1), "pid": 123, "started_at": time.Now().UTC()}})
	if err != nil {
		t.Fatalf("record first event: %v", err)
	}
	second, err := s.RecordEvent(ctx, Event{Type: "hook.run.started", HookID: "lint", RunID: "run-7", Message: "hook run started", Details: map[string]any{"changed_files": 0, "changed_paths": []string{}, "change_ids": []string{}, "invocation_id": "inv-7"}})
	if err != nil {
		t.Fatalf("record second event: %v", err)
	}
	if first.ID == "" || second.ID == "" || first.ID == second.ID {
		t.Fatalf("unexpected event ids first=%s second=%s", first.ID, second.ID)
	}

	events, err := s.ListEvents(ctx, EventQuery{Limit: 10})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 2 || events[0].Type != "hook.run.started" || events[1].Type != "daemon.started" {
		t.Fatalf("unexpected events: %#v", events)
	}
	if events[1].Details["session_id"] != "s1" {
		t.Fatalf("unexpected details: %#v", events[1].Details)
	}

	events, err = s.ListEvents(ctx, EventQuery{HookID: "lint", Limit: 10})
	if err != nil {
		t.Fatalf("list hook events: %v", err)
	}
	if len(events) != 1 || events[0].HookID != "lint" || events[0].RunID != "run-7" {
		t.Fatalf("unexpected hook events: %#v", events)
	}
}

func TestListEventsAfterCursorAscending(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)

	first, err := s.RecordEvent(ctx, Event{Type: "daemon.shutdown.requested", Details: map[string]any{"session_id": "s1", "repo_root": "/repo"}})
	if err != nil {
		t.Fatalf("record first event: %v", err)
	}
	time.Sleep(time.Millisecond)
	second, err := s.RecordEvent(ctx, Event{Type: "workspace.snapshot.failed", Details: map[string]any{"repo_root": "/repo", "error": "boom"}})
	if err != nil {
		t.Fatalf("record second event: %v", err)
	}
	time.Sleep(time.Millisecond)
	if _, err := s.RecordEvent(ctx, Event{Type: "watch.snapshot.persist.failed", Details: map[string]any{"files": 3, "paths": 1, "full": false, "error": "boom"}}); err != nil {
		t.Fatalf("record third event: %v", err)
	}

	events, err := s.ListEvents(ctx, EventQuery{AfterCreatedAt: first.CreatedAt, AfterID: first.ID, Ascending: true, Limit: 10})
	if err != nil {
		t.Fatalf("list events after cursor: %v", err)
	}
	if len(events) != 2 || events[0].ID != second.ID || events[0].Type != "workspace.snapshot.failed" || events[1].Type != "watch.snapshot.persist.failed" {
		t.Fatalf("unexpected cursor events: %#v", events)
	}
}

func TestRecordEventPanicsForMissingRequiredDetailsInTests(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)

	assertPanics(t, func() {
		_, _ = s.RecordEvent(ctx, Event{Type: "watch.snapshot.persist.failed", Details: map[string]any{"error": "boom"}})
	})
}

func TestRecordEventPanicsForUnknownEventTypesInTests(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)

	assertPanics(t, func() {
		_, _ = s.RecordEvent(ctx, Event{Type: "not.documented"})
	})
}

func TestAppendAndListHookLogs(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)

	if _, err := s.AppendHookLog(ctx, models.HookLog{HookID: "lint", RunID: "run-7", Line: "first"}); err != nil {
		t.Fatalf("append first log: %v", err)
	}
	if _, err := s.AppendHookLog(ctx, models.HookLog{HookID: "lint", RunID: "run-7", Line: "second"}); err != nil {
		t.Fatalf("append second log: %v", err)
	}
	if _, err := s.AppendHookLog(ctx, models.HookLog{HookID: "test", RunID: "run-8", Line: "other"}); err != nil {
		t.Fatalf("append other log: %v", err)
	}

	logs, err := s.ListHookLogs(ctx, HookLogQuery{HookID: "lint", RunID: "run-7"})
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if len(logs) != 2 || logs[0].Line != "first" || logs[1].Line != "second" {
		t.Fatalf("unexpected logs: %#v", logs)
	}
}

func TestAppendHookLogEventRecordsLogAndAuditEvent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(ctx, t)

	log, err := s.AppendHookLogEvent(ctx, models.HookLog{HookID: "lint", RunID: "run-7", Line: "hello"})
	if err != nil {
		t.Fatalf("append hook log event: %v", err)
	}
	if log.ID == "" {
		t.Fatalf("expected generated log id")
	}

	logs, err := s.ListHookLogs(ctx, HookLogQuery{HookID: "lint", RunID: "run-7"})
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if len(logs) != 1 || logs[0].ID != log.ID || logs[0].Line != "hello" {
		t.Fatalf("unexpected logs: %#v", logs)
	}

	events, err := s.ListEvents(ctx, EventQuery{HookID: "lint", Limit: 10})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 || events[0].Type != "hook.log" || events[0].RunID != "run-7" || events[0].Message != "hello" {
		t.Fatalf("unexpected hook log events: %#v", events)
	}
	if events[0].Details["line_id"] != log.ID || events[0].Details["line"] != "hello" {
		t.Fatalf("unexpected hook log event details: %#v", events[0].Details)
	}
}

func newTestStore(ctx context.Context, t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hooks.db")
	s, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func changedFilePaths(files []models.ChangedFile) string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	sort.Strings(paths)
	return strings.Join(paths, ",")
}

func countEvents(events []Event, eventType string) int {
	count := 0
	for _, event := range events {
		if event.Type == eventType {
			count++
		}
	}
	return count
}

func eventByType(events []Event, eventType string) *Event {
	for i := range events {
		if events[i].Type == eventType {
			return &events[i]
		}
	}
	return nil
}

func assertPanics(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	fn()
}
