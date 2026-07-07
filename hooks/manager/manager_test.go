package manager

import (
	"context"
	"path/filepath"
	"testing"

	hooks "github.com/obot-platform/discobox/hooks"
	"github.com/obot-platform/discobox/hooks/api/model"
	"github.com/obot-platform/discobox/hooks/models"
	"github.com/obot-platform/discobox/hooks/store"
	"github.com/obot-platform/discobox/hooks/watcher"
)

func TestRunHookActivatesHookWhenAlreadyQueued(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "hooks.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	review := hooks.Hook{ID: "review", Name: "Review", Type: hooks.HookTypeFile, Engine: hooks.HookEngineScript, Phase: "review"}
	if err := st.RefreshDefinitions(ctx, []hooks.Hook{review}); err != nil {
		t.Fatalf("refresh definitions: %v", err)
	}
	if err := st.Enqueue(ctx, []string{"review"}, []models.ChangedFile{{Path: "review.go", Kind: watcher.Modified}}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	signaled := false
	mgr, err := New(Config{Store: st, Hooks: []hooks.Hook{review}, SessionID: "s1", RepoRoot: t.TempDir(), SignalRun: func() { signaled = true }})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	resp, err := mgr.RunHook(ctx, "review", model.RunRequest{})
	if err != nil {
		t.Fatalf("run hook: %v", err)
	}
	if !resp.Skipped || resp.Reason != "already_queued" {
		t.Fatalf("expected already_queued skip, got %#v", resp)
	}
	if !signaled {
		t.Fatal("expected drain signal")
	}
	active := mgr.ActiveHookIDs()
	if len(active) != 1 || active[0] != "review" {
		t.Fatalf("expected active review hook, got %v", active)
	}
}

func TestRunHookActivatesLSPHook(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "hooks.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	lsp := hooks.Hook{ID: "go-lsp", Name: "Go LSP", Type: hooks.HookTypeFile, Engine: hooks.HookEngineLSP, Pattern: "**/*.go", LanguageID: "go"}
	if err := st.RefreshDefinitions(ctx, []hooks.Hook{lsp}); err != nil {
		t.Fatalf("refresh definitions: %v", err)
	}

	activated := ""
	mgr, err := New(Config{
		Store: st, Hooks: []hooks.Hook{lsp}, SessionID: "s1", RepoRoot: t.TempDir(),
		ActivateLSP: func(hookID string, _ bool) (bool, error) {
			activated = hookID
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	resp, err := mgr.RunHook(ctx, "go-lsp", model.RunRequest{})
	if err != nil {
		t.Fatalf("run hook: %v", err)
	}
	if resp.Skipped || !resp.Enqueued {
		t.Fatalf("expected LSP run to activate, got %#v", resp)
	}
	if activated != "go-lsp" {
		t.Fatalf("expected activation of go-lsp, got %q", activated)
	}
}

func TestRunHookSkipsLSPWithoutActivator(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "hooks.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	lsp := hooks.Hook{ID: "go-lsp", Name: "Go LSP", Type: hooks.HookTypeFile, Engine: hooks.HookEngineLSP, Pattern: "**/*.go", LanguageID: "go"}
	if err := st.RefreshDefinitions(ctx, []hooks.Hook{lsp}); err != nil {
		t.Fatalf("refresh definitions: %v", err)
	}

	mgr, err := New(Config{Store: st, Hooks: []hooks.Hook{lsp}, SessionID: "s1", RepoRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	resp, err := mgr.RunHook(ctx, "go-lsp", model.RunRequest{})
	if err != nil {
		t.Fatalf("run hook: %v", err)
	}
	if !resp.Skipped || resp.Reason != "lsp_hooks_run_continuously" {
		t.Fatalf("expected continuous-run skip, got %#v", resp)
	}
}
