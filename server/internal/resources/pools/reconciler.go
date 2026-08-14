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

// poolHeartbeatTimeout is how long a registered pool may go without a status
// heartbeat before the reconciler reads it as `offline`. The agent reports
// every 30s (pool-agent statusReportInterval), so this is three missed beats:
// long enough that one slow report or dropped connection does not flap the
// state, short enough that a dead host is called out within a scan or two.
var poolHeartbeatTimeout = 90 * time.Second

// heartbeatStale reports a pool whose agent has stopped answering: it has
// heartbeated before, and the last beat is older than poolHeartbeatTimeout. A
// pool that has never heartbeated is not stale — it is still registering, and
// that path has its own timeout (registrationExpired).
func heartbeatStale(pool *model.Pool) bool {
	if poolHeartbeatTimeout <= 0 || pool.LastSeenAt == nil {
		return false
	}
	return time.Since(*pool.LastSeenAt) > poolHeartbeatTimeout
}

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
func (r *PoolReconciler) Reconcile(ctx context.Context, id string) (reconcile.Result, error) {
	projectID, poolID, err := splitPoolDirtyID(id)
	if err != nil {
		return reconcile.Result{}, err
	}
	pool, err := r.store.GetPool(ctx, projectID, poolID)
	if errors.Is(err, store.ErrNotFound) {
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, err
	}
	generation := pool.Generation
	switch pool.DesiredState {
	case model.DesiredStatePresent:
		return r.reconcileActive(ctx, pool, generation)
	case model.DesiredStateDeleted:
		return reconcile.Result{}, r.reconcileDeleted(ctx, pool, generation)
	default:
		return reconcile.Result{}, fmt.Errorf("unsupported pool desired state %q", pool.DesiredState)
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

func (r *PoolReconciler) reconcileActive(ctx context.Context, pool *model.Pool, generation int64) (reconcile.Result, error) {
	project, provider, runtimeProvider, err := r.resolve(ctx, pool)
	if err != nil {
		return reconcile.Result{}, err
	}
	if provider == nil || provider.Disabled {
		return reconcile.Result{}, nil
	}
	if runtimeProvider == nil {
		return reconcile.Result{}, nil // provider type does not own pool runtimes
	}

	// Converged with no recorded error means this generation has already been
	// brought up once and we are re-checking it, not launching it. The pending
	// stamp is for launches only: a created pool's runtime keeps serving what
	// it already hosts while a spec change or retry converges, so repainting
	// it `pending` would misreport a live host as not-yet-up.
	alreadySuccessful := pool.Converged() && pool.ErrorMessage == nil
	if !alreadySuccessful && !pool.EverCreated() {
		pool.SetState(model.PoolStatePending)
		if err := r.update(ctx, pool, generation); err != nil {
			return reconcile.Result{}, err
		}
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
			return reconcile.Result{}, updateErr
		}
		return reconcile.Result{}, err
	}

	current, err := r.store.GetPoolByID(ctx, pool.ID, store.WithPoolGeneration(generation))
	if errors.Is(err, store.ErrGenerationConflict) {
		return reconcile.Result{}, reconcile.Superseded("pool generation changed")
	}
	if err != nil {
		return reconcile.Result{}, err
	}
	// Ready/Schedulable/Degraded and RegisteredAt are agent-reported fields,
	// written by RegisterPool/UpdatePoolStatus over their own HTTP calls, which
	// can land concurrently with EnsurePool/RepairPool waiting on container
	// health. Only RuntimeState is safe to copy from the pre-call local object;
	// copying the others would stomp a registration that already landed in the
	// DB with the stale pre-call value.
	current.RuntimeState = pool.RuntimeState
	current.ObservedGeneration = generation
	// The state is derived on every success, never carried over. A pool whose
	// agent has called home is active; one whose runtime exists but has not
	// been heard from is still registering. Preserving current.State on a
	// drift re-check instead only worked while registration wrote `active`
	// itself: the create reconcile converges the generation before the agent
	// registers, so every later pass took the preserve branch and a pool that
	// registered afterwards would stay `registering` forever.
	state := model.PoolStateRegistering
	if current.RegisteredAt != nil || current.Ready {
		state = model.PoolStateActive
	}
	current.SetState(state)
	// A recorded error is this reconciler's own verdict on an earlier attempt,
	// and the attempt that just succeeded disproves it, so success always
	// clears it. Skipping the clear because the row still carries an error is
	// what made ErrorMessage a one-way latch: it was unreachable for exactly
	// the pool that needed it, and a recovered pool reported its old failure
	// forever. Nothing else writes a pool's ErrorMessage, and a competing
	// intent is already caught by the generation guard above.
	current.ErrorMessage = nil
	// `offline` is a liveness observation, not a convergence verdict (ADR
	// 0017 §4): the host stopped answering and is expected back. It is derived
	// here, after the success derivation, so a runtime that converged but
	// whose agent has gone silent still reads offline. The message is derived
	// with it — freshly, every pass, so it is never a latch.
	if state == model.PoolStateActive && heartbeatStale(current) {
		current.RecordFailure(model.PoolStateOffline,
			fmt.Sprintf("pool agent has not reported since %s", current.LastSeenAt.UTC().Format(time.RFC3339)))
	}
	if err := r.update(ctx, current, generation); err != nil {
		return reconcile.Result{}, err
	}
	return armRegistrationTimeout(current), nil
}

// armRegistrationTimeout is the deadline half of registrationExpired: a pool
// whose runtime came up but whose agent has not registered yet needs one wake
// at the moment that stops being worth waiting for, and nothing else would
// deliver it — registration lands over the agent's own HTTP call, and a pool
// that never registers produces no event at all.
//
// It arms ONLY for a pool that is still waiting. Arming it unconditionally
// would put a timer on every healthy pool in the fleet, which is the drift
// re-check the 60s scan already does.
func armRegistrationTimeout(pool *model.Pool) reconcile.Result {
	if poolRegistrationTimeout <= 0 || pool.StateChangedAt.IsZero() {
		return reconcile.Result{}
	}
	if pool.State != model.PoolStateRegistering || pool.RegisteredAt != nil || pool.LastSeenAt != nil {
		return reconcile.Result{}
	}
	return reconcile.RequeueAt(pool.StateChangedAt.Add(poolRegistrationTimeout))
}

// registrationExpired reports a pool whose runtime creation succeeded but
// whose agent never registered within the timeout.
func (r *PoolReconciler) registrationExpired(pool *model.Pool) bool {
	if poolRegistrationTimeout <= 0 {
		return false
	}
	return pool.State == model.PoolStateRegistering &&
		pool.RegisteredAt == nil &&
		pool.LastSeenAt == nil &&
		time.Since(pool.StateChangedAt) > poolRegistrationTimeout
}

func (r *PoolReconciler) reconcileDeleted(ctx context.Context, pool *model.Pool, generation int64) error {
	assigned, err := r.store.CountSandboxesForPool(ctx, pool.ProjectID, pool.ID)
	if err != nil {
		return err
	}
	if assigned > 0 {
		message := fmt.Sprintf("pool has %d assigned sandbox(es)", assigned)
		pool.ObservedGeneration = generation
		pool.RecordFailure(model.PoolStateFailed, message)
		if updateErr := r.update(ctx, pool, generation); updateErr != nil {
			return updateErr
		}
		return fmt.Errorf("%s", message)
	}
	project, provider, runtimeProvider, err := r.resolve(ctx, pool)
	if err != nil {
		return err
	}
	if runtimeProvider != nil && provider != nil {
		if err := runtimeProvider.RemovePool(ctx, r.pools, project, provider, pool); err != nil {
			pool.ObservedGeneration = generation
			pool.RecordFailure(model.PoolStateFailed, err.Error())
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
	pool.SetState(model.PoolStateDeleted)
	pool.ErrorMessage = nil
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
// (there is no runtime yet). A created pool keeps its state: its runtime is
// stateful and stays serving whatever it already hosts, so a failed
// convergence is an ErrorMessage against this generation, not a state — a
// live, heartbeating pool with a failing reconcile is degraded, not offline.
// `offline` is reserved for its ADR 0017 §4 meaning, a host that stopped
// answering, which is derived from heartbeat staleness. Either way the
// failure is attributed to the generation that produced it, so a recorded
// failure with ObservedGeneration == Generation means "the latest intent was
// attempted, and it lost" — schedulers rely on that to tell a settled failure
// from one with a repair pending.
//
// Ready and Schedulable are cleared only on the never-created path: for a
// created pool they are the agent's facts (see the ownership table in
// DESIGN.md), and a live agent would repaint them within a heartbeat anyway.
func (r *PoolReconciler) failReconcile(pool *model.Pool, generation int64, message string) {
	pool.ObservedGeneration = generation
	if !pool.EverCreated() {
		pool.Ready = false
		pool.Schedulable = false
		pool.RecordFailure(model.PoolStateFailed, message)
		return
	}
	if heartbeatStale(pool) {
		pool.RecordFailure(model.PoolStateOffline, message)
		return
	}
	pool.ErrorMessage = &message
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
