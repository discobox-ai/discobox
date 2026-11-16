package sandboxes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/discobox-ai/discobox/server/internal/apperrors"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// SandboxPowerResourceType is the reconcile-engine resource type that powers a
// sandbox down on its own request and starts it at the wake it asked for
// (ADR 26-10-16-337 §§5–6).
//
// It is its own resource, beside the sandbox's, for two reasons. The sandbox
// reconcile ensures the container on every run, which is no work to repeat on
// every status a sandbox reports while idle. And the wake is a timer
// (Result.RequeueAt) on this resource's dirty row alone, so no other reconcile
// of the sandbox — a spec edit, a secret rebind — returns and drops it.
const SandboxPowerResourceType = "sandboxPower"

// sandboxPowerReconciler carries out what the sandbox reported and what it
// asked for. It decides nothing: the sandbox decides it is idle or asleep, the
// runtime confirms it with the sandbox under its power lock and decides what
// powering down is, and the wake is the sandbox's own.
type sandboxPowerReconciler struct {
	store     *store.Store
	providers *ProviderManager
	now       func() time.Time
}

func (r *sandboxPowerReconciler) Reconcile(ctx context.Context, id string) (reconcile.Result, error) {
	projectID, sandboxID, err := splitSandboxDirtyID(id)
	if err != nil {
		return reconcile.Result{}, err
	}
	// From the primary: the pool's ready report marks this reconcile, and a
	// replica that has not caught up would read the pool as not ready yet.
	sb, err := r.store.GetSandbox(ctx, projectID, sandboxID, store.SandboxFromPrimary())
	if errors.Is(err, store.ErrNotFound) {
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, err
	}
	// An archive clears the wake as it lands (outside instructions do); an
	// archived or deleted sandbox has nothing to power either way.
	if sb.DesiredState != model.DesiredStatePresent {
		return reconcile.Result{}, nil
	}
	if sb.WakeAt != nil && !r.now().Before(*sb.WakeAt) {
		result, wakeErr := r.wake(ctx, sb)
		if wakeErr == nil {
			return result, nil
		}
		// A wake its pool did not take is kept and fails the reconcile, but it
		// does not hold the sandbox up: a sandbox started by use that asks to
		// power down is still powered down, and the kept wake starts it again
		// once its pool takes the start.
		kept := *sb.WakeAt
		if reason, ok := reportedPowerDown(sb); ok {
			if err := r.powerDown(ctx, sb, reason); err != nil {
				return reconcile.Result{}, errors.Join(wakeErr, err)
			}
		}
		// The power-down's confirmation is the sandbox's own account of its
		// wakes, read now: one its boot already handed over is no longer
		// pending, and is not sent again.
		if sb.WakeAt == nil || !sb.WakeAt.Equal(kept) {
			slog.Default().Info("a kept wake was replaced by the sandbox's confirmation", "sandboxId", sb.ID, "kept", kept)
			if sb.WakeAt != nil {
				return reconcile.RequeueAt(*sb.WakeAt), nil
			}
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, wakeErr
	}
	if reason, ok := reportedPowerDown(sb); ok {
		if err := r.powerDown(ctx, sb, reason); err != nil {
			return reconcile.Result{}, err
		}
	}
	if sb.WakeAt != nil {
		return reconcile.RequeueAt(*sb.WakeAt), nil
	}
	return reconcile.Result{}, nil
}

// powerDown sends the runtime a power-down for what the sandbox last reported,
// and records the wake its confirmation carried, or that it carried none: the
// confirmation is read from the sandbox's wake files at that moment, so a
// wake already handed to a boot, or removed, is cleared from the row rather
// than left to start the sandbox again. The stored status is a poll
// and a push old, and stays stored after the sandbox stops, so it is never
// acted on as it stands: the runtime asks the sandbox first, and a refusal —
// it attached a client, consumed its request, or is not running — is the
// instruction having nothing to do.
func (r *sandboxPowerReconciler) powerDown(ctx context.Context, sb *model.Sandbox, reason PowerDownReason) error {
	provider, err := r.providers.ResolveForSandbox(ctx, sb)
	if err != nil {
		return err
	}
	secretState, err := r.store.OpenSandboxSecretState(ctx, sb)
	if err != nil {
		return err
	}
	// The epoch is read before the power-down is sent: an outside instruction
	// that lands while it is in flight moves it, and the wake below is then
	// not written over the cancellation.
	epoch := sb.WakeEpoch
	wake, rotated, err := provider.PowerDown(ctx, sandboxRefFromSandbox(sb), secretState, reason)
	if errors.Is(err, ErrPowerDownRefused) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(rotated) > 0 || secretState != nil {
		if err := persistSandboxSecretState(ctx, r.store, sb, rotated); err != nil {
			return err
		}
	}
	slog.Default().Info("sandbox powered down on its own request", "sandboxId", sb.ID, "reason", reason, "wakeAt", wake.At)
	var at *time.Time
	if !wake.At.IsZero() {
		confirmed := wake.At.UTC()
		at = &confirmed
	} else if sb.WakeAt == nil {
		return nil
	}
	written, err := r.store.SetSandboxWakeIfEpoch(ctx, sb.ProjectID, sb.ID, epoch, at)
	if err != nil {
		return err
	}
	if written {
		sb.WakeAt = at
	}
	return nil
}

// wakePoolWait is how long a due wake whose pool is not active and ready
// waits before the power reconcile looks again (ADR 26-11-15-829 §2). It is a
// backstop: a ready report from the pool marks its due wakes at once
// (MarkPoolDueWakesTx). A wait is not a failure, so it is a requeue, which
// accrues no backoff and holds no reconcile slot.
const wakePoolWait = time.Minute

// wake sends the sandbox's scheduled start, and spends the wake only once the
// pool has taken it (ADR 26-11-15-829 §1). The pool took it when it answered:
// accepted it, or refused it with an answer of its own, which is recorded as
// any failed start is and never retried (ADR 26-10-12-711). Every other
// outcome — the pool not reachable, an old agent without the fenced route, the
// reconcile's context ending as the server stops, anything failing before the
// send — keeps the wake and fails the reconcile, so the engine's backoff sends
// it again. A crash anywhere in between leaves it recorded.
//
// The start is fenced at the pool on the epoch read with the wake (§3): an
// outside instruction moves the epoch, so a start not yet executed, or one
// whose answer was lost and is sent again, is refused after a person's stop.
// The wake's own start does not move the epoch.
//
// It is not sent while the sandbox's pool is not active and ready (§2):
// every pool's agent is away for a while after a server start.
//
// The start is sent whatever the row says the sandbox is doing. The row is
// the pool's last report, and it lags the power-down this reconcile itself
// just sent: a wake already due when the sandbox powered down is requeued for
// now and runs before the pool reports `stopped`. Deciding on the row there
// would spend the wake on a sandbox that is going down. A sandbox that really
// is running takes the start as the no-op it is, and a wake still due inside
// it comes back at its next power-down.
func (r *sandboxPowerReconciler) wake(ctx context.Context, sb *model.Sandbox) (reconcile.Result, error) {
	if !poolTakesWakes(sb.Pool, r.now()) {
		return reconcile.RequeueAt(r.now().Add(wakePoolWait)), nil
	}
	epoch, at := sb.WakeEpoch, *sb.WakeAt
	provider, err := r.providers.ResolveForSandbox(ctx, sb)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("wake sandbox %s: %w", sb.ID, err)
	}
	slog.Default().Info("waking sandbox at the time it asked for", "sandboxId", sb.ID, "wakeAt", at)
	startErr := instructSandbox(ctx, r.store, provider, sb, sandboxStart, &epoch, "")
	if startNotTaken(ctx, startErr) {
		return reconcile.Result{}, fmt.Errorf("wake sandbox %s: the pool did not take the start, so the wake is kept: %w", sb.ID, startErr)
	}
	// The pool took it. Spent on the epoch and the wake this reconcile read,
	// so neither a cancellation nor a later wake is cleared.
	if _, err := r.store.SpendSandboxWake(ctx, sb.ProjectID, sb.ID, epoch, at); err != nil {
		return reconcile.Result{}, err
	}
	if startErr != nil {
		slog.Default().Warn("wake sandbox", "sandboxId", sb.ID, "error", startErr)
	}
	return reconcile.Result{}, nil
}

// startNotTaken is whether a wake's start failed without the pool's own
// answer to it (ADR 26-11-15-829 §1): it was never sent, the pool was not
// reachable or its agent has no fenced route, the reconcile's context ended,
// or the pool's fence could not authorize it. A fence refusal is kept too: an
// outside instruction that moved the epoch cleared the wake in the same
// write, so a refusal that finds the wake still recorded is the
// authorization callback failing, not the wake being superseded.
func startNotTaken(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	return ctx.Err() != nil ||
		errors.Is(err, errInstructionNotSent) ||
		errors.Is(err, sandbox.ErrPoolNotReachable) ||
		errors.Is(err, sandbox.ErrStartNotAuthorized)
}

// poolTakesWakes is whether a sandbox's pool can be sent its wake: active,
// and ready on a heartbeat from this server run (ADR 26-11-15-829 §2).
func poolTakesWakes(pool *model.Pool, now time.Time) bool {
	return pool != nil && pool.State == model.PoolStateActive && pool.Health(now) == model.PoolHealthReady
}

// MarkPoolDueWakesTx marks the power reconcile of every present sandbox on a
// pool whose wake is due, in the transaction that writes the pool, when that
// write turns the pool from one that cannot take wakes into one that can (ADR
// 26-11-15-829 §2): before is the pool as the writer found it, after as it
// leaves it. Both writers of what the gate reads hand it over: the pool's
// status report, which turns it ready, and the pool reconcile, which turns a
// pool back from `offline` active after the report that earned it — no report
// sees that turn. A wake that fell due while the server or the pool was down
// then fires within a reconcile of the pool coming back.
//
// The mark is intent, unlike the drift marks other pool observations earn
// (REVIEW.md, "Marks a pool observation earns"): §2 has it pull the row
// forward from its failure backoff as well as its backstop, and a pool turns
// able to take wakes once per return, not at every report, so it cannot
// become the retry at every poll that rule guards against.
func (s *Service) MarkPoolDueWakesTx(ctx context.Context, txStore *store.Store, txDB *gorm.DB, before, after *model.Pool) error {
	now := time.Now()
	if s.engine == nil || poolTakesWakes(before, now) || !poolTakesWakes(after, now) {
		return nil
	}
	ids, err := txStore.ListSandboxIDsWithDueWake(ctx, after.ProjectID, after.ID, now)
	if err != nil {
		return err
	}
	for _, sandboxID := range ids {
		if err := s.engine.MarkDirtyTx(ctx, txDB, SandboxPowerResourceType, SandboxDirtyID(after.ProjectID, sandboxID)); err != nil {
			return err
		}
	}
	return nil
}

// reportedPowerDown is the power-down a running sandbox's last status asked
// for (powerDownAsked).
func reportedPowerDown(sb *model.Sandbox) (PowerDownReason, bool) {
	if sb.RuntimeState != model.SandboxRuntimeStateRunning {
		return "", false
	}
	return powerDownAsked(sb.AgentStatus)
}

// powerDownAsked reads the power-down a sandbox status asks for: exit when its
// primary terminal's shell has exited, which wins over both others (ADR
// 26-11-16-372 §2), then sleep when a sleep request is pending, and idle when
// the sandbox reports itself idle. A sandbox whose power the control plane
// does not hold reports none of them.
func powerDownAsked(agentStatus []byte) (PowerDownReason, bool) {
	if len(agentStatus) == 0 {
		return "", false
	}
	var status struct {
		Autostop *struct {
			Idle            bool            `json:"idle"`
			Sleep           json.RawMessage `json:"sleep"`
			PrimaryExitedAt json.RawMessage `json:"primaryExitedAt"`
		} `json:"autostop"`
	}
	if err := json.Unmarshal(agentStatus, &status); err != nil || status.Autostop == nil {
		return "", false
	}
	switch {
	case len(status.Autostop.PrimaryExitedAt) > 0 && string(status.Autostop.PrimaryExitedAt) != "null":
		return PowerDownExit, true
	case len(status.Autostop.Sleep) > 0 && string(status.Autostop.Sleep) != "null":
		return PowerDownSleep, true
	case status.Autostop.Idle:
		return PowerDownIdle, true
	}
	return "", false
}

// SandboxAgentStatusReported marks the sandbox's power reconcile when the
// status just recorded asks for a power-down. The reconcile reads that status
// back and has the runtime confirm it before anything is powered down.
func (s *Service) SandboxAgentStatusReported(ctx context.Context, projectID, sandboxID string, status json.RawMessage) error {
	if _, ok := powerDownAsked(status); !ok || s.engine == nil {
		return nil
	}
	// Drift, not intent: this fires on every status an idle sandbox reports,
	// and an intent mark would cancel the backoff of a power-down that keeps
	// failing and retry it at every poll.
	return s.engine.MarkDirtyDrift(ctx, SandboxPowerResourceType, SandboxDirtyID(projectID, sandboxID))
}

// cancelSandboxWake is what every power instruction from outside does to the
// sandbox's scheduled start: cancels it, and moves the epoch whether or not
// one was set, so a power reconcile in flight cannot write one back
// (ADR 26-10-16-337 §6). The mark has the power reconcile drop its timer.
func (s *Service) cancelSandboxWake(ctx context.Context, projectID, sandboxID string) (int64, error) {
	var epoch int64
	err := s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		if err := txStore.CancelSandboxWakeTx(ctx, txDB, projectID, sandboxID); err != nil {
			return err
		}
		sb, err := txStore.GetSandbox(ctx, projectID, sandboxID)
		if err != nil {
			return err
		}
		epoch = sb.WakeEpoch
		if s.engine == nil {
			return nil
		}
		return s.engine.MarkDirtyTx(ctx, txDB, SandboxPowerResourceType, SandboxDirtyID(projectID, sandboxID))
	})
	return epoch, err
}

// CompleteSandboxSuspend records successful freeze completion, never admission.
// It takes no service lock: a dispatch awaiting the pool's callback holds it.
func (s *Service) CompleteSandboxSuspend(ctx context.Context, projectID, sandboxID, requestID string, epoch int64) error {
	if requestID == "" {
		return apperrors.NewStatusError(http.StatusBadRequest, "suspend request ID is required")
	}
	return s.store.Transaction(ctx, func(st *store.Store, tx *gorm.DB) error {
		changed, err := st.CompleteSandboxSuspendTx(ctx, tx, projectID, sandboxID, requestID, epoch)
		if err != nil || !changed || s.engine == nil {
			return err
		}
		return s.engine.MarkDirtyTx(ctx, tx, SandboxPowerResourceType, SandboxDirtyID(projectID, sandboxID))
	})
}
