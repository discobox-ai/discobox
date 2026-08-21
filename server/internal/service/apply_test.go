package service_test

import (
	"context"
	"net/http"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// primarySlug is the slug DefaultGitSourceSlugs assigns an unnamed primary
// source; withSource below sets it directly, bypassing normal create-time
// defaulting the same way park() does for source push.
const primarySlug = "primary"

// withSource gives an existing sandbox a primary source with a stable slug,
// so CompleteSandboxApply has something to match against. Unlike park(), it
// leaves the sandbox running: apply is a post-hoc client report, not part of
// the awaiting_source handshake, and should work regardless of phase.
func withSource(t *testing.T, st *store.Store, projectID, sandboxID string) {
	t.Helper()
	ctx := context.Background()
	sb, err := st.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	slug := primarySlug
	sb.Source = &model.GitSource{
		Kind: "git",
		Slug: &slug,
	}
	if err := st.UpdateSandbox(ctx, sb); err != nil {
		t.Fatalf("set source: %v", err)
	}
}

func TestCompleteSandboxApplyRecordsCommit(t *testing.T) {
	ctx := context.Background()
	svc, _, st, projectID := newSandboxTestService(t, nil)

	created, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{
		HarnessName: serverapi.NewOptString("shell"),
		Config:      serverapi.SandboxCreateConfig{Name: "alpha"},
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	withSource(t, st, projectID, created.ID)

	updated, err := svc.CompleteSandboxApply(ctx, projectID, created.ID, services.CompleteSandboxApplyBody{
		Slug:       primarySlug,
		Commit:     "0123456789abcdef0123456789abcdef01234567",
		HostCommit: "fedcba9876543210fedcba9876543210fedcba98",
		HostId:     "host_test",
		HostPath:   "/home/user/repo",
	})
	if err != nil {
		t.Fatalf("complete apply: %v", err)
	}
	if len(updated.AppliedCommits) != 1 {
		t.Fatalf("applied commits = %d, want 1", len(updated.AppliedCommits))
	}
	entry := updated.AppliedCommits[0]
	if entry.Slug != primarySlug {
		t.Errorf("slug = %q, want %q", entry.Slug, primarySlug)
	}
	if entry.Commit != "0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("commit = %q", entry.Commit)
	}
	if entry.HostCommit != "fedcba9876543210fedcba9876543210fedcba98" {
		t.Errorf("hostCommit = %q", entry.HostCommit)
	}
	if entry.HostID != "host_test" {
		t.Errorf("hostId = %q", entry.HostID)
	}
	if entry.HostPath != "/home/user/repo" {
		t.Errorf("hostPath = %q", entry.HostPath)
	}
	if entry.AppliedAt.IsZero() {
		t.Error("appliedAt was not set")
	}
}

// Multiple applies over a sandbox's life append rather than replace, per ADR
// 0014: it is an audit trail, and §2's base-narrowing depends on the latest
// entry, not the only one.
func TestCompleteSandboxApplyAccumulates(t *testing.T) {
	ctx := context.Background()
	svc, _, st, projectID := newSandboxTestService(t, nil)

	created, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{
		HarnessName: serverapi.NewOptString("shell"),
		Config:      serverapi.SandboxCreateConfig{Name: "alpha"},
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	withSource(t, st, projectID, created.ID)

	first := services.CompleteSandboxApplyBody{
		Slug:       primarySlug,
		Commit:     "1111111111111111111111111111111111111a",
		HostCommit: "2222222222222222222222222222222222222b",
		HostId:     "host_test",
		HostPath:   "/home/user/repo",
	}
	if _, err := svc.CompleteSandboxApply(ctx, projectID, created.ID, first); err != nil {
		t.Fatalf("complete first apply: %v", err)
	}
	second := first
	second.Commit = "3333333333333333333333333333333333333c"
	second.HostCommit = "4444444444444444444444444444444444444d"
	updated, err := svc.CompleteSandboxApply(ctx, projectID, created.ID, second)
	if err != nil {
		t.Fatalf("complete second apply: %v", err)
	}
	if len(updated.AppliedCommits) != 2 {
		t.Fatalf("applied commits = %d, want 2", len(updated.AppliedCommits))
	}
	if updated.AppliedCommits[0].Commit != first.Commit || updated.AppliedCommits[1].Commit != second.Commit {
		t.Fatalf("applied commits out of order: %+v", updated.AppliedCommits)
	}
}

// A slug that names neither the primary source nor a secondary one has
// nothing for the apply to be recorded against.
func TestCompleteSandboxApplyRejectsUnknownSlug(t *testing.T) {
	ctx := context.Background()
	svc, _, st, projectID := newSandboxTestService(t, nil)

	created, err := svc.CreateSandbox(ctx, projectID, services.CreateSandboxBody{
		HarnessName: serverapi.NewOptString("shell"),
		Config:      serverapi.SandboxCreateConfig{Name: "alpha"},
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	withSource(t, st, projectID, created.ID)

	_, err = svc.CompleteSandboxApply(ctx, projectID, created.ID, services.CompleteSandboxApplyBody{
		Slug:       "not-a-real-source",
		Commit:     "0123456789abcdef0123456789abcdef01234567",
		HostCommit: "fedcba9876543210fedcba9876543210fedcba98",
		HostId:     "host_test",
		HostPath:   "/home/user/repo",
	})
	if err == nil {
		t.Fatal("completing an apply for an unknown slug: got nil error, want conflict")
	}
	assertStatusError(t, err, http.StatusConflict)
}
