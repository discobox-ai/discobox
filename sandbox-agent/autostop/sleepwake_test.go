package autostop

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeRequest puts a sleep or wake file in place dated at.
func writeRequest(t *testing.T, dir, name string, at time.Time) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("note"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEvaluateReportsIdleSleepAndTheNextWake(t *testing.T) {
	c := &clock{now: epoch}
	p := newPolicy(t, c, nil)
	if state := p.Evaluate(epoch.Add(time.Minute)); state.Idle || !state.Sleep.IsZero() || !state.NextWake.IsZero() {
		t.Fatalf("state = %+v, want nothing to report", state)
	}
	writeRequest(t, p.sleepDir, "agent", epoch.Add(2*time.Minute))
	writeRequest(t, p.wakeDir, "ci", epoch.Add(3*time.Hour))
	writeRequest(t, p.wakeDir, "review", epoch.Add(time.Hour))
	writeRequest(t, p.wakeDir, "past", epoch.Add(-time.Hour))
	writeRequest(t, p.wakeDir, ".draft", epoch.Add(30*time.Minute))
	state := p.Evaluate(epoch.Add(30 * time.Minute))
	if !state.Idle {
		t.Fatal("not idle at the idle timeout")
	}
	if state.Sleep.Name != "agent" {
		t.Fatalf("sleep = %+v, want the agent's request", state.Sleep)
	}
	if state.NextWake.Name != "review" || !state.NextWake.At.Equal(epoch.Add(time.Hour)) {
		t.Fatalf("next wake = %+v, want the earliest one in the future, ignoring dot files and past ones", state.NextWake)
	}
}

// A client who attached since the idle report must not be cut off.
func TestConfirmIdleRefusesASandboxThatIsNoLongerIdle(t *testing.T) {
	c := &clock{now: epoch}
	p := newPolicy(t, c, nil)
	c.Set(epoch.Add(31 * time.Minute))
	if _, err := p.Confirm(ReasonIdle); err != nil {
		t.Fatalf("confirm idle = %v, want confirmed", err)
	}
	release := p.Hold("attach to exec primary")
	defer release()
	if _, err := p.Confirm(ReasonIdle); !errors.Is(err, ErrNotConfirmed) {
		t.Fatalf("confirm idle with a client connected = %v, want ErrNotConfirmed", err)
	}
}

// A frozen sandbox keeps /run, so a confirmed sleep must consume its request
// or the sandbox would resume straight into another sleep.
func TestConfirmSleepConsumesEveryPendingRequest(t *testing.T) {
	c := &clock{now: epoch}
	p := newPolicy(t, c, nil)
	if _, err := p.Confirm(ReasonSleep); !errors.Is(err, ErrNotConfirmed) {
		t.Fatalf("confirm sleep with none pending = %v, want ErrNotConfirmed", err)
	}
	writeRequest(t, p.sleepDir, "agent", epoch)
	writeRequest(t, p.sleepDir, "other", epoch)
	if _, err := p.Confirm(ReasonSleep); err != nil {
		t.Fatalf("confirm sleep = %v", err)
	}
	if state := p.Evaluate(epoch); !state.Sleep.IsZero() {
		t.Fatalf("sleep = %+v after confirming, want consumed", state.Sleep)
	}
	if _, err := p.Confirm(ReasonSleep); !errors.Is(err, ErrNotConfirmed) {
		t.Fatalf("second confirm = %v, want ErrNotConfirmed", err)
	}
}

// A wake that fell due while the sandbox ran is still owed: it is what the
// confirmation hands over, ahead of any later one.
func TestConfirmCarriesTheEarliestPendingWakeDueOnesFirst(t *testing.T) {
	c := &clock{now: epoch}
	p := newPolicy(t, c, nil)
	writeRequest(t, p.sleepDir, "agent", epoch)
	writeRequest(t, p.wakeDir, "later", epoch.Add(time.Hour))
	if wake, err := p.Confirm(ReasonSleep); err != nil || wake.Name != "later" {
		t.Fatalf("confirm = %+v, %v, want the future wake", wake, err)
	}
	writeRequest(t, p.sleepDir, "agent", epoch)
	writeRequest(t, p.wakeDir, "missed", epoch.Add(-time.Hour))
	if wake, err := p.Confirm(ReasonSleep); err != nil || wake.Name != "missed" {
		t.Fatalf("confirm = %+v, %v, want the due wake no boot handed over", wake, err)
	}
}

func TestTakeDueWakesMovesThemAsideOldestFirst(t *testing.T) {
	c := &clock{now: epoch}
	p := newPolicy(t, c, nil)
	writeRequest(t, p.wakeDir, "second", epoch.Add(-time.Minute))
	writeRequest(t, p.wakeDir, "first", epoch.Add(-time.Hour))
	writeRequest(t, p.wakeDir, "future", epoch.Add(time.Hour))
	paths, err := p.TakeDueWakes()
	if err != nil {
		t.Fatal(err)
	}
	done := filepath.Join(p.wakeDir, WakeDoneDir)
	want := []string{filepath.Join(done, "first"), filepath.Join(done, "second")}
	if len(paths) != 2 || paths[0] != want[0] || paths[1] != want[1] {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	if data, err := os.ReadFile(paths[0]); err != nil || string(data) != "note" {
		t.Fatalf("handed-over wake = %q, %v, want its note kept", data, err)
	}
	if paths, err := p.TakeDueWakes(); err != nil || len(paths) != 0 {
		t.Fatalf("second take = %v, %v, want nothing: a wake is handed over once", paths, err)
	}
	if state := p.Evaluate(epoch); state.NextWake.Name != "future" {
		t.Fatalf("next wake = %+v, want the future one left in place", state.NextWake)
	}
}

func TestRunNeverPowersOffOnceTheControlPlaneHoldsThePower(t *testing.T) {
	c := &clock{now: epoch}
	p, calls, _ := runPolicy(t, c, nil, func() error { return nil })
	p.SetControlPlanePower(true)
	c.Set(epoch.Add(time.Hour))
	select {
	case <-calls:
		t.Fatal("powered off while the control plane holds the power")
	case <-time.After(50 * time.Millisecond):
	}
	if state, ok := p.Status(); !ok || !state.Idle || !state.ControlPlanePower {
		t.Fatalf("status = %+v, %v, want idle reported for the control plane", state, ok)
	}
}

// Before the control plane holds the power, a sleep is still honored the only
// way the sandbox can: a power-off now, the request consumed.
func TestRunPowersOffOnASleepRequestWithoutTheControlPlane(t *testing.T) {
	c := &clock{now: epoch}
	p, calls, _ := runPolicy(t, c, nil, func() error { return nil })
	writeRequest(t, p.sleepDir, "agent", epoch)
	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("never powered off on a sleep request")
	}
	if state := p.Evaluate(epoch); !state.Sleep.IsZero() {
		t.Fatalf("sleep = %+v after powering off, want consumed", state.Sleep)
	}
}

// The agent restarting within a boot takes nothing: the wakes it would move
// aside are for a primary launch that is not coming (ADR 26-10-16-337 §7).
func TestTakeDueWakesHandsOverOncePerBoot(t *testing.T) {
	c := &clock{now: epoch}
	p := newPolicy(t, c, nil)
	if paths, err := p.TakeDueWakes(); err != nil || len(paths) != 0 {
		t.Fatalf("first take = %v, %v, want nothing due", paths, err)
	}
	writeRequest(t, p.wakeDir, "fell-due-while-running", epoch.Add(-time.Minute))
	again := New(Config{LeaseDir: p.leaseDir, SleepDir: p.sleepDir, WakeDir: p.wakeDir, ControlPlaneMarker: p.controlPlaneMarker, HandoverMarker: p.handoverMarker, Now: c.Now})
	if paths, err := again.TakeDueWakes(); err != nil || len(paths) != 0 {
		t.Fatalf("take after an agent restart = %v, %v, want nothing: this boot already handed over", paths, err)
	}
	if len(ReadRequests(p.wakeDir)) != 1 {
		t.Fatal("the wake that fell due while running was moved aside")
	}
}

func TestTheControlPlaneMarkerFollowsTheHandover(t *testing.T) {
	p := newPolicy(t, &clock{now: epoch}, nil)
	// At boot nothing has made its directory yet.
	p.controlPlaneMarker = filepath.Join(t.TempDir(), "fresh-run", "discobox", "control-plane-power")
	p.SetControlPlanePower(true)
	if _, err := os.Stat(p.controlPlaneMarker); err != nil {
		t.Fatalf("marker missing after the handover: %v", err)
	}
	p.SetControlPlanePower(false)
	if _, err := os.Stat(p.controlPlaneMarker); !os.IsNotExist(err) {
		t.Fatalf("marker still there after the power was taken back: %v", err)
	}
}
