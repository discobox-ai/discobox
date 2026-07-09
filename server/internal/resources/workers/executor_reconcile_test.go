package workers_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/resources/sandboxes"
	"github.com/obot-platform/discobox/server/internal/resources/workers"
	"github.com/obot-platform/discobox/server/internal/store"
)

func TestReconcileWorkerMarksLaunchFailure(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "failing", Name: "failing"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	worker := &model.Worker{
		ID:                 "worker-1",
		ProjectID:          "project-1",
		ProviderInstanceID: "provider-1",
		ResourceLifecycle:  model.NewResourceLifecycle(model.WorkerCreateOperation, nil),
	}
	worker.IncrementGeneration()
	if err := appStore.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}

	launchErr := errors.New("launch failed")
	manager := sandboxes.NewProviderManager()
	manager.RegisterProvider("failing", failingWorkerProvider{err: launchErr})
	executor := workers.NewWorkerReconcileExecutor(appStore, workers.WithWorkerProviderManager(manager))

	err := executor.ReconcileWorkerJob(ctx, worker.ProjectID, worker.ProviderInstanceID, worker.ID, "job-1", worker.Generation)
	if !errors.Is(err, launchErr) {
		t.Fatalf("reconcile error = %v, want launch failed", err)
	}

	updated, err := appStore.GetWorker(ctx, worker.ID)
	if err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if updated.Phase != model.WorkerPhaseFailed || updated.LastOperationStatus != model.OperationStatusFailed {
		t.Fatalf("worker phase/status = %q/%q, want failed/failed", updated.Phase, updated.LastOperationStatus)
	}
	if updated.ErrorMessage == nil || *updated.ErrorMessage != launchErr.Error() {
		t.Fatalf("worker error message = %v, want %q", updated.ErrorMessage, launchErr.Error())
	}
}

func TestReconcileWorkerChecksRuntimeForSuccessfulGeneration(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "counting", Name: "counting"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	worker := &model.Worker{
		ID:                 "worker-1",
		ProjectID:          "project-1",
		ProviderInstanceID: "provider-1",
		Ready:              true,
		Schedulable:        true,
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState:        model.WorkerDesiredStateActive,
			Phase:               model.WorkerPhaseActive,
			LastOperationStatus: model.OperationStatusSuccess,
			Generation:          1,
			ObservedGeneration:  1,
		},
	}
	if err := appStore.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}

	providerImpl := &countingWorkerProvider{}
	manager := sandboxes.NewProviderManager()
	manager.RegisterProvider("counting", providerImpl)
	executor := workers.NewWorkerReconcileExecutor(appStore, workers.WithWorkerProviderManager(manager))

	if err := executor.ReconcileWorkerJob(ctx, worker.ProjectID, worker.ProviderInstanceID, worker.ID, "job-1", worker.Generation); err != nil {
		t.Fatalf("reconcile worker: %v", err)
	}
	if providerImpl.calls != 1 {
		t.Fatalf("ReconcileWorker calls = %d, want 1", providerImpl.calls)
	}
	updated, err := appStore.GetWorker(ctx, worker.ID)
	if err != nil {
		t.Fatalf("get updated worker: %v", err)
	}
	if updated.Phase != model.WorkerPhaseActive {
		t.Fatalf("worker phase = %q, want %q", updated.Phase, model.WorkerPhaseActive)
	}
}

func TestReconcileWorkerPreservesConcurrentStatusUpdate(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "registering", Name: "registering"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	worker := &model.Worker{
		ID:                 "worker-1",
		ProjectID:          "project-1",
		ProviderInstanceID: "provider-1",
		ResourceLifecycle:  model.NewResourceLifecycle(model.WorkerCreateOperation, nil),
	}
	worker.IncrementGeneration()
	if err := appStore.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}

	providerImpl := &registeringWorkerProvider{store: appStore}
	manager := sandboxes.NewProviderManager()
	manager.RegisterProvider("registering", providerImpl)
	executor := workers.NewWorkerReconcileExecutor(appStore, workers.WithWorkerProviderManager(manager))

	if err := executor.ReconcileWorkerJob(ctx, worker.ProjectID, worker.ProviderInstanceID, worker.ID, "job-1", worker.Generation); err != nil {
		t.Fatalf("reconcile worker: %v", err)
	}
	updated, err := appStore.GetWorker(ctx, worker.ID)
	if err != nil {
		t.Fatalf("get updated worker: %v", err)
	}
	if !updated.Ready || !updated.Schedulable {
		t.Fatalf("worker ready/schedulable = %v/%v, want true/true", updated.Ready, updated.Schedulable)
	}
	if updated.Phase != model.WorkerPhaseActive || updated.LastOperationStatus != model.OperationStatusSuccess {
		t.Fatalf("worker phase/status = %q/%q, want active/success", updated.Phase, updated.LastOperationStatus)
	}
	if string(updated.RuntimeState) != `{"instanceId":"runtime-1"}` {
		t.Fatalf("runtime state = %s, want runtime-1", updated.RuntimeState)
	}
	if updated.AvailableCPUVCPUs != 4 || updated.AvailableMemoryBytes != 8<<30 || updated.AvailableStorageBytes != 20<<30 {
		t.Fatalf("worker capacity = cpu %v memory %d storage %d", updated.AvailableCPUVCPUs, updated.AvailableMemoryBytes, updated.AvailableStorageBytes)
	}
	if updated.LastSeenAt == nil {
		t.Fatal("worker last seen was not preserved")
	}
}

func TestReconcileWorkerDeletedRemovesRuntimeBeforeMarkingDeleted(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "removing", Name: "removing"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	worker := &model.Worker{
		ID:                 "worker-1",
		ProjectID:          "project-1",
		ProviderInstanceID: "provider-1",
		RuntimeState:       []byte(`{"instanceId":"runtime-1"}`),
		Ready:              true,
		Schedulable:        true,
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState:        model.WorkerDesiredStateDeleted,
			Phase:               model.WorkerPhaseDeleting,
			LastOperationStatus: model.OperationStatusPending,
			Generation:          2,
			ObservedGeneration:  1,
		},
	}
	if err := appStore.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}

	providerImpl := &countingWorkerProvider{}
	manager := sandboxes.NewProviderManager()
	manager.RegisterProvider("removing", providerImpl)
	executor := workers.NewWorkerReconcileExecutor(appStore, workers.WithWorkerProviderManager(manager))

	if err := executor.ReconcileWorkerJob(ctx, worker.ProjectID, worker.ProviderInstanceID, worker.ID, "job-1", worker.Generation); err != nil {
		t.Fatalf("reconcile worker delete: %v", err)
	}
	if providerImpl.removeCalls != 1 {
		t.Fatalf("RemoveWorker calls = %d, want 1", providerImpl.removeCalls)
	}
	updated, err := appStore.GetWorker(ctx, worker.ID)
	if err != nil {
		t.Fatalf("get updated worker: %v", err)
	}
	if updated.Phase != model.WorkerPhaseDeleted || updated.LastOperationStatus != model.OperationStatusSuccess {
		t.Fatalf("worker phase/status = %q/%q, want deleted/success", updated.Phase, updated.LastOperationStatus)
	}
	if updated.DesiredState != model.WorkerDesiredStateDeleted {
		t.Fatalf("worker desired state = %q, want deleted", updated.DesiredState)
	}
	if updated.RevokedAt == nil {
		t.Fatal("expected worker to be revoked after runtime removal")
	}
	if updated.RuntimeState != nil {
		t.Fatalf("runtime state = %s, want nil", updated.RuntimeState)
	}
}

func TestReconcileWorkerDeletedRefusesAssignedSandboxes(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "removing", Name: "removing"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	worker := &model.Worker{
		ID:                 "worker-1",
		ProjectID:          "project-1",
		ProviderInstanceID: "provider-1",
		RuntimeState:       []byte(`{"instanceId":"runtime-1"}`),
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState:        model.WorkerDesiredStateDeleted,
			Phase:               model.WorkerPhaseDeleting,
			LastOperationStatus: model.OperationStatusPending,
			Generation:          2,
			ObservedGeneration:  1,
		},
	}
	if err := appStore.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}
	providerID := provider.ID
	if err := appStore.CreateSandbox(ctx, &model.Sandbox{
		ID:                 "sandbox-1",
		ProjectID:          "project-1",
		CreatedByUserID:    "user-1",
		ProviderInstanceID: &providerID,
		WorkerID:           &worker.ID,
		Name:               "assigned sandbox",
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	providerImpl := &countingWorkerProvider{}
	manager := sandboxes.NewProviderManager()
	manager.RegisterProvider("removing", providerImpl)
	executor := workers.NewWorkerReconcileExecutor(appStore, workers.WithWorkerProviderManager(manager))

	if err := executor.ReconcileWorkerJob(ctx, worker.ProjectID, worker.ProviderInstanceID, worker.ID, "job-1", worker.Generation); err == nil {
		t.Fatal("expected assigned worker delete to fail")
	}
	if providerImpl.removeCalls != 0 {
		t.Fatalf("RemoveWorker calls = %d, want 0", providerImpl.removeCalls)
	}
	if providerImpl.repairCalls != 0 {
		t.Fatalf("RepairWorker calls = %d, want 0", providerImpl.repairCalls)
	}
	updated, err := appStore.GetWorker(ctx, worker.ID)
	if err != nil {
		t.Fatalf("get updated worker: %v", err)
	}
	if updated.Phase != model.WorkerPhaseFailed || updated.LastOperationStatus != model.OperationStatusFailed {
		t.Fatalf("worker phase/status = %q/%q, want failed/failed", updated.Phase, updated.LastOperationStatus)
	}
	if updated.RevokedAt != nil {
		t.Fatal("worker was revoked despite assigned sandbox")
	}
	if len(updated.RuntimeState) == 0 {
		t.Fatal("runtime state was cleared despite assigned sandbox")
	}
}

func TestReconcileWorkerRepairsAssignedWorkerAfterActiveReconcileFailure(t *testing.T) {
	ctx := context.Background()
	appStore := newExecutorTestStore(t)
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "repairing", Name: "repairing"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	worker := &model.Worker{
		ID:                 "worker-1",
		ProjectID:          "project-1",
		ProviderInstanceID: "provider-1",
		RuntimeState:       []byte(`{"instanceId":"stale-runtime"}`),
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState:        model.WorkerDesiredStateActive,
			Phase:               model.WorkerPhaseActive,
			LastOperationStatus: model.OperationStatusSuccess,
			Generation:          2,
			ObservedGeneration:  1,
		},
	}
	if err := appStore.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}
	providerID := provider.ID
	if err := appStore.CreateSandbox(ctx, &model.Sandbox{
		ID:                 "sandbox-1",
		ProjectID:          "project-1",
		CreatedByUserID:    "user-1",
		ProviderInstanceID: &providerID,
		WorkerID:           &worker.ID,
		Name:               "assigned sandbox",
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	providerImpl := &repairingWorkerProvider{reconcileErr: errors.New("runtime unhealthy")}
	manager := sandboxes.NewProviderManager()
	manager.RegisterProvider("repairing", providerImpl)
	executor := workers.NewWorkerReconcileExecutor(appStore, workers.WithWorkerProviderManager(manager))

	if err := executor.ReconcileWorkerJob(ctx, worker.ProjectID, worker.ProviderInstanceID, worker.ID, "job-1", worker.Generation); err != nil {
		t.Fatalf("reconcile worker: %v", err)
	}
	if providerImpl.repairCalls != 1 {
		t.Fatalf("RepairWorker calls = %d, want 1", providerImpl.repairCalls)
	}
	updated, err := appStore.GetWorker(ctx, worker.ID)
	if err != nil {
		t.Fatalf("get updated worker: %v", err)
	}
	if updated.LastOperationStatus != model.OperationStatusSuccess || updated.Phase != model.WorkerPhaseRegistering {
		t.Fatalf("worker phase/status = %q/%q, want registering/success", updated.Phase, updated.LastOperationStatus)
	}
	if string(updated.RuntimeState) != `{"instanceId":"repaired-runtime"}` {
		t.Fatalf("runtime state = %s, want repaired runtime", updated.RuntimeState)
	}
}

type failingWorkerProvider struct {
	noopWorkerSandboxProvider
	err error
}

func (p failingWorkerProvider) ReconcileWorker(context.Context, sandboxes.WorkerManager, *model.Project, *model.SandboxProviderInstance, *model.Worker) error {
	return p.err
}

func (p failingWorkerProvider) RepairWorker(context.Context, sandboxes.WorkerManager, *model.Project, *model.SandboxProviderInstance, *model.Worker, string) error {
	return p.err
}

func (p failingWorkerProvider) RemoveWorker(context.Context, sandboxes.WorkerManager, *model.Project, *model.SandboxProviderInstance, *model.Worker) error {
	return p.err
}

type countingWorkerProvider struct {
	noopWorkerSandboxProvider
	calls       int
	removeCalls int
	repairCalls int
}

func (p *countingWorkerProvider) ReconcileWorker(context.Context, sandboxes.WorkerManager, *model.Project, *model.SandboxProviderInstance, *model.Worker) error {
	p.calls++
	return nil
}

func (p *countingWorkerProvider) RepairWorker(context.Context, sandboxes.WorkerManager, *model.Project, *model.SandboxProviderInstance, *model.Worker, string) error {
	p.repairCalls++
	return nil
}

func (p *countingWorkerProvider) RemoveWorker(_ context.Context, _ sandboxes.WorkerManager, _ *model.Project, _ *model.SandboxProviderInstance, worker *model.Worker) error {
	p.removeCalls++
	worker.RuntimeState = nil
	return nil
}

type registeringWorkerProvider struct {
	noopWorkerSandboxProvider
	store *store.Store
}

func (p *registeringWorkerProvider) ReconcileWorker(ctx context.Context, _ sandboxes.WorkerManager, _ *model.Project, _ *model.SandboxProviderInstance, worker *model.Worker) error {
	worker.RuntimeState = []byte(`{"instanceId":"runtime-1"}`)
	_, err := p.store.UpdateWorkerStatus(ctx, worker.ID, true, true, false, 4, 8<<30, 20<<30, []byte(`{"status":"ready"}`))
	return err
}

func (p *registeringWorkerProvider) RepairWorker(context.Context, sandboxes.WorkerManager, *model.Project, *model.SandboxProviderInstance, *model.Worker, string) error {
	return nil
}

func (p *registeringWorkerProvider) RemoveWorker(context.Context, sandboxes.WorkerManager, *model.Project, *model.SandboxProviderInstance, *model.Worker) error {
	return nil
}

type repairingWorkerProvider struct {
	noopWorkerSandboxProvider
	reconcileErr error
	repairCalls  int
}

func (p *repairingWorkerProvider) ReconcileWorker(context.Context, sandboxes.WorkerManager, *model.Project, *model.SandboxProviderInstance, *model.Worker) error {
	return p.reconcileErr
}

func (p *repairingWorkerProvider) RepairWorker(_ context.Context, _ sandboxes.WorkerManager, _ *model.Project, _ *model.SandboxProviderInstance, worker *model.Worker, _ string) error {
	p.repairCalls++
	worker.RuntimeState = []byte(`{"instanceId":"repaired-runtime"}`)
	worker.Ready = true
	worker.Schedulable = true
	worker.Degraded = false
	return nil
}

func (p *repairingWorkerProvider) RemoveWorker(context.Context, sandboxes.WorkerManager, *model.Project, *model.SandboxProviderInstance, *model.Worker) error {
	return nil
}

type noopWorkerSandboxProvider struct{}

func (noopWorkerSandboxProvider) Initialize(context.Context, *model.SandboxProviderInstance) error {
	return nil
}

func (noopWorkerSandboxProvider) Close() error {
	return nil
}

func (noopWorkerSandboxProvider) Definition() sandboxes.ProviderDefinition {
	return sandboxes.ProviderDefinition{Name: "test"}
}

func (noopWorkerSandboxProvider) Status() sandboxes.ProviderStatus {
	return sandboxes.ProviderStatus{Available: true, State: "ready"}
}

func (noopWorkerSandboxProvider) Reconcile(context.Context) error {
	return nil
}

func (noopWorkerSandboxProvider) RemoveProject(context.Context, string) error {
	return nil
}

func (noopWorkerSandboxProvider) List(context.Context) ([]*sandboxes.Sandbox, error) {
	return nil, nil
}

func (noopWorkerSandboxProvider) Create(context.Context, sandboxes.SandboxRef, []byte, sandboxes.CreateOptions) (*sandboxes.Sandbox, []byte, error) {
	return nil, nil, nil
}

func (noopWorkerSandboxProvider) Update(context.Context, sandboxes.SandboxRef, []byte, sandboxes.UpdateOptions) (*sandboxes.Sandbox, []byte, error) {
	return nil, nil, nil
}

func (noopWorkerSandboxProvider) Start(context.Context, sandboxes.SandboxRef, []byte) (*sandboxes.Sandbox, []byte, error) {
	return nil, nil, nil
}

func (noopWorkerSandboxProvider) Stop(context.Context, sandboxes.SandboxRef, []byte, time.Duration) (*sandboxes.Sandbox, []byte, error) {
	return nil, nil, nil
}

func (noopWorkerSandboxProvider) Remove(context.Context, sandboxes.SandboxRef, []byte, ...sandboxes.RemoveOption) ([]byte, error) {
	return nil, nil
}

func (noopWorkerSandboxProvider) Get(context.Context, sandboxes.SandboxRef, []byte) (*sandboxes.Sandbox, error) {
	return nil, nil
}

func (noopWorkerSandboxProvider) AcquireHTTPClient(context.Context, sandboxes.SandboxRef, []byte, []string) (*sandboxes.HTTPClientLease, error) {
	return nil, nil
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
