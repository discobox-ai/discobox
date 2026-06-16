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

type failingWorkerProvider struct {
	sandbox.Provider
	err error
}

func (p failingWorkerProvider) ReconcileWorker(context.Context, any, *model.Project, *model.SandboxProviderInstance, *model.Worker) error {
	return p.err
}
