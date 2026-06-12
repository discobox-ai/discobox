package sandbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/obot-platform/discobox/internal/model"
	"github.com/obot-platform/discobox/internal/store"
	"github.com/obot-platform/discobox/orchestration"
)

type WorkerReconciler struct {
	store   *store.Store
	manager *ProviderManager
}

type WorkerReconcilerOption func(*WorkerReconciler)

func NewWorkerReconciler(store *store.Store, options ...WorkerReconcilerOption) *WorkerReconciler {
	reconciler := &WorkerReconciler{store: store}
	for _, option := range options {
		if option != nil {
			option(reconciler)
		}
	}
	return reconciler
}

func WithWorkerProviderManager(manager *ProviderManager) WorkerReconcilerOption {
	return func(reconciler *WorkerReconciler) {
		reconciler.manager = manager
	}
}

func (r *WorkerReconciler) AssertWorkerGeneration(ctx context.Context, projectID, providerID, workerID string, generation int64) error {
	worker, err := r.store.GetWorker(ctx, workerID, store.WithWorkerGeneration(generation))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if errors.Is(err, store.ErrGenerationConflict) {
			return orchestration.Superseded("worker generation changed")
		}
		return err
	}
	if worker.ProjectID != projectID || worker.ProviderInstanceID != providerID {
		return orchestration.Superseded("worker placement changed")
	}
	return nil
}

func (r *WorkerReconciler) ReconcileWorkerJob(ctx context.Context, projectID, providerID, workerID, jobID string, generation int64) error {
	worker, err := r.store.GetWorker(ctx, workerID, store.WithWorkerGeneration(generation))
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if errors.Is(err, store.ErrGenerationConflict) {
		return orchestration.Superseded("worker generation changed")
	}
	if err != nil {
		return err
	}
	if worker.ProjectID != projectID || worker.ProviderInstanceID != providerID {
		return orchestration.Superseded("worker placement changed")
	}

	worker.LastJobID = &jobID
	switch worker.DesiredState {
	case model.WorkerDesiredStateActive:
		return r.reconcileActive(ctx, worker, generation)
	case model.WorkerDesiredStateDrained:
		return r.completeNoop(ctx, worker, generation, model.WorkerPhaseDraining, "draining worker")
	case model.WorkerDesiredStateDeleted:
		return r.completeNoop(ctx, worker, generation, model.WorkerPhaseDeleted, "deleting worker")
	default:
		return fmt.Errorf("unsupported worker desired state %q", worker.DesiredState)
	}
}

func (r *WorkerReconciler) reconcileActive(ctx context.Context, worker *model.Worker, generation int64) error {
	if worker.ObservedGeneration == generation && worker.LastOperationStatus == model.OperationStatusSuccess {
		return nil
	}
	status := "launching worker"
	worker.Phase = model.WorkerPhaseLaunching
	worker.MarkOperationRunning(&status)
	if err := r.update(ctx, worker, generation); err != nil {
		return err
	}

	project, err := r.store.GetProject(ctx, worker.ProjectID)
	if err != nil {
		return err
	}
	provider, err := r.store.GetSandboxProviderInstance(ctx, worker.ProjectID, worker.ProviderInstanceID)
	if err != nil {
		return err
	}
	runtimeProvider, err := r.resolveProvider(ctx, provider)
	if err != nil {
		return err
	}
	workerProvider, ok := runtimeProvider.(WorkerRuntimeReconciler)
	if !ok {
		return fmt.Errorf("sandbox provider %q does not reconcile workers", provider.ID)
	}
	if err := workerProvider.ReconcileWorker(ctx, r.store, project, provider, worker); err != nil {
		return err
	}

	worker.ObservedGeneration = generation
	worker.CompleteOperation(model.WorkerPhaseRegistering, nil)
	return r.update(ctx, worker, generation)
}

func (r *WorkerReconciler) completeNoop(ctx context.Context, worker *model.Worker, generation int64, phase string, status string) error {
	worker.MarkOperationRunning(&status)
	if err := r.update(ctx, worker, generation); err != nil {
		return err
	}
	worker.ObservedGeneration = generation
	worker.CompleteOperation(phase, nil)
	return r.update(ctx, worker, generation)
}

func (r *WorkerReconciler) update(ctx context.Context, worker *model.Worker, generation int64) error {
	if err := r.store.UpdateWorker(ctx, worker, store.WithWorkerGeneration(generation)); err != nil {
		if errors.Is(err, store.ErrGenerationConflict) {
			return orchestration.Superseded("worker generation changed")
		}
		return err
	}
	return nil
}

func (r *WorkerReconciler) resolveProvider(ctx context.Context, provider *model.SandboxProviderInstance) (Provider, error) {
	if r == nil || r.manager == nil {
		return nil, fmt.Errorf("sandbox provider manager is required")
	}
	return r.manager.ResolveInstance(ctx, provider)
}
