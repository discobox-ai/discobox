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
	pool := &model.Pool{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool-1", ProviderInstanceID: provider.ID}}
	if err := appStore.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	sb := &model.Sandbox{
		ID:                "sandbox-1",
		ProjectID:         "project-1",
		PoolID:            pool.ID,
		CreatedByUserID:   "user-1",
		Name:              "alpha",
		ResourceLifecycle: model.ResourceLifecycle{DesiredState: model.DesiredStatePresent, State: model.SandboxStatePending, Generation: 1},
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
	if updated.State != model.SandboxStateFailed {
		t.Fatalf("sandbox state = %q, want failed", updated.State)
	}
	if updated.ErrorMessage == nil || *updated.ErrorMessage != sandboxes.ErrNoSandboxCapacity.Error() {
		t.Fatalf("sandbox error message = %v, want no capacity", updated.ErrorMessage)
	}
}

// A start that could not be delivered is not a failed reconcile. The
// generation asked for the sandbox to exist, and it does; the sandbox is left
// stopped and reported as such, and the next thing to touch it starts it
// (ADR 0017 §§9, 12).
func TestReconcileToleratesAnUndeliverableStart(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)

	startErr := errors.New("worker API returned 500")
	sb := createSandboxForReconcile(t, appStore, model.ResourceLifecycle{
		DesiredState: model.DesiredStatePresent,
		State:        model.SandboxStatePending,
		Generation:   1,
	})

	executor := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(failingSandboxProvider{startErr: startErr}))
	if err := executor.ReconcileSandbox(ctx, sb); err != nil {
		t.Fatalf("reconcile = %v, want nil: the sandbox exists, which is what this generation was for", err)
	}

	updated, err := appStore.GetSandbox(ctx, sb.ProjectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if !updated.Converged() {
		t.Fatalf("generations = %d/%d, want converged", updated.ObservedGeneration, updated.Generation)
	}
	if updated.ErrorMessage != nil {
		t.Fatalf("error = %v, want none: a failed instruction is not a failed existence", *updated.ErrorMessage)
	}
}

// A sandbox whose container is gone is rebuilt, and deliberately left stopped:
// recovery brings back what somebody actually uses, not everything that once
// ran (ADR 0017 §13).
func TestReconcileRebuildsAMissingRuntimeWithoutStartingIt(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	sb := createSandboxForReconcile(t, appStore, model.ResourceLifecycle{
		DesiredState:       model.DesiredStatePresent,
		State:              model.SandboxStateStopped,
		Generation:         2,
		ObservedGeneration: 1,
	})
	provider := &missingRuntimeProvider{}
	reconciler := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(provider))

	if err := reconciler.ReconcileSandbox(ctx, sb); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if provider.createCalls != 1 {
		t.Fatalf("create calls = %d, want 1", provider.createCalls)
	}
	if provider.startCalls != 0 {
		t.Fatalf("start calls = %d, want 0: a rebuilt sandbox stays stopped until used", provider.startCalls)
	}
	updated, err := appStore.GetSandbox(ctx, sb.ProjectID, sb.ID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if !updated.Converged() {
		t.Fatalf("generations = %d/%d, want converged", updated.ObservedGeneration, updated.Generation)
	}
	if updated.State != model.SandboxStateStopped {
		t.Fatalf("state = %q, want stopped: the runtime reports what it is, and nothing started it", updated.State)
	}
}

// A brand-new sandbox is created with Start set, because asking for a sandbox
// means asking for one that runs. A rebuild is not — that is the whole
// difference between restoring a sandbox and resurrecting it (ADR 0017 §13).
func TestReconcileOnlyStartsASandboxThatHasNeverRun(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		state     string
		wantStart bool
	}{
		{name: "first create", state: model.SandboxStatePending, wantStart: true},
		{name: "resumed after its push", state: model.SandboxStateAwaitingSource, wantStart: true},
		{name: "rebuild after the container was lost", state: model.SandboxStateStopped, wantStart: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			appStore := newExecutorTestStore(t)
			sb := createSandboxForReconcile(t, appStore, model.ResourceLifecycle{
				DesiredState:       model.DesiredStatePresent,
				State:              tc.state,
				Generation:         2,
				ObservedGeneration: 1,
			})
			provider := &recordingCreateProvider{}
			reconciler := sandboxes.NewSandboxReconciler(appStore, sandboxes.WithSandboxProvider(provider))

			if err := reconciler.ReconcileSandbox(ctx, sb); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if provider.lastStart != tc.wantStart {
				t.Fatalf("create asked to start = %v, want %v", provider.lastStart, tc.wantStart)
			}
		})
	}
}

type recordingCreateProvider struct {
	sandboxes.Provider
	lastStart bool
}

func (p *recordingCreateProvider) Create(_ context.Context, _ sandboxes.SandboxRef, state []byte, opts sandboxes.CreateOptions) (*sandboxes.Sandbox, []byte, error) {
	p.lastStart = opts.Start
	return &sandboxes.Sandbox{ID: "runtime-1"}, state, nil
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

func (p *missingRuntimeProvider) Start(context.Context, sandboxes.SandboxRef, []byte) ([]byte, error) {
	p.startCalls++
	return nil, nil
}

func (p *missingRuntimeProvider) Stop(context.Context, sandboxes.SandboxRef, []byte, time.Duration) ([]byte, error) {
	p.stopCalls++
	if p.stopCalls == 1 {
		return nil, sandboxes.ErrNotFound
	}
	return []byte(`{"runtime":"stopped"}`), nil
}

func (p *missingRuntimeProvider) Restart(context.Context, sandboxes.SandboxRef, []byte, time.Duration) ([]byte, error) {
	return nil, nil
}

func TestReconcileSandboxMarksDeleteFailure(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)

	removeErr := errors.New("remove failed")
	sb := createSandboxForReconcile(t, appStore, model.ResourceLifecycle{
		DesiredState:       model.DesiredStateDeleted,
		State:              model.SandboxStateRunning,
		Generation:         2,
		ObservedGeneration: 1,
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
		DesiredState:       model.DesiredStateDeleted,
		State:              model.SandboxStateRunning,
		Generation:         2,
		ObservedGeneration: 1,
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

func (p failingSandboxProvider) Start(context.Context, sandboxes.SandboxRef, []byte) ([]byte, error) {
	if p.startErr != nil {
		return nil, p.startErr
	}
	return []byte(`{"runtime":"running"}`), nil
}

func (p failingSandboxProvider) Stop(context.Context, sandboxes.SandboxRef, []byte, time.Duration) ([]byte, error) {
	if p.stopErr != nil {
		return nil, p.stopErr
	}
	return []byte(`{"runtime":"stopped"}`), nil
}

func (p failingSandboxProvider) Restart(context.Context, sandboxes.SandboxRef, []byte, time.Duration) ([]byte, error) {
	return nil, nil
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
	pool := &model.Pool{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool-1", ProviderInstanceID: provider.ID}}
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
	if updated.State != model.SandboxStateFailed {
		t.Fatalf("sandbox state = %q, want failed", updated.State)
	}
	if updated.ErrorMessage == nil || *updated.ErrorMessage != message {
		t.Fatalf("sandbox error message = %v, want %q", updated.ErrorMessage, message)
	}
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
