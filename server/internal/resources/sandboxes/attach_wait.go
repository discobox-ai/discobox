package sandboxes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// ErrSandboxProvisioning marks a sandbox that can be reached but is not ready
// to be used: its reconciler has not finished the generation it is acting on,
// or it is parked waiting for its source to be pushed.
//
// Reachable and usable are not the same thing, and the gap is the push-delivered
// source. Such a sandbox has a container — and therefore a runtime state naming
// its pool — from the moment it parks, so the acquire succeeds while its
// workspace is still empty. Using it then would auto-start it (ADR 0017 §12)
// and launch the harness against an unmaterialized workspace, which is the one
// thing the whole push handshake exists to prevent.
var ErrSandboxProvisioning = errors.New("sandbox is still being provisioned")

// sandboxReachableStallTimeout bounds the wait an action makes for its sandbox
// (ADR 0039 tier 1, ADR 26-10-12-711 §6).
//
// It is a stall budget rather than a cap on how long the wait may take: every
// write to this sandbox or to the pool hosting it restarts it, so a multi-
// gigabyte image pull that keeps reporting progress waits as long as the pull
// takes, and a sandbox that has gone silent gives up in two minutes. A pool
// host that is not there is bounded by sandboxPoolHostWaitCeiling instead,
// because that refusal moves the mark this budget reads. The tiers
// below take budgets that fit inside this one, so the innermost stage to stall
// is the one that reports.
const sandboxReachableStallTimeout = 2 * time.Minute

// sandboxReachablePollInterval is how long the wait sleeps between attempts.
//
// The wait is a poll because the thing it is waiting on is a row (ADR 0081).
// This is the whole of the latency it adds once the gate opens, and it is set
// against work that takes seconds at best: a container being created, an image
// being pulled, a source tree being materialized.
const sandboxReachablePollInterval = 500 * time.Millisecond

// sandboxPoolHostWaitCeiling bounds how long tier 1 waits for the pool host
// itself to come back, whatever the stall budget says.
//
// The stall budget cannot bound this one, because the wait causes the progress
// it reads. Reaching the host is the provider's acquire, a failed acquire marks
// the pool for reconcile, and every reconcile of a pool stamps its provision
// progress — one of the signals provisioningMark counts as movement. Left to
// the stall budget alone, a wait on a host that is never coming back renews
// itself for as long as the pool keeps reconciling.
//
// The create path meets the same problem with the same answer, and this is its
// poolAgentWaitCeiling: long enough for a container recreate that pulls a new
// sandbox base image, and no longer.
//
// A var so a test can reach the ceiling without spending five minutes on it.
var sandboxPoolHostWaitCeiling = 5 * time.Minute

// acquireForAction is the acquire for a request that is about to act in the
// sandbox: it returns a lease only onto a sandbox that can take the request.
//
// It waits for a sandbox that is still being provisioned instead of refusing
// it, which is what lets a client create a sandbox and use it in the next call
// rather than polling for readiness (ADR 0039 tier 1, widened from attach to
// every action by ADR 26-10-12-711 §6). It then has the pool start the sandbox
// (ensureRunning), and a sandbox the pool holds with no container is repaired
// once or waited for (rebuildOnUse).
//
// Only what the control plane can see is waited on here: the sandbox has been
// dispatched to a pool, that pool is up, the sandbox is usable rather than
// still mid-provisioning or mid-source-delivery, and its container is there.
// What the sandbox agent is doing is not observable from here, and is waited
// on by the tiers that can see it.
func (s *Service) acquireForAction(ctx context.Context, projectID, sandboxID string, scopes []string, use services.SandboxUse) (*services.HTTPClientLease, *model.Sandbox, error) {
	var lease *services.HTTPClientLease
	sandboxModel, err := s.awaitUsable(ctx, projectID, sandboxID, use, waitFromTheStart, func(ctx context.Context) (*model.Sandbox, error) {
		acquired, sandboxModel, err := s.acquireSandboxHTTPClient(ctx, projectID, sandboxID, scopes, use.Kind)
		if err != nil {
			return sandboxModel, err
		}
		if sandboxProvisioningPending(sandboxModel) {
			// Reachable but not usable yet.
			err = apperrors.StatusError{
				Status:  http.StatusConflict,
				Message: fmt.Sprintf("sandbox is still being provisioned: state=%s", sandboxModel.State),
				Cause:   ErrSandboxProvisioning,
			}
		} else {
			err = s.ensureRunning(ctx, sandboxModel)
		}
		if err != nil {
			// The lease goes back rather than being held across the wait: it
			// is a pooled client onto the pool agent, not a claim on the
			// sandbox.
			if acquired != nil {
				acquired.Release()
			}
			return sandboxModel, err
		}
		lease = acquired
		return sandboxModel, nil
	})
	if err != nil {
		return nil, sandboxModel, err
	}
	return lease, sandboxModel, nil
}

// runtimeHasNoContainer reports whether the pool refused for want of the
// sandbox's container while holding its tree, which is what a rebuild answers.
//
// The pool says it two ways. One is ErrNoContainer. The other is ErrArchived
// for a sandbox this side means to exist: a repair tears the runtime down
// before it rebuilds it, the teardown is the archive's and leaves its marker,
// and only the create that follows clears it. So "archived" there is a rebuild
// between its two halves, or one whose second half failed — the state the one
// automatic repair leaves when it fails. A sandbox archived by intent never
// gets this far: it is refused before its pool is asked.
func runtimeHasNoContainer(err error) bool {
	return errors.Is(err, ErrNoContainer) || errors.Is(err, ErrArchived)
}

// useWait is when a use of a sandbox starts waiting for it.
type useWait bool

const (
	// waitFromTheStart waits out every "not yet" from the first attempt: a
	// request into the sandbox, which has nothing better to do than wait.
	waitFromTheStart useWait = true
	// waitOnceRebuilding answers every refusal at once until the use finds
	// the sandbox with no container and a rebuild coming, and waits from
	// then: a power instruction, which is a command that wants an answer.
	waitOnceRebuilding useWait = false
)

// awaitUsable runs attempt until it succeeds, waiting between attempts for the
// sandbox to become usable, and returns the sandbox the last attempt read.
//
// An attempt that finds the sandbox with no container sets a rebuild in
// motion where one is due (rebuildOnUse), and is waited for from then on
// whatever wait says.
//
// Every pass re-reads authoritative state and asks again, so there is no window
// in which the transition that opens the gate can be missed.
func (s *Service) awaitUsable(ctx context.Context, projectID, sandboxID string, use services.SandboxUse, wait useWait, attempt func(context.Context) (*model.Sandbox, error)) (*model.Sandbox, error) {
	stallDeadline := time.Now().Add(sandboxReachableStallTimeout)
	var observed provisioningMark
	var hostDownSince time.Time
	// The last "not yet", which is what a caller's deadline is answered with.
	var refusal error
	waiting := wait == waitFromTheStart
	// Whether this wait has already asked for the rebuild of a sandbox that
	// lost its container without failing. Once is enough: the mark is durable.
	var marked bool
	for {
		sandboxModel, err := attempt(ctx)
		if err == nil {
			return sandboxModel, nil
		}
		if ctx.Err() != nil {
			// The caller's time ran out inside the attempt, and what its reads
			// said then is about the context, not the sandbox: a store read
			// cut short reads as "sandbox not found".
			return sandboxModel, awaitEnded(ctx, refusal)
		}
		// An attempt reports a provider-side refusal without the row it read,
		// and the row carries half of the question of whether waiting can help.
		if sandboxModel == nil {
			sandboxModel, _ = s.store.GetSandbox(ctx, projectID, sandboxID)
		}
		if sandboxModel != nil && runtimeHasNoContainer(err) {
			rebuilding, rebuildErr := s.rebuildOnUse(ctx, sandboxModel, use, &marked)
			if rebuildErr != nil {
				return sandboxModel, rebuildErr
			}
			if !rebuilding {
				return sandboxModel, err
			}
			waiting = true
			// The row this pass read may be one generation behind the intent
			// just recorded, and the mark below has to see the new one.
			if current, getErr := s.store.GetSandbox(ctx, projectID, sandboxID); getErr == nil {
				sandboxModel = current
			}
		} else if !waiting || !sandboxCanBecomeReachable(err, sandboxModel) {
			return sandboxModel, err
		}
		// A host that is not there has its own ceiling, for the reason
		// sandboxPoolHostWaitCeiling gives: this wait stamps the progress that
		// would otherwise re-arm the stall budget forever.
		if errors.Is(err, sandbox.ErrPoolNotReachable) {
			if hostDownSince.IsZero() {
				hostDownSince = time.Now()
			} else if time.Since(hostDownSince) >= sandboxPoolHostWaitCeiling {
				return sandboxModel, err
			}
		} else {
			hostDownSince = time.Time{}
		}
		// The first pass always counts as progress, which only re-arms a budget
		// armed a moment ago.
		if mark := s.provisioningMark(ctx, sandboxModel); mark != observed {
			observed = mark
			stallDeadline = time.Now().Add(sandboxReachableStallTimeout)
		}
		if !time.Now().Before(stallDeadline) {
			// The last refusal names what never became true, which is more use
			// than "timed out" on its own.
			return sandboxModel, err
		}
		refusal = err
		select {
		case <-ctx.Done():
			return sandboxModel, awaitEnded(ctx, refusal)
		case <-time.After(sandboxReachablePollInterval):
		}
	}
}

// awaitEnded is what a wait whose context is done returns. A caller's deadline
// is a bound like the stall budget and gets the same answer: the refusal that
// never cleared. A cancel is the caller leaving, and is told so.
func awaitEnded(ctx context.Context, refusal error) error {
	if refusal != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return refusal
	}
	return ctx.Err()
}

// provisioningMark is what the wait watches for change: the gate it is waiting
// on, plus the counters that tick while the work behind that gate proceeds.
//
// Both resources are in it. A sandbox waiting on a pool that is still coming up
// does not change at all, and `sandbox.ErrPoolNotReachable` is a refusal only
// the pool can clear. Unrelated project traffic is in neither, so a busy project
// cannot hold a stalled sandbox open.
type provisioningMark struct {
	sandboxState      string
	sandboxGeneration int64
	sandboxObserved   int64
	sandboxRuntime    string
	sandboxProgressAt int64
	poolState         string
	poolReady         bool
	poolProgressAt    int64
}

// provisioningMark reads the mark for one sandbox and the pool hosting it.
//
// Named fields rather than the rows' `updated_at`, and the difference is the
// stall budget. Both rows are rewritten on a timer by things that are liveness
// rather than progress — the pool agent's status heartbeat every 30 seconds,
// and the complete state sync restamping every sandbox it hosts every 60 — and
// either would refresh a two-minute budget forever, so a sandbox that was never
// coming up would wait until its caller gave up instead. It is the same trap
// ResourceLifecycle.SetState guards against, for the same reason.
//
// What is here is the gate (the sandbox's lifecycle state and convergence, its
// runtime state, the pool's state and readiness) and the progress counters that
// justify waiting through a long provision (the sandbox's pull progress, and the
// pool's own provisioning, including image preload). A new signal that means "this is
// still moving" belongs here too.
func (s *Service) provisioningMark(ctx context.Context, sb *model.Sandbox) provisioningMark {
	mark := provisioningMark{
		sandboxState:      sb.State,
		sandboxGeneration: sb.Generation,
		sandboxObserved:   sb.ObservedGeneration,
		sandboxRuntime:    sb.RuntimeState,
		sandboxProgressAt: markTime(sb.ProvisionProgressAt),
	}
	if sb.PoolID == "" {
		return mark
	}
	// A pool that cannot be read is not progress. The acquire is what decides
	// whether that is fatal; here it only means the mark did not move.
	pool, err := s.store.GetPool(ctx, sb.ProjectID, sb.PoolID)
	if err != nil {
		return mark
	}
	mark.poolState = pool.State
	mark.poolReady = pool.IsReady()
	mark.poolProgressAt = markTime(pool.ProvisionProgressAt)
	return mark
}

func markTime(at *time.Time) int64 {
	if at == nil {
		return 0
	}
	return at.UnixNano()
}

// sandboxProvisioningPending reports whether the sandbox is reachable but not
// ready to be used yet.
//
// Two things say so. An unobserved generation means the reconciler is still
// acting on the sandbox's current intent — the state after complete-source-push,
// where the container exists but the pushed source has not been materialized
// into it. `awaiting_source` means the push itself has not happened. Attaching
// through either would start the sandbox out from under the delivery it is
// waiting on (ADR 0039).
func sandboxProvisioningPending(sb *model.Sandbox) bool {
	if sb == nil {
		return false
	}
	return !sb.Converged() || sb.State == model.SandboxStateAwaitingSource
}

// sandboxCanBecomeReachable reports whether waiting can turn this failed
// acquire into a success.
//
// Three refusals are "not yet": the sandbox has no runtime state naming a pool,
// because its reconciler has not created the runtime and persisted it yet; the
// pool it is on is not taking traffic yet, whether its row says so or its
// driver sees a host still coming up; and the sandbox is reachable but still
// being provisioned. Every other refusal — a row that is not there, a
// scope the caller does not hold, a provider that cannot be resolved — is an
// answer, and waiting on one would replace a precise error with a deadline.
//
// The row answers the other half. The same refusals are what a sandbox on its
// way out produces, and no write will ever clear them for one: an archived
// sandbox has no container by intent (ADR 0022 §5), a deleting one is going
// away, and a settled failure needs new intent rather than another wait
// (ADR 0017 §4).
//
// A failure is settled only while its generation is observed. Once new intent
// is recorded — an upgrade, typed or automatic (ADR 0121), or a repair — the
// row still reads `failed` until the reconciler writes, but the create it is
// retrying is exactly what the wait is for, and the answer arrives either way:
// `ready`, or `failed` again and settled.
func sandboxCanBecomeReachable(err error, sb *model.Sandbox) bool {
	if sb == nil || sb.DesiredState != model.DesiredStatePresent {
		return false
	}
	if !errors.Is(err, sandbox.ErrNotFound) &&
		!errors.Is(err, sandbox.ErrPoolNotReachable) &&
		!errors.Is(err, ErrSandboxProvisioning) {
		return false
	}
	switch sb.State {
	case model.SandboxStateFailed:
		return !sb.Converged()
	case model.SandboxStateArchived, model.SandboxStateDeleted:
		return false
	}
	return true
}
