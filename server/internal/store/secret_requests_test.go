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
			Type: model.SecretTypeToken, Host: "api.example.com", Status: st,
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
