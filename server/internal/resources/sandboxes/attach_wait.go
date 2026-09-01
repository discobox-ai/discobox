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

// ErrSandboxPoolNotReachable marks the acquire refusal that means the pool
// hosting the sandbox is not taking traffic yet. It is a condition, not an
// answer: the pool agent registers and reports ready on its own, so a caller
// that can wait will see it clear.
var ErrSandboxPoolNotReachable = errors.New("sandbox pool is not reachable")

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

// sandboxReachableStallTimeout bounds tier 1 of the attach wait (ADR 0039).
//
// It is a stall budget rather than a cap on how long the wait may take: every
// write to this sandbox or to the pool hosting it restarts it, so a multi-
// gigabyte image pull that keeps reporting progress waits as long as the pull
// takes, and a sandbox that has gone silent gives up in two minutes. The tiers
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

// AwaitSandboxHTTPClient is AcquireSandboxHTTPClient for a caller that means
// "I want to use this sandbox now": it waits for the sandbox to become
// reachable instead of refusing an attach that arrived while it was still
// being provisioned (ADR 0039 tier 1).
//
// Only what the control plane can see is waited on here: the sandbox has been
// dispatched to a pool, that pool is up, and the sandbox is usable rather than
// still mid-provisioning or mid-source-delivery. What the container and the
// sandbox agent are doing is not observable from here, and is waited on by the
// tiers that can see it.
//
// Every pass re-reads authoritative state and asks again, so there is no window
// in which the transition that opens the gate can be missed.
func (s *Service) AwaitSandboxHTTPClient(ctx context.Context, projectID, sandboxID string, scopes []string) (*services.HTTPClientLease, *model.Sandbox, error) {
	stallDeadline := time.Now().Add(sandboxReachableStallTimeout)
	var observed provisioningMark
	for {
		lease, sandboxModel, err := s.AcquireSandboxHTTPClient(ctx, projectID, sandboxID, scopes)
		if err == nil && !sandboxProvisioningPending(sandboxModel) {
			return lease, sandboxModel, nil
		}
		if err == nil {
			// Reachable but not usable yet. The lease goes back rather than
			// being held across the wait: it is a pooled client onto the pool
			// agent, not a claim on the sandbox.
			if lease != nil {
				lease.Release()
			}
			err = apperrors.StatusError{
				Status:  http.StatusConflict,
				Message: fmt.Sprintf("sandbox is still being provisioned: state=%s", sandboxModel.State),
				Cause:   ErrSandboxProvisioning,
			}
		}
		// Acquire reports a provider-side refusal without the row it read, and
		// the row carries half of the question of whether waiting can help.
		if sandboxModel == nil {
			sandboxModel, _ = s.store.GetSandbox(ctx, projectID, sandboxID)
		}
		if !sandboxCanBecomeReachable(err, sandboxModel) {
			return nil, sandboxModel, err
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
			return nil, sandboxModel, err
		}
		select {
		case <-ctx.Done():
			return nil, sandboxModel, ctx.Err()
		case <-time.After(sandboxReachablePollInterval):
		}
	}
}

// provisioningMark is what the wait watches for change: the gate it is waiting
// on, plus the counters that tick while the work behind that gate proceeds.
//
// Both resources are in it. A sandbox waiting on a pool that is still coming up
// does not change at all, and `ErrSandboxPoolNotReachable` is a refusal only the
// pool row can clear. Unrelated project traffic is in neither, so a busy project
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
	poolStagedAt      int64
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
// pool's own provisioning and image staging). A new signal that means "this is
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
	mark.poolReady = pool.Ready
	mark.poolProgressAt = markTime(pool.ProvisionProgressAt)
	mark.poolStagedAt = markTime(pool.ImageStagedAt)
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
// pool it is on is not taking traffic yet; and the sandbox is reachable but
// still being provisioned. Every other refusal — a row that is not there, a
// scope the caller does not hold, a provider that cannot be resolved — is an
// answer, and waiting on one would replace a precise error with a deadline.
//
// The row answers the other half. The same refusals are what a sandbox on its
// way out produces, and no write will ever clear them for one: an archived
// sandbox has no container by intent (ADR 0022 §5), a deleting one is going
// away, and a settled failure needs new intent rather than another wait
// (ADR 0017 §4).
func sandboxCanBecomeReachable(err error, sb *model.Sandbox) bool {
	if !errors.Is(err, sandbox.ErrNotFound) &&
		!errors.Is(err, ErrSandboxPoolNotReachable) &&
		!errors.Is(err, ErrSandboxProvisioning) {
		return false
	}
	if sb == nil || sb.DesiredState != model.DesiredStatePresent {
		return false
	}
	switch sb.State {
	case model.SandboxStateFailed, model.SandboxStateArchived, model.SandboxStateDeleted:
		return false
	}
	return true
}
