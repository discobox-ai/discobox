package sandboxes_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/resources/sandboxes"
	"github.com/obot-platform/discobox/server/internal/store"
)

func TestReconcileSandboxNoCapacityFailsFast(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)

	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "digitalocean", Name: "do"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	pool := &model.Pool{ID: "pool-1", ProjectID: "project-1", Name: "pool-1", ProviderInstanceID: provider.ID}
	if err := appStore.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	sb := &model.Sandbox{
		ID:                "sandbox-1",
		ProjectID:         "project-1",
		PoolID:            pool.ID,
		CreatedByUserID:   "user-1",
		Name:              "alpha",
		ResourceLifecycle: model.NewResourceLifecycle(model.SandboxCreateOperation),
	}
	sb.IncrementGeneration()
	if err := appStore.CreateSandbox(ctx, sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	executor := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(noCapacityProvider{}))
	err := executor.ReconcileSandbox(ctx, sb)
	if !errors.Is(err, sandboxes.ErrNoSandboxCapacity) {
		t.Fatalf("reconcile error = %v, want ErrNoSandboxCapacity", err)
	}

	updated, err := appStore.GetSandbox(ctx, sb.ProjectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if updated.Phase != model.SandboxPhaseFailed || updated.LastOperationStatus != model.SandboxOperationStatusFailed {
		t.Fatalf("sandbox phase/status = %q/%q, want failed/failed", updated.Phase, updated.LastOperationStatus)
	}
	if updated.ErrorMessage == nil || *updated.ErrorMessage != sandboxes.ErrNoSandboxCapacity.Error() {
		t.Fatalf("sandbox error message = %v, want no capacity", updated.ErrorMessage)
	}
}

func TestReconcileSandboxMarksStartFailure(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)

	startErr := errors.New("worker API returned 500")
	sb := createSandboxForReconcile(t, appStore, model.ResourceLifecycle{
		DesiredState:        model.SandboxDesiredStateRunning,
		Phase:               model.SandboxPhasePending,
		ActiveOperation:     stringPtr(model.SandboxOperationCreate),
		LastOperationStatus: model.SandboxOperationStatusPending,
		Generation:          1,
	})

	executor := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(failingSandboxProvider{startErr: startErr}))
	err := executor.ReconcileSandbox(ctx, sb)
	if !errors.Is(err, startErr) {
		t.Fatalf("reconcile error = %v, want %v", err, startErr)
	}

	assertSandboxFailed(t, appStore, sb.ProjectID, sb.ID, startErr.Error())
}

func TestReconcileSandboxMarksStopFailure(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)

	stopErr := errors.New("stop failed")
	sb := createSandboxForReconcile(t, appStore, model.ResourceLifecycle{
		DesiredState:        model.SandboxDesiredStateStopped,
		Phase:               model.SandboxPhaseStopping,
		ActiveOperation:     stringPtr(model.SandboxOperationStop),
		LastOperationStatus: model.SandboxOperationStatusPending,
		Generation:          2,
		ObservedGeneration:  1,
	})

	executor := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(failingSandboxProvider{stopErr: stopErr}))
	err := executor.ReconcileSandbox(ctx, sb)
	if !errors.Is(err, stopErr) {
		t.Fatalf("reconcile error = %v, want %v", err, stopErr)
	}

	assertSandboxFailed(t, appStore, sb.ProjectID, sb.ID, stopErr.Error())
}

func TestReconcileStoppedSandboxRecreatesMissingRuntimeThenStopsIt(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	sb := createSandboxForReconcile(t, appStore, model.ResourceLifecycle{
		DesiredState:        model.SandboxDesiredStateStopped,
		Phase:               model.SandboxPhaseStopping,
		ActiveOperation:     stringPtr(model.SandboxOperationStop),
		LastOperationStatus: model.SandboxOperationStatusPending,
		Generation:          2,
		ObservedGeneration:  1,
	})
	provider := &missingRuntimeProvider{}
	reconciler := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(provider))

	if err := reconciler.ReconcileSandbox(ctx, sb); err != nil {
		t.Fatalf("reconcile stopped sandbox: %v", err)
	}
	if provider.createCalls != 1 || provider.stopCalls != 2 || provider.startCalls != 0 {
		t.Fatalf("provider calls create/stop/start = %d/%d/%d, want 1/2/0", provider.createCalls, provider.stopCalls, provider.startCalls)
	}
	updated, err := appStore.GetSandbox(ctx, sb.ProjectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if updated.Phase != model.SandboxPhaseStopped || updated.ObservedGeneration != updated.Generation {
		t.Fatalf("sandbox phase/generations = %q/%d/%d, want stopped and observed", updated.Phase, updated.ObservedGeneration, updated.Generation)
	}
}

type missingRuntimeProvider struct {
	sandboxes.Provider
	createCalls int
	startCalls  int
	stopCalls   int
}

func (p *missingRuntimeProvider) Create(context.Context, sandboxes.SandboxRef, []byte, sandboxes.CreateOptions) (*sandboxes.Sandbox, []byte, error) {
	p.createCalls++
	return &sandboxes.Sandbox{ID: "runtime-1", Status: sandboxes.StatusCreated}, []byte(`{"runtime":"created"}`), nil
}

func (p *missingRuntimeProvider) Start(context.Context, sandboxes.SandboxRef, []byte) (*sandboxes.Sandbox, []byte, error) {
	p.startCalls++
	return nil, nil, nil
}

func (p *missingRuntimeProvider) Stop(context.Context, sandboxes.SandboxRef, []byte, time.Duration) (*sandboxes.Sandbox, []byte, error) {
	p.stopCalls++
	if p.stopCalls == 1 {
		return nil, nil, sandboxes.ErrNotFound
	}
	return &sandboxes.Sandbox{ID: "runtime-1", Status: "stopped"}, []byte(`{"runtime":"stopped"}`), nil
}

func TestReconcileSandboxMarksDeleteFailure(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)

	removeErr := errors.New("remove failed")
	sb := createSandboxForReconcile(t, appStore, model.ResourceLifecycle{
		DesiredState:        model.SandboxDesiredStateDeleted,
		Phase:               model.SandboxPhaseDeleting,
		ActiveOperation:     stringPtr(model.SandboxOperationDelete),
		LastOperationStatus: model.SandboxOperationStatusPending,
		Generation:          2,
		ObservedGeneration:  1,
	})

	executor := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(failingSandboxProvider{removeErr: removeErr}))
	err := executor.ReconcileSandbox(ctx, sb)
	if !errors.Is(err, removeErr) {
		t.Fatalf("reconcile error = %v, want %v", err, removeErr)
	}

	assertSandboxFailed(t, appStore, sb.ProjectID, sb.ID, removeErr.Error())
}

func TestReconcileSandboxSoftDeletesAfterRuntimeRemoval(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	sb := createSandboxForReconcile(t, appStore, model.ResourceLifecycle{
		DesiredState:        model.SandboxDesiredStateDeleted,
		Phase:               model.SandboxPhaseDeleting,
		ActiveOperation:     stringPtr(model.SandboxOperationDelete),
		LastOperationStatus: model.SandboxOperationStatusPending,
		Generation:          2,
		ObservedGeneration:  1,
	})

	reconciler := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(failingSandboxProvider{}))
	if err := reconciler.ReconcileSandbox(ctx, sb); err != nil {
		t.Fatalf("reconcile delete: %v", err)
	}

	if _, err := appStore.GetSandbox(ctx, sb.ProjectID, sb.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get deleted sandbox = %v, want not found", err)
	}
	assigned, err := appStore.CountSandboxesForPool(ctx, sb.ProjectID, sb.PoolID)
	if err != nil {
		t.Fatalf("count assigned sandboxes: %v", err)
	}
	if assigned != 0 {
		t.Fatalf("assigned sandboxes = %d, want 0", assigned)
	}
}

type noCapacityProvider struct {
	sandboxes.Provider
}

func (noCapacityProvider) Create(context.Context, sandboxes.SandboxRef, []byte, sandboxes.CreateOptions) (*sandboxes.Sandbox, []byte, error) {
	return nil, nil, sandboxes.ErrNoSandboxCapacity
}

type failingSandboxProvider struct {
	sandboxes.Provider
	createErr error
	startErr  error
	stopErr   error
	removeErr error
}

func (p failingSandboxProvider) Create(context.Context, sandboxes.SandboxRef, []byte, sandboxes.CreateOptions) (*sandboxes.Sandbox, []byte, error) {
	if p.createErr != nil {
		return nil, nil, p.createErr
	}
	return &sandboxes.Sandbox{ID: "runtime-1", Status: sandboxes.StatusCreated}, []byte(`{"runtime":"created"}`), nil
}

func (p failingSandboxProvider) Start(context.Context, sandboxes.SandboxRef, []byte) (*sandboxes.Sandbox, []byte, error) {
	if p.startErr != nil {
		return nil, nil, p.startErr
	}
	now := time.Now().UTC()
	return &sandboxes.Sandbox{ID: "runtime-1", Status: "running", StartedAt: &now}, []byte(`{"runtime":"running"}`), nil
}

func (p failingSandboxProvider) Stop(context.Context, sandboxes.SandboxRef, []byte, time.Duration) (*sandboxes.Sandbox, []byte, error) {
	if p.stopErr != nil {
		return nil, nil, p.stopErr
	}
	now := time.Now().UTC()
	return &sandboxes.Sandbox{ID: "runtime-1", Status: "stopped", StoppedAt: &now}, []byte(`{"runtime":"stopped"}`), nil
}

func (p failingSandboxProvider) Remove(context.Context, sandboxes.SandboxRef, []byte, ...sandboxes.RemoveOption) ([]byte, error) {
	if p.removeErr != nil {
		return nil, p.removeErr
	}
	return nil, nil
}

func createSandboxForReconcile(t *testing.T, appStore *store.Store, lifecycle model.ResourceLifecycle) *model.Sandbox {
	t.Helper()
	ctx := context.Background()
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "test", Name: "test"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	pool := &model.Pool{ID: "pool-1", ProjectID: "project-1", Name: "pool-1", ProviderInstanceID: provider.ID}
	if err := appStore.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	sb := &model.Sandbox{
		ID:                "sandbox-1",
		ProjectID:         "project-1",
		PoolID:            pool.ID,
		CreatedByUserID:   "user-1",
		Name:              "alpha",
		ResourceLifecycle: lifecycle,
	}
	if err := appStore.CreateSandbox(ctx, sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	return sb
}

func assertSandboxFailed(t *testing.T, appStore *store.Store, projectID, sandboxID, message string) {
	t.Helper()
	updated, err := appStore.GetSandbox(context.Background(), projectID, sandboxID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if updated.Phase != model.SandboxPhaseFailed || updated.LastOperationStatus != model.SandboxOperationStatusFailed {
		t.Fatalf("sandbox phase/status = %q/%q, want failed/failed", updated.Phase, updated.LastOperationStatus)
	}
	if updated.ErrorMessage == nil || *updated.ErrorMessage != message {
		t.Fatalf("sandbox error message = %v, want %q", updated.ErrorMessage, message)
	}
}

func stringPtr(value string) *string {
	return &value
}

func newExecutorTestStore(t *testing.T) *store.Store {
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
	project := &model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project", Slug: "project"}
	if err := db.Write.WithContext(ctx).Create(project).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	return store.New(db.Write, db.Read)
}
