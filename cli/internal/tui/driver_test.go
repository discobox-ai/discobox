package tui

import (
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
)

// driver runs a model the way the Bubble Tea runtime does: every command goes
// on its own goroutine and its message comes back through a queue.
//
// The simpler helper in testsupport_test.go runs each command to its end and
// leaves the ones that park, which is fine for a window whose commands all
// return — but a pane's pump parks until the terminal says something, and
// leaving it abandons a goroutine still holding the output channel. The next pump
// then races it for the same bytes, and the output goes to whichever one wins.
// Anything with a live terminal in it is driven from here instead.
//
// A driven test runs inside synctest.Test (newDriver fails one that does not),
// so every time below is the bubble's: it moves only when every goroutine in
// the test is blocked. The window's timers then fire when the test's own work
// says they are due, not when a loaded machine happens to get there, and a
// settle or a wait is over only once whatever it waited for has had its say.
type driver struct {
	t    *testing.T
	m    *Model
	msgs chan tea.Msg
	done chan struct{}
}

func newDriver(t *testing.T, m *Model) *driver {
	t.Helper()
	inBubble(t)
	d := &driver{t: t, m: m, msgs: make(chan tea.Msg, 256), done: make(chan struct{})}
	t.Cleanup(func() {
		// The workspace on screen and every parked one are closed, and every
		// stream closed ends the goroutines reading it: the bubble the test
		// runs in is over only once they are. (Quitting closes only the
		// parked ones; the process exiting takes the rest.)
		m.closeWorkspace()
		m.closeParked()
		close(d.done)
	})
	return d
}

// inBubble fails a test that drives a window outside a synctest bubble.
//
// There is no asking whether a goroutine is in one, so it is told by
// synctest.Wait, which panics outside a bubble — and inside one waits until
// the bubble is idle, harmless before the window has started. The recover
// takes any panic for that one, which is all Wait can raise.
func inBubble(t *testing.T) {
	t.Helper()
	defer func() {
		if recover() != nil {
			t.Fatal("a driven window runs inside synctest.Test: its timers fire on the test's clock, not the machine's")
		}
	}()
	synctest.Wait()
}

// start runs the model's Init, as the runtime does, in a 120x40 window.
func (d *driver) start() { d.startAt(120, 40) }

// startAt runs the model's Init in a window of the given size.
func (d *driver) startAt(width, height int) {
	d.dispatch(tea.WindowSizeMsg{Width: width, Height: height})
	d.run(d.m.Init())
}

// key presses a key and settles.
func (d *driver) key(spec string) {
	d.dispatch(keyPress(spec))
	d.settle()
}

// dispatch applies one message and runs whatever it returns.
func (d *driver) dispatch(msg tea.Msg) {
	d.t.Helper()
	_, cmd := d.m.Update(msg)
	// The runtime draws after every message.
	d.m.View()
	d.run(cmd)
}

// run puts a command on its own goroutine, expanding batches the way the
// runtime does so each half is independently in flight.
func (d *driver) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		if l, ok := asListener(msg); ok {
			// The runtime runs a listener like any command: it is this
			// goroutine's to wait on, until what it waits for comes.
			msg = l.run()
		}
		if msg == nil {
			return
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, sub := range batch {
				d.run(sub)
			}
			return
		}
		if timer, ok := msg.(timerMsg); ok {
			// A timer fires here, as the runtime fires one: when it is due,
			// unless the test is over by then.
			select {
			case at := <-time.After(timer.after):
				msg = timer.fire(at)
			case <-d.done:
				return
			}
		}
		select {
		case d.msgs <- msg:
		case <-d.done:
		}
	}()
}

// settle delivers whatever messages arrive in the next 200ms, so the model
// catches up with the commands in flight. The bubble's clock passes them only
// once every one of those commands has returned or parked.
func (d *driver) settle() {
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case msg := <-d.msgs:
			d.dispatch(msg)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// wait pumps until a condition holds, and fails the test if it never does.
//
// It returns the moment the condition holds. Its bound, commandWait, is the
// bubble's time, which the window's timers share: a wait the window answers
// only after a poll or two still ends, and one it never answers fails as soon
// as the window has nothing left to do for that long.
func (d *driver) wait(what string, cond func() bool) {
	d.t.Helper()
	deadline := time.Now().Add(commandWait)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		select {
		case msg := <-d.msgs:
			d.dispatch(msg)
		case <-time.After(time.Millisecond):
		}
	}
	if !cond() {
		d.t.Fatalf("timed out waiting for %s; frame:\n%s", what, frameText(d.m))
	}
}
