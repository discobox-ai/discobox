package monitors

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// A watcher error — an inotify queue overflow here — must not stall the
// watch: fsnotify sends events and errors from one reader, so an error left
// unread holds every later event back, and the wait wakes only on its ticker.
//
// Nothing here depends on how fast the runner is (#1798). The wait is held
// still while the queue overflows, so the overflow is certain; the test then
// lets it through each look it takes at the queue, so it knows the wait has
// read up to the overflow; and it adds the event just after a tick, so a wait
// that left the error unread misses it by a whole tick.
func TestWaitWakesPromptlyAfterAWatchError(t *testing.T) {
	limit, err := os.ReadFile("/proc/sys/fs/inotify/max_queued_events")
	if err != nil {
		t.Skip("needs inotify")
	}
	queued, err := strconv.Atoi(strings.TrimSpace(string(limit)))
	if err != nil {
		t.Fatal(err)
	}
	q := testQueue(t)
	g := newLookGate(t, q)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- q.Wait(ctx, Filter{Instance: "b"}) }()
	t.Cleanup(func() {
		g.close()
		cancel()
	})

	// The wait's first look reaches the gate once its watch is in place; let
	// it past the first FIFO, and it is held at the second.
	g.pass(0)
	// While it is held nothing takes the watch's events: fsnotify's reader
	// reads one buffer of them and blocks handing over the first, and the
	// kernel's queue fills and overflows. Each file is two events, its create
	// and its remove; files for the queue and that buffer (at most 2048
	// events of these names) twice over.
	for i := range queued + 2048 {
		name := filepath.Join(q.Pending, "noise-"+strconv.Itoa(i))
		if err := os.WriteFile(name, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(name); err != nil {
			t.Fatal(err)
		}
	}
	g.serve()

	// The wait looks again for each event it reads, and there are at least
	// the kernel's whole queue of them before the overflow. Once those are
	// through and a second passes without a look, it has stopped reading
	// events and looks only on its ticker.
	bound := time.Now().Add(2 * time.Minute)
	for g.looks.Load() < int64(queued) || time.Since(time.Unix(0, g.last.Load())) < time.Second {
		if time.Now().After(bound) {
			t.Fatalf("the wait took %d looks in 2m; want %d and then a quiet second", g.looks.Load(), queued)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Add the event just after the next tick's look, a whole tick before the
	// one after it. The gate goes first: it serves one reader at a time, and
	// the add reads the queue too.
	ticked := g.looks.Load()
	for g.looks.Load() == ticked {
		if time.Now().After(bound) {
			t.Fatal("the wait stopped looking on its ticker")
		}
		time.Sleep(time.Millisecond)
	}
	g.close()
	start := time.Now()
	if _, err := q.Add(Event{Instance: "b", Title: "this one"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("wait never returned")
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("wait took %v to see an event after a watch error, want < 1s: it woke only on its ticker", took)
	}
}

// lookGate holds a queue's readers at two FIFOs named as its first events,
// so each look at the queue — which reads every event file in id order —
// waits for the test to let it through. Opening a FIFO's read end blocks until
// its write end is opened; the gate opens and closes that, and the reader
// reads an empty file, which the queue skips as unreadable.
//
// Two, taken in turn, because a writer cannot tell when a reader has closed:
// a reader still holding the one it was let through would let the gate
// through again. Every look opens the first before the second, so serving
// them alternately is one serve per look — for one reader at a time: a second
// reader reaching a FIFO just after the first was let through waits there
// until a look arrives at the other.
type lookGate struct {
	t     *testing.T
	fifos [2]string
	// looks counts the looks let through, and last is when the latest was,
	// in Unix nanoseconds.
	looks   atomic.Int64
	last    atomic.Int64
	stopped atomic.Bool
	served  chan struct{}
}

func newLookGate(t *testing.T, q *Queue) *lookGate {
	if err := os.MkdirAll(q.Pending, 0o755); err != nil {
		t.Fatal(err)
	}
	g := &lookGate{t: t}
	for i := range g.fifos {
		g.fifos[i] = q.path(q.Pending, "e"+strconv.Itoa(i+1))
		if err := syscall.Mkfifo(g.fifos[i], 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

// pass lets one reader through FIFO i, waiting for it to arrive.
func (g *lookGate) pass(i int) {
	f, err := os.OpenFile(g.fifos[i], os.O_WRONLY, 0)
	if err != nil {
		if !g.stopped.Load() {
			g.t.Error(err)
		}
		return
	}
	_ = f.Close()
}

// serve lets every look through from now on, the held one first.
func (g *lookGate) serve() {
	g.served = make(chan struct{})
	go func() {
		defer close(g.served)
		for !g.stopped.Load() {
			g.pass(1)
			g.looks.Add(1)
			g.last.Store(time.Now().UnixNano())
			if g.stopped.Load() {
				return
			}
			g.pass(0)
		}
	}()
}

// close lets the gate's waiters on both sides go and removes the FIFOs, so
// no later look is held; closing it again does nothing. Opening a FIFO for
// both reading and writing does not block, and stands for the missing end of
// whoever is waiting at it.
func (g *lookGate) close() {
	g.stopped.Store(true)
	var held []*os.File
	for _, name := range g.fifos {
		if f, err := os.OpenFile(name, os.O_RDWR, 0); err == nil {
			held = append(held, f)
		}
		_ = os.Remove(name)
	}
	if g.served != nil {
		<-g.served
	}
	for _, f := range held {
		_ = f.Close()
	}
}
