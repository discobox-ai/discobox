package sandboxruntime

import (
	"context"
	"log/slog"
	"net"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
)

// firstStateBatch is what the state channel publishes at its first poll, a
// complete sync, with the runtime as it finds it now.
func firstStateBatch(t *testing.T, r *MachineSandboxRuntime) SandboxStateBatch {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	batches := make(chan SandboxStateBatch, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.WatchSandboxStates(ctx, slog.Default(), func(_ context.Context, batch SandboxStateBatch) error {
			select {
			case batches <- batch:
			default:
			}
			cancel()
			return nil
		})
	}()
	<-done
	select {
	case batch := <-batches:
		return batch
	default:
		t.Fatal("the state channel published nothing")
		return SandboxStateBatch{}
	}
}

// waitForNoResume waits out the state poll's background resume of sandboxID.
func waitForNoResume(t *testing.T, r *MachineSandboxRuntime, sandboxID string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		if _, resuming := r.resuming.Load(sandboxID); !resuming {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the state poll's resume did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// reportedState is the state batch reports for sandboxID, or "" for none.
func reportedState(batch SandboxStateBatch, sandboxID string) string {
	for _, observation := range batch.States {
		if observation.SandboxID == sandboxID {
			return observation.State
		}
	}
	return ""
}

// The issue's done-when (#1523): a machine its provider paused (Archil at a
// sandbox's session limit, here the fake driver's hook) or stopped, with no
// call from discobox, is reported as it is on the state channel, as a pause
// (`suspended`) or stopped; reading it wakes nothing; and the next use
// resumes the paused one as it was, booting nothing, and thaws it.
func TestMachinePausedByItsProviderIsReportedAndResumedByUse(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_p")); err != nil {
		t.Fatal(err)
	}
	inst, err := f.owner.Instance(ctx, "sbx-p")
	if err != nil {
		t.Fatal(err)
	}
	agent := f.agents.get(t, "sbx_p")
	deliveries := len(agent.revisions())
	f.driver.takeEvents("sbx-p")
	if got := reportedState(firstStateBatch(t, r), "sbx_p"); got != StateRunning {
		t.Fatalf("state channel before the pause = %q, want running", got)
	}

	if err := fake.Pause(ctx, f.owner, inst); err != nil {
		t.Fatal(err)
	}
	if sb, err := r.GetSandbox(ctx, "sbx_p"); err != nil || sb.Status != StatusPaused {
		t.Fatalf("paused sandbox = %+v, %v; want paused", sb, err)
	}
	if state, err := f.owner.State(ctx, inst); err != nil || state != machine.StatePaused {
		t.Fatalf("machine after the pool read it = %q, %v; want it still paused", state, err)
	}

	// The next use resumes it: the same guest and agent, no boot.
	if err := r.EnsureSandboxRunning(ctx, "sbx_p"); err != nil {
		t.Fatal(err)
	}
	if state, err := f.owner.State(ctx, inst); err != nil || state != machine.StateRunning {
		t.Fatalf("machine after use = %q, %v; want running", state, err)
	}
	if events := f.driver.takeEvents("sbx-p"); !slices.Equal(events, []string{"resume"}) {
		t.Fatalf("what the use did to the machine = %v, want a resume alone", events)
	}
	if f.agents.get(t, "sbx_p") != agent {
		t.Fatal("the resumed machine's agent was started again, want the one it was paused with")
	}
	// Its document delivered again, and its agent not told it resumed: a
	// wake due across a provider's pause is still owed.
	if got := len(agent.revisions()); got != deliveries+1 {
		t.Fatalf("runtime-config deliveries after the resume = %d, want %d", got, deliveries+1)
	}
	if confirms, resumes := agent.autostopCalls(); confirms != 0 || resumes != 0 {
		t.Fatalf("the resume from a provider's pause made %d autostop confirms and %d resumed calls, want none", confirms, resumes)
	}
	if _, err := sandboxAgentHealth(ctx, mustDialer(t, r, "sbx_p")); err != nil {
		t.Fatalf("reach the resumed sandbox: %v", err)
	}

	// Paused again, the state channel reports the pause it finds; its poll
	// then resumes it in the background, which is waited out.
	if err := fake.Pause(ctx, f.owner, inst); err != nil {
		t.Fatal(err)
	}
	if got := reportedState(firstStateBatch(t, r), "sbx_p"); got != StateSuspended {
		t.Fatalf("state channel after the provider's pause = %q, want suspended", got)
	}
	waitForNoResume(t, r, "sbx_p")

	// Stopped behind the pool's back, it is reported stopped.
	if err := (&engine.Engine{Driver: f.owner, Pool: machineTestPool}).Stop(ctx, inst, time.Minute); err != nil {
		t.Fatal(err)
	}
	if got := reportedState(firstStateBatch(t, r), "sbx_p"); got != StateStopped {
		t.Fatalf("state channel after the provider's stop = %q, want stopped", got)
	}
}

// An explicit start resumes a paused machine as use does, and a stop stops
// it, the engine resuming it first so its guest shuts down in order.
func TestMachinePausedByItsProviderStartsAndStops(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_q")); err != nil {
		t.Fatal(err)
	}
	inst, err := f.owner.Instance(ctx, "sbx-q")
	if err != nil {
		t.Fatal(err)
	}
	f.driver.takeEvents("sbx-q")

	if err := fake.Pause(ctx, f.owner, inst); err != nil {
		t.Fatal(err)
	}
	if err := r.StartSandbox(ctx, "sbx_q", nil); err != nil {
		t.Fatal(err)
	}
	if sb, err := r.GetSandbox(ctx, "sbx_q"); err != nil || sb.Status != StatusRunning {
		t.Fatalf("started sandbox = %+v, %v; want running", sb, err)
	}
	if events := f.driver.takeEvents("sbx-q"); !slices.Equal(events, []string{"resume"}) {
		t.Fatalf("what the start did to the machine = %v, want a resume alone", events)
	}

	if err := fake.Pause(ctx, f.owner, inst); err != nil {
		t.Fatal(err)
	}
	if err := r.StopSandbox(ctx, "sbx_q", nil); err != nil {
		t.Fatal(err)
	}
	if sb, err := r.GetSandbox(ctx, "sbx_q"); err != nil || sb.Status != StatusStopped {
		t.Fatalf("stopped sandbox = %+v, %v; want stopped", sb, err)
	}
	if state, err := f.owner.State(ctx, inst); err != nil || state != machine.StateStopped {
		t.Fatalf("machine after the stop = %q, %v; want stopped", state, err)
	}
}

// A paused machine whose placement moved while it was paused (here, its
// record of it lost) is resumed, then placed as a running machine is: applied
// in place, its agent restarted onto what it boots on now, and the machine
// not restarted (ADR 26-11-23-458).
func TestMachinePausedWithAMovedPlacementIsPlacedInPlace(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_s")); err != nil {
		t.Fatal(err)
	}
	inst, err := f.owner.Instance(ctx, "sbx-s")
	if err != nil {
		t.Fatal(err)
	}
	f.driver.takeEvents("sbx-s")
	if err := fake.Pause(ctx, f.owner, inst); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(r.placedRecordPath("sbx_s")); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsureSandboxRunning(ctx, "sbx_s"); err != nil {
		t.Fatal(err)
	}
	if got, want := f.driver.takeEvents("sbx-s"), []string{"resume", "stop agent", "apply in place"}; !slices.Equal(got, want) {
		t.Fatalf("what the use did to the machine = %v, want %v: a resume, then its placement applied in place", got, want)
	}
	if _, ok, err := r.readPlaced("sbx_s"); err != nil || !ok {
		t.Fatalf("placement record = %v, %v; want it written again", ok, err)
	}
	if _, err := sandboxAgentHealth(ctx, mustDialer(t, r, "sbx_s")); err != nil {
		t.Fatalf("reach the restarted sandbox: %v", err)
	}
}

// A machine its provider paused since the last poll still reads as running to
// the runtime, so a request is answered from that (answersRunning) and dials
// it; the dial that fails is what finds it, and the next use resumes it
// rather than answering running again.
func TestMachinePausedSinceThePollIsResumedAfterADialFails(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_t")); err != nil {
		t.Fatal(err)
	}
	inst, err := f.owner.Instance(ctx, "sbx-t")
	if err != nil {
		t.Fatal(err)
	}
	f.driver.takeEvents("sbx-t")
	if err := fake.Pause(ctx, f.owner, inst); err != nil {
		t.Fatal(err)
	}
	if !r.answersRunning(ctx, "sbx_t") {
		t.Fatal("a sandbox paused since the runtime last looked is not answered running from what it saw; the case this tests is gone")
	}
	// A paused machine answers nothing at its URL: here, a closed port.
	closed, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := closed.Addr().String()
	_ = closed.Close()
	cache := &machineEndpointCache{}
	cache.endpoint.Store(&machine.Endpoint{URL: "http://" + address})
	r.endpoints.Store("sbx_t", cache)
	dial := mustDialer(t, r, "sbx_t")
	if _, err := sandboxAgentHealth(ctx, dial); err == nil {
		t.Fatal("a dial to a paused machine's closed URL answered")
	}
	if r.answersRunning(ctx, "sbx_t") {
		t.Fatal("a sandbox whose dial failed is still answered running from what was seen")
	}
	if err := r.EnsureSandboxRunning(ctx, "sbx_t"); err != nil {
		t.Fatal(err)
	}
	if events := f.driver.takeEvents("sbx-t"); !slices.Equal(events, []string{"resume"}) {
		t.Fatalf("what the use did to the machine = %v, want a resume alone", events)
	}
	if _, err := sandboxAgentHealth(ctx, mustDialer(t, r, "sbx_t")); err != nil {
		t.Fatalf("reach the resumed sandbox: %v", err)
	}
}

// The state poll publishes the pause it finds before it resumes the machine:
// a resume publishes its own states, reported later, and the control plane
// drops a report older than one it holds, so a pause published after them
// would never be seen (as on a live boxd pool, where the server went from
// running to starting and never showed the pause).
func TestThePollReportsAPauseBeforeItResumesIt(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_u")); err != nil {
		t.Fatal(err)
	}
	inst, err := f.owner.Instance(ctx, "sbx-u")
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.Pause(ctx, f.owner, inst); err != nil {
		t.Fatal(err)
	}
	type report struct {
		at    time.Time
		state string
	}
	var (
		mu      sync.Mutex
		reports []report
	)
	watchCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.WatchSandboxStates(watchCtx, slog.Default(), func(_ context.Context, batch SandboxStateBatch) error {
			// A publish is a call to the control plane, which takes time:
			// the batch that carries the pause is slowed, so a resume begun
			// before it is out would publish first.
			for _, o := range batch.States {
				if o.SandboxID == "sbx_u" && o.State == StateSuspended {
					time.Sleep(time.Second)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			for _, o := range batch.States {
				if o.SandboxID == "sbx_u" {
					reports = append(reports, report{at: batch.ReportedAt, state: o.State})
					if o.State == StateRunning && len(reports) > 1 {
						cancel()
					}
				}
			}
			return nil
		})
	}()
	<-done
	waitForNoResume(t, r, "sbx_u")
	mu.Lock()
	defer mu.Unlock()
	pause := -1
	for i, rep := range reports {
		if rep.state == StateSuspended {
			pause = i
			break
		}
	}
	if pause < 0 {
		t.Fatalf("reports %v: the pause was never published", reports)
	}
	for _, rep := range reports[:pause] {
		if rep.at.After(reports[pause].at) {
			t.Fatalf("reports %v: a report newer than the pause went out before it, so the control plane would drop the pause", reports)
		}
	}
	if last := reports[len(reports)-1].state; last != StateRunning {
		t.Fatalf("reports %v: want the poll's resume to end running", reports)
	}
}
