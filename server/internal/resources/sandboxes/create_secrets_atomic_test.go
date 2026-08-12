package sandboxes

import (
	"context"
	"testing"

	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/reconcile"
	"github.com/obot-platform/discobox/server/internal/store"
)

// The reconciler wakes on the create transaction's commit, so a sandbox must
// never be observable without its secret assignments: a launch that misses
// them is permanent (assignments are excluded from the spec fingerprint, so
// late rows never read as drift). These tests pin the atomicity.

func createIntentFixture(t *testing.T) (context.Context, *Service, *store.Store) {
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
	return ctx, NewService(appStore, nil, "user-1", engine), appStore
}

func TestCreateSandboxIntentPersistsSecretsWithSandbox(t *testing.T) {
	ctx, svc, st := createIntentFixture(t)
	sandbox := &model.Sandbox{ID: "sb-1", ProjectID: "project-1", CreatedByUserID: "user-1", Name: "sb-1", PoolID: "pool-1"}
	created, err := svc.createSandboxIntent(ctx, sandbox, []*model.SandboxSecret{
		{ProjectID: "project-1", SandboxID: "sb-1", SecretID: "sec-1", EnvName: "TOKEN", Sentinel: "sentinel-1"},
	})
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
	if len(assignments) != 1 || assignments[0].EnvName != "TOKEN" || assignments[0].Sentinel != "sentinel-1" {
		t.Fatalf("assignments = %#v, want the TOKEN assignment committed with the sandbox", assignments)
	}
}

func TestCreateSandboxIntentRollsBackSandboxOnSecretFailure(t *testing.T) {
	ctx, svc, st := createIntentFixture(t)
	sandbox := &model.Sandbox{ID: "sb-1", ProjectID: "project-1", CreatedByUserID: "user-1", Name: "sb-1", PoolID: "pool-1"}
	// Two assignments for the same env name violate idx_sandbox_secret_env,
	// failing the second insert inside the transaction.
	_, err := svc.createSandboxIntent(ctx, sandbox, []*model.SandboxSecret{
		{ProjectID: "project-1", SandboxID: "sb-1", SecretID: "sec-1", EnvName: "TOKEN", Sentinel: "sentinel-1"},
		{ProjectID: "project-1", SandboxID: "sb-1", SecretID: "sec-2", EnvName: "TOKEN", Sentinel: "sentinel-2"},
	})
	if err == nil {
		t.Fatal("create intent succeeded, want a failed secret insert to fail the create")
	}
	if _, err := st.GetSandbox(ctx, "project-1", "sb-1"); err == nil {
		t.Fatal("sandbox row survived a failed secret insert; the create must be atomic")
	}
}
