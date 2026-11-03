package poolruntime

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/resources/pools"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// A provider whose config was updated is rebuilt, and initialized again, by
// the first resolve after it. That resolve is often the one a pool's own
// reconcile makes. Initialize asks every pool of the provider to reconcile; the
// pool already reconciling must not fail that resolve, or the provider is never
// cached and every later pass fails the same way. The provider's other pools
// must still be asked.
func TestProviderInitializedInsideAPoolReconcileResolves(t *testing.T) {
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
	if err := appStore.UpsertProject(ctx, &model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "project-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "test", Name: "Test"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	for _, id := range []string{"pool-1", "pool-2"} {
		pool := &model.Pool{ID: id, ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: id, ProviderInstanceID: provider.ID}}
		if err := appStore.CreatePool(ctx, pool); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		time.Sleep(time.Millisecond) // pools list in created_at order
	}

	controlPlane := pools.NewControlPlane(appStore, engine)
	manager := sandbox.NewProviderManager(noProviderSecrets{})
	manager.RegisterFactory("test", func(context.Context, *model.SandboxProviderInstance) (sandbox.Provider, error) {
		return New(&testRuntimeProvider{}, sandbox.ProviderDefinition{}, controlPlane, nil), nil
	})

	// A pool reconcile that resolves its provider, as PoolReconciler.resolve
	// does, and nothing else: the real reconciler would go on to reconcile the
	// pool's runtime, which is not what this is about.
	var mu sync.Mutex
	resolveErrs := map[string][]error{}
	if err := engine.Register(pools.PoolResourceType, reconcilerFunc(func(ctx context.Context, id string) (reconcile.Result, error) {
		current, err := appStore.GetSandboxProviderInstance(ctx, provider.ProjectID, provider.ID)
		if err == nil {
			_, err = manager.ResolveInstance(ctx, current)
		}
		mu.Lock()
		resolveErrs[id] = append(resolveErrs[id], err)
		mu.Unlock()
		return reconcile.Result{}, err
	})); err != nil {
		t.Fatalf("register pool reconciler: %v", err)
	}
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("start engine: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = engine.Stop(stopCtx)
	})

	// Only pool-1's reconcile is asked for; Initialize, inside it, is what
	// must ask for pool-2's.
	if err := engine.MarkDirty(ctx, pools.PoolResourceType, pools.PoolDirtyID("project-1", "pool-1")); err != nil {
		t.Fatalf("mark pool-1: %v", err)
	}
	pool1, pool2 := pools.PoolDirtyID("project-1", "pool-1"), pools.PoolDirtyID("project-1", "pool-2")
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		first, ran := resolveErrs[pool1], len(resolveErrs[pool2]) > 0
		mu.Unlock()
		if len(first) > 0 && first[0] != nil {
			t.Fatalf("pool-1's reconcile resolved its provider with %v, want it resolved", first[0])
		}
		if len(first) > 0 && ran {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pool-1 reconciled %d times, pool-2 reconciled = %v; want pool-2 asked to reconcile by Initialize", len(first), ran)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type reconcilerFunc func(context.Context, string) (reconcile.Result, error)

func (f reconcilerFunc) Reconcile(ctx context.Context, id string) (reconcile.Result, error) {
	return f(ctx, id)
}

type noProviderSecrets struct{}

func (noProviderSecrets) OpenSandboxProviderSecrets(context.Context, *model.SandboxProviderInstance) (map[string]json.RawMessage, error) {
	return nil, nil
}
