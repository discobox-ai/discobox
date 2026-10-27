package sandboxruntime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"

	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
)

// A machine's power is its engine's: a start boots it and places what its
// agent boots on, a stop shuts it down in order, and its state is read from
// the driver (ADR 26-10-09-106 §2, now the pool's engine). Every operation
// for one sandbox holds its power lock, as a container's does (power.go).

const (
	// machineStartTimeout bounds a machine's boot until its guest answers.
	machineStartTimeout = 10 * time.Minute
	// machineStopTimeout is how long a machine gets to power off in order
	// before it is forced off.
	machineStopTimeout = time.Minute
)

func (r *MachineSandboxRuntime) StartSandbox(ctx context.Context, sandboxID string, _ *workerapimodel.PoolSandboxOperationRequest) error {
	lock := r.sandboxLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	return r.startLocked(ctx, sandboxID)
}

func (r *MachineSandboxRuntime) StopSandbox(ctx context.Context, sandboxID string, _ *workerapimodel.PoolSandboxOperationRequest) error {
	lock := r.sandboxLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	return r.stopLocked(ctx, sandboxID)
}

func (r *MachineSandboxRuntime) RestartSandbox(ctx context.Context, sandboxID string, _ *workerapimodel.PoolSandboxOperationRequest) error {
	lock := r.sandboxLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	if err := r.stopLocked(ctx, sandboxID); err != nil {
		return err
	}
	return r.startLocked(ctx, sandboxID)
}

// SuspendSandbox is refused: disco-vm freezes no machine whole, and a
// suspend is never turned into a stop (ADR 26-10-16-420 §3).
func (r *MachineSandboxRuntime) SuspendSandbox(ctx context.Context, sandboxID string) error {
	lock := r.sandboxLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	sb, err := r.powerTarget(ctx, sandboxID)
	if err != nil {
		return err
	}
	if sb.Status != StatusRunning {
		return ErrNotRunning
	}
	return fmt.Errorf("%w: a machine sandbox cannot be frozen whole", ErrUnsupported)
}

// PowerDownSandbox stops a sandbox that confirms it may go down. A sleep is a
// stop here, the power-down every runtime has: a machine is not frozen.
func (r *MachineSandboxRuntime) PowerDownSandbox(ctx context.Context, sandboxID string, reason PowerDownReason) (Wake, error) {
	lock := r.sandboxLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	sb, err := r.GetSandbox(ctx, sandboxID)
	if err != nil {
		return Wake{}, err
	}
	if sb.Status != StatusRunning {
		return Wake{}, ErrPowerDownRefused
	}
	wake, err := r.confirmPowerDown(ctx, sandboxID, reason)
	if err != nil {
		return Wake{}, err
	}
	slog.Default().Info("powering sandbox down on its own request", "sandboxId", sandboxID, "reason", reason, "wakeAt", wake.At)
	if err := r.stopLocked(ctx, sandboxID); err != nil {
		return Wake{}, err
	}
	return wake, nil
}

// EnsureSandboxRunning starts a stopped sandbox on demand and returns once its
// agent answers; one running on a boot under way waits that boot out (ADR
// 0017 §12, ADR 26-10-12-711 §2).
//
// A sandbox the runtime last saw running, on no boot under way, whose machine
// the pool's own record says holds what it boots on now, is answered from
// that, with no power lock and no call to the driver: every request to a
// sandbox comes through here, and must not wait behind a create or a stop of
// it, nor on the driver's API (pool-agent/REVIEW.md, "Running is not
// ready"). Being seen running is a power state, which any poll sets, so it is
// never enough alone: a machine running with nothing placed in it is placed
// by the start below.
func (r *MachineSandboxRuntime) EnsureSandboxRunning(ctx context.Context, sandboxID string) error {
	if r.answersRunning(ctx, sandboxID) {
		return nil
	}
	for {
		booting, err := r.startUnlessRunning(ctx, sandboxID)
		if err != nil || !booting {
			return err
		}
		// Waited out with the lock released, then looked at again: a boot
		// can end without the sandbox up.
		if _, err := r.awaitBoot(ctx, sandboxID); err != nil {
			return err
		}
	}
}

// answersRunning reports whether a sandbox can be answered as running from
// what this runtime holds, the pool's records and what it last saw, with no
// lock and no call to the driver.
func (r *MachineSandboxRuntime) answersRunning(ctx context.Context, sandboxID string) bool {
	if status, ok := r.lastObserved(sandboxID); !ok || status != StatusRunning || r.SandboxBooting(sandboxID) || !r.hostsSandbox(sandboxID) || r.isArchived(sandboxID) {
		return false
	}
	record, ok, err := r.readMachineRecord(sandboxID)
	if err != nil || !ok {
		return false
	}
	current, err := r.placementCurrent(ctx, sandboxID, record.Instance)
	return err == nil && current
}

// startUnlessRunning starts the sandbox unless it runs, under its power lock,
// and reports a run on a boot still under way.
func (r *MachineSandboxRuntime) startUnlessRunning(ctx context.Context, sandboxID string) (bool, error) {
	lock := r.sandboxLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	sb, err := r.powerTarget(ctx, sandboxID)
	if err != nil {
		return false, err
	}
	if sb.Status == StatusRunning {
		if r.SandboxBooting(sandboxID) {
			return true, nil
		}
		if current, err := r.placementCurrent(ctx, sandboxID, sb.ID); err != nil || current {
			return false, err
		}
	}
	return false, r.startLocked(ctx, sandboxID)
}

// powerTarget is the machine a power operation acts on, and says why there is
// none: an archived sandbox, one whose tree is here and whose machine is not,
// or one this pool has never held.
func (r *MachineSandboxRuntime) powerTarget(ctx context.Context, sandboxID string) (*Sandbox, error) {
	sb, err := r.GetSandbox(ctx, sandboxID)
	if err == nil || !errors.Is(err, ErrNotFound) {
		return sb, err
	}
	switch {
	case r.isArchived(sandboxID):
		return nil, ErrArchived
	case r.hostsSandbox(sandboxID):
		return nil, ErrNoContainer
	}
	return nil, err
}

// startLocked boots the sandbox's machine, places what its agent boots on and
// starts the agent, restarting the machine when it had booted on something
// else, then waits for the agent and delivers its runtime-config document
// (finishBoot). The caller holds the sandbox's power lock.
func (r *MachineSandboxRuntime) startLocked(ctx context.Context, sandboxID string) error {
	if err := r.harnessStartAllowed(sandboxID); err != nil {
		return err
	}
	if r.isArchived(sandboxID) {
		return ErrArchived
	}
	sb, err := r.powerTarget(ctx, sandboxID)
	if err != nil {
		return err
	}
	// A running machine is started already only once what it boots on is
	// placed in it: one whose start stopped short of that — the pool agent
	// gone mid-start, or the stop after a failed placement failing too — runs
	// with no agent, and is placed now.
	running := sb.Status == StatusRunning
	if running {
		current, err := r.placementCurrent(ctx, sandboxID, sb.ID)
		if err != nil || current {
			return err
		}
		slog.InfoContext(ctx, "placing what a running sandbox machine boots on, which its start did not finish", "sandboxId", sandboxID)
	}
	record, ok, err := r.readMachineRecord(sandboxID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoContainer
	}
	inst, err := r.instance(ctx, sandboxID)
	if err != nil {
		return err
	}
	r.publishSandboxState(ctx, sandboxID, StateStarting)
	r.publishSandboxPhase(ctx, sandboxID, PhaseStartingContainer)
	r.urls.Delete(sandboxID)
	boot := r.beginBoot(sandboxID)
	started := running
	fail := func(err error) error {
		// Not answered running from what was seen until a start finishes.
		r.observed.Delete(sandboxID)
		// A machine left running with nothing placed, or a placement cut
		// short, would read as running to every start after, which places
		// nothing in a running machine: it is stopped, so the next start
		// places it again. Detached, since the caller going away is one way
		// to get here.
		if started {
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), machineStopTimeout+time.Minute)
			if stopErr := r.stopMachine(stopCtx, sandboxID); stopErr != nil {
				slog.WarnContext(stopCtx, "stop a sandbox machine whose start failed", "sandboxId", sandboxID, "error", stopErr)
			}
			cancel()
		}
		r.publishSandboxState(ctx, sandboxID, StateStopped)
		r.endBoot(sandboxID, boot, err)
		return err
	}
	if !running {
		if err := r.engine.Start(ctx, inst, engine.StartOptions{Timeout: machineStartTimeout}); err != nil {
			// A start that fails part way may leave the machine up.
			started = r.engine.State(context.WithoutCancel(ctx), inst) == engine.Running
			return fail(fmt.Errorf("start sandbox %s's machine: %w", sandboxID, err))
		}
		started = true
	}
	if !record.Started {
		record.Started = true
		if err := r.writeMachineRecord(sandboxID, record); err != nil {
			return fail(err)
		}
	}
	r.observe(sandboxID, StatusRunning)
	moved, err := r.place(ctx, sandboxID, inst, record)
	if err != nil {
		return fail(err)
	}
	if moved {
		// Its agent started at boot on what was placed before: restarted, it
		// boots on what was placed now, as a container placed before its
		// start does.
		slog.InfoContext(ctx, "restarting a sandbox machine onto what was placed in it", "sandboxId", sandboxID)
		if err := r.engine.Stop(ctx, inst, machineStopTimeout); err != nil && r.engine.State(ctx, inst) != engine.Stopped {
			return fail(fmt.Errorf("restart sandbox %s's machine: %w", sandboxID, err))
		}
		if err := r.engine.Start(ctx, inst, engine.StartOptions{Timeout: machineStartTimeout}); err != nil {
			started = r.engine.State(context.WithoutCancel(ctx), inst) == engine.Running
			return fail(fmt.Errorf("restart sandbox %s's machine: %w", sandboxID, err))
		}
		r.urls.Delete(sandboxID)
		if _, err := r.place(ctx, sandboxID, inst, record); err != nil {
			return fail(err)
		}
	}
	r.publishSandboxPhase(ctx, sandboxID, PhaseWaitingForAgent)
	return r.finishBoot(ctx, sandboxID, boot)
}

// stopLocked shuts the sandbox's machine down. The caller holds the sandbox's
// power lock.
func (r *MachineSandboxRuntime) stopLocked(ctx context.Context, sandboxID string) error {
	if _, err := r.powerTarget(ctx, sandboxID); err != nil {
		return err
	}
	r.publishSandboxState(ctx, sandboxID, StateStopping)
	return r.stopMachine(ctx, sandboxID)
}

// stopMachine shuts the machine down in order, forcing it off after
// machineStopTimeout, and reports it stopped. Forced off is still off.
func (r *MachineSandboxRuntime) stopMachine(ctx context.Context, sandboxID string) error {
	inst, err := r.instance(ctx, sandboxID)
	if err != nil {
		return err
	}
	r.urls.Delete(sandboxID)
	if err := r.engine.Stop(ctx, inst, machineStopTimeout); err != nil && r.engine.State(ctx, inst) != engine.Stopped {
		return fmt.Errorf("stop sandbox %s's machine: %w", sandboxID, err)
	}
	record, _, err := r.readMachineRecord(sandboxID)
	if err != nil {
		return err
	}
	status := StatusStopped
	if !record.Started {
		status = StatusCreated
	}
	r.observe(sandboxID, status)
	r.publishSandboxState(ctx, sandboxID, StateStopped)
	return nil
}
