package sandboxes

import (
	"context"
	"strings"
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
	if got != cfg.ID {
		t.Fatalf("resolved = %q, want %q", got, cfg.ID)
	}

	// An explicit selector still wins over the default.
	if got, err := svc.resolveHarnessConfigID(ctx, project, unset, serverapi.NewOptString("codex")); err != nil || got != cfg.ID {
		t.Fatalf("slug resolve = %q, %v; want %q", got, err, cfg.ID)
	}
}

// Nothing named and no default is an error, not a shell (ADR 0046). The chain
// used to end at the built-in `shell`, which answered a project with a harness
// configured and no default set with a sandbox that had no harness in it.
func TestResolveHarnessConfigIDFailsWithNothingToResolve(t *testing.T) {
	ctx := context.Background()
	svc, _ := newBindingFixture(t)
	unset := serverapi.OptString{}

	_, err := svc.resolveHarnessConfigID(ctx, &model.Project{ID: "project-1"}, unset, unset)
	if err == nil {
		t.Fatal("no harness and no default should fail rather than resolve to a shell")
	}
	// The refusal has to say what to do about it, both ways.
	if !strings.Contains(err.Error(), "--harness") || !strings.Contains(err.Error(), "set-default") {
		t.Fatalf("error = %q, want it to name both ways forward", err)
	}
}

// A default pointing at a config that has since been deleted is an absent
// default, and lands on the same refusal rather than a "not found".
func TestResolveHarnessConfigIDTreatsAStaleDefaultAsAbsent(t *testing.T) {
	ctx := context.Background()
	svc, _ := newBindingFixture(t)
	unset := serverapi.OptString{}

	stale := &model.Project{ID: "project-1", DefaultHarnessConfigID: "does-not-exist"}
	_, err := svc.resolveHarnessConfigID(ctx, stale, unset, unset)
	if err == nil {
		t.Fatal("a stale default should fail rather than resolve to a shell")
	}
	if !strings.Contains(err.Error(), "no default") {
		t.Fatalf("error = %q, want the absent-default refusal", err)
	}
}
