package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"
)

// Saying what a slow operation is actually doing (ADR 0060).
//
// The busy line answers "is this window alive"; it does not answer "what is it
// waiting for", and creating a discobox is the case where the difference shows.
// Behind a cold image pull the wait is minutes, and "creating the discobox…"
// for the whole of it is indistinguishable from a hang.
//
// Two kinds of work report here, and they are the same to the window. Creating
// and pushing a source are this client's own steps, reported by the shared
// creation path as it takes them. Provisioning is the pool agent's, read off
// the discobox record by whoever is waiting on it. Both arrive as lines, and
// the window's only job is to put the newest one where the busy line goes.

// narration is one operation's progress feed.
//
// The channel is buffered and its sends never block: a report is a status line,
// and a window that stopped reading must not be able to stall the work it was
// reporting on.
type narration struct {
	gen int
	ch  chan string
}

// report delivers one line, dropping it when the window is not keeping up. The
// send never blocks: a status line must not be able to stall the work it
// describes.
func (n narration) report(text string) {
	select {
	case n.ch <- text:
	default:
	}
}

// close ends the feed, which is what releases the command waiting on it. The
// operation that opened the feed closes it when it returns, so the goroutine
// reading it lasts exactly as long as the work does.
func (n narration) close() { close(n.ch) }

// narrationMsg is one line from a narrated operation.
type narrationMsg struct {
	source narration
	text   string
}

// narrationCapacity is how many reports may be in flight. Two would do — the
// producers report one step at a time — but a pull reports twice a second and
// the window may be mid-render, so there is room for a short burst before any
// is dropped.
const narrationCapacity = 8

// narrate opens a feed for an operation about to start, returning the sink it
// reports on and the command that delivers the first line.
//
// Opening one ends whatever was reporting before it. Only one operation owns
// the busy line, so a report from an earlier one is stale by definition.
func (m *Model) narrate() (narration, tea.Cmd) {
	m.endNarration()
	feed := narration{gen: m.busyGen, ch: make(chan string, narrationCapacity)}
	return feed, m.nextNarration(feed)
}

// nextNarration waits for the feed's next line. It re-arms itself, so one call
// follows an operation from its first step to its last.
func (m *Model) nextNarration(feed narration) tea.Cmd {
	return func() tea.Msg {
		text, ok := <-feed.ch
		if !ok {
			// The operation finished and closed its feed. Nothing to report and
			// nothing to re-arm, which is also what stops this goroutine.
			return nil
		}
		return narrationMsg{source: feed, text: text}
	}
}

// narrated reports one line, and asks for the next.
func (m *Model) narrated(msg narrationMsg) tea.Cmd {
	if msg.source.gen != m.busyGen {
		// From an operation the window has moved on from. Dropping it is the
		// point of the generation: the line belongs to whatever is running now.
		return nil
	}
	m.busy = msg.text + "…"
	// The waiting dialog is the same report, larger: while one is up it is the
	// only thing on screen, so it says what the busy line would have.
	if m.dialog != nil && m.dialog.kind == dlgStatus {
		m.dialog.body = msg.text
	}
	return m.nextNarration(msg.source)
}

// endNarration takes the busy line back.
//
// Bumping the generation is what makes it final: a report already on its way
// from the operation that just ended is dropped rather than landing on top of
// whatever the window says next.
func (m *Model) endNarration() {
	m.busyGen++
	if m.stopNarrating != nil {
		m.stopNarrating()
		m.stopNarrating = nil
	}
}

// watchProvisioning narrates what the discobox being attached to is being made
// to do, until it has nothing left to report or the window stops caring.
//
// It runs beside the attach rather than in front of it: the attach waits for
// readiness on its own (ADR 0039) and this only says what that wait is for, so
// a failure here costs a status line and never a terminal.
func (m *Model) watchProvisioning(sandboxID string) tea.Cmd {
	feed, next := m.narrate()
	ctx, stop := context.WithCancel(m.ctx)
	m.stopNarrating = stop
	watch := func() tea.Msg {
		defer feed.close()
		m.ds.WatchProvisioning(ctx, sandboxID, feed.report)
		// The watch returns when the discobox has nothing left to report,
		// which is the same moment the attach it runs beside can finish. That
		// is the signal the waiting dialog takes itself down on.
		return provisioningDoneMsg{sandboxID: sandboxID}
	}
	return tea.Batch(watch, next)
}

// provisioningDoneMsg says a discobox has nothing left to report, so anything
// showing that report can stop.
type provisioningDoneMsg struct{ sandboxID string }
