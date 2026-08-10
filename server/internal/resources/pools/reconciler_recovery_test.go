package pools

import (
	"context"
	"testing"
	"time"

	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/store"
	"github.com/obot-platform/discobox/server/internal/transport"
)

// TestReconcileClearsARecordedFailureOnSuccess pins the half of the pool's
// status the reconciler owns. ErrorMessage used to be a one-way latch: a
// successful reconcile re-read the row, saw the error from the attempt it had
// just superseded, and returned before reaching the clear. The pool then
// reported a failure forever — indefinitely, and with text describing a
// condition that no longer existed.
func TestReconcileClearsARecordedFailureOnSuccess(t *testing.T) {
	ctx := context.Background()
	appStore := newPoolReconcilerTestStore(t)
	manager := sandbox.NewProviderManager()
	manager.RegisterProvider("stub", stubPoolProvider{})

	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "stub", Name: "stub"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	registeredAt := time.Now().UTC()
	pool := &model.Pool{
		ID:           "pool-1",
		ProjectID:    "project-1",
		PoolManifest: model.PoolManifest{Name: "pool-1", ProviderInstanceID: provider.ID},
		Ready:        true,
		Schedulable:  true,
		RegisteredAt: &registeredAt,
		LastSeenAt:   &registeredAt,
	}
	pool.DesiredState = model.DesiredStatePresent
	// The state an earlier failed reconcile leaves behind: a created pool that
	// dropped to offline, with the reason recorded against its generation.
	pool.RecordFailure(model.PoolStateOffline, "runtime did not converge")
	if err := appStore.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}

	reconciler := NewPoolReconciler(appStore, manager, NewControlPlane(appStore, nil))
	if _, err := reconciler.Reconcile(ctx, PoolDirtyID(pool.ProjectID, pool.ID)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	updated, err := appStore.GetPool(ctx, pool.ProjectID, pool.ID)
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	if updated.ErrorMessage != nil {
		t.Fatalf("error message = %q, want none: the reconcile that just succeeded disproves it", *updated.ErrorMessage)
	}
	if updated.State != model.PoolStateActive {
		t.Fatalf("state = %q, want %q", updated.State, model.PoolStateActive)
	}
	if !updated.Converged() {
		t.Fatalf("generations = %d/%d, want converged", updated.ObservedGeneration, updated.Generation)
	}
}

// TestReconcilePromotesAPoolThatRegisteredAfterConverging pins the derivation
// that replaced carrying State forward. The create reconcile converges the
// generation while the pool is still `registering`, so by the time the agent
// calls home the pool is already "successful" and every later pass is a drift
// re-check. A re-check that preserved the recorded state would leave a
// registered, heartbeating pool reading `registering` forever — the promotion
// only used to happen because registration wrote `active` itself, which is the
// API layer writing the reconciler's fields.
func TestReconcilePromotesAPoolThatRegisteredAfterConverging(t *testing.T) {
	ctx := context.Background()
	appStore := newPoolReconcilerTestStore(t)
	manager := sandbox.NewProviderManager()
	manager.RegisterProvider("stub", stubPoolProvider{})

	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "stub", Name: "stub"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	registeredAt := time.Now().UTC()
	pool := &model.Pool{
		ID:           "pool-1",
		ProjectID:    "project-1",
		PoolManifest: model.PoolManifest{Name: "pool-1", ProviderInstanceID: provider.ID},
		RegisteredAt: &registeredAt,
		LastSeenAt:   &registeredAt,
	}
	pool.DesiredState = model.DesiredStatePresent
	pool.SetState(model.PoolStateRegistering)
	// Converged with no error: the runtime came up and this generation is done.
	pool.ObservedGeneration = pool.Generation
	if err := appStore.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}

	reconciler := NewPoolReconciler(appStore, manager, NewControlPlane(appStore, nil))
	if _, err := reconciler.Reconcile(ctx, PoolDirtyID(pool.ProjectID, pool.ID)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	updated, err := appStore.GetPool(ctx, pool.ProjectID, pool.ID)
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	if updated.State != model.PoolStateActive {
		t.Fatalf("state = %q, want %q: the agent has registered", updated.State, model.PoolStateActive)
	}
}

func newPoolReconcilerTestStore(t *testing.T) *store.Store {
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
	if err := db.Write.WithContext(ctx).Create(&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	return store.New(db.Write, db.Read)
}

// stubPoolProvider is a provider whose pool runtime always converges. Only the
// PoolRuntimeReconciler half is exercised; the sandbox surface is present
// because the reconciler resolves providers through sandbox.Provider.
type stubPoolProvider struct{}

func (stubPoolProvider) ReconcilePool(context.Context, sandbox.PoolManager, *model.Project, *model.SandboxProviderInstance, *model.Pool) error {
	return nil
}

func (stubPoolProvider) RepairPool(context.Context, sandbox.PoolManager, *model.Project, *model.SandboxProviderInstance, *model.Pool, string) error {
	return nil
}

func (stubPoolProvider) RemovePool(context.Context, sandbox.PoolManager, *model.Project, *model.SandboxProviderInstance, *model.Pool) error {
	return nil
}

func (stubPoolProvider) Initialize(context.Context, *model.SandboxProviderInstance) error { return nil }
func (stubPoolProvider) Close() error                                                     { return nil }
func (stubPoolProvider) Definition() sandbox.ProviderDefinition {
	return sandbox.ProviderDefinition{Name: "Stub"}
}
func (stubPoolProvider) Status() sandbox.ProviderStatus {
	return sandbox.ProviderStatus{Available: true}
}
func (stubPoolProvider) Reconcile(context.Context) error                  { return nil }
func (stubPoolProvider) RemoveProject(context.Context, string) error      { return nil }
func (stubPoolProvider) List(context.Context) ([]*sandbox.Sandbox, error) { return nil, nil }

func (stubPoolProvider) Create(context.Context, sandbox.SandboxRef, []byte, sandbox.CreateOptions) (*sandbox.Sandbox, []byte, error) {
	return nil, nil, nil
}

func (stubPoolProvider) Update(context.Context, sandbox.SandboxRef, []byte, sandbox.UpdateOptions) (*sandbox.Sandbox, []byte, error) {
	return nil, nil, nil
}

func (stubPoolProvider) Start(context.Context, sandbox.SandboxRef, []byte) ([]byte, error) {
	return nil, nil
}

func (stubPoolProvider) Stop(context.Context, sandbox.SandboxRef, []byte, time.Duration) ([]byte, error) {
	return nil, nil
}

func (stubPoolProvider) Restart(context.Context, sandbox.SandboxRef, []byte, time.Duration) ([]byte, error) {
	return nil, nil
}

func (stubPoolProvider) Archive(context.Context, sandbox.SandboxRef, []byte) ([]byte, error) {
	return nil, nil
}

func (stubPoolProvider) Remove(context.Context, sandbox.SandboxRef, []byte) ([]byte, error) {
	return nil, nil
}

func (stubPoolProvider) Get(context.Context, sandbox.SandboxRef, []byte) (*sandbox.Sandbox, error) {
	return nil, nil
}

func (stubPoolProvider) AcquireHTTPClient(context.Context, sandbox.SandboxRef, []byte, []string) (*transport.HTTPClientLease, error) {
	return nil, nil
}
