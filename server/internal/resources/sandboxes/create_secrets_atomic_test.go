package sandboxes

import (
	"context"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// The reconciler wakes on the create transaction's commit, so a sandbox must
// never be observable without its secret assignments: a launch that misses
// them is permanent (assignments are excluded from the spec fingerprint, so
// late rows never read as drift). These tests pin the atomicity.

func createIntentFixture(t *testing.T) (context.Context, *Service, *store.Store, *database.DB) {
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
	appStore := store.New(db.Write, db.Read)
	engine, err := reconcile.New(db.Write, reconcile.Options{SingleNode: true})
	if err != nil {
		t.Fatalf("create reconcile engine: %v", err)
	}
	if err := db.Write.WithContext(ctx).Create(&model.Project{
		ID: "project-1", OwnerUserID: "user-1", Name: "Project",
	}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "test", Name: "Test"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	pool := &model.Pool{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool-1", ProviderInstanceID: provider.ID}}
	if err := appStore.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	return ctx, NewService(appStore, nil, "user-1", engine), appStore, db
}

func TestCreateSandboxIntentPersistsSecretsWithSandbox(t *testing.T) {
	ctx, svc, st, _ := createIntentFixture(t)
	sandbox := &model.Sandbox{ID: "sb-1", ProjectID: "project-1", CreatedByUserID: "user-1", Name: "sb-1", PoolID: "pool-1"}
	sec := bearerSecret(t, st, "token", "")
	created, err := svc.createSandboxIntent(ctx, sandbox, nil, []*model.SecretBinding{
		{ProjectID: "project-1", SecretID: sec.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: "sb-1", Use: true, EnvName: "TOKEN"},
	}, resourcesecrets.SandboxBindings{})
	if err != nil {
		t.Fatalf("create intent: %v", err)
	}
	if created == nil {
		t.Fatal("created sandbox is nil")
	}
	assignments, err := st.ListSandboxSecrets(ctx, "project-1", "sb-1")
	if err != nil {
		t.Fatalf("list assignments: %v", err)
	}
	if len(assignments) != 1 || assignments[0].EnvName != "TOKEN" || assignments[0].SecretID != sec.ID || assignments[0].BindingID == "" {
		t.Fatalf("assignments = %#v, want the TOKEN binding materialized with the sandbox", assignments)
	}
}

func TestCreateSandboxIntentRollsBackSandboxOnSecretFailure(t *testing.T) {
	ctx, svc, st, _ := createIntentFixture(t)
	sandbox := &model.Sandbox{ID: "sb-1", ProjectID: "project-1", CreatedByUserID: "user-1", Name: "sb-1", PoolID: "pool-1"}
	// An inline value, then two use bindings of the same variable, which
	// violate the binding's uniqueness and fail the transaction.
	inlineSecrets, inline, err := inlineSandboxBindings(ctx, "project-1", sandbox, []services.SandboxSecretInput{{Env: "DB_PASS", Value: "hunter2"}})
	if err != nil {
		t.Fatalf("inline bindings: %v", err)
	}
	sec := bearerSecret(t, st, "token", "")
	_, err = svc.createSandboxIntent(ctx, sandbox, inlineSecrets, append(inline,
		&model.SecretBinding{ProjectID: "project-1", SecretID: sec.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: "sb-1", Use: true, EnvName: "TOKEN"},
		&model.SecretBinding{ProjectID: "project-1", SecretID: sec.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: "sb-1", Use: true, EnvName: "TOKEN"},
	), resourcesecrets.SandboxBindings{})
	if err == nil {
		t.Fatal("create intent succeeded, want a failed secret insert to fail the create")
	}
	if _, err := st.GetSandbox(ctx, "project-1", "sb-1"); err == nil {
		t.Fatal("sandbox row survived a failed secret insert; the create must be atomic")
	}
	// The inline value goes with the failed create, not left stored with
	// the route `*`.
	if _, err := st.GetSecret(ctx, "project-1", inlineSecrets[0].ID); err == nil {
		t.Fatal("the inline secret survived a failed create")
	}
}
