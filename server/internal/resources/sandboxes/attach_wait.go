package sandboxes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/obot-platform/discobox/server/internal/apperrors"
	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	services "github.com/obot-platform/discobox/server/internal/services"
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
// event about this sandbox or the pool hosting it restarts it, so a multi-
// gigabyte image pull that keeps reporting progress waits as long as the pull
// takes, and a sandbox that has gone silent gives up in two minutes. The tiers
// below take budgets that fit inside this one, so the innermost stage to stall
// is the one that reports.
const sandboxReachableStallTimeout = 2 * time.Minute

// sandboxReachableRecheckInterval is how long the wait will sit on a silent
// subscription before re-asking anyway. It is slow enough not to be the
// mechanism — the events are — and fast enough that a dropped one costs seconds
// rather than the whole stall budget.
const sandboxReachableRecheckInterval = 15 * time.Second

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
// The wait is event-driven. Writes publish a project event after commit, and
// the subscription is taken before the first attempt, so the transition that
// opens the gate cannot land in the window between a failed acquire and the
// subscription.
func (s *Service) AwaitSandboxHTTPClient(ctx context.Context, projectID, sandboxID string, scopes []string) (*services.HTTPClientLease, *model.Sandbox, error) {
	var projectEvents <-chan model.ProjectEvent
	if s.broker != nil {
		subscription, cancel := context.WithCancel(ctx)
		defer cancel()
		events, unsubscribe := s.broker.Subscribe(subscription, projectID)
		defer unsubscribe()
		projectEvents = events
	}

	stallDeadline := time.Now().Add(sandboxReachableStallTimeout)
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
		if projectEvents == nil {
			return nil, sandboxModel, err
		}
		// Acquire reports a provider-side refusal without the row it read, and
		// the row carries half of the question of whether waiting can help.
		if sandboxModel == nil {
			sandboxModel, _ = s.store.GetSandbox(ctx, projectID, sandboxID)
		}
		if !sandboxCanBecomeReachable(err, sandboxModel) {
			return nil, sandboxModel, err
		}
		progressed, waitErr := awaitSandboxProgress(ctx, projectEvents, sandboxID, sandboxModel.PoolID, stallDeadline)
		if waitErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, sandboxModel, ctxErr
			}
			// The last refusal names what never became true, which is more use
			// than "timed out" on its own.
			return nil, sandboxModel, err
		}
		if progressed {
			stallDeadline = time.Now().Add(sandboxReachableStallTimeout)
		}
	}
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
// way out produces, and no event will ever clear them for one: an archived
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

// awaitSandboxProgress blocks until something that could change the answer has
// happened, and reports whether that was progress on this sandbox: an event
// about it, or about the pool that has to be up for it to be reachable.
// Unrelated project traffic neither wakes the wait nor refreshes its budget, so
// a busy project cannot hold a stalled sandbox open.
//
// It also returns on its own after sandboxReachableRecheckInterval, without
// calling that progress. That is a backstop rather than a poll: the broker
// drops an event into a subscriber that is not reading — the acquire this loop
// runs between waits is exactly such a moment — and a dropped wake-up would
// otherwise cost the whole stall budget for a sandbox that is already up.
func awaitSandboxProgress(ctx context.Context, projectEvents <-chan model.ProjectEvent, sandboxID, poolID string, stallDeadline time.Time) (bool, error) {
	stall := time.NewTimer(time.Until(stallDeadline))
	defer stall.Stop()
	recheck := time.NewTimer(sandboxReachableRecheckInterval)
	defer recheck.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-stall.C:
			return false, errors.New("timed out waiting for the sandbox to become reachable")
		case <-recheck.C:
			return false, nil
		case event, ok := <-projectEvents:
			if !ok {
				return false, errors.New("project event stream closed")
			}
			if eventConcernsSandbox(event, sandboxID, poolID) {
				return true, nil
			}
		}
	}
}

func eventConcernsSandbox(event model.ProjectEvent, sandboxID, poolID string) bool {
	switch event.ResourceType {
	case sandboxEventResourceType:
		return event.ResourceID == sandboxID
	case poolEventResourceType:
		return poolID != "" && event.ResourceID == poolID
	}
	return false
}

// The event resource types this wait filters on, taken from the resources
// themselves so the filter cannot drift from what the store publishes.
var (
	sandboxEventResourceType = (&model.Sandbox{}).EventResourceType()
	poolEventResourceType    = (&model.Pool{}).EventResourceType()
)
