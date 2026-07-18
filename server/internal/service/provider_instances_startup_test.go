package service

import (
	"context"
	"testing"
	"time"

	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/reconcile"
	"github.com/obot-platform/discobox/server/internal/resources/pools"
	"github.com/obot-platform/discobox/server/internal/store"
)

func newProviderInstanceTestStore(ctx context.Context, t *testing.T) (*store.Store, *database.DB) {
	t.Helper()
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
	appStore := store.New(db.Write, db.Read)
	project := &model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project", Slug: "project"}
	if err := appStore.UpsertProject(ctx, project); err != nil {
		t.Fatalf("create project: %v", err)
	}
	return appStore, db
}

func createProviderWithPool(ctx context.Context, t *testing.T, appStore *store.Store, providerID, poolID string) {
	t.Helper()
	if _, err := appStore.GetSandboxProviderInstance(ctx, "project-1", providerID); err != nil {
		provider := &model.SandboxProviderInstance{ID: providerID, ProjectID: "project-1", Type: "docker", Name: providerID}
		if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
			t.Fatalf("create provider %s: %v", providerID, err)
		}
	}
	if err := appStore.CreatePool(ctx, &model.Pool{ID: poolID, ProjectID: "project-1", Name: poolID, ProviderInstanceID: providerID}); err != nil {
		t.Fatalf("create pool %s: %v", poolID, err)
	}
}

func TestEnqueueProviderPoolsMarksEveryProviderPoolDirty(t *testing.T) {
	ctx := context.Background()
	appStore, db := newProviderInstanceTestStore(ctx, t)
	createProviderWithPool(ctx, t, appStore, "provider-1", "pool-1")
	createProviderWithPool(ctx, t, appStore, "provider-1", "pool-2")
	createProviderWithPool(ctx, t, appStore, "provider-2", "pool-3")

	engine := newStartedTestReconcileEngine(ctx, t, db)
	svc := New(appStore, engine, JobManagerOptions{})
	if err := svc.enqueueProviderPools(ctx, "project-1", "provider-1"); err != nil {
		t.Fatalf("enqueue provider pools: %v", err)
	}

	dirty, err := engine.ListDirty(ctx, pools.PoolResourceType)
	if err != nil {
		t.Fatalf("list dirty: %v", err)
	}
	got := map[string]bool{}
	for _, mark := range dirty {
		got[mark.ResourceID] = true
	}
	for _, id := range []string{pools.PoolDirtyID("project-1", "pool-1"), pools.PoolDirtyID("project-1", "pool-2")} {
		if !got[id] {
			t.Fatalf("missing dirty mark for %s; got %#v", id, got)
		}
	}
	if got[pools.PoolDirtyID("project-1", "pool-3")] {
		t.Fatalf("marked pool from a different provider: %#v", got)
	}
}

func TestEnsureExistingSandboxProviderInstancesSchedulesPoolReconcile(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	appStore, db := newProviderInstanceTestStore(ctx, t)
	createProviderWithPool(ctx, t, appStore, "provider-1", "pool-1")

	engine := newStartedTestReconcileEngine(ctx, t, db)
	svc := New(appStore, engine, JobManagerOptions{})
	if err := svc.EnsureExistingSandboxProviderInstances(ctx); err != nil {
		t.Fatalf("ensure existing providers: %v", err)
	}

	// Pool reconciliation rides the level-triggered reconcile engine: startup
	// must mark the provider's pools dirty rather than append a job row.
	dirty, err := engine.ListDirty(ctx, pools.PoolResourceType)
	if err != nil {
		t.Fatalf("list dirty: %v", err)
	}
	if len(dirty) != 1 {
		t.Fatalf("dirty pool marks = %d, want 1 (%#v)", len(dirty), dirty)
	}
	if want := pools.PoolDirtyID("project-1", "pool-1"); dirty[0].ResourceID != want {
		t.Fatalf("dirty pool id = %q, want %q", dirty[0].ResourceID, want)
	}
}

func newStartedTestReconcileEngine(ctx context.Context, t *testing.T, db *database.DB) *reconcile.Engine {
	t.Helper()

	engine, err := reconcile.New(db.Write, reconcile.Options{SingleNode: true})
	if err != nil {
		t.Fatalf("new reconcile engine: %v", err)
	}
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("start reconcile engine: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := engine.Stop(stopCtx); err != nil {
			t.Fatalf("stop reconcile engine: %v", err)
		}
	})
	return engine
}
