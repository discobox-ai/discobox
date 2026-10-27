package secrets_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

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
		Routes:        []model.SecretRoute{{Host: "*"}},
		MaxBindingTTL: 3600, EncryptedValue: mustTokenValue(t, token),
	}
	if err := st.CreateSecret(context.Background(), sec); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	return sec
}

// mustBind binds a secret for use in FOO on a target, as a person would.
func mustBind(t *testing.T, st *store.Store, secretID, kind, targetID string) *model.SecretBinding {
	t.Helper()
	binding := &model.SecretBinding{
		ProjectID: "project-1", SecretID: secretID, TargetKind: kind, TargetID: targetID, Use: true, EnvName: "FOO",
	}
	if err := st.CreateSecretBinding(context.Background(), binding, store.BindingChange{Actor: "user-1"}); err != nil {
		t.Fatalf("create binding: %v", err)
	}
	return binding
}

// mustAssign gives a sandbox the sentinel of FOO, naming the binding that
// delivers it there, if one does.
func mustAssign(t *testing.T, st *store.Store, sandboxID, secretID, sentinel string) {
	t.Helper()
	ctx := context.Background()
	sandbox, err := st.GetSandbox(ctx, "project-1", sandboxID)
	if err != nil {
		t.Fatalf("read sandbox: %v", err)
	}
	effective, err := st.ResolveSandboxBindings(ctx, sandbox, time.Now().UTC())
	if err != nil {
		t.Fatalf("resolve bindings: %v", err)
	}
	row := &model.SandboxSecret{ProjectID: "project-1", SandboxID: sandboxID, SecretID: secretID, EnvName: "FOO", Sentinel: sentinel}
	if applied, ok := effective.UseFor("FOO"); ok {
		row.BindingID = applied.Binding.ID
	}
	if err := st.CreateSandboxSecret(ctx, row); err != nil {
		t.Fatalf("create assignment: %v", err)
	}
}

// mustDeliver binds a secret for use in env on a sandbox and gives the
// sandbox its sentinel there, as materializing the binding would.
func mustDeliver(t *testing.T, st *store.Store, sandboxID, secretID, env, sentinel string) *model.SecretBinding {
	t.Helper()
	ctx := context.Background()
	binding := &model.SecretBinding{
		ProjectID: "project-1", SecretID: secretID, TargetKind: model.SecretBindingTargetSandbox, TargetID: sandboxID, Use: true, EnvName: env,
	}
	if err := st.CreateSecretBinding(ctx, binding, store.BindingChange{Actor: "user-1"}); err != nil {
		t.Fatalf("create binding: %v", err)
	}
	if err := st.CreateSandboxSecret(ctx, &model.SandboxSecret{
		ProjectID: "project-1", SandboxID: sandboxID, SecretID: secretID, EnvName: env, Sentinel: sentinel, BindingID: binding.ID,
	}); err != nil {
		t.Fatalf("create assignment: %v", err)
	}
	return binding
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
	mustBind(t, st, sec.ID, model.SecretBindingTargetProject, "project-1")
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
	mustBind(t, st, sec.ID, model.SecretBindingTargetHarnessConfig, "ac-1")
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

// A sentinel exists because a binding does: one whose binding is gone, or
// lapsed, or that never had one, resolves to nothing and asks nobody for
// anything (ADR 26-10-27-880 §1).
func TestResolveSandboxSecretDeniedWithoutABinding(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	sec := mustSecret(t, st, "manual", "real-token")
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-B")
	lapsed := time.Now().UTC().Add(-time.Minute)
	binding := mustDeliver(t, st, "sb-1", sec.ID, "LAPSED", "SENTINEL-L")
	binding.ExpiresAt = &lapsed
	if err := st.UpdateSecretBinding(ctx, binding, store.BindingChange{Actor: "user-1"}); err != nil {
		t.Fatal(err)
	}

	for _, sentinel := range []string{"SENTINEL-B", "SENTINEL-L"} {
		res, err := svc.ResolveSandboxSecret(ctx, "pool-1", "sb-1", sentinel, "api.example.com")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if res.Status != model.SecretRequestStatusDenied || res.Value != nil {
			t.Fatalf("%s = %+v, want denied with no value", sentinel, res)
		}
	}
	pending, err := st.ListSecretRequests(ctx, "project-1", model.SecretRequestStatusPending)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending requests = %+v, %v; want none", pending, err)
	}
}

// A value is handed out only for a host every route list allows: the
// secret's, the binding's, and the parent's it was handed on under.
func TestResolveSandboxSecretHoldsToEveryRouteList(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)
	sec := mustSecret(t, st, "narrowed", "real-token")
	createSandbox(t, st, "sb-1", "pool-1")
	binding := mustDeliver(t, st, "sb-1", sec.ID, "TOKEN", "SENTINEL-N")
	binding.Routes = []model.SecretRoute{{Host: "api.example.com"}}
	if err := st.UpdateSecretBinding(ctx, binding, store.BindingChange{Actor: "user-1"}); err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]string{"api.example.com": model.SecretRequestStatusApproved, "other.example.com": model.SecretRequestStatusDenied} {
		res, err := svc.ResolveSandboxSecret(ctx, "pool-1", "sb-1", "SENTINEL-N", host)
		if err != nil || res.Status != want {
			t.Fatalf("%s = %+v, %v; want %s", host, res, err, want)
		}
	}
}

func TestResolveSandboxSecretSandboxGrantDoesNotLeakToOtherSandbox(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	sec := mustSecret(t, st, "iso", "real-token")
	createSandbox(t, st, "sb-1", "pool-1")
	mustBind(t, st, sec.ID, model.SecretBindingTargetSandbox, "sb-1")
	createSandbox(t, st, "sb-2", "pool-1")
	mustAssign(t, st, "sb-2", sec.ID, "SENTINEL-D")

	res, err := svc.ResolveSandboxSecret(ctx, "pool-1", "sb-2", "SENTINEL-D", "api.example.com")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Status != model.SecretRequestStatusDenied {
		t.Fatalf("status = %q, want denied (the binding is on sb-1)", res.Status)
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
	mustBind(t, st, sec.ID, model.SecretBindingTargetProject, "project-1")
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
	prev, err := st.ResolveSandboxBindings(ctx, mustGetSandbox(t, st, "sb-1"), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	applied, _ := prev.UseFor("FOO")
	if err := st.CreateSandboxSecret(ctx, &model.SandboxSecret{
		ProjectID: "project-1", SandboxID: "sb-1", SecretID: sec.ID, EnvName: "PREV_FOO", Sentinel: "SENTINEL-P", BindingID: applied.Binding.ID,
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

func mustGetSandbox(t *testing.T, st *store.Store, sandboxID string) *model.Sandbox {
	t.Helper()
	sandbox, err := st.GetSandbox(context.Background(), "project-1", sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	return sandbox
}
