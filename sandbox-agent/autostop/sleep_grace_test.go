package autostop

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/programstatus"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

func TestSleepWaitsForGraceAndPrimaryWorking(t *testing.T) {
	progress := 50
	for _, tc := range []struct {
		name     string
		age      time.Duration
		status   *programstatus.Status
		readErr  error
		eligible bool
	}{
		{"minimum", 15*time.Second - time.Nanosecond, nil, nil, false},
		{"absent", 15 * time.Second, nil, nil, true},
		{"working", 15 * time.Second, &programstatus.Status{State: programstatus.Working}, nil, false},
		{"before ceiling", 2*time.Minute - time.Nanosecond, &programstatus.Status{State: programstatus.Working}, nil, false},
		{"ceiling", 2 * time.Minute, &programstatus.Status{State: programstatus.Working}, nil, true},
		{"idle", 15 * time.Second, &programstatus.Status{State: programstatus.Idle}, nil, true},
		{"progress alone", 15 * time.Second, &programstatus.Status{State: programstatus.Idle, Progress: &progress}, nil, true},
		{"blocked", 15 * time.Second, &programstatus.Status{State: programstatus.Blocked}, nil, true},
		{"done", 15 * time.Second, &programstatus.Status{State: programstatus.Done}, nil, true},
		{"error", 15 * time.Second, &programstatus.Status{State: programstatus.Error}, nil, true},
		{"read failure", 15 * time.Second, nil, errors.New("shim unavailable"), false},
		{"read failure ceiling", 2 * time.Minute, nil, errors.New("shim unavailable"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &clock{now: epoch}
			p := newPolicy(t, c, nil)
			p.primaryStatus = func() (*programstatus.Status, error) { return tc.status, tc.readErr }
			writeRequest(t, p.sleepDir, "agent", epoch)
			c.Set(epoch.Add(tc.age))
			state := p.Evaluate(c.Now())
			if !state.Sleep.IsZero() != tc.eligible {
				t.Fatalf("sleep=%+v eligible=%v", state.Sleep, tc.eligible)
			}
			if _, err := p.Confirm(ReasonIdle); !errors.Is(err, ErrNotConfirmed) {
				t.Fatalf("pending sleep allowed idle: %v", err)
			}
			_, err := p.Confirm(ReasonSleep)
			if (err == nil) != tc.eligible {
				t.Fatalf("confirm=%v eligible=%v", err, tc.eligible)
			}
			if _, err := os.Stat(filepath.Join(p.sleepDir, "agent")); os.IsNotExist(err) != tc.eligible {
				t.Fatalf("request consumption: %v", err)
			}
		})
	}
}

func TestSleepRechecksRequestsAndPrimary(t *testing.T) {
	c := &clock{now: epoch}
	p := newPolicy(t, c, nil)
	var working bool
	p.primaryStatus = func() (*programstatus.Status, error) {
		if working {
			return &programstatus.Status{State: programstatus.Working}, nil
		}
		return nil, nil
	}
	writeRequest(t, p.sleepDir, "old", epoch)
	c.Set(epoch.Add(20 * time.Second))
	if p.Evaluate(c.Now()).Sleep.IsZero() {
		t.Fatal("sleep not eligible")
	}
	working = true
	if _, err := p.Confirm(ReasonSleep); !errors.Is(err, ErrNotConfirmed) {
		t.Fatalf("working resumed: %v", err)
	}
	working = false
	writeRequest(t, p.sleepDir, "new", c.Now())
	if _, err := p.Confirm(ReasonSleep); !errors.Is(err, ErrNotConfirmed) {
		t.Fatalf("new request: %v", err)
	}
	c.Set(epoch.Add(36 * time.Second))
	if _, err := p.Confirm(ReasonSleep); err != nil {
		t.Fatal(err)
	}
	if len(ReadRequests(p.sleepDir)) != 0 {
		t.Fatal("did not consume batch")
	}
}

func TestSleepRefreshCancellationAndRestart(t *testing.T) {
	c := &clock{now: epoch}
	p := newPolicy(t, c, nil)
	read := func() (*programstatus.Status, error) { return &programstatus.Status{State: programstatus.Working}, nil }
	p.primaryStatus = read
	path := writeRequest(t, p.sleepDir, "agent", epoch)
	c.Set(epoch.Add(30 * time.Second))
	restarted := New(Config{Execs: p.execs, PrimaryStatus: read, SleepDir: p.sleepDir, LeaseDir: p.leaseDir, WakeDir: p.wakeDir, ControlPlaneMarker: p.controlPlaneMarker, HandoverMarker: p.handoverMarker, Now: c.Now})
	if !restarted.Evaluate(c.Now()).Sleep.IsZero() {
		t.Fatal("restart bypassed working")
	}
	c.Set(epoch.Add(2 * time.Minute))
	if restarted.Evaluate(c.Now()).Sleep.IsZero() {
		t.Fatal("ceiling not honored after restart")
	}
	writeRequest(t, p.sleepDir, "agent", c.Now())
	if !restarted.Evaluate(c.Now()).Sleep.IsZero() {
		t.Fatal("refresh did not restart grace")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	restarted.SetIdleTimeout(time.Second)
	c.Set(c.Now().Add(2 * time.Second))
	if !restarted.Evaluate(c.Now()).Idle {
		t.Fatal("cancellation did not restore idle policy")
	}
}

func TestLocalSleepDoesNotBypassGraceThroughIdle(t *testing.T) {
	c := &clock{now: epoch}
	p, calls, _ := runPolicy(t, c, nil, func() error { return nil })
	p.SetIdleTimeout(time.Second)
	writeRequest(t, p.sleepDir, "agent", epoch)
	c.Set(epoch.Add(2 * time.Second))
	select {
	case <-calls:
		t.Fatal("idle bypassed sleep grace")
	case <-time.After(20 * time.Millisecond):
	}
	c.Set(epoch.Add(SleepGrace))
	select {
	case <-calls:
	case <-time.After(time.Second):
		t.Fatal("eligible local sleep did not power off")
	}
}

func TestSecondaryWorkingDoesNotDeferSleep(t *testing.T) {
	c := &clock{now: epoch}
	p := newPolicy(t, c, func() []execs.Exec {
		return []execs.Exec{{ID: "secondary", ProgramStatus: &programstatus.Status{State: programstatus.Working}}}
	})
	writeRequest(t, p.sleepDir, "agent", epoch)
	c.Set(epoch.Add(SleepGrace))
	if _, err := p.Confirm(ReasonSleep); err != nil {
		t.Fatalf("secondary deferred sleep: %v", err)
	}
}
