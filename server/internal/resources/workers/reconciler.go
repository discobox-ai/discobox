package workers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/reconcile"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/store"
)

// WorkerResourceType is the reconcile-engine resource type for workers. The
// dirty id is the worker id (globally unique).
const WorkerResourceType = "worker"

// ProviderChain marks a worker's provider dirty. The worker reconciler calls
// it after every reconcile so the pool re-evaluates its scaling math — the
// level-triggered replacement for the old terminal-observer job chaining.
type ProviderChain func(ctx context.Context, projectID, providerID string) error

// Reconcile drives the worker toward its desired state, reading the LATEST
// persisted intent (there is no accepted-generation payload: level-triggered
// reconciliation always converges on current state). The worker's own
// generation guards the writes; a mid-run generation conflict means newer
// intent arrived, whose transactional mark re-runs us — so conflicts settle
// this run as a no-op rather than failing it.
func (r *WorkerReconciler) Reconcile(ctx context.Context, workerID string) error {
	worker, err := r.store.GetWorker(ctx, workerID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	err = r.ReconcileWorker(ctx, worker)
	if errors.Is(err, reconcile.ErrSuperseded) {
		err = nil // superseded: the newer intent's mark re-runs us
	}
	// Chain the provider on success and on failure alike (the pool's math
	// changed either way). Marks coalesce, so this is one upsert.
	if r.chain != nil {
		if chainErr := r.chain(ctx, worker.ProjectID, worker.ProviderInstanceID); chainErr != nil && err == nil {
			err = chainErr
		}
	}
	return err
}

// stuckWorkerCutoff is how long a worker may sit with an in-flight operation
// (pending/running) before the scan re-marks it. Normal reconciles finish in
// seconds; a stale in-flight operation means a mark was lost.
const stuckWorkerCutoff = 10 * time.Minute

// ScanDirty is the level-triggered backstop: workers whose recorded operation
// has been in flight for implausibly long are re-marked. Terminal states
// (success/failed) are deliberately excluded — failed never-created workers
// are terminal by design, and failed created workers are re-driven by the
// worker pool at its own cadence.
func (r *WorkerReconciler) ScanDirty(ctx context.Context) ([]string, error) {
	return r.store.ListWorkerIDsWithStaleOperations(ctx, time.Now().Add(-stuckWorkerCutoff))
}

// WorkerReconciler converges workers toward their desired state. It implements
// reconcile.Reconciler and reconcile.Scanner (see reconciler.go for the engine
// entry points); this file holds the convergence logic.
type WorkerReconciler struct {
	store         *store.Store
	manager       *sandbox.ProviderManager
	workerManager sandbox.WorkerManager
	chain         ProviderChain
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

func WithWorkerProviderManager(manager *sandbox.ProviderManager) WorkerReconcilerOption {
	return func(reconciler *WorkerReconciler) {
		reconciler.manager = manager
	}
}

func WithWorkerManager(manager sandbox.WorkerManager) WorkerReconcilerOption {
	return func(reconciler *WorkerReconciler) {
		reconciler.workerManager = manager
	}
}

// WithProviderChain marks the worker's provider dirty after every reconcile so
// the pool re-evaluates its scaling math.
func WithProviderChain(chain ProviderChain) WorkerReconcilerOption {
	return func(reconciler *WorkerReconciler) {
		reconciler.chain = chain
	}
}

// ReconcileWorker converges the given (freshly loaded) worker toward its
// desired state. The worker's current generation guards every write, so newer
// intent arriving mid-run surfaces as a Superseded error, which the reconciler
// maps to a clean settle (the newer intent's own mark re-runs the reconcile).
func (r *WorkerReconciler) ReconcileWorker(ctx context.Context, worker *model.Worker) error {
	generation := worker.Generation
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

func (r *WorkerReconciler) reconcileDeleted(ctx context.Context, worker *model.Worker, generation int64) error {
	assigned, err := r.store.CountSandboxesForWorker(ctx, worker.ID)
	if err != nil {
		return err
	}
	if assigned > 0 {
		message := fmt.Sprintf("worker has %d assigned sandbox(es)", assigned)
		worker.ObservedGeneration = generation
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
		worker.ObservedGeneration = generation
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

func (r *WorkerReconciler) reconcileActive(ctx context.Context, worker *model.Worker, generation int64) error {
	alreadySuccessful := worker.ObservedGeneration == generation && worker.LastOperationStatus == model.OperationStatusSuccess
	status := "launching worker"
	if alreadySuccessful {
		status = "checking worker"
	} else {
		worker.SetPhase(model.WorkerPhaseLaunching)
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
			r.failReconcile(worker, generation, repairErr.Error())
			if updateErr := r.update(ctx, worker, generation); updateErr != nil {
				return updateErr
			}
			return repairErr
		}
	}
	current, err := r.store.GetWorker(ctx, worker.ID, store.WithWorkerGeneration(generation))
	if errors.Is(err, store.ErrGenerationConflict) {
		return reconcile.Superseded("worker generation changed")
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

// failReconcile records a failed active reconcile. The worker keeps its slot in
// the pool either way and is retried in place: a worker that never completed its
// initial create reports the terminal-looking "failed" phase (there is no
// runtime yet), and one that was already created drops to "offline" (its runtime
// is stateful and must be reconciled back to health). Neither is schedulable
// until it recovers.
//
// Either way the failure is attributed to the generation that produced it, so
// a recorded failure with ObservedGeneration == Generation means "the latest
// intent was attempted, and it lost". Schedulers rely on that to tell a settled
// failure from one with a repair pending (which bumps the generation).
func (r *WorkerReconciler) failReconcile(worker *model.Worker, generation int64, message string) {
	worker.ObservedGeneration = generation
	worker.Ready = false
	worker.Schedulable = false
	if !worker.EverCreated() {
		worker.FailOperation(message)
		return
	}
	worker.FailOperationRetryable(model.WorkerPhaseOffline, message)
}

func (r *WorkerReconciler) repairAssignedWorker(ctx context.Context, workerProvider sandbox.WorkerRuntimeReconciler, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker, cause error) error {
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
			return reconcile.Superseded("worker generation changed")
		}
		return err
	}
	return nil
}

func (r *WorkerReconciler) reconcileManager() sandbox.WorkerManager {
	if r == nil {
		return nil
	}
	return r.workerManager
}

func (r *WorkerReconciler) resolveProvider(ctx context.Context, provider *model.SandboxProviderInstance) (sandbox.Provider, error) {
	if r == nil || r.manager == nil {
		return nil, fmt.Errorf("sandbox provider manager is required")
	}
	return r.manager.ResolveInstance(ctx, provider)
}
