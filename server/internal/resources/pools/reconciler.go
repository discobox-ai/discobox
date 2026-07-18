package pools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/reconcile"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/store"
)

// PoolResourceType is the reconcile-engine resource type for pools. A pool is
// its own runtime host (ADR-0006), so a pool reconcile converges one
// container/VM/pod.
const PoolResourceType = "pool"

// PoolDirtyID encodes the composite identity a pool reconcile needs. Pool
// lookups are project-scoped, so the dirty id carries both.
func PoolDirtyID(projectID, poolID string) string {
	return projectID + "/" + poolID
}

func splitPoolDirtyID(id string) (projectID, poolID string, err error) {
	projectID, poolID, ok := strings.Cut(id, "/")
	if !ok || projectID == "" || poolID == "" {
		return "", "", fmt.Errorf("invalid pool dirty id %q", id)
	}
	return projectID, poolID, nil
}

// poolRegistrationTimeout is how long a freshly created runtime may sit in
// the registering phase before the reconciler repairs it in place: the
// container/VM came up but its agent never called home, so it is replaced
// under the same pool identity with a fresh bootstrap token.
var poolRegistrationTimeout = 2 * time.Minute

// PoolReconciler converges one pool's runtime host toward its desired state.
// It implements reconcile.Reconciler (and reconcile.Scanner as the drift and
// lost-mark backstop).
type PoolReconciler struct {
	store   *store.Store
	manager *sandbox.ProviderManager
	pools   *ControlPlane
}

func NewPoolReconciler(appStore *store.Store, manager *sandbox.ProviderManager, controlPlane *ControlPlane) *PoolReconciler {
	return &PoolReconciler{store: appStore, manager: manager, pools: controlPlane}
}

// Reconcile loads the latest project + pool + provider state and converges
// the pool's runtime. Missing pools and missing or disabled providers are
// converged trivially (nothing to do), settling the dirty row.
func (r *PoolReconciler) Reconcile(ctx context.Context, id string) error {
	projectID, poolID, err := splitPoolDirtyID(id)
	if err != nil {
		return err
	}
	pool, err := r.store.GetPool(ctx, projectID, poolID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	generation := pool.Generation
	switch pool.DesiredState {
	case model.PoolDesiredStateActive:
		return r.reconcileActive(ctx, pool, generation)
	case model.PoolDesiredStateDeleted:
		return r.reconcileDeleted(ctx, pool, generation)
	default:
		return fmt.Errorf("unsupported pool desired state %q", pool.DesiredState)
	}
}

// ScanDirty is the level-triggered backstop: every pool is re-checked so
// drift without an event (lost mark, crashed watcher, driver that forgot to
// reschedule) heals on the next scan.
func (r *PoolReconciler) ScanDirty(ctx context.Context) ([]string, error) {
	projects, err := r.store.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for i := range projects {
		pools, err := r.store.ListPools(ctx, projects[i].ID)
		if err != nil {
			return nil, err
		}
		for j := range pools {
			ids = append(ids, PoolDirtyID(projects[i].ID, pools[j].ID))
		}
	}
	return ids, nil
}

func (r *PoolReconciler) reconcileActive(ctx context.Context, pool *model.Pool, generation int64) error {
	project, provider, runtimeProvider, err := r.resolve(ctx, pool)
	if err != nil {
		return err
	}
	if provider == nil || provider.Disabled {
		return nil
	}
	if runtimeProvider == nil {
		return nil // provider type does not own pool runtimes
	}

	alreadySuccessful := pool.ObservedGeneration == generation && pool.LastOperationStatus == model.OperationStatusSuccess
	status := "launching pool host"
	if alreadySuccessful {
		status = "checking pool host"
	} else {
		pool.SetPhase(model.PoolPhaseLaunching)
	}
	pool.MarkOperationRunning(&status)
	if err := r.update(ctx, pool, generation); err != nil {
		return err
	}

	// A runtime that came up but whose agent never registered is repaired in
	// place: the container/VM is replaced under the same pool identity with a
	// fresh bootstrap token. Only a pool that never registered can be in this
	// state, and repair (not delete) preserves the user-owned pool row.
	if r.registrationExpired(pool) {
		err = runtimeProvider.RepairPool(ctx, r.pools, project, provider, pool, "pool agent did not register before timeout")
	} else if err = runtimeProvider.ReconcilePool(ctx, r.pools, project, provider, pool); err != nil {
		if repairErr := r.repairAssignedPool(ctx, runtimeProvider, project, provider, pool, err); repairErr != nil {
			err = repairErr
		} else {
			err = nil
		}
	}
	if err != nil {
		r.failReconcile(pool, generation, err.Error())
		if updateErr := r.update(ctx, pool, generation); updateErr != nil {
			return updateErr
		}
		return err
	}

	current, err := r.store.GetPoolByID(ctx, pool.ID, store.WithPoolGeneration(generation))
	if errors.Is(err, store.ErrGenerationConflict) {
		return reconcile.Superseded("pool generation changed")
	}
	if err != nil {
		return err
	}
	if current.LastOperationStatus == model.OperationStatusFailed {
		return nil
	}
	current.RuntimeState = pool.RuntimeState
	current.Ready = pool.Ready
	current.Schedulable = pool.Schedulable
	current.Degraded = pool.Degraded
	current.ObservedGeneration = generation
	phase := model.PoolPhaseRegistering
	if alreadySuccessful {
		phase = current.Phase
	} else if current.RegisteredAt != nil || current.Ready {
		phase = model.PoolPhaseActive
	}
	current.CompleteOperation(phase, nil)
	return r.update(ctx, current, generation)
}

// registrationExpired reports a pool whose runtime creation succeeded but
// whose agent never registered within the timeout.
func (r *PoolReconciler) registrationExpired(pool *model.Pool) bool {
	if poolRegistrationTimeout <= 0 {
		return false
	}
	return pool.Phase == model.PoolPhaseRegistering &&
		pool.LastOperationStatus == model.OperationStatusRunning &&
		pool.RegisteredAt == nil &&
		pool.LastSeenAt == nil &&
		time.Since(pool.PhaseChangedAt) > poolRegistrationTimeout
}

func (r *PoolReconciler) reconcileDeleted(ctx context.Context, pool *model.Pool, generation int64) error {
	assigned, err := r.store.CountSandboxesForPool(ctx, pool.ProjectID, pool.ID)
	if err != nil {
		return err
	}
	if assigned > 0 {
		message := fmt.Sprintf("pool has %d assigned sandbox(es)", assigned)
		pool.ObservedGeneration = generation
		pool.FailOperation(message)
		if updateErr := r.update(ctx, pool, generation); updateErr != nil {
			return updateErr
		}
		return fmt.Errorf("%s", message)
	}
	status := "deleting pool host"
	pool.MarkOperationRunning(&status)
	if err := r.update(ctx, pool, generation); err != nil {
		return err
	}

	project, provider, runtimeProvider, err := r.resolve(ctx, pool)
	if err != nil {
		return err
	}
	if runtimeProvider != nil && provider != nil {
		if err := runtimeProvider.RemovePool(ctx, r.pools, project, provider, pool); err != nil {
			pool.ObservedGeneration = generation
			pool.FailOperation(err.Error())
			if updateErr := r.update(ctx, pool, generation); updateErr != nil {
				return updateErr
			}
			return err
		}
	}

	now := time.Now().UTC()
	pool.Ready = false
	pool.Schedulable = false
	pool.Degraded = false
	pool.RevokedAt = &now
	pool.RuntimeState = nil
	pool.ObservedGeneration = generation
	pool.CompleteOperation(model.PoolPhaseDeleted, nil)
	if err := r.update(ctx, pool, generation); err != nil {
		return err
	}
	// The runtime is gone and the row is terminal: soft-delete it so listings
	// drop the pool and the name can be reused.
	if err := r.store.DeletePool(ctx, pool.ProjectID, pool.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// failReconcile records a failed active reconcile. A pool whose runtime never
// completed its initial create reports the terminal-looking "failed" phase
// (there is no runtime yet), and one that was already created drops to
// "offline" (its runtime is stateful and must be reconciled back to health).
// Neither is schedulable until it recovers. Either way the failure is
// attributed to the generation that produced it, so a recorded failure with
// ObservedGeneration == Generation means "the latest intent was attempted,
// and it lost" — schedulers rely on that to tell a settled failure from one
// with a repair pending.
func (r *PoolReconciler) failReconcile(pool *model.Pool, generation int64, message string) {
	pool.ObservedGeneration = generation
	pool.Ready = false
	pool.Schedulable = false
	if !pool.EverCreated() {
		pool.FailOperation(message)
		return
	}
	pool.FailOperationRetryable(model.PoolPhaseOffline, message)
}

// repairAssignedPool repairs a pool whose reconcile failed while sandboxes
// are assigned: the runtime is stateful, so it is replaced in place rather
// than latched to failure.
func (r *PoolReconciler) repairAssignedPool(ctx context.Context, runtimeProvider sandbox.PoolRuntimeReconciler, project *model.Project, provider *model.SandboxProviderInstance, pool *model.Pool, cause error) error {
	assigned, err := r.store.CountSandboxesForPool(ctx, pool.ProjectID, pool.ID)
	if err != nil {
		return err
	}
	if assigned == 0 {
		return cause
	}
	reason := cause.Error()
	if err := runtimeProvider.RepairPool(ctx, r.pools, project, provider, pool, reason); err != nil {
		return fmt.Errorf("%s; repair pool: %w", reason, err)
	}
	return nil
}

func (r *PoolReconciler) resolve(ctx context.Context, pool *model.Pool) (*model.Project, *model.SandboxProviderInstance, sandbox.PoolRuntimeReconciler, error) {
	project, err := r.store.GetProject(ctx, pool.ProjectID)
	if err != nil {
		return nil, nil, nil, err
	}
	provider, err := r.store.GetSandboxProviderInstance(ctx, pool.ProjectID, pool.ProviderInstanceID)
	if errors.Is(err, store.ErrNotFound) {
		return project, nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil, err
	}
	if provider.Disabled {
		return project, provider, nil, nil
	}
	if r.manager == nil {
		return nil, nil, nil, fmt.Errorf("sandbox provider manager is required")
	}
	runtimeProvider, err := r.manager.ResolveInstance(ctx, provider)
	if err != nil {
		return nil, nil, nil, err
	}
	poolRuntime, ok := runtimeProvider.(sandbox.PoolRuntimeReconciler)
	if !ok {
		return project, provider, nil, nil
	}
	return project, provider, poolRuntime, nil
}

func (r *PoolReconciler) update(ctx context.Context, pool *model.Pool, generation int64) error {
	if err := r.store.UpdatePoolWithGeneration(ctx, pool, generation); err != nil {
		if errors.Is(err, store.ErrGenerationConflict) {
			return reconcile.Superseded("pool generation changed")
		}
		return err
	}
	return nil
}
