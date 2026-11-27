package sandboxruntime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/vm/pkg/engine"

	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
)

// A machine's power is its engine's: a start boots it and places what its
// agent boots on, a stop shuts it down in order, and its state is read from
// the driver (ADR 26-10-09-106 §2, now the pool's engine). Every operation
// for one sandbox holds its power lock, as a container's does (power.go).
//
// A confirmed sleep and a suspend freeze the machine whole where its driver
// reports Capabilities.Pause, as a container is paused (ADR 26-11-21-395 §3):
// the pool records the freeze before it asks (machineRecord.Frozen), so a
// pause it did not make is never read as a sleep (§4), and every start thaws
// it with engine.Start, the one way disco-vm resumes a paused machine (§5).

const (
	// machineStartTimeout bounds a machine's boot until its guest answers.
	machineStartTimeout = 10 * time.Minute
	// machineStopTimeout is how long a machine gets to power off in order
	// before it is forced off.
	machineStopTimeout = time.Minute
)

// machineStateReadTimeout bounds every read of a machine's power state the
// pool makes under a sandbox's power lock to judge what a failed pause or
// resume left, or to settle a freeze: a driver that does not answer holds
// that lock, and every power operation on the sandbox, until the read gives
// up. disco-vm caps a read at a minute (engine.State); this holds it to less.
// A read that times out is the state unknown. A variable so tests can shorten
// it.
var machineStateReadTimeout = 30 * time.Second

func (r *MachineSandboxRuntime) StartSandbox(ctx context.Context, sandboxID string, req *workerapimodel.PoolSandboxOperationRequest) error {
	unlock, err := r.lockPower(ctx, sandboxID)
	if err != nil {
		return err
	}
	defer unlock()
	if err := r.authorizeExplicitStart(ctx, sandboxID, req); err != nil {
		return err
	}
	return r.startLocked(ctx, sandboxID)
}

func (r *MachineSandboxRuntime) StopSandbox(ctx context.Context, sandboxID string, _ *workerapimodel.PoolSandboxOperationRequest) error {
	unlock, err := r.lockPower(ctx, sandboxID)
	if err != nil {
		return err
	}
	defer unlock()
	return r.stopLocked(ctx, sandboxID)
}

func (r *MachineSandboxRuntime) RestartSandbox(ctx context.Context, sandboxID string, _ *workerapimodel.PoolSandboxOperationRequest) error {
	unlock, err := r.lockPower(ctx, sandboxID)
	if err != nil {
		return err
	}
	defer unlock()
	if err := r.stopLocked(ctx, sandboxID); err != nil {
		return err
	}
	return r.startLocked(ctx, sandboxID)
}

// SuspendSandbox freezes a running sandbox's machine whole, under its power
// lock, where its driver can (ADR 26-11-21-395 §3), as a container is paused
// (ADR 26-10-16-420 §3). A driver that cannot is refused, and a freeze that
// fails is returned: a person asked to keep what it holds, so a suspend is
// never turned into a stop.
//
// A sandbox still booting is refused like a stopped one: its boot waits for an
// agent the freeze would keep from ever answering.
func (r *MachineSandboxRuntime) SuspendSandbox(ctx context.Context, sandboxID string, req *workerapimodel.PoolSandboxOperationRequest) error {
	unlock, err := r.lockPower(ctx, sandboxID)
	if err != nil {
		return err
	}
	defer unlock()
	if err := r.finishSuspend(ctx, sandboxID); err != nil {
		return err
	}
	sb, err := r.powerTarget(ctx, sandboxID)
	if err != nil {
		return err
	}
	// A machine its provider paused reads suspended on the wire, so a
	// suspend of it is taken as asked: the freeze is recorded (engine.Pause
	// leaves a paused machine as it is), and the state poll no longer
	// resumes it.
	switch {
	case sb.Status == StatusSuspended:
		return r.suspendWithCompletion(ctx, sandboxID, req, func() error { return nil })
	case sb.Status != StatusRunning && sb.Status != StatusPaused:
		return ErrNotRunning
	case sb.Status == StatusRunning && r.SandboxBooting(sandboxID):
		return fmt.Errorf("%w: it is still starting", ErrNotRunning)
	case !r.engine.Driver.Capabilities().Pause:
		return fmt.Errorf("%w: the %s driver cannot freeze a machine whole", ErrUnsupported, r.engine.Driver.Name())
	}
	return r.suspendWithCompletion(ctx, sandboxID, req, func() error { return r.freezeLocked(ctx, sandboxID) })
}

// PowerDownSandbox carries out a power-down the sandbox confirms under the
// power lock (ADR 26-10-16-337 §5). A confirmed sleep freezes the machine
// where its driver can (ADR 26-11-21-395 §3), and stops it where the driver
// cannot or the freeze fails, as a container is stopped when its pause fails:
// the agent asked to go down, and a stop is the power-down every runtime has.
// An idle or an exit stops it.
func (r *MachineSandboxRuntime) PowerDownSandbox(ctx context.Context, sandboxID string, reason PowerDownReason) (Wake, error) {
	unlock, err := r.lockPower(ctx, sandboxID)
	if err != nil {
		return Wake{}, err
	}
	defer unlock()
	if err := r.finishSuspend(ctx, sandboxID); err != nil {
		return Wake{}, err
	}
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
	if reason == PowerDownSleep && r.engine.Driver.Capabilities().Pause {
		err := r.freezeLocked(ctx, sandboxID)
		if err == nil {
			return wake, nil
		}
		slog.Default().Warn("could not freeze a sleeping sandbox's machine; stopping it", "sandboxId", sandboxID, "error", err)
	}
	if err := r.stopLocked(ctx, sandboxID); err != nil {
		return Wake{}, err
	}
	return wake, nil
}

// machineFreezeTimeout bounds a freeze, which runs detached from its caller:
// one that went away part way would leave the machine paused or running with
// no saying which, and the record would be wrong whichever it guessed.
const machineFreezeTimeout = machineStopTimeout

// freezeLocked pauses the sandbox's machine whole (engine.Pause), recording
// first that the pool froze it, so its paused state reads suspended rather
// than as a pause its provider made (ADR 26-11-21-395 §4). A freeze the driver
// reports failed is judged by what the machine is then, since a pause that
// timed out may have taken effect: paused is frozen, and succeeds; running or
// stopped is a failure, and clears the record; a state that cannot be read
// keeps the record, and fails. Kept on a machine that runs, the record is
// settled (settleFrozen: the after-thaw steps, then cleared); cleared on one
// that paused, it would read as its provider's pause, which the state poll
// resumes, undoing the freeze. The caller holds the sandbox's power lock.
func (r *MachineSandboxRuntime) freezeLocked(ctx context.Context, sandboxID string) error {
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
	record.Frozen = true
	if err := r.writeMachineRecord(sandboxID, record); err != nil {
		return err
	}
	pauseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), machineFreezeTimeout)
	defer cancel()
	if err := r.engine.Pause(pauseCtx, inst); err != nil {
		err = fmt.Errorf("freeze sandbox %s's machine: %w", sandboxID, err)
		stateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), machineStateReadTimeout)
		defer cancel()
		switch r.engine.State(stateCtx, inst) {
		case engine.Paused:
			slog.WarnContext(ctx, "a sandbox machine's freeze reported a failure, but the machine is paused; it is frozen", "sandboxId", sandboxID, "error", err)
		case engine.Running, engine.Stopped:
			record.Frozen = false
			return errors.Join(err, r.writeMachineRecord(sandboxID, record))
		default:
			// Seen suspended until the driver answers: what status falls
			// back to while it cannot be asked, so nothing acts on the
			// running it was seen before the freeze.
			r.endpoints.Delete(sandboxID)
			r.observe(sandboxID, StatusSuspended)
			return err
		}
	}
	slog.InfoContext(ctx, "froze sandbox machine", "sandboxId", sandboxID)
	r.endpoints.Delete(sandboxID)
	r.observe(sandboxID, StatusSuspended)
	r.publishSandboxState(ctx, sandboxID, StateSuspended)
	return nil
}

// thawedLocked runs the after-thaw steps of a machine the pool froze that runs
// again, whoever resumed it (ADR 26-11-21-395 §§4–5, ADR 26-10-16-420 §4,
// afterThaw), then clears the record of the freeze: a pool agent gone part way
// runs them again at its next look, and each is safe to run twice. The caller
// holds the sandbox's power lock.
func (r *MachineSandboxRuntime) thawedLocked(ctx context.Context, sandboxID string, record *machineRecord) error {
	r.observe(sandboxID, StatusRunning)
	r.publishSandboxState(ctx, sandboxID, StateRunning)
	r.afterThaw(ctx, sandboxID)
	record.Frozen = false
	return r.writeMachineRecord(sandboxID, *record)
}

// settleFrozenInBackground settles, off the state poll, a sandbox whose machine
// the pool froze and that no longer reads paused (settleFrozen), unless
// something is already acting on it for the poll.
func (r *MachineSandboxRuntime) settleFrozenInBackground(ctx context.Context, logger *slog.Logger, sandboxID string) {
	record, ok, err := r.readMachineRecord(sandboxID)
	if err != nil || !ok || !record.Frozen {
		return
	}
	if _, busy := r.resuming.LoadOrStore(sandboxID, struct{}{}); busy {
		return
	}
	go func() {
		defer r.resuming.Delete(sandboxID)
		if err := r.settleFrozen(ctx, sandboxID); err != nil && ctx.Err() == nil {
			logger.Warn("settle a sandbox machine the pool froze", "sandboxId", sandboxID, "error", err)
		}
	}()
}

// settleFrozen settles, under the power lock, a machine the pool froze that
// something else changed (ADR 26-11-21-395 §4): one running again was thawed
// outside the pool (Archil resumes a paused sandbox for a request carrying its
// port token), and gets the after-thaw steps; one stopped is no longer
// frozen. One still paused is left suspended. It acts only on what the
// driver answers now, never on the state last seen, which is what a driver
// that cannot be asked leaves: settling a machine whose pause took effect
// while the driver did not answer would make it its provider's pause, which
// the state poll resumes.
func (r *MachineSandboxRuntime) settleFrozen(ctx context.Context, sandboxID string) error {
	unlock, err := r.lockPower(ctx, sandboxID)
	if err != nil {
		return err
	}
	defer unlock()
	record, ok, err := r.readMachineRecord(sandboxID)
	if err != nil || !ok || !record.Frozen {
		return err
	}
	inst, err := r.instance(ctx, sandboxID)
	if err != nil {
		return err
	}
	stateCtx, cancel := context.WithTimeout(ctx, machineStateReadTimeout)
	state := r.engine.State(stateCtx, inst)
	cancel()
	switch state {
	case engine.Running:
		slog.InfoContext(ctx, "a sandbox machine the pool froze runs again; running its after-thaw steps", "sandboxId", sandboxID)
		return r.thawedLocked(ctx, sandboxID, &record)
	case engine.Stopped:
		record.Frozen = false
		return r.writeMachineRecord(sandboxID, record)
	}
	return nil
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
	if err != nil || !ok || record.Frozen {
		return false
	}
	current, err := r.placementCurrent(ctx, sandboxID, record.Instance)
	return err == nil && current
}

// startUnlessRunning starts the sandbox unless it runs, under its power lock,
// and reports a run on a boot still under way.
func (r *MachineSandboxRuntime) startUnlessRunning(ctx context.Context, sandboxID string) (bool, error) {
	unlock, err := r.lockPower(ctx, sandboxID)
	if err != nil {
		return false, err
	}
	defer unlock()
	sb, err := r.powerTarget(ctx, sandboxID)
	if err != nil {
		return false, err
	}
	if sb.Status == StatusRunning {
		if r.SandboxBooting(sandboxID) {
			return true, nil
		}
		// One the pool froze, thawed outside it, is owed its after-thaw
		// steps, which the start below runs.
		record, _, err := r.readMachineRecord(sandboxID)
		if err != nil {
			return false, err
		}
		if !record.Frozen {
			if current, err := r.placementCurrent(ctx, sandboxID, sb.ID); err != nil || current {
				return false, err
			}
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
// starts the agent, wiring the running machine in place where it had booted
// on something else or on nothing placed (ADR 26-11-23-458) -- restarting it
// only where the agent placed cannot (place) -- then waits for the agent and
// delivers its runtime-config document
// (finishBoot). A machine the pool froze is thawed rather than booted (ADR
// 26-11-21-395 §5), and is then a running machine like any other: started
// already once what it boots on is placed. The caller holds the sandbox's
// power lock.
func (r *MachineSandboxRuntime) startLocked(ctx context.Context, sandboxID string) error {
	if err := r.finishSuspend(ctx, sandboxID); err != nil {
		return err
	}
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
	record, ok, err := r.readMachineRecord(sandboxID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoContainer
	}
	// One the pool froze that runs again was thawed outside it, and is owed
	// its after-thaw steps; one stopped since is no longer frozen. One still
	// frozen is resumed below, as one its provider paused is.
	switch {
	case record.Frozen && sb.Status == StatusRunning:
		if err := r.thawedLocked(ctx, sandboxID, &record); err != nil {
			return err
		}
	case record.Frozen && sb.Status != StatusSuspended:
		record.Frozen = false
		if err := r.writeMachineRecord(sandboxID, record); err != nil {
			return err
		}
	}
	// A running machine is started already only once what it boots on is
	// placed in it: one whose start stopped short of that — the pool agent
	// gone mid-start, or the stop after a failed placement failing too — runs
	// with no agent, and is placed now. One still to be given a new root
	// (upgradeMachine) is stopped for it: a root is given only to a stopped
	// machine. A paused machine, one its provider paused (StatusPaused) or
	// one the pool froze (StatusSuspended, ADR 26-11-21-395 §5), is resumed
	// (resumeLocked), and is then a running one; one still to be given a new
	// root is stopped for it instead, the engine's Stop resuming it first so
	// its guest writes out what it holds.
	running := sb.Status == StatusRunning
	resumed := false
	// thawed is a machine the pool froze, resumed here onto a placement
	// that moved while it was frozen: its agent is told it resumed once it
	// answers on what was placed (below).
	thawed := false
	if (sb.Status == StatusPaused || sb.Status == StatusSuspended) && !record.NewRoot {
		if err := r.resumeLocked(ctx, sandboxID, record); err != nil {
			// A resume that left the machine stopped (its host restarted,
			// its provider expired the pause, or the engine stopped what it
			// could not finish resuming) has lost what the pause kept: it
			// boots fresh, as a slept machine did before a sleep froze one.
			// One still paused stays so, its memory kept for a start that
			// resumes it: disco-vm cannot stop a paused machine it cannot
			// resume (disco-vm ADR 26-11-21-438 §6).
			inst, instErr := r.instance(ctx, sandboxID)
			if instErr != nil {
				return err
			}
			stateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), machineStateReadTimeout)
			state := r.engine.State(stateCtx, inst)
			cancel()
			if state != engine.Stopped {
				return err
			}
			slog.WarnContext(ctx, "a paused sandbox machine did not resume and is stopped; booting it fresh", "sandboxId", sandboxID, "error", err)
			if record.Frozen {
				record.Frozen = false
				if err := r.writeMachineRecord(sandboxID, record); err != nil {
					return err
				}
			}
		} else {
			running, resumed = true, true
		}
	}
	if running && !record.NewRoot {
		current, err := r.placementCurrent(context.WithoutCancel(ctx), sandboxID, sb.ID)
		if err != nil || current {
			if err == nil && resumed {
				// The pool's own freeze is thawed with the after-thaw steps
				// (afterThaw); a provider's pause gets its document alone.
				if record.Frozen {
					return r.thawedLocked(ctx, sandboxID, &record)
				}
				r.deliverAfterResume(ctx, sandboxID)
			}
			return err
		}
		// Placed onto what moved, a frozen machine is no longer frozen: its
		// agent is restarted onto the placement in place (ADR 26-11-23-458
		// §4), and told it resumed once it answers, which hands over the
		// wakes due as the thaw would have. On §6's path the machine is
		// restarted instead, and its boot hands them over.
		if record.Frozen {
			thawed = true
			record.Frozen = false
			if err := r.writeMachineRecord(sandboxID, record); err != nil {
				return err
			}
		}
		slog.InfoContext(ctx, "placing what a running sandbox machine boots on, which its start did not finish", "sandboxId", sandboxID)
	}
	// A paused or frozen one too: it is not stopped, and only a stopped
	// machine is given a root (engine.Upgrade). The stop resumes it to shut
	// it down.
	if record.NewRoot && (running || sb.Status == StatusPaused || sb.Status == StatusSuspended) {
		if err := r.stopLocked(ctx, sandboxID); err != nil {
			return err
		}
		running = false
		// The stop cleared the freeze on disk (stopMachine); this copy is
		// written again below (upgradeMachine), and must not put it back.
		record.Frozen = false
	}
	inst, err := r.instance(ctx, sandboxID)
	if err != nil {
		return err
	}
	r.publishSandboxState(ctx, sandboxID, StateStarting)
	r.endpoints.Delete(sandboxID)
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
		// A machine still paused is reported as such, not stopped
		// (StatusPaused).
		state := StateStopped
		if r.engine.State(context.WithoutCancel(ctx), inst) == engine.Paused {
			state = stateFromStatus(StatusPaused)
		}
		r.publishSandboxState(ctx, sandboxID, state)
		r.endBoot(sandboxID, boot, err)
		return err
	}
	if record.NewRoot {
		r.publishSandboxPhase(ctx, sandboxID, PhaseCreatingContainer)
		if inst, err = r.upgradeMachine(ctx, sandboxID, inst, &record); err != nil {
			return fail(err)
		}
	}
	// A machine made from a snapshot whose agent does not come up on its
	// first start blames the snapshot (ADR 26-11-03-986 §6): one never
	// started before this start, whose agent has never answered. A machine
	// started before may have answered unseen (its start cut short after the
	// boot, the pool agent gone), and then holds the sandbox's work on its
	// root — unless it keeps that work on its volume: then a root from the
	// snapshot that never answers (a rebuild's, upgradeMachine) is replaced
	// by a fresh one, and blames nothing, since the box has run and its
	// volume, which its agent reads at boot, is its own to break.
	fromSnapshot := record.Snapshot != "" && !r.snapshotProven(sandboxID)
	firstFromSnapshot := fromSnapshot && !record.Started
	rootFromSnapshot := fromSnapshot && record.Started && r.keepsDataOnVolume(inst, record)
	r.publishSandboxPhase(ctx, sandboxID, PhaseStartingContainer)
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
	var pending *placedRecord
	moved, _, err := r.place(ctx, sandboxID, inst, record, notRestarted)
	if err != nil {
		return fail(err)
	}
	if moved {
		// The agent placed cannot wire the running machine in place (ADR
		// 26-11-23-458 §6): restarted, it boots on what was placed now, its
		// volumes wired before anything else starts, as a container placed
		// before its start does (ADR 26-11-02-023 §3).
		slog.InfoContext(ctx, "restarting a sandbox machine onto what was placed in it", "sandboxId", sandboxID)
		after := restartedInOrder
		if err := r.engine.Stop(ctx, inst, machineStopTimeout); err != nil {
			if r.engine.State(ctx, inst) != engine.Stopped {
				return fail(fmt.Errorf("restart sandbox %s's machine: %w", sandboxID, err))
			}
			after = restartedForced
		}
		if err := r.engine.Start(ctx, inst, engine.StartOptions{Timeout: machineStartTimeout}); err != nil {
			started = r.engine.State(context.WithoutCancel(ctx), inst) == engine.Running
			return fail(fmt.Errorf("restart sandbox %s's machine: %w", sandboxID, err))
		}
		r.endpoints.Delete(sandboxID)
		if _, pending, err = r.place(ctx, sandboxID, inst, record, after); err != nil {
			return fail(err)
		}
	}
	r.publishSandboxPhase(ctx, sandboxID, PhaseWaitingForAgent)
	if err := r.finishBoot(ctx, sandboxID, boot); err != nil {
		if ctx.Err() == nil && errors.Is(err, errAgentNeverUp) {
			switch {
			case firstFromSnapshot:
				return r.remakeFresh(ctx, sandboxID, record.Snapshot, err)
			case rootFromSnapshot:
				return r.remakeOnFreshRoot(ctx, sandboxID, record.Snapshot, err)
			}
		}
		return err
	}
	if pending != nil {
		if err := r.recordForcedPlacement(ctx, sandboxID, *pending); err != nil {
			return err
		}
	}
	if thawed && !moved {
		// A restarted agent hands over nothing within a boot (its handover
		// marker is this boot's); the thaw's news does. Detached, and not the
		// start's to fail, as afterThaw's is not.
		if err := r.tellResumed(context.WithoutCancel(ctx), sandboxID); err != nil {
			slog.WarnContext(ctx, "tell a thawed sandbox placed in place that it resumed", "sandboxId", sandboxID, "error", err)
		}
	}
	// The agent is up, so its provisioning has wired what it wires this
	// start.
	if err := r.recordDataOnVolume(ctx, sandboxID, inst); err != nil {
		slog.WarnContext(ctx, "read whether a sandbox machine keeps its data on its volume", "sandboxId", sandboxID, "error", err)
	}
	return nil
}

// resumeLocked resumes a paused machine, one its provider paused
// (StatusPaused; Archil at a sandbox's session limit) or one the pool froze
// (StatusSuspended), through the engine's Start, which boots nothing: its
// guest, its memory and its agent carry on as they were (disco-vm ADR
// 26-11-21-438 §5). It is the one resume. The caller, startLocked, then runs
// what the pause owes where the machine holds what it boots on now (a
// provider's pause its runtime-config document, deliverAfterResume; the
// pool's freeze its after-thaw steps, thawedLocked), and otherwise places it
// as a running machine, its agent restarted onto what moved while it was
// paused (ADR 26-11-23-458 §4). A
// resume that fails reports the machine as the driver has it, unless the
// machine runs: then something else resumed it in between (Archil, for a
// request with its port token), and it is resumed. The caller holds the
// sandbox's power lock.
func (r *MachineSandboxRuntime) resumeLocked(ctx context.Context, sandboxID string, record machineRecord) error {
	inst, err := r.instance(ctx, sandboxID)
	if err != nil {
		return err
	}
	r.publishSandboxState(ctx, sandboxID, StateStarting)
	r.endpoints.Delete(sandboxID)
	// Detached, bounded by the start's own timeout: a resume cut short by its
	// caller leaving (a request's context on use) is stopped by the engine,
	// losing the memory the pause kept.
	if err := r.engine.Start(context.WithoutCancel(ctx), inst, engine.StartOptions{Timeout: machineStartTimeout}); err != nil {
		statusCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), machineStateReadTimeout)
		status, statusErr := r.status(statusCtx, sandboxID, inst, record)
		cancel()
		if statusErr == nil && status == StatusRunning {
			slog.InfoContext(ctx, "a paused sandbox machine was resumed by something else as the pool resumed it", "sandboxId", sandboxID, "error", err)
			r.publishSandboxState(ctx, sandboxID, StateRunning)
			return nil
		}
		if statusErr == nil {
			r.publishSandboxState(ctx, sandboxID, stateFromStatus(status))
		}
		return fmt.Errorf("resume sandbox %s's machine: %w", sandboxID, err)
	}
	r.observe(sandboxID, StatusRunning)
	r.publishSandboxState(ctx, sandboxID, StateRunning)
	return nil
}

// deliverAfterResume delivers the runtime-config document to a sandbox whose
// machine was resumed from its provider's pause, as a boot's wait does, since
// what the pool holds for it may have moved while it was paused. Its agent is
// not told it resumed (autostop/resumed): that sets aside the wakes now due
// and the next start's notes, which is what a thaw of the pool's own freeze
// means (ADR 26-10-16-420 §4), and a provider's pause was neither a sleep nor
// a start, so a wake due across it is still owed. Detached from the caller;
// a failure is logged, and the next start delivers again.
func (r *MachineSandboxRuntime) deliverAfterResume(ctx context.Context, sandboxID string) {
	ctx = context.WithoutCancel(ctx)
	slog.InfoContext(ctx, "resumed a sandbox machine its provider paused", "sandboxId", sandboxID)
	if err := logRuntimeConfigFailure(ctx, sandboxID, r.deliverRuntimeConfig(ctx, sandboxID, nil)); err != nil {
		slog.WarnContext(ctx, "deliver runtime config to a resumed sandbox machine", "sandboxId", sandboxID, "error", err)
	}
}

// recordForcedPlacement records a placement a machine was forced off on its
// way to booting on, now that its agent has answered, once that agent says it
// runs from the layer placed. The copy of a new layer made before the stop may
// not have reached the machine's disk, and an older agent still on it — the
// layer it held before, or a snapshot's — answers then, naming its own:
// recorded, every later start would skip the layer and start no agent.
// Unrecorded, the running machine's placement is not current
// (answersRunning), so its next request places the layer again and restarts
// it onto it.
//
// An agent that names no layer — one too old to say — or a health read that
// fails says nothing of which agent runs. Where the instance held no other
// layer by the pool's record — a first placement, or a move of its pins or
// bootstrap alone — the answer the boot's wait saw proves the layer anyway,
// since a lost copy would have left no agent to give it, and it is recorded;
// so a machine whose agent is too old to name its layer is not restarted at
// every request. On a move from another layer it proves nothing, and the
// placement is left unrecorded: not knowing costs one restart rather than a
// record that may be false. The caller holds the sandbox's power lock.
func (r *MachineSandboxRuntime) recordForcedPlacement(ctx context.Context, sandboxID string, placement placedRecord) error {
	before, had, err := r.readPlaced(sandboxID)
	if err != nil {
		return err
	}
	moved := had && before.Container == placement.Container && before.Layer != placement.Layer
	dial, err := r.SandboxDialer(ctx, sandboxID, SandboxAgentPort)
	var health agentlayer.Health
	if err == nil {
		health, err = sandboxAgentHealth(ctx, dial)
	}
	switch {
	case err == nil && health.Layer == placement.Layer:
	case err == nil && health.Layer != "":
		slog.WarnContext(ctx, "a sandbox machine forced off on its way to an agent layer runs another; it is placed again at its next request",
			"sandboxId", sandboxID, "placed", placement.Layer, "running", health.Layer)
		return nil
	case moved:
		slog.WarnContext(ctx, "a sandbox machine forced off on its way to a new agent layer does not say which it runs; it is placed again at its next request",
			"sandboxId", sandboxID, "placed", placement.Layer, "error", err)
		return nil
	}
	return r.writePlaced(sandboxID, placement)
}

// remakeFresh makes again, fresh from the driver's base, a sandbox's machine
// made from the snapshot keyed key whose agent never came up on its first
// start, and starts it (ADR 26-11-03-986 §6). The snapshot is dropped first,
// so no create after this one is made from it. The machine held nothing of
// the sandbox's yet: it had never run its agent. The caller holds the
// sandbox's power lock.
func (r *MachineSandboxRuntime) remakeFresh(ctx context.Context, sandboxID, key string, cause error) error {
	slog.WarnContext(ctx, "a sandbox machine made from a snapshot did not come up; dropping the snapshot and making the machine fresh",
		"sandboxId", sandboxID, "snapshot", snapshotMachineName(key), "error", cause)
	// Its plan is read before the record goes, so the failure reports it.
	var plan *SnapshotPlanName
	if record, ok, err := r.readSnapshotRecord(key); err == nil && ok {
		plan = record.Plan
	}
	if err := r.dropSnapshot(ctx, key); err != nil {
		slog.WarnContext(ctx, "drop a snapshot whose sandbox did not come up", "snapshot", snapshotMachineName(key), "error", err)
	}
	// The plan waits before it is built again, as a failed build's does: a
	// snapshot rebuilt at once would likely fail its next sandbox the same.
	// It is reported as a failed plan, with why (ADR 26-11-17-316 §2).
	reason := fmt.Sprintf("a sandbox machine made from it did not come up: %v", cause)
	if err := r.recordSnapshotFailure(key, time.Now(), reason, plan); err != nil {
		slog.WarnContext(ctx, "record a snapshot whose sandbox did not come up", "snapshot", snapshotMachineName(key), "error", err)
	}
	if err := r.remakeMachine(ctx, sandboxID); err != nil {
		return errors.Join(cause, err)
	}
	return r.startLocked(ctx, sandboxID)
}

// remakeOnFreshRoot gives a sandbox's machine that keeps its data on its
// volume a fresh root from the driver's base in place of the root from the
// snapshot keyed key whose agent never came up, its volumes kept, and starts
// it. The snapshot stands: the box has run, and its volume, which its agent
// reads at boot, is the box's own to break, so its agent never answering
// says nothing of a snapshot every box of its plan shares. A fresh root that
// does not come up either fails the start. The caller holds the sandbox's
// power lock.
func (r *MachineSandboxRuntime) remakeOnFreshRoot(ctx context.Context, sandboxID, key string, cause error) error {
	slog.WarnContext(ctx, "a sandbox machine's new root from a snapshot did not come up; giving it a fresh root, keeping its volumes",
		"sandboxId", sandboxID, "snapshot", snapshotMachineName(key), "error", cause)
	record, ok, err := r.readMachineRecord(sandboxID)
	if err != nil {
		return errors.Join(cause, err)
	}
	if !ok {
		return errors.Join(cause, ErrNoContainer)
	}
	// No snapshot's from here, whatever the driver does with the root.
	record.Snapshot = ""
	record.NewRoot, record.NewRootSnapshot, record.NewRootAsked = true, "", false
	if err := r.writeMachineRecord(sandboxID, record); err != nil {
		return errors.Join(cause, err)
	}
	return r.startLocked(ctx, sandboxID)
}

// stopLocked shuts the sandbox's machine down, a frozen one too, which the
// engine resumes to shut down in order. The caller holds the sandbox's power
// lock.
func (r *MachineSandboxRuntime) stopLocked(ctx context.Context, sandboxID string) error {
	if err := r.finishSuspend(ctx, sandboxID); err != nil {
		return err
	}
	if _, err := r.powerTarget(ctx, sandboxID); err != nil {
		return err
	}
	r.publishSandboxState(ctx, sandboxID, StateStopping)
	return r.stopMachine(ctx, sandboxID)
}

// stopMachine shuts the machine down in order, forcing it off after
// machineStopTimeout, and reports it stopped. Forced off is still off. A
// machine the pool froze is frozen no longer.
func (r *MachineSandboxRuntime) stopMachine(ctx context.Context, sandboxID string) error {
	inst, err := r.instance(ctx, sandboxID)
	if err != nil {
		return err
	}
	r.endpoints.Delete(sandboxID)
	if err := r.engine.Stop(ctx, inst, machineStopTimeout); err != nil && r.engine.State(ctx, inst) != engine.Stopped {
		return fmt.Errorf("stop sandbox %s's machine: %w", sandboxID, err)
	}
	record, ok, err := r.readMachineRecord(sandboxID)
	if err != nil {
		return err
	}
	if ok && record.Frozen {
		record.Frozen = false
		if err := r.writeMachineRecord(sandboxID, record); err != nil {
			return err
		}
	}
	status := StatusStopped
	if !record.Started {
		status = StatusCreated
	}
	r.observe(sandboxID, status)
	r.publishSandboxState(ctx, sandboxID, StateStopped)
	return nil
}
