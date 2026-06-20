package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hooks "github.com/obot-platform/discobox/hooks"
	"github.com/obot-platform/discobox/hooks/api/model"
	"github.com/obot-platform/discobox/hooks/manager"
	"github.com/obot-platform/discobox/hooks/models"
	"github.com/obot-platform/discobox/hooks/parser"
	hookstore "github.com/obot-platform/discobox/hooks/store"
	"github.com/obot-platform/discobox/hooks/watcher"
)

func TestRunSocketAPI(t *testing.T) {
	repo := t.TempDir()
	hooksDir := filepath.Join(repo, ".discobox", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hookPath := filepath.Join(hooksDir, "echo.sh")
	hookScript := `#!/bin/sh
#---
# name: Echo
# type: file
# pattern: "*.txt"
#---
echo "hook=$DISCOBOX_HOOK_ID"
echo "run=$DISCOBOX_HOOK_RUN_ID"
echo "changed=$DISCOBOX_CHANGED_FILES"
`
	if err := os.WriteFile(hookPath, []byte(hookScript), 0o755); err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	socketPath := filepath.Join(os.TempDir(), fmt.Sprintf("discobox-hooks-test-%d.sock", time.Now().UnixNano()))
	defer os.Remove(socketPath)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Config{
			SessionID:  "test-session",
			RepoRoot:   repo,
			DBPath:     filepath.Join(stateDir, "hooks.db"),
			SocketPath: socketPath,
			Version:    42,
			Debounce:   20 * time.Millisecond,
		})
	}()

	client := unixHTTPClient(socketPath)
	var ping PingResponse
	waitForJSON(t, client, http.MethodGet, "http://unix/ping", nil, &ping)
	if !ping.OK || ping.SessionID != "test-session" || ping.Version != 42 {
		t.Fatalf("unexpected ping: %+v", ping)
	}

	var hooksResp HooksResponse
	doJSON(t, client, http.MethodGet, "http://unix/hooks", nil, &hooksResp)
	if len(hooksResp.Hooks) != 1 || hooksResp.Hooks[0].Hook.ID != "echo" {
		t.Fatalf("unexpected hooks: %+v", hooksResp)
	}

	var emptyOutput OutputResponse
	doJSON(t, client, http.MethodGet, "http://unix/hooks/echo/output", nil, &emptyOutput)
	if emptyOutput.HookID != "echo" || emptyOutput.Output != "" {
		t.Fatalf("expected empty output before first run, got %+v", emptyOutput)
	}

	var pause ExecutionResponse
	doJSON(t, client, http.MethodPatch, "http://unix/hooks/echo/execution", ExecutionPatchRequest{Paused: true}, &pause)
	if !pause.Paused {
		t.Fatalf("expected hook paused")
	}
	doJSON(t, client, http.MethodPatch, "http://unix/hooks/echo/execution", ExecutionPatchRequest{Paused: false}, &pause)
	if pause.Paused {
		t.Fatalf("expected hook resumed")
	}

	var runResp RunResponse
	doJSON(t, client, http.MethodPost, "http://unix/hooks/echo/run", RunRequest{}, &runResp)
	if !runResp.Enqueued || runResp.HookID != "echo" {
		t.Fatalf("unexpected run: %+v", runResp)
	}

	var status StatusResponse
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		doJSON(t, client, http.MethodGet, "http://unix/status", nil, &status)
		if len(status.Hooks) == 1 && status.Hooks[0].RunCount > 0 && status.Hooks[0].Status == "success" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(status.Hooks) != 1 || status.Hooks[0].RunCount == 0 || status.Hooks[0].Status != "success" {
		t.Fatalf("hook did not run successfully: %+v", status)
	}

	var waitResp WaitResponse
	doJSON(t, client, http.MethodGet, "http://unix/wait?timeout_seconds=1", nil, &waitResp)
	if !waitResp.Settled || waitResp.Running || waitResp.Queued != 0 || len(waitResp.Hooks) != 1 || waitResp.Hooks[0].Status != "success" {
		t.Fatalf("unexpected wait response after successful run: %+v", waitResp)
	}

	var skipped RunResponse
	doJSON(t, client, http.MethodPost, "http://unix/hooks/echo/run", RunRequest{}, &skipped)
	if !skipped.Skipped || skipped.Reason != "already_succeeded" {
		t.Fatalf("expected already succeeded skip, got %+v", skipped)
	}

	var output OutputResponse
	doJSON(t, client, http.MethodGet, "http://unix/hooks/echo/output", nil, &output)
	if output.HookID != "echo" || !strings.Contains(output.Output, "hook=echo") || !strings.Contains(output.Output, "run=") {
		t.Fatalf("unexpected output: %+v", output)
	}

	var events EventsResponse
	doJSON(t, client, http.MethodGet, "http://unix/events?limit=50", nil, &events)
	if !hasEvent(events.Events, "daemon.started", "") || !hasEvent(events.Events, "hook.run.requested", "echo") || !hasEvent(events.Events, "hook.run.started", "echo") || !hasEvent(events.Events, "hook.run.finished", "echo") || !hasEvent(events.Events, "hook.log", "echo") || !hasEvent(events.Events, "hook.run.skipped", "echo") {
		t.Fatalf("expected audit events, got %+v", events.Events)
	}
	startedEvent := modelEventByType(events.Events, "hook.run.started", "echo")
	if startedEvent == nil {
		t.Fatalf("missing hook.run.started event: %+v", events.Events)
	}
	if value, ok := startedEvent.Details["change_ids"]; !ok || value == nil {
		t.Fatalf("hook.run.started event missing non-null change_ids: %#v", startedEvent.Details)
	}

	var shutdown ShutdownResponse
	doJSON(t, client, http.MethodPost, "http://unix/shutdown", nil, &shutdown)
	if !shutdown.OK {
		t.Fatalf("unexpected shutdown response: %+v", shutdown)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not shut down")
	}
}

func TestWaitSnapshotIncludesPendingLSP(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	st, err := hookstore.Open(ctx, hookstore.Options{Path: filepath.Join(t.TempDir(), "hooks.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	hook := hooks.Hook{ID: "go-lsp", Name: "Go LSP", Type: hooks.HookTypeFile, Engine: hooks.HookEngineLSP, Pattern: "**/*.go", LanguageID: "go"}
	if err := st.RefreshDefinitions(ctx, []hooks.Hook{hook}); err != nil {
		t.Fatalf("refresh definitions: %v", err)
	}
	mgr, err := manager.New(manager.Config{Store: st, Hooks: []hooks.Hook{hook}, SessionID: "test-session", RepoRoot: repo})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	r := &runtimeState{cfg: Config{RepoRoot: repo}, store: st, manager: mgr, ctx: ctx}
	uri := "file://" + filepath.ToSlash(filepath.Join(repo, "main.go"))

	r.markPendingLSP(hook.ID, uri)
	resp, err := r.waitSnapshot(ctx)
	if err != nil {
		t.Fatalf("wait snapshot with pending lsp: %v", err)
	}
	if resp.Settled || !resp.PendingLSP {
		t.Fatalf("expected pending LSP to keep wait unsettled, got %+v", resp)
	}

	r.clearPendingLSP(hook.ID, uri)
	resp, err = r.waitSnapshot(ctx)
	if err != nil {
		t.Fatalf("wait snapshot after clearing pending lsp: %v", err)
	}
	if !resp.Settled || resp.PendingLSP {
		t.Fatalf("expected wait to settle after LSP diagnostics, got %+v", resp)
	}
}

func TestSessionHooksRunOnlyWhenRequested(t *testing.T) {
	repo := t.TempDir()
	hooksDir := filepath.Join(repo, ".discobox", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hookPath := filepath.Join(hooksDir, "session.sh")
	hookScript := `#!/bin/sh
#---
# name: Session
# type: session
#---
echo session
`
	if err := os.WriteFile(hookPath, []byte(hookScript), 0o755); err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	socketPath := filepath.Join(os.TempDir(), fmt.Sprintf("discobox-hooks-session-test-%d.sock", time.Now().UnixNano()))
	defer os.Remove(socketPath)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Config{
			SessionID:  "session-test",
			RepoRoot:   repo,
			DBPath:     filepath.Join(stateDir, "hooks.db"),
			SocketPath: socketPath,
			Debounce:   20 * time.Millisecond,
		})
	}()

	client := unixHTTPClient(socketPath)
	var status StatusResponse
	waitForJSON(t, client, http.MethodGet, "http://unix/status", nil, &status)
	if len(status.Hooks) != 1 || status.Hooks[0].RunCount != 0 || status.Hooks[0].Status != "idle" {
		t.Fatalf("session hook should not run at startup: %+v", status)
	}

	var runResp RunResponse
	doJSON(t, client, http.MethodPost, "http://unix/hooks/session/run", RunRequest{}, &runResp)
	if !runResp.Enqueued || runResp.HookID != "session" {
		t.Fatalf("unexpected run response: %+v", runResp)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		doJSON(t, client, http.MethodGet, "http://unix/status", nil, &status)
		if len(status.Hooks) == 1 && status.Hooks[0].RunCount == 1 && status.Hooks[0].Status == "success" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(status.Hooks) != 1 || status.Hooks[0].RunCount != 1 || status.Hooks[0].Status != "success" {
		t.Fatalf("session hook did not run after request: %+v", status)
	}

	var shutdown ShutdownResponse
	doJSON(t, client, http.MethodPost, "http://unix/shutdown", nil, &shutdown)
	if !shutdown.OK {
		t.Fatalf("unexpected shutdown response: %+v", shutdown)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not shut down")
	}
}

func TestFailedPhaseRunDoesNotKeepPhaseActiveForLaterChanges(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	hookPath := filepath.Join(repo, "review.sh")
	hookScript := `#!/bin/sh
exit 1
`
	if err := os.WriteFile(hookPath, []byte(hookScript), 0o755); err != nil {
		t.Fatal(err)
	}
	hook := hooks.Hook{ID: "review", Name: "Review", Type: hooks.HookTypeFile, Engine: hooks.HookEngineScript, Phase: "review", Pattern: "*.go", AbsPath: hookPath, HasShebang: true, Executable: true}

	dbPath := filepath.Join(t.TempDir(), "hooks.db")
	st, err := hookstore.Open(ctx, hookstore.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.RefreshDefinitions(ctx, []hooks.Hook{hook}); err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r := &runtimeState{cfg: Config{SessionID: "phase-fail-test", RepoRoot: repo, DBPath: dbPath}, store: st, ctx: rctx, cancel: cancel, drainSignal: make(chan struct{}, 1), snapshotSignal: make(chan struct{}, 1)}
	mgr, err := manager.New(manager.Config{Store: st, Hooks: []hooks.Hook{hook}, SessionID: "phase-fail-test", RepoRoot: repo, Cancel: cancel, SignalRun: r.signalDrain})
	if err != nil {
		t.Fatal(err)
	}
	r.manager = mgr

	if err := st.Enqueue(ctx, []string{"review"}, []models.ChangedFile{{Path: "first.go", Kind: watcher.Modified}}); err != nil {
		t.Fatal(err)
	}
	mgr.ActivatePhase("review")
	if r.drainOne() {
		t.Fatal("expected failing review hook to stop drain")
	}
	st1 := hookStatusByID(t, st, ctx, "review")
	if st1.Status != models.StatusFailure || st1.RunCount != 1 || st1.FailCount != 1 {
		t.Fatalf("expected first review run to fail, got %#v", st1)
	}
	if active := mgr.ActivePhases(); len(active) != 0 {
		t.Fatalf("failed phase run left active phases enabled: %v", active)
	}

	if err := st.Enqueue(ctx, []string{"review"}, []models.ChangedFile{{Path: "later.go", Kind: watcher.Created}}); err != nil {
		t.Fatal(err)
	}
	if r.drainOne() {
		t.Fatal("later review change ran without a new explicit phase activation")
	}
	st2 := hookStatusByID(t, st, ctx, "review")
	if st2.Status != models.StatusQueued || st2.RunCount != 1 || st2.FailCount != 1 {
		t.Fatalf("expected later review change to remain queued, got %#v", st2)
	}

	mgr.ActivatePhase("review")
	if r.drainOne() {
		t.Fatal("expected explicitly activated failing review hook to stop drain")
	}
	st3 := hookStatusByID(t, st, ctx, "review")
	if st3.Status != models.StatusFailure || st3.RunCount != 2 || st3.FailCount != 2 {
		t.Fatalf("expected explicit phase activation to run queued review hook, got %#v", st3)
	}
}

func TestRunReloadsHooksWhenConfigDirectoryAppears(t *testing.T) {
	repo := t.TempDir()
	stateDir := t.TempDir()
	socketPath := filepath.Join(os.TempDir(), fmt.Sprintf("discobox-hooks-reload-test-%d.sock", time.Now().UnixNano()))
	defer os.Remove(socketPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Config{
			SessionID:  "reload-session",
			RepoRoot:   repo,
			DBPath:     filepath.Join(stateDir, "hooks.db"),
			SocketPath: socketPath,
			Debounce:   20 * time.Millisecond,
		})
	}()

	client := unixHTTPClient(socketPath)
	var initial HooksResponse
	waitForJSON(t, client, http.MethodGet, "http://unix/hooks", nil, &initial)
	if len(initial.Hooks) != 0 {
		t.Fatalf("expected no initial hooks, got %+v", initial)
	}

	hooksDir := filepath.Join(repo, ".discobox", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hookPath := filepath.Join(hooksDir, "reload.sh")
	hookScript := `#!/bin/sh
#---
# name: Reload
# type: file
# pattern: "*.txt"
#---
echo reload
`
	if err := os.WriteFile(hookPath, []byte(hookScript), 0o755); err != nil {
		t.Fatal(err)
	}

	var hooksResp HooksResponse
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		doJSON(t, client, http.MethodGet, "http://unix/hooks", nil, &hooksResp)
		if len(hooksResp.Hooks) == 1 && hooksResp.Hooks[0].Hook.ID == "reload" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(hooksResp.Hooks) != 1 || hooksResp.Hooks[0].Hook.ID != "reload" {
		t.Fatalf("daemon did not reload hook config: %+v", hooksResp)
	}

	var shutdown ShutdownResponse
	doJSON(t, client, http.MethodPost, "http://unix/shutdown", nil, &shutdown)
	if !shutdown.OK {
		t.Fatalf("unexpected shutdown response: %+v", shutdown)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not shut down")
	}
}

func TestIsHookConfigPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: ".discobox", want: true},
		{path: ".discobox/hooks", want: true},
		{path: ".discobox/hooks/lint.sh", want: true},
		{path: ".discobot/hooks/lint.sh", want: true},
		{path: "src/main.go"},
		{path: ".discobox-other/hooks/lint.sh"},
	}
	for _, tt := range tests {
		if got := isHookConfigPath(tt.path); got != tt.want {
			t.Fatalf("isHookConfigPath(%q) = %t, want %t", tt.path, got, tt.want)
		}
	}
}

func TestRunReconcilesStaleRunningHookOnStartup(t *testing.T) {
	repo := t.TempDir()
	hooksDir := filepath.Join(repo, ".discobox", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hookPath := filepath.Join(hooksDir, "review.sh")
	hookScript := `#!/bin/sh
#---
# name: Review
# type: file
# engine: script
# phase: review
# pattern: "*.go"
#---
echo review
`
	if err := os.WriteFile(hookPath, []byte(hookScript), 0o755); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(t.TempDir(), "hooks.db")
	st, err := hookstore.Open(context.Background(), hookstore.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RefreshDefinitions(context.Background(), []hooks.Hook{{ID: "review", Name: "Review", Type: hooks.HookTypeFile, Engine: hooks.HookEngineScript, Phase: "review"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Enqueue(context.Background(), []string{"review"}, []models.ChangedFile{{Path: "stale.go", Kind: watcher.Modified}}); err != nil {
		t.Fatal(err)
	}
	run, err := st.MarkRunning(context.Background(), "review", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	socketPath := filepath.Join(os.TempDir(), fmt.Sprintf("discobox-hooks-reconcile-test-%d.sock", time.Now().UnixNano()))
	defer os.Remove(socketPath)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Config{
			SessionID:  "test-session",
			RepoRoot:   repo,
			DBPath:     dbPath,
			SocketPath: socketPath,
			Version:    42,
			Debounce:   20 * time.Millisecond,
		})
	}()

	client := unixHTTPClient(socketPath)
	var status StatusResponse
	waitForJSON(t, client, http.MethodGet, "http://unix/status", nil, &status)
	if len(status.Hooks) != 1 || status.Hooks[0].Status != "queued" || status.Hooks[0].RunCount != 1 || status.Hooks[0].FailCount != 1 || status.Hooks[0].LastRunID != run.ID {
		t.Fatalf("expected stale running hook reconciled to queued failure, got %+v", status)
	}

	var shutdown ShutdownResponse
	doJSON(t, client, http.MethodPost, "http://unix/shutdown", nil, &shutdown)
	if !shutdown.OK {
		t.Fatalf("unexpected shutdown response: %+v", shutdown)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not shut down")
	}
}

func TestPrepareSocketPathRejectsLiveSocket(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "daemon.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	defer ln.Close()

	if err := prepareSocketPath(socketPath); err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("expected live socket error, got %v", err)
	}
}

func TestPrepareSocketPathRemovesStaleSocket(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "daemon.sock")
	if err := os.WriteFile(socketPath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareSocketPath(socketPath); err != nil {
		t.Fatalf("prepare stale socket path: %v", err)
	}
	if _, err := os.Stat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected stale socket removed, stat err=%v", err)
	}
}

func TestFlushBatchPersistsWatchedSnapshotAfterDurableProcessing(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	gitTestCommand(t, repo, "init")
	gitTestCommand(t, repo, "config", "user.email", "test@example.com")
	gitTestCommand(t, repo, "config", "user.name", "Test User")
	srcPath := filepath.Join(repo, "src")
	if err := os.MkdirAll(srcPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcPath, "app.go"), []byte("package src\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := hookstore.Open(ctx, hookstore.Options{Path: filepath.Join(t.TempDir(), "hooks.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	hook := hooks.Hook{ID: "lint", Name: "Lint", Type: hooks.HookTypeFile, Engine: hooks.HookEngineScript, Pattern: "**/*.go"}
	if err := st.RefreshDefinitions(ctx, []hooks.Hook{hook}); err != nil {
		t.Fatalf("refresh definitions: %v", err)
	}
	r := &runtimeState{
		cfg:         Config{RepoRoot: repo},
		store:       st,
		discovery:   &parser.Discovery{Hooks: []hooks.Hook{hook}},
		ctx:         ctx,
		drainSignal: make(chan struct{}, 1),
	}
	snapshotTime := time.Now().UTC()
	snapshot := map[string]watcher.Entry{
		"src/app.go": {Path: "src/app.go", Size: 12, ModTime: snapshotTime},
	}
	r.addBatch([]watcher.Change{{Path: "src/app.go", Kind: watcher.Modified}}, snapshot)

	before, err := st.LoadWatchedSnapshot(ctx)
	if err != nil {
		t.Fatalf("load snapshot before flush: %v", err)
	}
	if before != nil {
		t.Fatalf("snapshot persisted before durable processing: %#v", before)
	}

	r.flushBatch()

	after, err := st.LoadWatchedSnapshot(ctx)
	if err != nil {
		t.Fatalf("load snapshot after flush: %v", err)
	}
	if _, ok := after["src/app.go"]; !ok {
		t.Fatalf("expected snapshot persisted after flush, got %#v", after)
	}
	pending, err := st.NextPending(ctx)
	if err != nil {
		t.Fatalf("next pending: %v", err)
	}
	if pending == nil || pending.HookID != "lint" || len(pending.ChangedFiles) != 1 || pending.ChangedFiles[0].Path != "src/app.go" {
		t.Fatalf("expected durable queued hook before checkpoint advance, got %#v", pending)
	}
}

func TestFlushBatchQueuesWhenGitIgnoreFailsOpen(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "app.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := hookstore.Open(ctx, hookstore.Options{Path: filepath.Join(t.TempDir(), "hooks.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	hook := hooks.Hook{ID: "lint", Name: "Lint", Type: hooks.HookTypeFile, Engine: hooks.HookEngineScript, Pattern: "**/*.go"}
	if err := st.RefreshDefinitions(ctx, []hooks.Hook{hook}); err != nil {
		t.Fatalf("refresh definitions: %v", err)
	}
	r := &runtimeState{
		cfg:         Config{RepoRoot: repo},
		store:       st,
		discovery:   &parser.Discovery{Hooks: []hooks.Hook{hook}},
		ctx:         ctx,
		drainSignal: make(chan struct{}, 1),
	}
	snapshotTime := time.Now().UTC()
	snapshot := map[string]watcher.Entry{
		"app.go": {Path: "app.go", Size: 13, ModTime: snapshotTime},
	}
	r.addBatch([]watcher.Change{{Path: "app.go", Kind: watcher.Modified}}, snapshot)

	r.flushBatch()

	pending, err := st.NextPending(ctx)
	if err != nil {
		t.Fatalf("next pending: %v", err)
	}
	if pending == nil || pending.HookID != "lint" || len(pending.ChangedFiles) != 1 || pending.ChangedFiles[0].Path != "app.go" {
		t.Fatalf("expected fail-open ignore path to queue hook, got %#v", pending)
	}
	events, err := st.ListEvents(ctx, hookstore.EventQuery{Limit: 10})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if !hasStoreEvent(events, "file.change.ignore.failed") {
		t.Fatalf("expected audited ignore failure event, got %#v", events)
	}
}

func TestInitialWorkingTreeChangesExpandsStagedRename(t *testing.T) {
	repo := t.TempDir()
	gitTestCommand(t, repo, "init")
	gitTestCommand(t, repo, "config", "user.email", "test@example.com")
	gitTestCommand(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "old.go"), []byte("package old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestCommand(t, repo, "add", "old.go")
	gitTestCommand(t, repo, "commit", "-m", "initial")
	gitTestCommand(t, repo, "mv", "old.go", "new.go")

	changes := initialWorkingTreeChanges(repo)
	if len(changes) != 2 {
		t.Fatalf("expected two rename changes, got %#v", changes)
	}
	want := []watcher.Change{
		{Path: "new.go", Kind: watcher.Created},
		{Path: "old.go", Kind: watcher.Deleted},
	}
	for i := range want {
		if changes[i] != want[i] {
			t.Fatalf("change %d = %#v, want %#v; all changes %#v", i, changes[i], want[i], changes)
		}
	}
}

func TestCaptureWorkspaceSnapshotIncludesUntrackedAndOmitsLargeFiles(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	gitTestCommand(t, repo, "init")
	gitTestCommand(t, repo, "config", "user.email", "test@example.com")
	gitTestCommand(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestCommand(t, repo, "add", "tracked.txt")
	gitTestCommand(t, repo, "commit", "-m", "initial")

	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\nchange\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "big.bin"), bytes.Repeat([]byte("x"), int(defaultSnapshotMaxFileBytes)+1), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := hookstore.Open(ctx, hookstore.Options{Path: filepath.Join(t.TempDir(), "hooks.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	tempDir := filepath.Join(t.TempDir(), "session-tmp")
	r := &runtimeState{cfg: Config{RepoRoot: repo, TempDir: tempDir}, store: st, ctx: ctx}

	snapshot, err := r.captureWorkspaceSnapshot(ctx)
	if err != nil {
		t.Fatalf("capture snapshot: %v", err)
	}
	if snapshot == nil {
		t.Fatal("expected snapshot")
	}
	patch := string(snapshot.Patch)
	if !strings.Contains(patch, "tracked.txt") || !strings.Contains(patch, "new.txt") {
		t.Fatalf("snapshot patch missing tracked or untracked change:\n%s", patch)
	}
	if strings.Contains(patch, "big.bin") {
		t.Fatalf("snapshot patch captured oversized file:\n%s", patch)
	}
	if len(snapshot.OmittedFiles) != 1 || snapshot.OmittedFiles[0].Path != "big.bin" || snapshot.OmittedFiles[0].Reason != "too_large" {
		t.Fatalf("expected big.bin omission, got %#v", snapshot.OmittedFiles)
	}
	refs := gitTestOutput(t, repo, "for-each-ref", "--format=%(refname)", "refs/discobox", "refs/discobot")
	if strings.TrimSpace(refs) != "" {
		t.Fatalf("snapshot should not create git refs, got %q", refs)
	}
	if entries, err := os.ReadDir(tempDir); err != nil {
		t.Fatalf("read temp dir: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("snapshot temp dir should be clean after capture, got %d entries", len(entries))
	}
}

func TestCaptureWorkspaceSnapshotSkipsUnchangedOmittedOnlySnapshot(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	gitTestCommand(t, repo, "init")
	gitTestCommand(t, repo, "config", "user.email", "test@example.com")
	gitTestCommand(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestCommand(t, repo, "add", "tracked.txt")
	gitTestCommand(t, repo, "commit", "-m", "initial")
	if err := os.WriteFile(filepath.Join(repo, "big.bin"), bytes.Repeat([]byte("x"), int(defaultSnapshotMaxFileBytes)+1), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := hookstore.Open(ctx, hookstore.Options{Path: filepath.Join(t.TempDir(), "hooks.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	r := &runtimeState{cfg: Config{RepoRoot: repo}, store: st, ctx: ctx}

	first, err := r.captureWorkspaceSnapshot(ctx)
	if err != nil {
		t.Fatalf("first capture: %v", err)
	}
	if first == nil {
		t.Fatal("expected first omitted-only snapshot")
	}
	second, err := r.captureWorkspaceSnapshot(ctx)
	if err != nil {
		t.Fatalf("second capture: %v", err)
	}
	if second != nil {
		t.Fatalf("expected unchanged omitted-only snapshot to be skipped, got %#v", second)
	}
	snapshots, err := st.ListWorkspaceSnapshots(ctx, 0)
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("expected one stored snapshot, got %d: %#v", len(snapshots), snapshots)
	}
}

func TestCaptureWorkspaceSnapshotRenamedFileRemovesOldPath(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	gitTestCommand(t, repo, "init")
	gitTestCommand(t, repo, "config", "user.email", "test@example.com")
	gitTestCommand(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "old.txt"), []byte("contents\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestCommand(t, repo, "add", "old.txt")
	gitTestCommand(t, repo, "commit", "-m", "initial")
	gitTestCommand(t, repo, "mv", "old.txt", "renamed.txt")

	st, err := hookstore.Open(ctx, hookstore.Options{Path: filepath.Join(t.TempDir(), "hooks.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	r := &runtimeState{cfg: Config{RepoRoot: repo}, store: st, ctx: ctx}

	snapshot, err := r.captureWorkspaceSnapshot(ctx)
	if err != nil {
		t.Fatalf("capture snapshot: %v", err)
	}
	if snapshot == nil {
		t.Fatal("expected snapshot")
	}
	patch := string(snapshot.Patch)
	if !strings.Contains(patch, "old.txt") || !strings.Contains(patch, "renamed.txt") {
		t.Fatalf("snapshot patch should include both old and renamed paths:\n%s", patch)
	}
	got := map[string]watcher.ChangeKind{}
	for _, changed := range snapshot.ChangedFiles {
		got[changed.Path] = changed.Kind
	}
	if got["old.txt"] != watcher.Deleted || got["renamed.txt"] != watcher.Created {
		t.Fatalf("snapshot changed files = %#v, want old deleted and renamed created", snapshot.ChangedFiles)
	}
}

func TestNewRuntimeCleansSessionTempDir(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	tempDir := filepath.Join(t.TempDir(), "runtime", "tmp")
	staleDir := filepath.Join(tempDir, "snapshot-stale")
	if err := os.MkdirAll(staleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleDir, "index"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := newRuntime(ctx, Config{
		SessionID:  "session-temp-test",
		RepoRoot:   repo,
		DBPath:     filepath.Join(t.TempDir(), "hooks.db"),
		SocketPath: filepath.Join(t.TempDir(), "daemon.sock"),
		TempDir:    tempDir,
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	defer r.cancel()
	defer r.store.Close()
	if _, err := os.Stat(filepath.Join(staleDir, "index")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale temp file stat err = %v, want not exist", err)
	}
	if entries, err := os.ReadDir(tempDir); err != nil {
		t.Fatalf("read temp dir: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("temp dir should be empty after startup cleanup, got %d entries", len(entries))
	}
}

func TestSnapshotLoopSchedulesAgainWhenChangeArrivesDuringSnapshot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan int, 2)
	releaseFirst := make(chan struct{})
	calls := 0
	r := &runtimeState{
		cfg:            Config{SnapshotDebounce: 10 * time.Millisecond},
		ctx:            ctx,
		cancel:         cancel,
		snapshotSignal: make(chan struct{}, 1),
		snapshotCapture: func(context.Context) (*hookstore.WorkspaceSnapshot, error) {
			calls++
			started <- calls
			if calls == 1 {
				<-releaseFirst
			}
			return &hookstore.WorkspaceSnapshot{ID: fmt.Sprintf("snap-%d", calls)}, nil
		},
	}
	go r.snapshotLoop()

	r.requestSnapshot()
	select {
	case got := <-started:
		if got != 1 {
			t.Fatalf("first snapshot call = %d", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first snapshot")
	}
	r.requestSnapshot()
	close(releaseFirst)
	select {
	case got := <-started:
		if got != 2 {
			t.Fatalf("second snapshot call = %d", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for rescheduled snapshot")
	}
}

func TestSnapshotLoopRateLimitsCaptures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan time.Time, 2)
	r := &runtimeState{
		cfg: Config{
			SnapshotDebounce:    10 * time.Millisecond,
			SnapshotMinInterval: 100 * time.Millisecond,
		},
		ctx:            ctx,
		cancel:         cancel,
		snapshotSignal: make(chan struct{}, 1),
		snapshotCapture: func(context.Context) (*hookstore.WorkspaceSnapshot, error) {
			started <- time.Now()
			return &hookstore.WorkspaceSnapshot{ID: "snap"}, nil
		},
	}
	go r.snapshotLoop()

	r.requestSnapshot()
	var first time.Time
	select {
	case first = <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first snapshot")
	}
	r.requestSnapshot()
	select {
	case got := <-started:
		t.Fatalf("second snapshot started too soon after %s", got.Sub(first))
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case second := <-started:
		if delta := second.Sub(first); delta < 90*time.Millisecond {
			t.Fatalf("second snapshot started after %s, want at least 90ms", delta)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for rate-limited second snapshot")
	}
}

func TestInitialWorkingTreeChanges(t *testing.T) {
	repo := t.TempDir()
	gitTestCommand(t, repo, "init")
	gitTestCommand(t, repo, "config", "user.email", "test@example.com")
	gitTestCommand(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("clean\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "deleted.txt"), []byte("delete\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestCommand(t, repo, "add", ".")
	gitTestCommand(t, repo, "commit", "-m", "initial")

	if changes := initialWorkingTreeChanges(repo); len(changes) != 0 {
		t.Fatalf("clean repo initial changes = %#v, want none", changes)
	}

	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, "deleted.txt")); err != nil {
		t.Fatal(err)
	}

	changes := initialWorkingTreeChanges(repo)
	got := map[string]watcher.ChangeKind{}
	for _, change := range changes {
		got[change.Path] = change.Kind
	}
	want := map[string]watcher.ChangeKind{
		"deleted.txt":   watcher.Deleted,
		"tracked.txt":   watcher.Modified,
		"untracked.txt": watcher.Created,
	}
	if len(got) != len(want) {
		t.Fatalf("initial changes = %#v, want %#v", changes, want)
	}
	for path, kind := range want {
		if got[path] != kind {
			t.Fatalf("change %s = %q, want %q; all changes %#v", path, got[path], kind, changes)
		}
	}
}

func hookStatusByID(t *testing.T, st *hookstore.Store, ctx context.Context, hookID string) hookstore.StatusRow {
	t.Helper()
	statuses, err := st.ListStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range statuses {
		if status.Hook.ID == hookID {
			return status
		}
	}
	t.Fatalf("missing hook status for %q: %#v", hookID, statuses)
	return hookstore.StatusRow{}
}

func hasStoreEvent(events []hookstore.Event, eventType string) bool {
	for _, event := range events {
		if event.Type == eventType {
			return true
		}
	}
	return false
}

func gitTestCommand(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, string(out))
	}
}

func gitTestOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, string(out))
	}
	return string(out)
}

func TestRecordObservedChangesDuplicatesDetailsIntoAuditEvent(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	st, err := hookstore.Open(ctx, hookstore.Options{Path: filepath.Join(t.TempDir(), "hooks.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	r := &runtimeState{cfg: Config{RepoRoot: repo}, store: st}
	recorded, err := r.recordObservedChanges(ctx, []watcher.Change{{Path: "src/app.go", Kind: watcher.Modified}})
	if err != nil {
		t.Fatalf("record observed changes: %v", err)
	}
	if len(recorded) != 1 {
		t.Fatalf("expected one observed change, got %#v", recorded)
	}

	events, err := st.ListEvents(ctx, hookstore.EventQuery{Limit: 10})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 || events[0].Type != "file.change.observed" {
		t.Fatalf("expected file.change.observed event, got %#v", events)
	}
	details := events[0].Details
	if details["change_id"] != recorded[0].ID || details["path"] != "src/app.go" || details["kind"] != string(watcher.Modified) {
		t.Fatalf("event details do not duplicate observed change identity: %#v", details)
	}
	if _, ok := details["base_commit"]; !ok {
		t.Fatalf("event details missing base_commit: %#v", details)
	}
	if _, ok := details["diff"]; !ok {
		t.Fatalf("event details missing diff: %#v", details)
	}
	if _, ok := details["created_at"]; !ok {
		t.Fatalf("event details missing created_at: %#v", details)
	}
}

func TestEventsStreamSendsNewEvents(t *testing.T) {
	ctx := context.Background()
	st, err := hookstore.Open(ctx, hookstore.Options{Path: filepath.Join(t.TempDir(), "hooks.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if _, err := st.RecordEvent(ctx, hookstore.Event{Type: "daemon.shutdown.requested", Details: map[string]any{"session_id": "s1", "repo_root": "/repo"}}); err != nil {
		t.Fatalf("record initial event: %v", err)
	}

	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r := &runtimeState{store: st, ctx: rctx, cancel: cancel}
	server := httptest.NewServer(r.routes())
	defer server.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/events/stream?limit=10", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected stream status: %s", resp.Status)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("unexpected content type %q", got)
	}

	if _, err := st.RecordEvent(ctx, hookstore.Event{Type: "workspace.snapshot.failed", Message: "hello", Details: map[string]any{"repo_root": "/repo", "error": "boom"}}); err != nil {
		t.Fatalf("record streamed event: %v", err)
	}

	scanner := bufio.NewScanner(resp.Body)
	deadline := time.After(3 * time.Second)
	lines := make(chan string, 1)
	go func() {
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	for {
		select {
		case line := <-lines:
			if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"type":"workspace.snapshot.failed"`) && strings.Contains(line, `"message":"hello"`) {
				return
			}
			if strings.Contains(line, `"type":"daemon.shutdown.requested"`) {
				t.Fatalf("stream replayed existing event: %q", line)
			}
		case <-deadline:
			t.Fatal("timed out waiting for streamed event")
		}
	}
}

func hasEvent(events []model.Event, eventType, hookID string) bool {
	for _, event := range events {
		if event.Type == eventType && event.HookID == hookID {
			return true
		}
	}
	return false
}

func modelEventByType(events []model.Event, eventType, hookID string) *model.Event {
	for i := range events {
		if events[i].Type == eventType && events[i].HookID == hookID {
			return &events[i]
		}
	}
	return nil
}

func unixHTTPClient(socketPath string) *http.Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &http.Client{Transport: transport, Timeout: 2 * time.Second}
}

func waitForJSON(t *testing.T, client *http.Client, method, url string, body any, out any) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		lastErr = tryJSON(client, method, url, body, out)
		if lastErr == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("request never succeeded: %v", lastErr)
}

func doJSON(t *testing.T, client *http.Client, method, url string, body any, out any) {
	t.Helper()
	if err := tryJSON(client, method, url, body, out); err != nil {
		t.Fatal(err)
	}
}

func tryJSON(client *http.Client, method, url string, body any, out any) error {
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s returned %s", method, url, resp.Status)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
