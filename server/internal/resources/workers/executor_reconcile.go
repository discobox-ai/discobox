package workers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/obot-platform/discobox/orchestration"
	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/store"
)

type WorkerReconcileExecutor struct {
	store            *store.Store
	manager          *sandbox.ProviderManager
	workerManager    any
	terminalHandlers []WorkerReconcileTerminalHandler
}

type WorkerReconcileExecutorOption func(*WorkerReconcileExecutor)

func NewWorkerReconcileExecutor(store *store.Store, options ...WorkerReconcileExecutorOption) *WorkerReconcileExecutor {
	executor := &WorkerReconcileExecutor{store: store}
	for _, option := range options {
		if option != nil {
			option(executor)
		}
	}
	return executor
}

func WithWorkerProviderManager(manager *sandbox.ProviderManager) WorkerReconcileExecutorOption {
	return func(executor *WorkerReconcileExecutor) {
		executor.manager = manager
	}
}

func WithWorkerManager(manager any) WorkerReconcileExecutorOption {
	return func(executor *WorkerReconcileExecutor) {
		executor.workerManager = manager
	}
}

func WithWorkerReconcileTerminalHandler(handler WorkerReconcileTerminalHandler) WorkerReconcileExecutorOption {
	return func(executor *WorkerReconcileExecutor) {
		if handler != nil {
			executor.terminalHandlers = append(executor.terminalHandlers, handler)
		}
	}
}

func (r *WorkerReconcileExecutor) AssertWorkerGeneration(ctx context.Context, projectID, providerID, workerID string, generation int64) error {
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

func (r *WorkerReconcileExecutor) ReconcileWorkerJob(ctx context.Context, projectID, providerID, workerID, jobID string, generation int64) error {
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
		return r.reconcileDeleted(ctx, worker, generation)
	default:
		return fmt.Errorf("unsupported worker desired state %q", worker.DesiredState)
	}
}

func (r *WorkerReconcileExecutor) reconcileDeleted(ctx context.Context, worker *model.Worker, generation int64) error {
	assigned, err := r.store.CountSandboxesForWorker(ctx, worker.ID)
	if err != nil {
		return err
	}
	if assigned > 0 {
		message := fmt.Sprintf("worker has %d assigned sandbox(es)", assigned)
		worker.FailOperation(message)
		if updateErr := r.update(ctx, worker, generation); updateErr != nil {
			return updateErr
		}
		return fmt.Errorf("%s", message)
	}
	status := "deleting worker"
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
	workerProvider, ok := runtimeProvider.(sandbox.WorkerRuntimeReconciler)
	if !ok {
		return fmt.Errorf("sandbox provider %q does not reconcile workers", provider.ID)
	}
	if err := workerProvider.RemoveWorker(ctx, r.reconcileManager(), project, provider, worker); err != nil {
		worker.FailOperation(err.Error())
		if updateErr := r.update(ctx, worker, generation); updateErr != nil {
			return updateErr
		}
		return err
	}

	now := time.Now().UTC()
	worker.Ready = false
	worker.Schedulable = false
	worker.Degraded = false
	worker.RevokedAt = &now
	worker.RuntimeState = nil
	worker.ObservedGeneration = generation
	worker.CompleteOperation(model.WorkerPhaseDeleted, nil)
	return r.update(ctx, worker, generation)
}

func (r *WorkerReconcileExecutor) reconcileActive(ctx context.Context, worker *model.Worker, generation int64) error {
	alreadySuccessful := worker.ObservedGeneration == generation && worker.LastOperationStatus == model.OperationStatusSuccess
	status := "launching worker"
	if alreadySuccessful {
		status = "checking worker"
	} else {
		worker.Phase = model.WorkerPhaseLaunching
	}
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
	workerProvider, ok := runtimeProvider.(sandbox.WorkerRuntimeReconciler)
	if !ok {
		return fmt.Errorf("sandbox provider %q does not reconcile workers", provider.ID)
	}
	if err := workerProvider.ReconcileWorker(ctx, r.reconcileManager(), project, provider, worker); err != nil {
		if repairErr := r.repairAssignedWorker(ctx, workerProvider, project, provider, worker, err); repairErr != nil {
			worker.FailOperation(repairErr.Error())
			if updateErr := r.update(ctx, worker, generation); updateErr != nil {
				return updateErr
			}
			return repairErr
		}
	}
	current, err := r.store.GetWorker(ctx, worker.ID, store.WithWorkerGeneration(generation))
	if errors.Is(err, store.ErrGenerationConflict) {
		return orchestration.Superseded("worker generation changed")
	}
	if err != nil {
		return err
	}
	if current.LastOperationStatus == model.OperationStatusFailed {
		return nil
	}
	current.RuntimeState = worker.RuntimeState
	current.ObservedGeneration = generation
	phase := model.WorkerPhaseRegistering
	if alreadySuccessful {
		phase = current.Phase
	} else if current.RegisteredAt != nil || current.Ready {
		phase = model.WorkerPhaseActive
	}
	current.CompleteOperation(phase, nil)
	return r.update(ctx, current, generation)
}

func (r *WorkerReconcileExecutor) repairAssignedWorker(ctx context.Context, workerProvider sandbox.WorkerRuntimeReconciler, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker, cause error) error {
	assigned, err := r.store.CountSandboxesForWorker(ctx, worker.ID)
	if err != nil {
		return err
	}
	if assigned == 0 {
		return cause
	}
	reason := cause.Error()
	if err := workerProvider.RepairWorker(ctx, r.reconcileManager(), project, provider, worker, reason); err != nil {
		return fmt.Errorf("%s; repair worker: %w", reason, err)
	}
	return nil
}

func (r *WorkerReconcileExecutor) completeNoop(ctx context.Context, worker *model.Worker, generation int64, phase string, status string) error {
	worker.MarkOperationRunning(&status)
	if err := r.update(ctx, worker, generation); err != nil {
		return err
	}
	worker.ObservedGeneration = generation
	worker.CompleteOperation(phase, nil)
	return r.update(ctx, worker, generation)
}

func (r *WorkerReconcileExecutor) update(ctx context.Context, worker *model.Worker, generation int64) error {
	if err := r.store.UpdateWorker(ctx, worker, store.WithWorkerGeneration(generation)); err != nil {
		if errors.Is(err, store.ErrGenerationConflict) {
			return orchestration.Superseded("worker generation changed")
		}
		return err
	}
	return nil
}

func (r *WorkerReconcileExecutor) reconcileManager() any {
	if r != nil && r.workerManager != nil {
		return r.workerManager
	}
	return r.store
}

func (r *WorkerReconcileExecutor) resolveProvider(ctx context.Context, provider *model.SandboxProviderInstance) (sandbox.Provider, error) {
	if r == nil || r.manager == nil {
		return nil, fmt.Errorf("sandbox provider manager is required")
	}
	return r.manager.ResolveInstance(ctx, provider)
}
