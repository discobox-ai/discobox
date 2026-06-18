package sandbox_test

import (
	"context"
	"errors"
	"testing"

	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/server/internal/sandbox"
)

func TestReconcileWorkerMarksLaunchFailure(t *testing.T) {
	ctx := context.Background()
	appStore := newReconcilerTestStore(t)
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
	manager := sandbox.NewProviderManager()
	manager.RegisterProvider("failing", failingWorkerProvider{err: launchErr})
	reconciler := sandbox.NewWorkerReconciler(appStore, sandbox.WithWorkerProviderManager(manager))

	err := reconciler.ReconcileWorkerJob(ctx, worker.ProjectID, worker.ProviderInstanceID, worker.ID, "job-1", worker.Generation)
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
	appStore := newReconcilerTestStore(t)
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
	manager := sandbox.NewProviderManager()
	manager.RegisterProvider("counting", providerImpl)
	reconciler := sandbox.NewWorkerReconciler(appStore, sandbox.WithWorkerProviderManager(manager))

	if err := reconciler.ReconcileWorkerJob(ctx, worker.ProjectID, worker.ProviderInstanceID, worker.ID, "job-1", worker.Generation); err != nil {
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

func TestReconcileWorkerDeletedRemovesRuntimeBeforeMarkingDeleted(t *testing.T) {
	ctx := context.Background()
	appStore := newReconcilerTestStore(t)
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
			Phase:               model.WorkerPhaseDeleted,
			LastOperationStatus: model.OperationStatusPending,
			Generation:          2,
			ObservedGeneration:  1,
		},
	}
	if err := appStore.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}

	providerImpl := &countingWorkerProvider{}
	manager := sandbox.NewProviderManager()
	manager.RegisterProvider("removing", providerImpl)
	reconciler := sandbox.NewWorkerReconciler(appStore, sandbox.WithWorkerProviderManager(manager))

	if err := reconciler.ReconcileWorkerJob(ctx, worker.ProjectID, worker.ProviderInstanceID, worker.ID, "job-1", worker.Generation); err != nil {
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

type failingWorkerProvider struct {
	sandbox.Provider
	err error
}

func (p failingWorkerProvider) ReconcileWorker(context.Context, any, *model.Project, *model.SandboxProviderInstance, *model.Worker) error {
	return p.err
}

func (p failingWorkerProvider) RemoveWorker(context.Context, any, *model.Project, *model.SandboxProviderInstance, *model.Worker) error {
	return p.err
}

type countingWorkerProvider struct {
	sandbox.Provider
	calls       int
	removeCalls int
}

func (p *countingWorkerProvider) ReconcileWorker(context.Context, any, *model.Project, *model.SandboxProviderInstance, *model.Worker) error {
	p.calls++
	return nil
}

func (p *countingWorkerProvider) RemoveWorker(_ context.Context, _ any, _ *model.Project, _ *model.SandboxProviderInstance, worker *model.Worker) error {
	p.removeCalls++
	worker.RuntimeState = nil
	return nil
}
