package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// driver runs a model the way the Bubble Tea runtime does: every command goes
// on its own goroutine and its message comes back through a queue.
//
// The simpler helper in testsupport_test.go runs commands inline and gives up
// on the ones that block, which is fine for a window whose commands all return
// — but a pane's pump blocks until the terminal says something, and giving up
// on it abandons a goroutine still holding the output channel. The next pump
// then races it for the same bytes, and the output goes to whichever one wins.
// Anything with a live terminal in it is driven from here instead.
type driver struct {
	t    *testing.T
	m    *Model
	msgs chan tea.Msg
	done chan struct{}
}

func newDriver(t *testing.T, m *Model) *driver {
	d := &driver{t: t, m: m, msgs: make(chan tea.Msg, 256), done: make(chan struct{})}
	t.Cleanup(func() { close(d.done) })
	return d
}

// start runs the model's Init, as the runtime does.
func (d *driver) start() {
	d.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
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
	// The runtime draws after every message, and the window reads back what it
	// drew: a frame with anything on it, drawn inline, is the prompt printed on
	// the screen the window was started from. See clearPrinted.
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
		if msg == nil {
			return
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, sub := range batch {
				d.run(sub)
			}
			return
		}
		select {
		case d.msgs <- msg:
		case <-d.done:
		}
	}()
}

// settle delivers whatever messages have arrived, briefly, so the model catches
// up with the commands in flight.
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
func (d *driver) wait(what string, cond func() bool) {
	d.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
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
