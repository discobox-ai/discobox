package discovm

import (
	"context"
	"errors"
	"testing"

	"github.com/discobox-ai/vm/pkg/engine"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/resources/pools"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
)

// resolvedOnly is the server's pool provider around a runtime, as the factory
// builds it, with nothing to initialize (the test has no reconcile engine to
// mark the provider's pools on) and nothing to close (the test closes the
// runtime itself).
type resolvedOnly struct{ *poolruntime.Provider }

func (resolvedOnly) Initialize(context.Context, *model.SandboxProviderInstance) error { return nil }
func (resolvedOnly) Close() error                                                    { return nil }

// Deleting a pool on a disabled provider removes what it ran, as on an
// enabled one (ADR 26-11-22-507 §1): the pool reconciler resolves the
// disabled instance, and RemovePool takes the pool's machine and everything
// in its scope. On a remote driver nothing else would ever find them.
func TestFakePoolOnADisabledProviderIsRemovedWithItsDelete(t *testing.T) {
	r, _ := newFakePool(t, "")
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
	if err := appStore.UpsertProject(ctx, &model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "project-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	instance := fakeInstance(t)
	instance.Name = "fake"
	instance.Disabled = true
	if err := appStore.CreateSandboxProviderInstance(ctx, instance); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	pool := &model.Pool{ID: "pool_disabled1", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool", ProviderInstanceID: instance.ID}}
	if err := appStore.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })

	// The pool as it ran before its provider was disabled: its machine, and
	// what its agent made in its scope.
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), func(context.Context) error { return nil }); err != nil {
		t.Fatalf("EnsurePool() error = %v", err)
	}
	scope, err := machinePool(pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := newDriver(fakeConfig(t), scope, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := engine.Open(driver, scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Create(ctx, engine.CreateOptions{Name: "snap"}); err != nil {
		t.Fatalf("create a machine in the pool's scope: %v", err)
	}
	t.Cleanup(func() {
		left, _ := agent.List(context.Background())
		for _, inst := range left {
			_ = agent.Remove(context.Background(), inst, true)
		}
	})

	manager := sandbox.NewProviderManager(appStore)
	controlPlane := pools.NewControlPlane(appStore, manager, nil)
	manager.RegisterFactory("fake", func(context.Context, *model.SandboxProviderInstance) (sandbox.Provider, error) {
		return resolvedOnly{poolruntime.New(r, Definition("fake"), controlPlane, nil)}, nil
	})
	reconciler := pools.NewPoolReconciler(appStore, manager, controlPlane, nil)

	if err := db.Write.Model(&model.Pool{}).Where("id = ?", pool.ID).UpdateColumn("desired_state", model.DesiredStateDeleted).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, pools.PoolDirtyID("project-1", pool.ID)); err != nil {
		t.Fatalf("reconcile the delete: %v", err)
	}

	if _, err := r.host.(*poolMachine).instance(ctx, pool); err == nil {
		t.Fatal("the pool's machine outlived its delete")
	}
	left, err := agent.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("the pool's scope %s still holds %d machines after its delete, want none", scope, len(left))
	}
	if _, err := appStore.GetPool(ctx, "project-1", pool.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("pool row after its delete: %v, want it gone", err)
	}
}
