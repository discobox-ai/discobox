package secrets_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func newResolveFixture(t *testing.T) (*resourcesecrets.Service, *store.Store) {
	t.Helper()
	svc, st, _ := newResolveFixtureDB(t)
	return svc, st
}

// newResolveFixtureDB is the same fixture with the write handle, for the few
// tests that have to set up state no service writes — a row stamped in the
// past, above all.
func newResolveFixtureDB(t *testing.T) (*resourcesecrets.Service, *store.Store, *gorm.DB) {
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
		ID: "project-1", OwnerUserID: "user-1", Name: "Project",
	}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	st := store.New(db.Write, db.Read)
	if err := st.CreateSandboxProviderInstance(ctx, &model.SandboxProviderInstance{
		ID: "prov-1", ProjectID: "project-1", Type: "test", Name: "test",
	}); err != nil {
		t.Fatalf("create provider instance: %v", err)
	}
	if err := st.CreatePool(ctx, &model.Pool{
		ID:           "pool-1",
		ProjectID:    "project-1",
		PoolManifest: model.PoolManifest{Name: "pool-1", ProviderInstanceID: "prov-1"},
	}); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	return resourcesecrets.NewService(st), st, db.Write
}

func createSandbox(t *testing.T, st *store.Store, sandboxID, poolID string) {
	t.Helper()
	createSandboxWithHarness(t, st, sandboxID, poolID, "")
}

func createSandboxWithHarness(t *testing.T, st *store.Store, sandboxID, poolID, harnessConfigID string) {
	t.Helper()
	sb := &model.Sandbox{
		ID:              sandboxID,
		ProjectID:       "project-1",
		PoolID:          poolID,
		CreatedByUserID: "user-1",
		Name:            sandboxID,
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
		ProjectID: "project-1", Name: name, Type: model.SecretTypeToken,
		MaxGrantTTL: 3600, EncryptedValue: mustTokenValue(t, token),
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
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-A")

	res, err := svc.ResolveSandboxSecret(ctx, "pool-1", "sb-1", "SENTINEL-A", "api.example.com")
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
	createSandboxWithHarness(t, st, "sb-1", "pool-1", "ac-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-C")

	res, err := svc.ResolveSandboxSecret(ctx, "pool-1", "sb-1", "SENTINEL-C", "api.example.com")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Status != model.SecretRequestStatusApproved || res.Value == nil || res.Value.Token != "real-token" {
		t.Fatalf("resolution = %#v, want approved real-token", res)
	}
	// The pool's proxy records which secret a request spent (ADR 26-10-01-240).
	if res.SecretID != sec.ID {
		t.Fatalf("resolution names secret %q, want %q", res.SecretID, sec.ID)
	}
}

func TestResolveSandboxSecretPendingWithoutGrant(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	sec := mustSecret(t, st, "manual", "real-token")
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-B")

	res, err := svc.ResolveSandboxSecret(ctx, "pool-1", "sb-1", "SENTINEL-B", "api.example.com")
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
	if _, err := svc.ResolveSandboxSecret(ctx, "pool-1", "sb-1", "SENTINEL-B", "api.example.com"); err != nil {
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
	createSandbox(t, st, "sb-1", "pool-1")
	mustGrant(t, st, sec.ID, model.SecretGrantScopeSandbox, "sb-1")
	createSandbox(t, st, "sb-2", "pool-1")
	mustAssign(t, st, "sb-2", sec.ID, "SENTINEL-D")

	res, err := svc.ResolveSandboxSecret(ctx, "pool-1", "sb-2", "SENTINEL-D", "api.example.com")
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

	_, err := svc.ResolveSandboxSecret(ctx, "pool-1", "sb-1", "NOPE", "api.example.com")
	if !errors.Is(err, apperrors.ErrNotFound) && !isNotFoundStatus(err) {
		t.Fatalf("err = %v, want not found", err)
	}
}

func isNotFoundStatus(err error) bool {
	var statusErr interface{ StatusCode() int }
	return errors.As(err, &statusErr) && statusErr.StatusCode() == 404
}

// The scope a harness declares for a variable goes out with the value bound to
// it, read from the config as it stands: a login stored before the harness
// named one gains it without anybody signing in again (ADR 26-10-13-595 §2).
func TestResolveSandboxSecretCarriesTheHarnessRejectionScope(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	scope := []model.HarnessSecretRejectionRule{{Host: "api.example.com", Paths: []string{"/v1/messages"}}}
	config := &model.HarnessConfig{
		ID: "ac-1", ProjectID: "project-1", Slug: "claude-code", Name: "Claude Code", RunCommand: []string{"claude"},
		Secrets: []model.HarnessConfigSecret{{Name: "OTHER", RejectionScope: []model.HarnessSecretRejectionRule{{Host: "other.example.com"}}}},
	}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create harness config: %v", err)
	}
	sec := mustSecret(t, st, "login", "real-token")
	mustGrant(t, st, sec.ID, model.SecretGrantScopeProject, "project-1")
	createSandboxWithHarness(t, st, "sb-1", "pool-1", "ac-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-S")
	createSandbox(t, st, "sb-bare", "pool-1")
	mustAssign(t, st, "sb-bare", sec.ID, "SENTINEL-B")

	resolve := func(sandboxID, sentinel string) *model.SandboxSecretResolution {
		t.Helper()
		res, err := svc.ResolveSandboxSecret(ctx, "pool-1", sandboxID, sentinel, "api.example.com")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if res.Status != model.SecretRequestStatusApproved {
			t.Fatalf("status = %q, want approved", res.Status)
		}
		return res
	}
	// The harness declares a scope, but not for the variable this is bound to.
	if res := resolve("sb-1", "SENTINEL-S"); res.ReportScope != nil {
		t.Fatalf("report scope = %#v, want none for a variable the harness names none for", res.ReportScope)
	}
	config.Secrets = append(config.Secrets, model.HarnessConfigSecret{Name: "FOO", RejectionScope: scope})
	if err := st.UpdateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("update harness config: %v", err)
	}
	if res := resolve("sb-1", "SENTINEL-S"); !reflect.DeepEqual(res.ReportScope, scope) {
		t.Fatalf("report scope = %#v, want the one the harness now declares for FOO", res.ReportScope)
	}
	// A configure sandbox holds the stored login under the prefixed name, and
	// runs the same CLI with it.
	if err := st.CreateSandboxSecret(ctx, &model.SandboxSecret{
		ProjectID: "project-1", SandboxID: "sb-1", SecretID: sec.ID, EnvName: "PREV_FOO", Sentinel: "SENTINEL-P",
	}); err != nil {
		t.Fatalf("create assignment: %v", err)
	}
	if res := resolve("sb-1", "SENTINEL-P"); !reflect.DeepEqual(res.ReportScope, scope) {
		t.Fatalf("report scope = %#v, want FOO's for the previous credential a configure run is handed", res.ReportScope)
	}
	if res := resolve("sb-bare", "SENTINEL-B"); res.ReportScope != nil {
		t.Fatalf("report scope = %#v, want none for a sandbox with no harness config", res.ReportScope)
	}
}
