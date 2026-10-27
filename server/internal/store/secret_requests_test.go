package store_test

import (
	"context"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
)

func TestListSecretRequestsStatusFilter(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	for _, st := range []string{model.SecretRequestStatusPending, model.SecretRequestStatusPending, model.SecretRequestStatusApproved} {
		if err := s.CreateSecretRequest(ctx, &model.SecretRequest{
			ProjectID: "project-1", RequestedBy: "sandbox:sb-1", SandboxID: "sb-1",
			Routes: []model.SecretRoute{{Host: "api.example.com"}}, Status: st,
		}); err != nil {
			t.Fatalf("create request: %v", err)
		}
	}

	all, err := s.ListSecretRequests(ctx, "project-1", "")
	if err != nil || len(all) != 3 {
		t.Fatalf("all = %d (err=%v), want 3", len(all), err)
	}
	pending, err := s.ListSecretRequests(ctx, "project-1", model.SecretRequestStatusPending)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending = %d (err=%v), want 2", len(pending), err)
	}
	approved, err := s.ListSecretRequests(ctx, "project-1", model.SecretRequestStatusApproved)
	if err != nil || len(approved) != 1 {
		t.Fatalf("approved = %d (err=%v), want 1", len(approved), err)
	}
	if approved[0].SandboxID != "sb-1" {
		t.Fatalf("sandbox id = %q, want sb-1", approved[0].SandboxID)
	}
}

// Deleting a sandbox closes its open asks as denied — the agent that would
// read the answer is gone — and leaves other sandboxes' asks and answered ones
// alone. A refresh request is about the secret, not the sandbox, and stays
// open.
func TestDeletingASandboxDeniesItsOpenRequests(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreWithDB(t, nil)
	createTestPool(t, s, "project-1", "pool-1")
	for _, id := range []string{"sb-1", "sb-2"} {
		if err := s.CreateSandbox(ctx, &model.Sandbox{ID: id, ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: id}); err != nil {
			t.Fatalf("create sandbox: %v", err)
		}
	}
	request := func(sandboxID, status string) string {
		req := &model.SecretRequest{
			ProjectID: "project-1", RequestedBy: "agent:" + sandboxID, SandboxID: sandboxID,
			Routes: []model.SecretRoute{{Host: "api.example.com"}}, Status: status,
		}
		if err := s.CreateSecretRequest(ctx, req); err != nil {
			t.Fatalf("create request: %v", err)
		}
		return req.ID
	}
	open := request("sb-1", model.SecretRequestStatusPending)
	approved := request("sb-1", model.SecretRequestStatusApproved)
	other := request("sb-2", model.SecretRequestStatusPending)
	refresh := &model.SecretRefreshRequest{ProjectID: "project-1", SecretID: "sec-1", SandboxID: "sb-1", Cause: model.SecretRefreshCauseStale, Status: model.SecretRefreshStatusPending}
	if err := s.CreateSecretRefreshRequest(ctx, refresh); err != nil {
		t.Fatalf("create refresh request: %v", err)
	}

	if err := s.DeleteSandbox(ctx, "project-1", "sb-1"); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}

	for id, want := range map[string]string{
		open:     model.SecretRequestStatusDenied,
		approved: model.SecretRequestStatusApproved,
		other:    model.SecretRequestStatusPending,
	} {
		req, err := s.GetSecretRequest(ctx, "project-1", id)
		if err != nil {
			t.Fatalf("get request: %v", err)
		}
		if req.Status != want {
			t.Fatalf("request %s (sandbox %s) = %s, want %s", id, req.SandboxID, req.Status, want)
		}
		if id == open && (req.DecidedAt == nil || req.DecidedBy != model.SecretBindingActorSystem) {
			t.Fatal("the denied request was not stamped decided")
		}
	}
	if got, err := s.GetSecretRefreshRequest(ctx, "project-1", refresh.ID); err != nil || got.Status != model.SecretRefreshStatusPending {
		t.Fatalf("refresh request = %+v, %v; want it open", got, err)
	}
}
