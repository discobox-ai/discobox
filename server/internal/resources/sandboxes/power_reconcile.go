package sandboxes

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
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
	sb, err := r.store.GetSandbox(ctx, projectID, sandboxID)
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
		return reconcile.Result{}, r.wake(ctx, sb)
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
// and records the wake its confirmation carried. The stored status is a poll
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
	if wake.At.IsZero() {
		return nil
	}
	at := wake.At.UTC()
	written, err := r.store.SetSandboxWakeIfEpoch(ctx, sb.ProjectID, sb.ID, epoch, &at)
	if err != nil {
		return err
	}
	if written {
		sb.WakeAt = &at
	}
	return nil
}

// wake spends the sandbox's scheduled start: it claims it under the epoch it
// read, so a stop that landed in between is not undone, and sends the start a
// person's start sends only if the claim held. It is sent once
// (ADR 26-10-16-337 §6, ADR 26-10-12-711): a start that fails is logged and
// nothing retries it.
//
// The start is sent whatever the row says the sandbox is doing. The row is
// the pool's last report, and it lags the power-down this reconcile itself
// just sent: a wake already due when the sandbox powered down is requeued for
// now and runs before the pool reports `stopped`. Deciding on the row there
// would spend the wake on a sandbox that is going down. A sandbox that really
// is running takes the start as the no-op it is, and a wake still due inside
// it comes back at its next power-down.
func (r *sandboxPowerReconciler) wake(ctx context.Context, sb *model.Sandbox) error {
	claimed, err := r.store.SetSandboxWakeIfEpoch(ctx, sb.ProjectID, sb.ID, sb.WakeEpoch, nil)
	if err != nil || !claimed {
		return err
	}
	provider, err := r.providers.ResolveForSandbox(ctx, sb)
	if err != nil {
		slog.Default().Warn("wake sandbox", "sandboxId", sb.ID, "error", err)
		return nil
	}
	slog.Default().Info("waking sandbox at the time it asked for", "sandboxId", sb.ID, "wakeAt", sb.WakeAt)
	if err := instructSandbox(ctx, r.store, provider, sb, sandboxStart); err != nil {
		slog.Default().Warn("wake sandbox", "sandboxId", sb.ID, "error", err)
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

// powerDownAsked reads the power-down a sandbox status asks for: sleep when a
// sleep request is pending, which wins, and idle when the sandbox reports
// itself idle. A sandbox whose power the control plane does not hold reports
// neither.
func powerDownAsked(agentStatus json.RawMessage) (PowerDownReason, bool) {
	if len(agentStatus) == 0 {
		return "", false
	}
	var status struct {
		Autostop *struct {
			Idle  bool            `json:"idle"`
			Sleep json.RawMessage `json:"sleep"`
		} `json:"autostop"`
	}
	if err := json.Unmarshal(agentStatus, &status); err != nil || status.Autostop == nil {
		return "", false
	}
	switch {
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
func (s *Service) cancelSandboxWake(ctx context.Context, projectID, sandboxID string) error {
	return s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		if err := txStore.CancelSandboxWakeTx(ctx, txDB, projectID, sandboxID); err != nil {
			return err
		}
		if s.engine == nil {
			return nil
		}
		return s.engine.MarkDirtyTx(ctx, txDB, SandboxPowerResourceType, SandboxDirtyID(projectID, sandboxID))
	})
}
