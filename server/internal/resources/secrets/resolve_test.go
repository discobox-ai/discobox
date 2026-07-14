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
	createSandboxWithHarness(t, st, sandboxID, workerID, "")
}

func createSandboxWithHarness(t *testing.T, st *store.Store, sandboxID, workerID, harnessConfigID string) {
	t.Helper()
	sb := &model.Sandbox{
		ID:              sandboxID,
		ProjectID:       "project-1",
		CreatedByUserID: "user-1",
		Name:            sandboxID,
		WorkerID:        &workerID,
	}
	if harnessConfigID != "" {
		sb.HarnessConfigID = &harnessConfigID
	}
	if err := st.CreateSandbox(context.Background(), sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
}

func mustSecret(t *testing.T, st *store.Store, name, token string) *model.Secret {
	t.Helper()
	sec := &model.Secret{
		ProjectID: "project-1", Name: name, Type: model.SecretTypeBearer,
		DefaultGrantTTL: 3600, EncryptedValue: mustTokenValue(t, token),
	}
	if err := st.CreateSecret(context.Background(), sec); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	return sec
}

func mustGrant(t *testing.T, st *store.Store, secretID, scope, scopeKey string) {
	t.Helper()
	if err := st.CreateSecretGrant(context.Background(), &model.SecretGrant{
		ProjectID: "project-1", SecretID: secretID, Scope: scope, ScopeKey: scopeKey,
	}); err != nil {
		t.Fatalf("create grant: %v", err)
	}
}

func mustAssign(t *testing.T, st *store.Store, sandboxID, secretID, sentinel string) {
	t.Helper()
	if err := st.CreateSandboxSecret(context.Background(), &model.SandboxSecret{
		ProjectID: "project-1", SandboxID: sandboxID, SecretID: secretID, EnvName: "FOO", Sentinel: sentinel,
	}); err != nil {
		t.Fatalf("create assignment: %v", err)
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

func TestResolveSandboxSecretProjectGrantReturnsValue(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	sec := mustSecret(t, st, "prj", "real-token")
	mustGrant(t, st, sec.ID, model.SecretGrantScopeProject, "project-1")
	createSandbox(t, st, "sb-1", "worker-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-A")

	res, err := svc.ResolveSandboxSecret(ctx, "worker-1", "sb-1", "SENTINEL-A", "api.example.com")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Status != model.SecretRequestStatusApproved {
		t.Fatalf("status = %q, want approved", res.Status)
	}
	if res.Value == nil || res.Value.Token != "real-token" {
		t.Fatalf("value = %#v, want real-token", res.Value)
	}

	// A covering grant means no pending request is ever created.
	pending, err := st.ListSecretRequests(ctx, "project-1", model.SecretRequestStatusPending)
	if err != nil {
		t.Fatalf("list requests: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending requests = %d, want 0", len(pending))
	}
}

func TestResolveSandboxSecretHarnessConfigGrantReturnsValue(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	sec := mustSecret(t, st, "ac", "real-token")
	if err := st.CreateHarnessConfig(ctx, &model.HarnessConfig{
		ID: "ac-1", ProjectID: "project-1", Slug: "codex", Name: "Codex", RunCommand: []string{"codex"},
	}); err != nil {
		t.Fatalf("create harness config: %v", err)
	}
	mustGrant(t, st, sec.ID, model.SecretGrantScopeHarnessConfig, "ac-1")
	createSandboxWithHarness(t, st, "sb-1", "worker-1", "ac-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-C")

	res, err := svc.ResolveSandboxSecret(ctx, "worker-1", "sb-1", "SENTINEL-C", "api.example.com")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Status != model.SecretRequestStatusApproved || res.Value == nil || res.Value.Token != "real-token" {
		t.Fatalf("resolution = %#v, want approved real-token", res)
	}
}

func TestResolveSandboxSecretPendingWithoutGrant(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	sec := mustSecret(t, st, "manual", "real-token")
	createSandbox(t, st, "sb-1", "worker-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-B")

	res, err := svc.ResolveSandboxSecret(ctx, "worker-1", "sb-1", "SENTINEL-B", "api.example.com")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Status != model.SecretRequestStatusPending {
		t.Fatalf("status = %q, want pending", res.Status)
	}
	if res.Value != nil {
		t.Fatal("pending resolution must not carry a value")
	}

	// A second resolve for the same host reuses the pending request rather than
	// piling up duplicates.
	if _, err := svc.ResolveSandboxSecret(ctx, "worker-1", "sb-1", "SENTINEL-B", "api.example.com"); err != nil {
		t.Fatalf("resolve again: %v", err)
	}
	pending, err := st.ListSecretRequests(ctx, "project-1", model.SecretRequestStatusPending)
	if err != nil {
		t.Fatalf("list requests: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending requests = %d, want 1", len(pending))
	}
	if pending[0].SecretID != sec.ID || pending[0].SandboxID != "sb-1" {
		t.Fatalf("pending request = %#v, want secret %s sandbox sb-1", pending[0], sec.ID)
	}
}

func TestResolveSandboxSecretSandboxGrantDoesNotLeakToOtherSandbox(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	sec := mustSecret(t, st, "iso", "real-token")
	mustGrant(t, st, sec.ID, model.SecretGrantScopeSandbox, "sb-1")
	createSandbox(t, st, "sb-2", "worker-1")
	mustAssign(t, st, "sb-2", sec.ID, "SENTINEL-D")

	res, err := svc.ResolveSandboxSecret(ctx, "worker-1", "sb-2", "SENTINEL-D", "api.example.com")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Status != model.SecretRequestStatusPending {
		t.Fatalf("status = %q, want pending (grant is scoped to sb-1)", res.Status)
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
