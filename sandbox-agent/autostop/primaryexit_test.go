package autostop

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

// primaryStub is the primary terminal as CurrentPrimary reads it, changed by
// a test as the shim would change its record.
type primaryStub struct {
	mu   sync.Mutex
	exec execs.Exec
	ok   bool
}

func (s *primaryStub) get() (execs.Exec, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exec, s.ok
}

func (s *primaryStub) set(e execs.Exec) {
	s.mu.Lock()
	s.exec, s.ok = e, true
	s.mu.Unlock()
}

// exitedRun is a primary whose login shell ran in this boot and ended.
func exitedRun() execs.Exec {
	code := int64(0)
	return execs.Exec{
		ID:          "primary-exec",
		Status:      execs.StatusExited,
		StartedAt:   at(-time.Hour),
		ExitedAt:    at(-time.Minute),
		ExitCode:    &code,
		CurrentBoot: true,
	}
}

func newExitPolicy(t *testing.T, c *clock, primary *primaryStub) *Policy {
	t.Helper()
	p := newPolicy(t, c, nil)
	p.primary = primary.get
	return p
}

// The shell ending, however it ended, is the exit; anything short of it, or
// any launch that never ran a shell, is not (ADR 26-11-16-372 §1).
func TestPrimaryExitedIsTheShellsRunEndingInThisBoot(t *testing.T) {
	running := exitedRun()
	running.Status, running.ExitedAt, running.ExitCode = execs.StatusRunning, nil, nil
	killed := exitedRun()
	killed.Status, killed.Error = execs.StatusFailed, "signal: killed"
	startFailed := exitedRun()
	startFailed.Status, startFailed.StartedAt, startFailed.Error = execs.StatusFailed, nil, "start: no such shell"
	lost := exitedRun()
	lost.Status = execs.StatusLost
	earlierBoot := exitedRun()
	earlierBoot.CurrentBoot = false
	for name, tc := range map[string]struct {
		exec execs.Exec
		want bool
	}{
		"shell exited":                 {exitedRun(), true},
		"shell killed":                 {killed, true},
		"shell running (harness quit)": {running, false},
		"start failed":                 {startFailed, false},
		"lost across a boot":           {lost, false},
		"run of an earlier boot":       {earlierBoot, false},
	} {
		t.Run(name, func(t *testing.T) {
			c := &clock{now: epoch}
			primary := &primaryStub{}
			primary.set(tc.exec)
			p := newExitPolicy(t, c, primary)
			p.SetControlPlanePower(true)
			state := p.Evaluate(epoch)
			if got := !state.PrimaryExitedAt.IsZero(); got != tc.want {
				t.Fatalf("primary exited = %v (%v), want %v", got, state.PrimaryExitedAt, tc.want)
			}
			if tc.want && !state.PrimaryExitedAt.Equal(*tc.exec.ExitedAt) {
				t.Fatalf("primaryExitedAt = %v, want the run's end %v", state.PrimaryExitedAt, *tc.exec.ExitedAt)
			}
			_, err := p.Confirm(ReasonExit)
			if tc.want && err != nil {
				t.Fatalf("confirm exit = %v, want confirmed", err)
			}
			if !tc.want && !errors.Is(err, ErrNotConfirmed) {
				t.Fatalf("confirm exit = %v, want ErrNotConfirmed", err)
			}
		})
	}
}

// No primary at all, as before the first launch, is no exit.
func TestNoPrimaryIsNoExit(t *testing.T) {
	c := &clock{now: epoch}
	p := newExitPolicy(t, c, &primaryStub{})
	if state := p.Evaluate(epoch); !state.PrimaryExitedAt.IsZero() {
		t.Fatalf("primaryExitedAt = %v, want none", state.PrimaryExitedAt)
	}
	if _, err := p.Confirm(ReasonExit); !errors.Is(err, ErrNotConfirmed) {
		t.Fatalf("confirm exit = %v, want ErrNotConfirmed", err)
	}
}

// An attach that revived the primary between the report and the confirmation
// started a new run, and is not cut off (ADR 26-11-16-372 §3).
func TestConfirmExitRefusesAPrimaryRevivedSinceTheReport(t *testing.T) {
	c := &clock{now: epoch}
	primary := &primaryStub{}
	primary.set(exitedRun())
	p := newExitPolicy(t, c, primary)
	if state := p.Evaluate(epoch); state.PrimaryExitedAt.IsZero() {
		t.Fatal("exit not reported")
	}
	revived := primary.exec
	revived.Status, revived.StartedAt, revived.ExitedAt, revived.ExitCode = execs.StatusRunning, at(0), nil, nil
	primary.set(revived)
	if _, err := p.Confirm(ReasonExit); !errors.Is(err, ErrNotConfirmed) {
		t.Fatalf("confirm exit after a revive = %v, want ErrNotConfirmed", err)
	}
}

// Nothing else in the sandbox holds it up: a client connected elsewhere, a
// lease, and a pending sleep still in its grace (ADR 26-11-16-372 §3). The
// confirmation leaves the sleep request alone and carries the pending wake.
func TestConfirmExitIgnoresClientsLeasesAndSleepGrace(t *testing.T) {
	c := &clock{now: epoch}
	primary := &primaryStub{}
	primary.set(exitedRun())
	p := newExitPolicy(t, c, primary)
	release := p.Hold("attach to exec other")
	defer release()
	writeRequest(t, p.leaseDir, "build", epoch.Add(time.Hour))
	writeRequest(t, p.sleepDir, "agent", epoch)
	writeRequest(t, p.wakeDir, "ci", epoch.Add(time.Hour))
	wake, err := p.Confirm(ReasonExit)
	if err != nil {
		t.Fatalf("confirm exit = %v, want confirmed", err)
	}
	if wake.Name != "ci" {
		t.Fatalf("wake = %+v, want the pending one", wake)
	}
	if len(ReadRequests(p.sleepDir)) != 1 {
		t.Fatal("confirming exit consumed the sleep request")
	}
}

// Without the control plane, the sandbox powers itself off on the exit, as it
// does on idle (ADR 26-11-16-372 §4); with it, it only reports.
func TestRunPowersOffOnAnExitWithoutTheControlPlane(t *testing.T) {
	c := &clock{now: epoch}
	primary := &primaryStub{}
	primary.set(exitedRun())
	_, calls := runExitPolicy(t, c, primary)
	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("never powered off on the primary's exit")
	}
}

func TestRunOnlyReportsAnExitUnderTheControlPlane(t *testing.T) {
	c := &clock{now: epoch}
	primary := &primaryStub{}
	primary.set(exitedRun())
	p, calls := runExitPolicy(t, c, primary)
	p.SetControlPlanePower(true)
	select {
	case <-calls:
		t.Fatal("powered off while the control plane holds the power")
	case <-time.After(50 * time.Millisecond):
	}
	if state, ok := p.Status(); !ok || state.PrimaryExitedAt.IsZero() {
		t.Fatalf("status = %+v, %v, want the exit reported", state, ok)
	}
}

// runExitPolicy runs a policy reading primary, as runPolicy does, and returns
// it and a channel that receives each power-off.
func runExitPolicy(t *testing.T, c *clock, primary *primaryStub) (*Policy, <-chan struct{}) {
	t.Helper()
	calls := make(chan struct{}, 16)
	dir := t.TempDir()
	p := New(Config{
		Primary:            primary.get,
		LeaseDir:           filepath.Join(dir, "keepalive"),
		SleepDir:           filepath.Join(dir, "sleep"),
		ControlPlaneMarker: filepath.Join(dir, "control-plane-power"),
		HandoverMarker:     filepath.Join(dir, "wakes-handed-over"),
		Interval:           time.Millisecond,
		Now:                c.Now,
		PowerOff: func(context.Context) error {
			calls <- struct{}{}
			return nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return p, calls
}
