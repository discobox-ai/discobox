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
			Type: model.SecretTypeToken, Hosts: []string{"api.example.com"}, Status: st,
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
// read the answer is gone — but leaves a refresh request open, since it is
// about the secret rather than the sandbox, and leaves other sandboxes' asks
// and answered ones alone.
func TestDeletingASandboxDeniesItsOpenRequests(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreWithDB(t, nil)
	createTestPool(t, s, "project-1", "pool-1")
	for _, id := range []string{"sb-1", "sb-2"} {
		if err := s.CreateSandbox(ctx, &model.Sandbox{ID: id, ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: id}); err != nil {
			t.Fatalf("create sandbox: %v", err)
		}
	}
	request := func(sandboxID, status, reason string) string {
		req := &model.SecretRequest{
			ProjectID: "project-1", RequestedBy: "sandbox:" + sandboxID, SandboxID: sandboxID,
			Type: model.SecretTypeToken, Hosts: []string{"api.example.com"}, Status: status, Reason: reason,
		}
		if reason == model.SecretRequestReasonRefresh {
			req.SecretID = "sec-1"
		}
		if err := s.CreateSecretRequest(ctx, req); err != nil {
			t.Fatalf("create request: %v", err)
		}
		return req.ID
	}
	open := request("sb-1", model.SecretRequestStatusPending, "")
	approved := request("sb-1", model.SecretRequestStatusApproved, "")
	refresh := request("sb-1", model.SecretRequestStatusPending, model.SecretRequestReasonRefresh)
	other := request("sb-2", model.SecretRequestStatusPending, "")

	if err := s.DeleteSandbox(ctx, "project-1", "sb-1"); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}

	for id, want := range map[string]string{
		open:     model.SecretRequestStatusDenied,
		approved: model.SecretRequestStatusApproved,
		refresh:  model.SecretRequestStatusPending,
		other:    model.SecretRequestStatusPending,
	} {
		req, err := s.GetSecretRequest(ctx, "project-1", id)
		if err != nil {
			t.Fatalf("get request: %v", err)
		}
		if req.Status != want {
			t.Fatalf("request %s (sandbox %s, reason %q) = %s, want %s", id, req.SandboxID, req.Reason, req.Status, want)
		}
		if id == open && req.ClosedAt == nil {
			t.Fatal("the denied request was not stamped closed")
		}
	}
}
