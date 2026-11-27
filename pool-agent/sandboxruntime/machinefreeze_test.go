package sandboxruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
)

// A machine pool freezes a sleeping sandbox where its driver can, and every
// start thaws it (ADR 26-11-21-395).

// freezeFixture is a machine fixture with one running sandbox, sandboxID,
// and its machine as the account's owner sees it.
func freezeFixture(t *testing.T, config json.RawMessage, sandboxID string) (*machineFixture, machine.Instance) {
	t.Helper()
	f := newMachineFixture(t, config)
	ctx := context.Background()
	if _, err := f.runtime.CreateSandbox(ctx, machineCreate(sandboxID)); err != nil {
		t.Fatal(err)
	}
	inst, err := f.owner.Instance(ctx, machineNameFor(t, sandboxID))
	if err != nil {
		t.Fatal(err)
	}
	f.driver.takeEvents(inst.Name)
	return f, inst
}

func machineNameFor(t *testing.T, sandboxID string) string {
	t.Helper()
	name, err := machineName(sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func requireStatus(t *testing.T, r *MachineSandboxRuntime, sandboxID string, want Status, when string) *Sandbox {
	t.Helper()
	sb, err := r.GetSandbox(context.Background(), sandboxID)
	if err != nil {
		t.Fatalf("%s: %v", when, err)
	}
	if sb.Status != want {
		t.Fatalf("%s: sandbox is %s, want %s", when, sb.Status, want)
	}
	return sb
}

func requireFrozenRecord(t *testing.T, r *MachineSandboxRuntime, sandboxID string, want bool, when string) {
	t.Helper()
	record, _, err := r.readMachineRecord(sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Frozen != want {
		t.Fatalf("%s: the record says frozen = %v, want %v", when, record.Frozen, want)
	}
}

// A confirmed sleep freezes the machine, which reads suspended, is reported a
// sleep, and is left alone by the state poll's resume of a provider's pause. A
// use thaws it on the machine it had, boots nothing, and runs the after-thaw
// steps: the runtime-config document, and the agent told it resumed.
func TestMachineSleepFreezesAndAUseThawsIt(t *testing.T) {
	f, inst := freezeFixture(t, nil, "sbx_20")
	ctx := context.Background()
	r := f.runtime
	agent := f.agents.get(t, "sbx_20")
	deliveries := len(agent.revisions())

	if _, err := r.PowerDownSandbox(ctx, "sbx_20", PowerDownSleep); err != nil {
		t.Fatalf("sleep: %v", err)
	}
	if state := r.engine.State(ctx, inst); state != engine.Paused {
		t.Fatalf("a slept machine is %s, want paused", state)
	}
	sb := requireStatus(t, r, "sbx_20", StatusSuspended, "slept")
	if state := stateFromStatus(sb.Status); state != StateSuspended {
		t.Fatalf("a slept machine is reported %s, want %s", state, StateSuspended)
	}
	requireFrozenRecord(t, r, "sbx_20", true, "slept")
	if err := r.resumePaused(ctx, "sbx_20"); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, r, "sbx_20", StatusSuspended, "after the state poll's resume of a provider's pause")

	if err := r.EnsureSandboxRunning(ctx, "sbx_20"); err != nil {
		t.Fatalf("use a slept sandbox: %v", err)
	}
	thawed := requireStatus(t, r, "sbx_20", StatusRunning, "used")
	if thawed.ID != inst.ID {
		t.Fatalf("thawed onto machine %s, want %s", thawed.ID, inst.ID)
	}
	if events := f.driver.takeEvents(inst.Name); !slices.Equal(events, []string{"resume"}) {
		t.Fatalf("a thaw did %v, want only a resume: no boot, nothing placed again", events)
	}
	requireFrozenRecord(t, r, "sbx_20", false, "used")
	if confirms, resumes := agent.autostopCalls(); confirms != 1 || resumes != 1 {
		t.Fatalf("agent confirmed %d power-downs and was told of %d thaws, want 1 and 1", confirms, resumes)
	}
	if got := len(agent.revisions()); got != deliveries+1 {
		t.Fatalf("runtime-config deliveries = %d, want %d: one after the thaw", got, deliveries+1)
	}
}

// A sleep on a driver that cannot freeze, or whose freeze fails, stops the
// machine, as a container is stopped when its pause fails; a failed freeze
// leaves no record that the pool froze it.
func TestMachineSleepStopsWhereItCannotFreeze(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config json.RawMessage
		refuse bool
	}{
		{name: "no capability", config: json.RawMessage(`{"noPause":true}`)},
		{name: "a failed freeze", refuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, inst := freezeFixture(t, tc.config, "sbx_21")
			ctx := context.Background()
			f.driver.pauseRefused.Store(tc.refuse)
			if _, err := f.runtime.PowerDownSandbox(ctx, "sbx_21", PowerDownSleep); err != nil {
				t.Fatalf("sleep: %v", err)
			}
			if state := f.runtime.engine.State(ctx, inst); state != engine.Stopped {
				t.Fatalf("machine is %s, want stopped", state)
			}
			requireStatus(t, f.runtime, "sbx_21", StatusStopped, "slept")
			requireFrozenRecord(t, f.runtime, "sbx_21", false, "slept")
		})
	}
}

// An idle power-down stops a machine that could be frozen: only a sleep
// freezes.
func TestMachineIdleStillStops(t *testing.T) {
	f, _ := freezeFixture(t, nil, "sbx_22")
	if _, err := f.runtime.PowerDownSandbox(context.Background(), "sbx_22", PowerDownIdle); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, f.runtime, "sbx_22", StatusStopped, "idle")
}

// A suspend freezes a running machine, and again is a no-op. One that fails
// is returned with the machine running and no record of a freeze: a suspend
// never turns into a stop. A driver that cannot freeze is refused, and so is a
// stopped sandbox.
func TestMachineSuspend(t *testing.T) {
	f, inst := freezeFixture(t, nil, "sbx_23")
	ctx := context.Background()
	r := f.runtime

	f.driver.pauseRefused.Store(true)
	if err := r.SuspendSandbox(ctx, "sbx_23", nil); err == nil {
		t.Fatal("a refused freeze suspended the sandbox")
	}
	requireStatus(t, r, "sbx_23", StatusRunning, "after a failed suspend")
	requireFrozenRecord(t, r, "sbx_23", false, "after a failed suspend")

	f.driver.pauseRefused.Store(false)
	for _, when := range []string{"suspended", "suspended again"} {
		if err := r.SuspendSandbox(ctx, "sbx_23", nil); err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		requireStatus(t, r, "sbx_23", StatusSuspended, when)
	}
	if state := r.engine.State(ctx, inst); state != engine.Paused {
		t.Fatalf("a suspended machine is %s, want paused", state)
	}

	if err := r.StopSandbox(ctx, "sbx_23", nil); err != nil {
		t.Fatalf("stop a suspended sandbox: %v", err)
	}
	requireStatus(t, r, "sbx_23", StatusStopped, "stopped while suspended")
	requireFrozenRecord(t, r, "sbx_23", false, "stopped while suspended")
	if err := r.SuspendSandbox(ctx, "sbx_23", nil); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("suspend a stopped sandbox = %v, want ErrNotRunning", err)
	}
}

// A machine the pool froze and then stopped is frozen no longer: a pause its
// provider makes later reads as the provider's, never as a sleep.
func TestAPauseAfterAFrozenMachineStoppedIsTheProviders(t *testing.T) {
	f, inst := freezeFixture(t, nil, "sbx_24")
	ctx := context.Background()
	r := f.runtime
	if err := r.SuspendSandbox(ctx, "sbx_24", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.RestartSandbox(ctx, "sbx_24", nil); err != nil {
		t.Fatalf("restart a suspended sandbox: %v", err)
	}
	requireStatus(t, r, "sbx_24", StatusRunning, "restarted")
	if err := fake.Pause(ctx, f.owner, inst); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, r, "sbx_24", StatusPaused, "paused by its provider")
}

// A machine the pool froze that something else resumed (Archil, for a request
// with its port token) gets its after-thaw steps from the state poll, once.
func TestAFrozenMachineThawedOutsideThePoolIsSettled(t *testing.T) {
	f, inst := freezeFixture(t, nil, "sbx_25")
	ctx := context.Background()
	r := f.runtime
	agent := f.agents.get(t, "sbx_25")
	if err := r.SuspendSandbox(ctx, "sbx_25", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Start(ctx, inst, machine.BootOptions{}); err != nil {
		t.Fatalf("resume behind the pool's back: %v", err)
	}
	requireStatus(t, r, "sbx_25", StatusRunning, "resumed outside the pool")
	for range 2 {
		if err := r.settleFrozen(ctx, "sbx_25"); err != nil {
			t.Fatal(err)
		}
	}
	requireFrozenRecord(t, r, "sbx_25", false, "settled")
	if _, resumes := agent.autostopCalls(); resumes != 1 {
		t.Fatalf("agent told of %d thaws, want 1", resumes)
	}
}

// A thaw its provider refuses leaves the machine frozen, still suspended, and
// fails the use.
func TestARefusedThawStaysSuspended(t *testing.T) {
	f, inst := freezeFixture(t, nil, "sbx_26")
	ctx := context.Background()
	r := f.runtime
	if err := r.SuspendSandbox(ctx, "sbx_26", nil); err != nil {
		t.Fatal(err)
	}
	f.driver.startRefused.Store(true)
	if err := r.EnsureSandboxRunning(ctx, "sbx_26"); err == nil {
		t.Fatal("a refused thaw answered running")
	}
	if state := r.engine.State(ctx, inst); state != engine.Paused {
		t.Fatalf("a machine whose thaw was refused is %s, want paused", state)
	}
	requireStatus(t, r, "sbx_26", StatusSuspended, "after a refused thaw")
	requireFrozenRecord(t, r, "sbx_26", true, "after a refused thaw")
}

// A frozen machine owed a new root is stopped for it and boots on the new
// root, and its record no longer says the pool froze it: a pause its provider
// makes later is the provider's.
func TestAFrozenMachineOwedANewRootIsStoppedForIt(t *testing.T) {
	f, inst := freezeFixture(t, nil, "sbx_27")
	ctx := context.Background()
	r := f.runtime
	writeMachineFiles(t, r, inst)
	if err := r.SuspendSandbox(ctx, "sbx_27", nil); err != nil {
		t.Fatal(err)
	}
	record, _, err := r.readMachineRecord("sbx_27")
	if err != nil {
		t.Fatal(err)
	}
	record.NewRoot = true
	if err := r.writeMachineRecord("sbx_27", record); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsureSandboxRunning(ctx, "sbx_27"); err != nil {
		t.Fatalf("start a frozen machine owed a new root: %v", err)
	}
	if f.driver.upgrades != 1 {
		t.Fatalf("upgrades = %d, want one", f.driver.upgrades)
	}
	requireStatus(t, r, "sbx_27", StatusRunning, "on its new root")
	requireFrozenRecord(t, r, "sbx_27", false, "on its new root")
	assertNewRootKeptVolumes(t, r, inst)
}

// A resume that finds the machine already resumed by something else in
// between (Archil, for a request with its port token) is a resume, not a
// failed start; the start then runs the freeze's after-thaw steps.
func TestAResumeRacingAResumeOutsideThePoolIsAResume(t *testing.T) {
	f, inst := freezeFixture(t, nil, "sbx_28")
	ctx := context.Background()
	r := f.runtime
	agent := f.agents.get(t, "sbx_28")
	if err := r.SuspendSandbox(ctx, "sbx_28", nil); err != nil {
		t.Fatal(err)
	}
	record, _, err := r.readMachineRecord("sbx_28")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Start(ctx, inst, machine.BootOptions{}); err != nil {
		t.Fatalf("resume behind the pool's back: %v", err)
	}
	unlock := holdPower(t, r, "sbx_28")
	err = r.resumeLocked(ctx, "sbx_28", record)
	unlock()
	if err != nil {
		t.Fatalf("a resume of a machine resumed in between: %v", err)
	}
	if err := r.EnsureSandboxRunning(ctx, "sbx_28"); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, r, "sbx_28", StatusRunning, "resumed")
	requireFrozenRecord(t, r, "sbx_28", false, "resumed")
	if _, resumes := agent.autostopCalls(); resumes != 1 {
		t.Fatalf("agent told of %d thaws, want 1", resumes)
	}
}

// A suspend of a machine its provider paused, which reads suspended on the
// wire, records the freeze: it stays suspended, the state poll does not
// resume it, and a start thaws it with the after-thaw steps.
func TestASuspendOfAProviderPauseIsAFreeze(t *testing.T) {
	f, inst := freezeFixture(t, nil, "sbx_29")
	ctx := context.Background()
	r := f.runtime
	agent := f.agents.get(t, "sbx_29")
	if err := fake.Pause(ctx, f.owner, inst); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, r, "sbx_29", StatusPaused, "paused by its provider")
	if err := r.SuspendSandbox(ctx, "sbx_29", nil); err != nil {
		t.Fatalf("suspend a machine its provider paused: %v", err)
	}
	requireStatus(t, r, "sbx_29", StatusSuspended, "suspended")
	requireFrozenRecord(t, r, "sbx_29", true, "suspended")
	if err := r.resumePaused(ctx, "sbx_29"); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, r, "sbx_29", StatusSuspended, "after the state poll")
	if err := r.EnsureSandboxRunning(ctx, "sbx_29"); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, r, "sbx_29", StatusRunning, "used")
	if _, resumes := agent.autostopCalls(); resumes != 1 {
		t.Fatalf("agent told of %d thaws, want 1", resumes)
	}
}

// A freeze whose pause takes effect and then reports a failure, as one that
// timed out does, is a freeze: the record is kept, the box is suspended, and a
// sleep does not stop it. One whose machine cannot then be read keeps the
// record too, and fails; the state poll settles it once the machine reads
// running again.
func TestAFreezeThatFailsLateIsJudgedByTheMachine(t *testing.T) {
	t.Run("paused after all", func(t *testing.T) {
		f, inst := freezeFixture(t, nil, "sbx_30")
		ctx := context.Background()
		r := f.runtime
		f.driver.pauseLate.Store(true)
		if _, err := r.PowerDownSandbox(ctx, "sbx_30", PowerDownSleep); err != nil {
			t.Fatalf("sleep: %v", err)
		}
		if state := r.engine.State(ctx, inst); state != engine.Paused {
			t.Fatalf("machine is %s, want paused: a freeze that took effect is no reason to stop it", state)
		}
		requireStatus(t, r, "sbx_30", StatusSuspended, "slept")
		requireFrozenRecord(t, r, "sbx_30", true, "slept")
		if err := r.resumePaused(ctx, "sbx_30"); err != nil {
			t.Fatal(err)
		}
		requireStatus(t, r, "sbx_30", StatusSuspended, "after the state poll")
	})
	t.Run("state unknown", func(t *testing.T) {
		f, _ := freezeFixture(t, nil, "sbx_31")
		ctx := context.Background()
		r := f.runtime
		agent := f.agents.get(t, "sbx_31")
		f.driver.pauseRefused.Store(true)
		f.driver.stateUnknown.Store(true)
		if err := r.SuspendSandbox(ctx, "sbx_31", nil); err == nil {
			t.Fatal("a freeze whose machine cannot be read succeeded")
		}
		requireFrozenRecord(t, r, "sbx_31", true, "with the machine unread")
		// While the driver cannot be asked, the box is seen suspended, and
		// a settle leaves the record: it acts on no state last seen.
		requireStatus(t, r, "sbx_31", StatusSuspended, "with the machine unread")
		if err := r.settleFrozen(ctx, "sbx_31"); err != nil {
			t.Fatal(err)
		}
		requireFrozenRecord(t, r, "sbx_31", true, "settled with the machine unread")
		f.driver.stateUnknown.Store(false)
		if err := r.settleFrozen(ctx, "sbx_31"); err != nil {
			t.Fatal(err)
		}
		requireStatus(t, r, "sbx_31", StatusRunning, "settled")
		requireFrozenRecord(t, r, "sbx_31", false, "settled")
		if _, resumes := agent.autostopCalls(); resumes != 1 {
			t.Fatalf("agent told of %d thaws, want 1", resumes)
		}
	})
}

// A resume that leaves the machine stopped (a host restart, a provider that
// expired the pause) boots it fresh in the same start, as a slept machine did
// before a sleep froze one, rather than failing every start until someone
// stops it. A resume that leaves it paused fails, and keeps it suspended
// (TestARefusedThawStaysSuspended).
func TestAResumeThatLosesTheMachineBootsItFresh(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pause  func(*machineFixture, machine.Instance) error
		frozen bool
	}{
		{name: "the pool's freeze", pause: func(f *machineFixture, _ machine.Instance) error {
			return f.runtime.SuspendSandbox(context.Background(), "sbx_32", nil)
		}, frozen: true},
		{name: "a provider's pause", pause: func(f *machineFixture, inst machine.Instance) error {
			return fake.Pause(context.Background(), f.owner, inst)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, inst := freezeFixture(t, nil, "sbx_32")
			ctx := context.Background()
			r := f.runtime
			if err := tc.pause(f, inst); err != nil {
				t.Fatal(err)
			}
			requireFrozenRecord(t, r, "sbx_32", tc.frozen, "paused")
			want := StatusPaused
			if tc.frozen {
				want = StatusSuspended
			}
			requireStatus(t, r, "sbx_32", want, "paused")
			f.driver.takeEvents(inst.Name)
			f.driver.resumeLost.Store(true)
			if err := r.EnsureSandboxRunning(ctx, "sbx_32"); err != nil {
				t.Fatalf("use a box whose resume lost its machine: %v", err)
			}
			requireStatus(t, r, "sbx_32", StatusRunning, "booted fresh")
			requireFrozenRecord(t, r, "sbx_32", false, "booted fresh")
			if events := f.driver.takeEvents(inst.Name); !slices.Contains(events, "boot") {
				t.Fatalf("what the use did = %v, want a fresh boot", events)
			}
		})
	}
}

// The read of what a lost resume left is bounded: a driver that stops
// answering then fails the start once machineStateReadTimeout passes, rather
// than holding the sandbox's power lock while it hangs, and the next start,
// with the driver answering again, boots the machine fresh.
func TestALostResumesStateReadIsBounded(t *testing.T) {
	saved := machineStateReadTimeout
	machineStateReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { machineStateReadTimeout = saved })
	f, _ := freezeFixture(t, nil, "sbx_33")
	ctx := context.Background()
	r := f.runtime
	if err := r.SuspendSandbox(ctx, "sbx_33", nil); err != nil {
		t.Fatal(err)
	}
	f.driver.resumeLost.Store(true)
	f.driver.stateHangsAfterLoss.Store(true)
	done := make(chan error, 1)
	go func() { done <- r.EnsureSandboxRunning(ctx, "sbx_33") }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a start whose lost resume could not be read succeeded")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a start held the power lock while the driver hung")
	}
	f.driver.stateHangsAfterLoss.Store(false)
	f.driver.stateHangs.Store(false)
	if err := r.EnsureSandboxRunning(ctx, "sbx_33"); err != nil {
		t.Fatalf("start once the driver answers: %v", err)
	}
	requireStatus(t, r, "sbx_33", StatusRunning, "booted fresh")
	requireFrozenRecord(t, r, "sbx_33", false, "booted fresh")
}

// A machine the pool froze whose placement moved while it was frozen (here,
// its record of it lost) is resumed and placed in place, its agent restarted
// onto the placement, and not booted (ADR 26-11-23-458 §4). A restarted
// agent hands over nothing within a boot, so the restarted one is told it
// resumed, which hands over the wakes due as the thaw would have.
func TestAFrozenMachineWhosePlacementMovedIsToldItResumed(t *testing.T) {
	f, inst := freezeFixture(t, nil, "sbx_34")
	ctx := context.Background()
	r := f.runtime
	if err := r.SuspendSandbox(ctx, "sbx_34", nil); err != nil {
		t.Fatal(err)
	}
	requireFrozenRecord(t, r, "sbx_34", true, "frozen")
	if err := os.Remove(r.placedRecordPath("sbx_34")); err != nil {
		t.Fatal(err)
	}
	f.driver.takeEvents(inst.Name)
	if err := r.EnsureSandboxRunning(ctx, "sbx_34"); err != nil {
		t.Fatal(err)
	}
	if got, want := f.driver.takeEvents(inst.Name), []string{"resume", "apply in place"}; !slices.Equal(got, want) {
		t.Fatalf("what the use did = %v, want %v", got, want)
	}
	requireFrozenRecord(t, r, "sbx_34", false, "placed in place")
	if _, resumes := f.agents.get(t, "sbx_34").autostopCalls(); resumes != 1 {
		t.Fatalf("the restarted agent was told of %d thaws, want 1", resumes)
	}
}
