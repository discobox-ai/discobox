package pools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/pkg/pooloutbound"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
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
// the `registering` state before the reconciler repairs it in place: the
// container/VM came up but its agent never called home, so it is replaced
// under the same pool identity, and its new box enrolled anew.
var poolRegistrationTimeout = 2 * time.Minute

// poolHeartbeatTimeout is how long a registered pool may go without a status
// heartbeat before the reconciler reads it as `offline`. The agent reports
// every 30s (pool-agent statusReportInterval), so this is three missed beats:
// long enough that one slow report or dropped connection does not flap the
// state, short enough that a dead host is called out within a scan or two.
var poolHeartbeatTimeout = model.PoolHeartbeatTimeout

// poolHostComingUpRequeue is how long to wait before re-checking a pool whose
// host is up but not yet taking traffic — a container whose healthcheck has not
// passed yet. A pool's container probes its agent every 5s, so this is one
// probe interval: the soonest the answer can have changed.
var poolHostComingUpRequeue = 5 * time.Second

// poolSlotDrainingRequeue is how soon a pool whose box moved nothing, because
// a slot still drains from its last handover (sandbox.ErrPoolSlotDraining),
// is reconciled again. A drain ends when what it carries does, within 30
// minutes.
var poolSlotDrainingRequeue = 30 * time.Second

// heartbeatStale reports a pool whose agent has stopped answering: it has
// heartbeated before, and the last beat is older than poolHeartbeatTimeout. A
// pool that has never heartbeated is not stale — it is still registering, and
// that path has its own timeout (registrationExpired).
func heartbeatStale(pool *model.Pool) bool {
	if poolHeartbeatTimeout <= 0 || pool.LastSeenAt == nil {
		return false
	}
	lastSeen := *pool.LastSeenAt
	// A previous server run's silence must not immediately re-create an
	// offline failure while this run is waiting for its first heartbeat.
	if pool.HealthCheckStartedAt != nil && pool.HealthCheckStartedAt.After(lastSeen) {
		lastSeen = *pool.HealthCheckStartedAt
	}
	return time.Since(lastSeen) > poolHeartbeatTimeout
}

// PoolReconciler converges one pool's runtime host toward its desired state.
// It implements reconcile.Reconciler (and reconcile.Scanner as the drift and
// lost-mark backstop).
type PoolReconciler struct {
	store   *store.Store
	manager *sandbox.ProviderManager
	pools   *ControlPlane
	// sandboxes is handed the pool as each settle found it and left it, so a
	// pool this reconcile makes active while it reports ready sends its
	// sandboxes' due wakes (ADR 26-11-15-829 §2).
	sandboxes SandboxDelivery

	// hostWaits is, by pool, when its passes began waiting on a host still
	// coming up (sandbox.ErrPoolNotReachable). Those passes requeue every
	// few seconds, so the wait is logged when it begins and when a pass
	// ends it, not on every pass.
	hostWaitsMu sync.Mutex
	hostWaits   map[string]time.Time
}

func NewPoolReconciler(appStore *store.Store, manager *sandbox.ProviderManager, controlPlane *ControlPlane, sandboxes SandboxDelivery) *PoolReconciler {
	return &PoolReconciler{store: appStore, manager: manager, pools: controlPlane, sandboxes: sandboxes}
}

// Reconcile loads the latest project + pool + provider state and converges
// the pool's runtime. Missing pools, and providers that are gone or whose
// type this server cannot build, are converged trivially (nothing to do),
// settling the dirty row; a disabled provider is converged like any other
// (ADR 26-11-22-507). A run whose
// generation-guarded write lost to newer intent settles too: that intent's
// mark re-runs the pool, and returning the error would back that run off as
// if this one had failed.
func (r *PoolReconciler) Reconcile(ctx context.Context, id string) (reconcile.Result, error) {
	result, err := r.reconcile(ctx, id)
	if errors.Is(err, reconcile.ErrSuperseded) {
		return reconcile.Result{}, nil
	}
	return result, err
}

func (r *PoolReconciler) reconcile(ctx context.Context, id string) (reconcile.Result, error) {
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
	if err := r.pools.ResolveOutbound(ctx, pool); err != nil {
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

// noteWaiting keeps what a never-registered attempt's pass waited on, an
// error marked sandbox.ErrPoolNotReachable, or clears it when the pass waited
// on nothing, so that an attempt that expires names only a wait it never got
// past (ADR 26-11-15-089 §5). It writes only when that changes: a host
// waited on is asked again every few seconds. A write that fails loses only
// the display.
func (r *PoolReconciler) noteWaiting(ctx context.Context, pool *model.Pool, generation int64, waiting string) {
	b := pool.BootstrapFacts()
	if b == nil || pool.RegisteredAt != nil || b.Ended(generation) || b.Waiting == waiting {
		return
	}
	_, _ = sandbox.ReportBootstrap(ctx, model.BootstrapEvidence{Waiting: waiting, WaitEnded: waiting == ""})
}

func (r *PoolReconciler) reconcileActive(ctx context.Context, pool *model.Pool, generation int64) (reconcile.Result, error) {
	start := time.Now()
	// A pass that does not wait on the host ends any wait earlier passes
	// began, however it ends; one that settles says how long it was.
	waitingOnHost := false
	defer func() {
		if !waitingOnHost {
			r.endHostWait(pool.ID)
		}
	}()
	// An attempt that expired without saying so ends on this pass: record why,
	// show the pool failed, and settle with the error. The next attempt starts
	// on a later pass (ADR 26-11-09-593 §2).
	if b := pool.BootstrapFacts(); b != nil && pool.RegisteredAt == nil && !b.Ended(generation) {
		if reason := b.Expired(time.Now()); reason != "" {
			_, _ = r.store.RecordPoolBootstrap(ctx, pool.ID, b.Attempt, generation, model.BootstrapEvidence{Failure: reason})
			failure := fmt.Sprintf("pool %s bootstrap %s: %s", pool.ID, b.Operation, reason)
			r.failReconcile(pool, generation, failure)
			if err := r.update(ctx, pool, generation); err != nil {
				return reconcile.Result{}, err
			}
			return reconcile.Result{}, fmt.Errorf("%s", failure)
		}
	}

	project, provider, runtimeProvider, err := r.resolve(ctx, pool)
	if err != nil {
		return reconcile.Result{}, err
	}
	if provider == nil {
		return reconcile.Result{}, nil
	}
	if runtimeProvider == nil {
		return reconcile.Result{}, nil // provider type does not own pool runtimes
	}

	// An ended attempt does not end a pool that never registered: a later pass
	// starts the next one, whether the last one failed or belongs to an
	// earlier generation (ADR 26-11-09-593 §1). A failed one is replaced once
	// the engine's backoff schedule for the attempts that failed so far has
	// passed. The engine's own count cannot pace this: an attempt whose
	// provider call succeeds but whose agent never registers settles that
	// pass cleanly, which resets it. New intent is not paced. The
	// replacement happens only once the provider is callable, so that the
	// new attempt's first provider call is the repair it owes (§3).
	if b := pool.BootstrapFacts(); b != nil && pool.RegisteredAt == nil && b.Ended(generation) {
		if b.Failure != "" && b.Generation == generation {
			due := b.EndedAt.Add(reconcile.Backoff(reconcile.DefaultBackoffBase, reconcile.DefaultBackoffMax, b.Failed))
			if time.Now().Before(due) {
				return reconcile.RequeueAt(due), nil
			}
		}
		next, err := r.store.ReplacePoolBootstrap(ctx, pool.ID, b.Attempt, generation)
		if errors.Is(err, store.ErrGenerationConflict) {
			return reconcile.Result{}, reconcile.Superseded("pool bootstrap attempt changed")
		}
		if err != nil {
			return reconcile.Result{}, err
		}
		data, err := json.Marshal(next)
		if err != nil {
			return reconcile.Result{}, err
		}
		pool.Bootstrap = data
	}
	ctx = sandbox.WithBootstrapExecutor(ctx, r.pools, pool)
	// A provider that keeps the box the pool's host last provisioned still
	// settles cleanly; the upgrade watch hears it here (ADR 26-11-14-624 §1).
	var keptMu sync.Mutex
	var kept error
	ctx = sandbox.WithBoxKeptReporter(ctx, func(reason error) {
		keptMu.Lock()
		defer keptMu.Unlock()
		kept = reason
	})
	// A provider that starts the pool's agent, or is about to restart it in a
	// running box, says when and which, and the watch hears it at once, not
	// at the settle: the pass that starts it is often not the one that
	// settles (ADR 26-11-15-294 §1, 26-11-15-602 §1).
	poolID := pool.ID
	var agentStarted bool
	ctx = sandbox.WithAgentStartReporter(ctx, func(at time.Time, kind sandbox.AgentStartKind) {
		keptMu.Lock()
		agentStarted = true
		keptMu.Unlock()
		if r.pools != nil {
			r.pools.Upgrades().agentStarted(poolID, at, kind)
		}
	})
	// repair names why this pass replaces the runtime rather than reconciling
	// it in place; empty means reconcile. An attempt owes a repair until one
	// succeeds, so a pass that dies before it leaves it owed (§3).
	var repair string
	if b := pool.BootstrapFacts(); b != nil && pool.RegisteredAt == nil && b.Generation == generation {
		repair = b.Repair
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

	if pool.OutboundAppliedRevision != pool.Outbound.Revision() || len(pool.OutboundProbes) == 0 {
		if resolved, resolveErr := pool.Outbound.ResolveLocalForwarder(); resolveErr == nil {
			pool.OutboundStatus.Probes["server"] = pooloutbound.Check(ctx, resolved)
			if encoded, marshalErr := json.Marshal(pool.OutboundStatus.Probes); marshalErr == nil {
				pool.OutboundProbes = encoded
			}
		}
	}
	// The engine calls this only for a new/replaced agent, before it removes
	// the old runtime or preloads images. Persist the gate first so stale ready
	// heartbeats from the old agent cannot admit work during a replacement.
	begin := func(ctx context.Context) error {
		pool.SetState(model.PoolStateRegistering)
		return r.update(ctx, pool, generation)
	}

	// A runtime that came up but whose agent never registered is repaired in
	// place: the container/VM is replaced under the same pool identity, and
	// its new box enrolled anew. Only a pool that never registered can be in this
	// state, and repair (not delete) preserves the user-owned pool row.
	if repair == "" && r.registrationExpired(pool) {
		repair = "pool agent did not register before timeout"
	}
	if repair != "" {
		if err = runtimeProvider.RepairPool(ctx, r.pools, project, provider, pool, repair, begin); err == nil {
			_, _ = sandbox.ReportBootstrap(ctx, model.BootstrapEvidence{Repaired: true})
		}
	} else if err = runtimeProvider.ReconcilePool(ctx, r.pools, project, provider, pool, begin); err != nil {
		// A pool whose last handover still drains moved nothing and failed
		// nothing: it serves as it was. This pass settles nothing, records no
		// failure, and asks again once the drain may have ended.
		if errors.Is(err, sandbox.ErrPoolSlotDraining) {
			slog.InfoContext(ctx, "pool waits for its last handover to drain before it moves", "pool", pool.ID, "requeue", poolSlotDrainingRequeue)
			return reconcile.RequeueAfter(poolSlotDrainingRequeue), nil
		}
		// A host that is still coming up is not a failed reconcile. Repairing
		// one removes and recreates the container whose healthcheck has not
		// passed yet, which restarts that healthcheck — and a pool container
		// restarts for ordinary reasons (a Docker restart, a development image
		// rebuild), usually with a sandbox attaching to it waiting on exactly
		// that healthcheck (ADR 0039 tier 1). So this pass settles nothing and
		// asks again: no failure is recorded, no repair runs, the pool keeps
		// the state it had, and this generation is not marked observed over a
		// runtime the reconcile never reached.
		if errors.Is(err, sandbox.ErrPoolNotReachable) {
			waitingOnHost = true
			r.beginHostWait(ctx, pool.ID, err)
			r.noteWaiting(ctx, pool, generation, err.Error())
			return reconcile.RequeueAfter(poolHostComingUpRequeue), nil
		}
		// begin's guarded write lost to newer intent mid-call: repairing now
		// would replace the host for intent that no longer stands.
		if errors.Is(err, reconcile.ErrSuperseded) {
			return reconcile.Result{}, err
		}
		if errors.Is(err, sandbox.ErrPoolConfigApply) {
			r.failReconcile(pool, generation, err.Error())
			if updateErr := r.update(ctx, pool, generation); updateErr != nil {
				return reconcile.Result{}, updateErr
			}
			return reconcile.Result{}, err
		}
	}
	// The pass waited on nothing: a wait an earlier pass recorded is past.
	r.noteWaiting(ctx, pool, generation, "")
	// A repair's error is never read as a host coming up: it may come before
	// the repair replaced anything (discovm's first lookup), and reconciling
	// in place would then trust the runtime the repair was owed for.
	if err != nil {
		// The attempt ends with this error, and the pass settles failed. A
		// later pass starts a new attempt (ADR 26-11-09-593 §2).
		if b := pool.BootstrapFacts(); b != nil && pool.RegisteredAt == nil && b.Generation == generation {
			_, _ = sandbox.ReportBootstrap(ctx, model.BootstrapEvidence{Failure: err.Error()})
			r.failReconcile(pool, generation, err.Error())
			if updateErr := r.update(ctx, pool, generation); updateErr != nil {
				return reconcile.Result{}, updateErr
			}
			return reconcile.Result{}, err
		}
	}
	if err != nil && repair == "" {
		if repairErr := r.repairAssignedPool(ctx, runtimeProvider, project, provider, pool, err, begin); repairErr != nil {
			err = repairErr
		} else {
			// The settle says this pass repaired the pool, and why.
			repair = firstLine(err.Error())
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
	found := *current
	// Ready/Schedulable/Degraded and RegisteredAt are agent-reported fields,
	// written by the agent's UpdatePoolStatus over its own HTTP calls, which
	// can land concurrently with EnsurePool/RepairPool waiting on container
	// health. Only what the provider writes — RuntimeState and HostCredential —
	// is safe to copy from the pre-call local object; copying the others would
	// stomp a registration that already landed in the DB with the stale
	// pre-call value.
	current.OutboundProbes = pool.OutboundProbes
	current.OutboundAppliedRevision = pool.OutboundAppliedRevision
	current.OutboundRestartRequired = pool.OutboundRestartRequired
	current.RuntimeState = pool.RuntimeState
	current.HostCredential = pool.HostCredential
	current.ObservedGeneration = generation
	settledAt := time.Now().UTC()
	current.ReconciledAt = &settledAt
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
	current.ClearFailure()
	// `offline` is a liveness observation, not a convergence verdict (ADR
	// 0017 §4): the host stopped answering and is expected back. It is derived
	// here, after the success derivation, so a runtime that converged but
	// whose agent has gone silent still reads offline. The message is derived
	// with it — freshly, every pass, so it is never a latch.
	if state == model.PoolStateActive && heartbeatStale(current) {
		current.RecordFailure(model.PoolStateOffline,
			fmt.Sprintf("pool agent has not reported since %s", current.LastSeenAt.UTC().Format(time.RFC3339)), "")
	}
	// Before the row: a watch pass that reads this settle must already know
	// whether it kept the previous box, or it would take the old agent's next
	// report for the new one's. A row write that then fails leaves the record
	// for an earlier settle, which only keeps the pool waited on.
	keptMu.Lock()
	if r.pools != nil {
		r.pools.Upgrades().settled(current.ID, settledAt, kept)
	}
	did := settleNews{repair: repair, agentStarted: agentStarted, kept: kept}
	keptMu.Unlock()
	if err := r.settle(ctx, &found, current, generation); err != nil {
		return reconcile.Result{}, err
	}
	did.hostWait = r.endHostWait(pool.ID)
	logSettled(ctx, &found, current, did, settledAt.Sub(start))
	return armRegistrationTimeout(current), nil
}

// beginHostWait records that the pool's passes now wait on its host coming
// up, and logs it if they were not already.
func (r *PoolReconciler) beginHostWait(ctx context.Context, poolID string, reason error) {
	r.hostWaitsMu.Lock()
	defer r.hostWaitsMu.Unlock()
	if _, ok := r.hostWaits[poolID]; ok {
		return
	}
	if r.hostWaits == nil {
		r.hostWaits = map[string]time.Time{}
	}
	r.hostWaits[poolID] = time.Now()
	slog.InfoContext(ctx, "waiting for the pool's host to come up", "pool", poolID, "reason", firstLine(reason.Error()))
}

// endHostWait ends the pool's wait on its host, and answers how long it
// was, zero when there was none.
func (r *PoolReconciler) endHostWait(poolID string) time.Duration {
	r.hostWaitsMu.Lock()
	defer r.hostWaitsMu.Unlock()
	began, ok := r.hostWaits[poolID]
	if !ok {
		return 0
	}
	delete(r.hostWaits, poolID)
	return time.Since(began)
}

// settleNews is what a settling pass did beyond re-checking the pool.
type settleNews struct {
	repair       string
	agentStarted bool
	kept         error
	hostWait     time.Duration
}

// logSettled logs a pass that settled, and how long it took. The drift scan
// re-checks every pool every minute, so a pass that found the pool as it was
// and did nothing to it logs at debug; one that is the pool's first settle in
// this run, observed a new generation, moved its state, repaired it, started
// its agent, kept its previous box or waited on its host logs at info: the
// steps of a restart are those.
func logSettled(ctx context.Context, found, current *model.Pool, did settleNews, took time.Duration) {
	level := slog.LevelDebug
	if !reconciledThisRun(found) || found.ObservedGeneration != current.ObservedGeneration || found.State != current.State ||
		did.repair != "" || did.agentStarted || did.kept != nil || did.hostWait > 0 {
		level = slog.LevelInfo
	}
	attrs := []any{"pool", current.ID, "state", current.State, "generation", current.ObservedGeneration, "duration", took.Round(time.Millisecond)}
	if did.hostWait > 0 {
		attrs = append(attrs, "host_wait", did.hostWait.Round(time.Millisecond))
	}
	if did.repair != "" {
		attrs = append(attrs, "repair", did.repair)
	}
	if did.agentStarted {
		attrs = append(attrs, "agent_started", true)
	}
	if did.kept != nil {
		attrs = append(attrs, "kept_previous_box", firstLine(did.kept.Error()))
	}
	slog.Log(ctx, level, "pool reconcile settled", attrs...)
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
	if b := pool.BootstrapFacts(); b != nil && pool.RegisteredAt == nil && b.Generation == pool.Generation {
		return reconcile.RequeueAt(b.Allowance())
	}
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
	if b := pool.BootstrapFacts(); b != nil && pool.RegisteredAt == nil && b.Generation == pool.Generation {
		return false
	}

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
		pool.RecordFailure(model.PoolStateFailed, message, "")
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
			pool.RecordFailure(model.PoolStateFailed, err.Error(), "")
			if updateErr := r.update(ctx, pool, generation); updateErr != nil {
				return updateErr
			}
			return err
		}
	}

	now := time.Now().UTC()
	pool.RevokedAt = &now
	pool.RuntimeState = nil
	pool.HostCredential = nil
	pool.ObservedGeneration = generation
	pool.SetState(model.PoolStateDeleted)
	pool.ClearFailure()
	if err := r.update(ctx, pool, generation); err != nil {
		return err
	}
	// The runtime is gone and the row is terminal: delete it outright
	// (ADR 0010) so listings drop the pool and the name can be reused.
	if err := r.store.DeletePool(ctx, pool.ProjectID, pool.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// failReconcile records a failed active reconcile. A pool whose runtime never
// completed its initial create reports the terminal-looking "failed" state
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
// Agent health belongs to status reports. A lifecycle write never overwrites
// one, even if a heartbeat arrived while the runtime operation was failing.
func (r *PoolReconciler) failReconcile(pool *model.Pool, generation int64, message string) {
	settledAt := time.Now().UTC()
	pool.ReconciledAt = &settledAt
	pool.ObservedGeneration = generation
	if !pool.EverCreated() {
		pool.RecordFailure(model.PoolStateFailed, message, "")
		return
	}
	if heartbeatStale(pool) {
		pool.RecordFailure(model.PoolStateOffline, message, "")
		return
	}
	pool.ErrorMessage = &message
}

// repairAssignedPool repairs a pool whose reconcile failed while sandboxes
// are assigned: the runtime is stateful, so it is replaced in place rather
// than latched to failure.
func (r *PoolReconciler) repairAssignedPool(ctx context.Context, runtimeProvider sandbox.PoolRuntime, project *model.Project, provider *model.SandboxProviderInstance, pool *model.Pool, cause error, begin func(context.Context) error) error {
	assigned, err := r.store.CountSandboxesForPool(ctx, pool.ProjectID, pool.ID)
	if err != nil {
		return err
	}
	if assigned == 0 {
		return cause
	}
	reason := cause.Error()
	if err := runtimeProvider.RepairPool(ctx, r.pools, project, provider, pool, reason, begin); err != nil {
		return fmt.Errorf("%s; repair pool: %w", reason, err)
	}
	return nil
}

func (r *PoolReconciler) resolve(ctx context.Context, pool *model.Pool) (*model.Project, *model.SandboxProviderInstance, sandbox.PoolRuntime, error) {
	project, err := r.store.GetProject(ctx, pool.ProjectID)
	if err != nil {
		return nil, nil, nil, err
	}
	provider, err := convergedProvider(ctx, r.store, r.manager, pool)
	if err != nil {
		return nil, nil, nil, err
	}
	if provider == nil {
		return project, nil, nil, nil
	}
	runtimeProvider, err := r.manager.ResolveInstance(ctx, provider)
	if err != nil {
		return nil, nil, nil, err
	}
	poolRuntime, ok := runtimeProvider.(sandbox.PoolRuntime)
	if !ok {
		return project, provider, nil, nil
	}
	return project, provider, poolRuntime, nil
}

// convergedProvider is the pool's provider instance when the reconciler
// converges a host for it, and nil when it converges nothing: the instance is
// gone, or its type cannot be built on this server (ADR 26-11-22-507 §2). A
// disabled instance is converged like an enabled one (§1). Both the
// reconciler and the upgrade watch decide it here, and a pool it answers nil
// for is deleted as its row alone.
func convergedProvider(ctx context.Context, appStore *store.Store, manager *sandbox.ProviderManager, pool *model.Pool) (*model.SandboxProviderInstance, error) {
	if manager == nil {
		return nil, fmt.Errorf("sandbox provider manager is required")
	}
	provider, err := appStore.GetSandboxProviderInstance(ctx, pool.ProjectID, pool.ProviderInstanceID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !manager.Buildable(provider.Type) {
		return nil, nil
	}
	return provider, nil
}

// settle writes a successful reconcile's pool and hands the sandbox service
// the pool as this reconcile found it and as it leaves it, in one
// transaction. A pool back from `offline` turns `active` here, after the
// report that earned it, so it is here that its due wakes are sent: no report
// sees that turn (ADR 26-11-15-829 §2).
func (r *PoolReconciler) settle(ctx context.Context, found, pool *model.Pool, generation int64) error {
	if r.sandboxes == nil {
		return r.update(ctx, pool, generation)
	}
	return r.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		if err := txStore.UpdatePoolWithGeneration(ctx, pool, generation); err != nil {
			if errors.Is(err, store.ErrGenerationConflict) {
				return reconcile.Superseded("pool generation changed")
			}
			return err
		}
		return r.sandboxes.MarkPoolDueWakesTx(ctx, txStore, txDB, found, pool)
	})
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
