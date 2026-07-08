package secrets_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/obot-platform/discobox/server/internal/apperrors"
	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/model"
	resourcesecrets "github.com/obot-platform/discobox/server/internal/resources/secrets"
	"github.com/obot-platform/discobox/server/internal/store"
)

func newResolveFixture(t *testing.T) (*resourcesecrets.Service, *store.Store) {
	t.Helper()
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	if err := db.Write.WithContext(ctx).Create(&model.Project{
		ID: "project-1", OwnerUserID: "user-1", Name: "Project", Slug: "project",
	}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	st := store.New(db.Write, db.Read)
	return resourcesecrets.NewService(st), st
}

func createSandbox(t *testing.T, st *store.Store, sandboxID, workerID string) {
	t.Helper()
	sb := &model.Sandbox{
		ID:              sandboxID,
		ProjectID:       "project-1",
		CreatedByUserID: "user-1",
		Name:            sandboxID,
		WorkerID:        &workerID,
	}
	if err := st.CreateSandbox(context.Background(), sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
}

func mustTokenValue(t *testing.T, token string) []byte {
	t.Helper()
	//nolint:gosec // Test marshals a secret value before store encryption.
	b, err := json.Marshal(model.SecretValue{Token: token})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestResolveSandboxSecretAutoApproveReturnsValueAndReusesGrant(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	sec := &model.Secret{
		ProjectID: "project-1", Name: "auto", Type: model.SecretTypeBearer,
		AutoApprove: true, DefaultGrantTTL: 3600, EncryptedValue: mustTokenValue(t, "real-token"),
	}
	if err := st.CreateSecret(ctx, sec); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	createSandbox(t, st, "sb-1", "worker-1")
	if err := st.CreateSandboxSecret(ctx, &model.SandboxSecret{
		ProjectID: "project-1", SandboxID: "sb-1", SecretID: sec.ID, EnvName: "FOO", Sentinel: "SENTINEL-A",
	}); err != nil {
		t.Fatalf("create assignment: %v", err)
	}

	req, err := svc.ResolveSandboxSecret(ctx, "worker-1", "sb-1", "SENTINEL-A", "api.example.com")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if req.Status != model.SecretRequestStatusApproved {
		t.Fatalf("status = %q, want approved", req.Status)
	}
	if req.Value == nil || req.Value.Token != "real-token" {
		t.Fatalf("value = %#v, want real-token", req.Value)
	}

	again, err := svc.ResolveSandboxSecret(ctx, "worker-1", "sb-1", "SENTINEL-A", "api.example.com")
	if err != nil {
		t.Fatalf("resolve again: %v", err)
	}
	if again.ID != req.ID {
		t.Fatalf("expected grant reuse, got new request %s vs %s", again.ID, req.ID)
	}
}

func TestResolveSandboxSecretPendingWithoutAutoApprove(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	sec := &model.Secret{
		ProjectID: "project-1", Name: "manual", Type: model.SecretTypeBearer,
		DefaultGrantTTL: 3600, EncryptedValue: mustTokenValue(t, "real-token"),
	}
	if err := st.CreateSecret(ctx, sec); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	createSandbox(t, st, "sb-1", "worker-1")
	if err := st.CreateSandboxSecret(ctx, &model.SandboxSecret{
		ProjectID: "project-1", SandboxID: "sb-1", SecretID: sec.ID, EnvName: "FOO", Sentinel: "SENTINEL-B",
	}); err != nil {
		t.Fatalf("create assignment: %v", err)
	}

	req, err := svc.ResolveSandboxSecret(ctx, "worker-1", "sb-1", "SENTINEL-B", "api.example.com")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if req.Status != model.SecretRequestStatusPending {
		t.Fatalf("status = %q, want pending", req.Status)
	}
	if req.Value != nil {
		t.Fatal("pending request must not carry a value")
	}
	if req.SecretID != sec.ID {
		t.Fatalf("secret id = %q, want %q", req.SecretID, sec.ID)
	}

	// A second resolve for the same host reuses the pending request.
	again, err := svc.ResolveSandboxSecret(ctx, "worker-1", "sb-1", "SENTINEL-B", "api.example.com")
	if err != nil {
		t.Fatalf("resolve again: %v", err)
	}
	if again.ID != req.ID {
		t.Fatalf("expected pending reuse, got %s vs %s", again.ID, req.ID)
	}
}

func TestResolveSandboxSecretUnknownSentinel(t *testing.T) {
	ctx := context.Background()
	svc, _ := newResolveFixture(t)

	_, err := svc.ResolveSandboxSecret(ctx, "worker-1", "sb-1", "NOPE", "api.example.com")
	if !errors.Is(err, apperrors.ErrNotFound) && !isNotFoundStatus(err) {
		t.Fatalf("err = %v, want not found", err)
	}
}

func isNotFoundStatus(err error) bool {
	var statusErr interface{ StatusCode() int }
	return errors.As(err, &statusErr) && statusErr.StatusCode() == 404
}
