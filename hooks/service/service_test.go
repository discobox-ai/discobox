package service

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	hooks "github.com/discobox-ai/discobox/hooks"
	"github.com/discobox-ai/discobox/hooks/api/model"
	"github.com/discobox-ai/discobox/hooks/models"
	"github.com/discobox-ai/discobox/hooks/store"
	"github.com/discobox-ai/discobox/hooks/watcher"
)

type testHookSet map[string]hooks.Hook

func (s testHookSet) HookByID(id string) (hooks.Hook, bool) {
	h, ok := s[id]
	return h, ok
}

func TestRunHookSkipsAfterSuccessUnlessForced(t *testing.T) {
	ctx := context.Background()
	svc, st := newTestService(ctx, t)

	resp, err := svc.RunHook(ctx, "lint", model.RunRequest{})
	if err != nil {
		t.Fatalf("run hook: %v", err)
	}
	if !resp.Enqueued || resp.HookID != "lint" {
		t.Fatalf("unexpected run response: %#v", resp)
	}
	if err := st.Enqueue(ctx, []string{"lint"}, []models.ChangedFile{{Path: "old.go", Kind: watcher.Modified}}); err != nil {
		t.Fatalf("seed run inputs: %v", err)
	}
	run, err := st.MarkRunning(ctx, "lint", nil)
	if err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if err := st.FinishRun(ctx, run.ID, models.RunResult{Status: models.StatusSuccess}); err != nil {
		t.Fatalf("finish run: %v", err)
	}

	resp, err = svc.RunHook(ctx, "lint", model.RunRequest{})
	if err != nil {
		t.Fatalf("run hook after success: %v", err)
	}
	if !resp.Skipped || resp.Reason != "already_succeeded" {
		t.Fatalf("expected success skip, got %#v", resp)
	}

	resp, err = svc.RunHook(ctx, "lint", model.RunRequest{Force: true})
	if err != nil {
		t.Fatalf("forced run hook: %v", err)
	}
	if !resp.Enqueued || resp.Skipped {
		t.Fatalf("expected forced enqueue, got %#v", resp)
	}
	pending, err := st.NextPending(ctx)
	if err != nil {
		t.Fatalf("next pending: %v", err)
	}
	if pending == nil || len(pending.ChangedFiles) != 1 || pending.ChangedFiles[0].Path != "old.go" {
		t.Fatalf("expected forced run to copy previous inputs, got %#v", pending)
	}
}

func TestOutputAssemblesLatestRunLogs(t *testing.T) {
	ctx := context.Background()
	svc, st := newTestService(ctx, t)

	run, err := st.MarkRunning(ctx, "lint", nil)
	if err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if _, err := st.AppendHookLog(ctx, models.HookLog{HookID: "lint", RunID: run.ID, Line: "first"}); err != nil {
		t.Fatalf("append first log: %v", err)
	}
	if _, err := st.AppendHookLog(ctx, models.HookLog{HookID: "lint", RunID: run.ID, Line: "second"}); err != nil {
		t.Fatalf("append second log: %v", err)
	}

	out, err := svc.Output(ctx, "lint")
	if err != nil {
		t.Fatalf("output: %v", err)
	}
	if out.HookID != "lint" || out.Output != "first\nsecond\n" {
		t.Fatalf("unexpected output: %#v", out)
	}
}

func TestRunHookEnqueuesPhaseHooksDirectly(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "hooks.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	review := hooks.Hook{ID: "review", Name: "Review", Type: hooks.HookTypeSession, Engine: hooks.HookEngineScript, Phase: "review"}
	if err := st.RefreshDefinitions(ctx, []hooks.Hook{review}); err != nil {
		t.Fatalf("refresh definitions: %v", err)
	}
	svc, err := New(Config{Store: st, HookSet: testHookSet{"review": review}})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	resp, err := svc.RunHook(ctx, "review", model.RunRequest{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Skipped || !resp.Enqueued {
		t.Fatalf("expected enqueue, got %#v", resp)
	}
}

func TestHookOperationsReturnNotFound(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(ctx, t)
	if _, err := svc.SetHookExecution(ctx, "missing", model.ExecutionPatchRequest{Paused: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("set execution error = %v, want ErrNotFound", err)
	}
	if _, err := svc.RunHook(ctx, "missing", model.RunRequest{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("run error = %v, want ErrNotFound", err)
	}
	if _, err := svc.Output(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("output error = %v, want ErrNotFound", err)
	}
}

func newTestService(ctx context.Context, t *testing.T) (*Service, *store.Store) {
	t.Helper()
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "hooks.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	hook := hooks.Hook{ID: "lint", Name: "Lint", Type: hooks.HookTypeSession, Engine: hooks.HookEngineScript}
	if err := st.RefreshDefinitions(ctx, []hooks.Hook{hook}); err != nil {
		t.Fatalf("refresh definitions: %v", err)
	}
	svc, err := New(Config{Store: st, HookSet: testHookSet{"lint": hook}})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc, st
}
