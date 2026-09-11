package autostop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

// clock is a settable Now for a Policy.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Set(now time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

var epoch = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

func newPolicy(t *testing.T, c *clock, list func() []execs.Exec) *Policy {
	t.Helper()
	return New(Config{
		Execs:       list,
		LeaseDir:    filepath.Join(t.TempDir(), "keepalive"),
		IdleTimeout: 30 * time.Minute,
		Now:         c.Now,
	})
}

func at(d time.Duration) *time.Time {
	t := epoch.Add(d)
	return &t
}

// A sandbox nothing has happened in is idle from the moment the agent started,
// so a fresh or just-auto-started sandbox gets a whole window.
func TestEvaluateCountsFromAgentStart(t *testing.T) {
	c := &clock{now: epoch}
	p := newPolicy(t, c, nil)
	state := p.Evaluate(epoch.Add(time.Minute))
	if !state.LastActivityAt.Equal(epoch) || state.LastActivity != "sandbox agent start" {
		t.Fatalf("last activity = %v %q, want the agent's start", state.LastActivityAt, state.LastActivity)
	}
	if want := epoch.Add(30 * time.Minute); !state.StopsAt.Equal(want) {
		t.Fatalf("stops at = %v, want %v", state.StopsAt, want)
	}
}

func TestEvaluateReadsTitleChangesAndClients(t *testing.T) {
	c := &clock{now: epoch}
	execList := []execs.Exec{
		{ID: "claude", TitleChangedAt: at(10 * time.Minute)},
		{ID: "shell", LastAccessedAt: at(5 * time.Minute)},
		{ID: "ended"},
	}
	p := newPolicy(t, c, func() []execs.Exec { return execList })
	state := p.Evaluate(epoch.Add(20 * time.Minute))
	if !state.LastActivityAt.Equal(*at(10 * time.Minute)) || state.LastActivity != "title change on exec claude" {
		t.Fatalf("last activity = %v %q, want the title change", state.LastActivityAt, state.LastActivity)
	}

	// An attached client is activity now, however long ago it last typed.
	execList[1].AttacherCount = 1
	now := epoch.Add(3 * time.Hour)
	state = p.Evaluate(now)
	if !state.LastActivityAt.Equal(now) || state.LastActivity != "client attached to exec shell" {
		t.Fatalf("last activity = %v %q, want the attached client now", state.LastActivityAt, state.LastActivity)
	}
}

// A connection holds the sandbox while it is open, and closing it starts the
// clock.
func TestHoldIsActivityUntilReleased(t *testing.T) {
	c := &clock{now: epoch}
	p := newPolicy(t, c, nil)
	release := p.Hold("tcp tunnel to localhost:5173")
	now := epoch.Add(2 * time.Hour)
	if state := p.Evaluate(now); !state.LastActivityAt.Equal(now) || state.LastActivity != "client connected: tcp tunnel to localhost:5173" {
		t.Fatalf("last activity = %v %q, want now while the tunnel is open", state.LastActivityAt, state.LastActivity)
	}
	c.Set(now)
	release()
	release() // a second release changes nothing
	later := now.Add(time.Hour)
	state := p.Evaluate(later)
	if !state.LastActivityAt.Equal(now) || state.LastActivity != "client left: tcp tunnel to localhost:5173" {
		t.Fatalf("last activity = %v %q, want the tunnel's close", state.LastActivityAt, state.LastActivity)
	}
}

// An exec that ends takes its shim's record of access with it. A client that
// was on it a moment ago must still count until the timeout runs from then —
// found end to end, where a sandbox stopped 49 seconds after a client left
// because the command it had attached to finished in between.
func TestActivityOutlivesTheExecThatReportedIt(t *testing.T) {
	c := &clock{now: epoch}
	execList := []execs.Exec{{ID: "make-test", LastAccessedAt: at(40 * time.Minute)}}
	p := newPolicy(t, c, func() []execs.Exec { return execList })
	if state := p.Evaluate(epoch.Add(41 * time.Minute)); !state.LastActivityAt.Equal(*at(40 * time.Minute)) {
		t.Fatalf("last activity = %v, want the client's access", state.LastActivityAt)
	}
	execList = nil // the command finished and its record went with it
	state := p.Evaluate(epoch.Add(42 * time.Minute))
	if !state.LastActivityAt.Equal(*at(40 * time.Minute)) || state.LastActivity != "client on exec make-test" {
		t.Fatalf("last activity = %v %q after the exec ended, want the access it reported", state.LastActivityAt, state.LastActivity)
	}
}

// Leases are the one activity never remembered: removing one releases it.
func TestRemovingALeaseReleasesIt(t *testing.T) {
	c := &clock{now: epoch}
	p := newPolicy(t, c, nil)
	if err := p.prepareLeaseDir(); err != nil {
		t.Fatal(err)
	}
	lease := filepath.Join(p.leaseDir, "build")
	if err := os.WriteFile(lease, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	until := epoch.Add(3 * time.Hour)
	if err := os.Chtimes(lease, until, until); err != nil {
		t.Fatal(err)
	}
	if state := p.Evaluate(epoch.Add(time.Minute)); !state.LastActivityAt.Equal(until) {
		t.Fatalf("last activity = %v, want the lease", state.LastActivityAt)
	}
	if err := os.Remove(lease); err != nil {
		t.Fatal(err)
	}
	if state := p.Evaluate(epoch.Add(2 * time.Minute)); !state.LastActivityAt.Equal(epoch) {
		t.Fatalf("last activity = %v after the lease was removed, want the agent's start", state.LastActivityAt)
	}
}

func TestLeaseMtimeIsActivityIncludingTheFuture(t *testing.T) {
	c := &clock{now: epoch}
	p := newPolicy(t, c, nil)
	if err := p.prepareLeaseDir(); err != nil {
		t.Fatal(err)
	}
	build := filepath.Join(p.leaseDir, "build")
	if err := os.WriteFile(build, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	until := epoch.Add(2 * time.Hour)
	if err := os.Chtimes(build, until, until); err != nil {
		t.Fatal(err)
	}
	// A symlink is not a lease, however new the file it points at.
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	far := epoch.Add(100 * time.Hour)
	if err := os.Chtimes(target, far, far); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(p.leaseDir, "link")); err != nil {
		t.Fatal(err)
	}

	state := p.Evaluate(epoch.Add(time.Minute))
	if !state.LeaseUntil.Equal(until) {
		t.Fatalf("lease until = %v, want %v", state.LeaseUntil, until)
	}
	if !state.LastActivityAt.Equal(until) || state.LastActivity != "lease "+build {
		t.Fatalf("last activity = %v %q, want the lease", state.LastActivityAt, state.LastActivity)
	}
	if want := until.Add(30 * time.Minute); !state.StopsAt.Equal(want) {
		t.Fatalf("stops at = %v, want %v", state.StopsAt, want)
	}

	// Once the lease's time has passed it is ordinary past activity.
	if state := p.Evaluate(until.Add(time.Minute)); !state.LeaseUntil.IsZero() {
		t.Fatalf("lease until = %v after it passed, want zero", state.LeaseUntil)
	}
}

func TestPrepareLeaseDirIsStickyAndWorldWritable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the sticky bit is POSIX-only; the lease dir lives in the Linux guest")
	}
	p := newPolicy(t, &clock{now: epoch}, nil)
	if err := p.prepareLeaseDir(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p.leaseDir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode()&(os.ModePerm|os.ModeSticky), os.ModeSticky|0o777; got != want {
		t.Fatalf("lease dir mode = %v, want %v", got, want)
	}
}

// runPolicy runs p with a fast ticker and reports each power-off attempt.
func runPolicy(t *testing.T, c *clock, list func() []execs.Exec, powerOff func() error) (*Policy, <-chan struct{}, context.CancelFunc) {
	t.Helper()
	calls := make(chan struct{}, 16)
	p := New(Config{
		Execs:       list,
		LeaseDir:    filepath.Join(t.TempDir(), "keepalive"),
		IdleTimeout: 30 * time.Minute,
		Interval:    time.Millisecond,
		Now:         c.Now,
		PowerOff: func(context.Context) error {
			calls <- struct{}{}
			return powerOff()
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
	_ = done
	return p, calls, cancel
}

func TestRunPowersOffOnceIdle(t *testing.T) {
	c := &clock{now: epoch}
	_, calls, _ := runPolicy(t, c, nil, func() error { return nil })
	select {
	case <-calls:
		t.Fatal("powered off before the idle timeout")
	case <-time.After(20 * time.Millisecond):
	}
	c.Set(epoch.Add(30 * time.Minute))
	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("never powered off once idle")
	}
	select {
	case <-calls:
		t.Fatal("powered off again after an accepted power-off")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestRunNeverPowersOffWhileAClientIsAttached(t *testing.T) {
	c := &clock{now: epoch}
	attached := []execs.Exec{{ID: "claude", AttacherCount: 1}}
	_, calls, _ := runPolicy(t, c, func() []execs.Exec { return attached }, func() error { return nil })
	c.Set(epoch.Add(24 * time.Hour))
	select {
	case <-calls:
		t.Fatal("powered off with a client attached")
	case <-time.After(30 * time.Millisecond):
	}
}

func TestRunRetriesAFailedPowerOff(t *testing.T) {
	c := &clock{now: epoch}
	var mu sync.Mutex
	attempts := 0
	_, calls, _ := runPolicy(t, c, nil, func() error {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts == 1 {
			return errors.New("dbus unavailable")
		}
		return nil
	})
	c.Set(epoch.Add(time.Hour))
	for range 2 {
		select {
		case <-calls:
		case <-time.After(2 * time.Second):
			t.Fatal("power-off was not retried")
		}
	}
}

// Status speaks only for a policy that is in force.
func TestStatusOnlyWhileRunning(t *testing.T) {
	c := &clock{now: epoch}
	p := newPolicy(t, c, nil)
	if _, ok := p.Status(); ok {
		t.Fatal("status reported before Run")
	}
	var nilPolicy *Policy
	if _, ok := nilPolicy.Status(); ok {
		t.Fatal("nil policy reported status")
	}
	nilPolicy.Hold("nothing")()

	running, _, cancel := runPolicy(t, c, nil, func() error { return nil })
	deadline := time.Now().Add(2 * time.Second)
	for {
		if state, ok := running.Status(); ok {
			if !state.StopsAt.Equal(epoch.Add(30 * time.Minute)) {
				t.Fatalf("stops at = %v", state.StopsAt)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("status never reported while running")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
}
