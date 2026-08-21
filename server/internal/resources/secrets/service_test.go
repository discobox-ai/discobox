package secrets_test

import (
	"context"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func TestCreateSecretRequestAllowsNoMatchingSecret(t *testing.T) {
	ctx := testPrincipalContext()
	svc := newTestService(t)

	req, err := svc.CreateSecretRequest(ctx, "project-1", services.CreateSecretRequestBody{
		Type: serverapi.CreateSecretRequestBodyTypeGit,
		Host: serverapi.NewOptString("github.com"),
	})
	if err != nil {
		t.Fatalf("create secret request: %v", err)
	}
	if req.Status != model.SecretRequestStatusPending {
		t.Fatalf("status = %q, want %q", req.Status, model.SecretRequestStatusPending)
	}
	if req.SecretID != "" {
		t.Fatalf("secret id = %q, want empty", req.SecretID)
	}
}

func TestApproveSecretRequestUsesSelectedSecretID(t *testing.T) {
	ctx := testPrincipalContext()
	svc := newTestService(t)

	selected, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
		Name: "selected bearer token",
		Type: serverapi.CreateSecretBodyTypeBearer,
		Value: serverapi.SecretValue{
			Token: serverapi.NewOptString("selected-token"),
		},
	})
	if err != nil {
		t.Fatalf("create selected secret: %v", err)
	}

	req, err := svc.CreateSecretRequest(ctx, "project-1", services.CreateSecretRequestBody{
		Type: serverapi.CreateSecretRequestBodyTypeGit,
		Host: serverapi.NewOptString("github.com"),
	})
	if err != nil {
		t.Fatalf("create advisory request: %v", err)
	}

	approved, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
		SecretId: selected.ID,
	})
	if err != nil {
		t.Fatalf("approve secret request: %v", err)
	}
	if approved.Status != model.SecretRequestStatusApproved {
		t.Fatalf("status = %q, want %q", approved.Status, model.SecretRequestStatusApproved)
	}
	if approved.SecretID != selected.ID {
		t.Fatalf("secret id = %q, want %q", approved.SecretID, selected.ID)
	}
	if approved.GrantID == "" {
		t.Fatal("approved request should reference the minted grant")
	}

	// Approval mints a standing grant. A non-sandbox request defaults to project scope.
	grants, err := svc.ListSecretGrants(ctx, "project-1", selected.ID)
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("grants = %d, want 1", len(grants))
	}
	if grants[0].ID != approved.GrantID {
		t.Fatalf("grant id = %q, want %q", grants[0].ID, approved.GrantID)
	}
	if grants[0].Scope != model.SecretGrantScopeProject || grants[0].ScopeKey != "project-1" {
		t.Fatalf("grant scope = %q/%q, want project/project-1", grants[0].Scope, grants[0].ScopeKey)
	}
}

func newTestService(t *testing.T) *resourcesecrets.Service {
	t.Helper()

	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	if err := db.Write.WithContext(ctx).Create(&model.Project{
		ID:          "project-1",
		OwnerUserID: "user-1",
		Name:        "Project",
	}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}

	return resourcesecrets.NewService(store.New(db.Write, db.Read))
}

func testPrincipalContext() context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{
		Type:   auth.PrincipalTypeUser,
		UserID: "user-1",
	})
}
