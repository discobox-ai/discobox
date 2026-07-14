package sandboxes

import (
	"context"
	"testing"

	serverapi "github.com/obot-platform/discobox/api/gen"
	"github.com/obot-platform/discobox/server/internal/model"
)

func TestResolveHarnessConfigIDDefaultsToProjectDefault(t *testing.T) {
	ctx := context.Background()
	svc, st := newBindingFixture(t)
	cfg := codexConfig(t, st)

	unset := serverapi.OptString{}

	// No explicit selector + a project default → the default is pinned.
	project := &model.Project{ID: "project-1", DefaultHarnessConfigID: cfg.ID}
	got, err := svc.resolveHarnessConfigID(ctx, project, unset, unset)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got == nil || *got != cfg.ID {
		t.Fatalf("resolved = %v, want %q", got, cfg.ID)
	}

	// No default set → unresolved (harness-less sandbox).
	if got, err := svc.resolveHarnessConfigID(ctx, &model.Project{ID: "project-1"}, unset, unset); err != nil || got != nil {
		t.Fatalf("no-default resolve = %v, %v; want nil, nil", got, err)
	}

	// Default points at a deleted config → unresolved, not an error.
	stale := &model.Project{ID: "project-1", DefaultHarnessConfigID: "does-not-exist"}
	if got, err := svc.resolveHarnessConfigID(ctx, stale, unset, unset); err != nil || got != nil {
		t.Fatalf("stale-default resolve = %v, %v; want nil, nil", got, err)
	}

	// An explicit selector still wins over the default.
	if got, err := svc.resolveHarnessConfigID(ctx, project, unset, serverapi.NewOptString("codex")); err != nil || got == nil || *got != cfg.ID {
		t.Fatalf("slug resolve = %v, %v; want %q", got, err, cfg.ID)
	}
}
