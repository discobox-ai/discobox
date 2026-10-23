package tui

import (
	"context"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/discobox-ai/discobox/termpane"
)

// A pull request's files changed are discobox-review, run in the discobox the
// tab is on (DataSource.PullReview): from the pull request's base to its head
// (BaseSHA, HeadSHA), read from the commits the discobox has, under the review
// its git directory keeps. That is the review the agent that made the change
// wrote while it worked — its card, its reviewers' approvals and comments —
// and everything the window does over a working tree it does here, but check
// out a branch or edit a file: a commit has neither.
//
// It is drawn in the tab, in place of the conversation, from a terminal of its
// own. Every key but the leader is the window's, so quitting it (q) is the way
// back; while it is still starting, q or Esc is. The session ends with the
// window, the tab, the workspace and the console, which waits for those ends
// on its way out (EndReviews). Each f starts one over the head the tab last
// read, so a push the tab reads never moves the one on screen.
//
// A session that ends before anything was typed at it ended on its own — an
// error, an image whose discobox-review is too old — and its last screen
// stays up to be read until a key takes the tab back.

// reviewWindow is the files changed of one pull request at one head.
type reviewWindow struct {
	// term draws the session; stream is nil until it has started.
	term   *termpane.Model
	stream Terminal
	// head is the commit it was opened over.
	head string
	// width and height are what it was last told it has, so a frame that
	// changes neither does not resize it; x and y are where its grid was
	// last drawn, which is what the pointer is read against.
	width, height int
	x, y          int
	// box and execID are the session's, once it has started, and ds what
	// ends it, within ctx; cancel ends a start still under way.
	box, execID string
	ds          DataSource
	ctx         context.Context
	cancel      context.CancelFunc
	// ends counts the ends still on their way to the discobox, which the
	// console waits for before it exits.
	ends *sync.WaitGroup
	// typed is whether anything has been sent to the session; exited that
	// it ended without having been, and its last screen is held.
	typed, exited bool
}

// reviewStartTimeout is how long a session is given to start: one in a
// discobox that is waking can take a while, and one that never comes must
// not leave the tab waiting with no way to say so.
const reviewStartTimeout = 2 * time.Minute

// reviewStartTimedOutMsg is that time up, for the window that asked.
type reviewStartTimedOutMsg struct{}

// reviewMsg is a message the window's session sent, back to the window that
// sent it: one from a window since replaced is dropped.
type reviewMsg struct {
	pane   int
	window *reviewWindow
	msg    tea.Msg
}

// reviewStartedMsg is the session a window asked for, started or refused.
type reviewStartedMsg struct {
	exec   Exec
	stream Terminal
	err    error
}

// fromReview tags a window's commands so that what they send comes back to it.
func fromReview(pane int, w *reviewWindow, cmd tea.Cmd) tea.Cmd {
	return tagCmd(cmd, func(msg tea.Msg) tea.Msg { return reviewMsg{pane: pane, window: w, msg: msg} })
}

// openReview turns the tab to its files changed, starting a session over the
// head it last read.
func (m *Model) openReview(p *pane) tea.Cmd {
	v := p.issue
	pull := v.issue.Pull
	v.files = true
	v.closeReview()
	ctx, cancel := context.WithCancel(m.ctx)
	cols, rows := m.paneCells(m.paneWidthOf(p))
	ds, box := m.ds, p.sandbox.ID
	w := &reviewWindow{
		// The leader never reaches it — the tab takes it first — so it has
		// no prefix of its own: every other key is discobox-review's.
		term: termpane.New(termpane.WithListen(listen), termpane.WithKeys(paneKeymap)),
		head: pull.HeadSHA,
		box:  box, ds: ds, ctx: m.ctx, cancel: cancel, ends: &m.reviewEnds,
	}
	w.term.SetSize(cols, rows)
	v.review = w
	// The start is counted with the ends: a console quitting while it is
	// under way waits for it, and one that lands for a window already closed
	// is ended here, where nothing is left to hand it to.
	m.reviewEnds.Add(1)
	start := *pull
	return fromReview(p.id, w, tea.Batch(
		func() tea.Msg {
			defer m.reviewEnds.Done()
			exec, stream, err := ds.PullReview(ctx, box, start, cols, rows)
			if err == nil && ctx.Err() != nil {
				_ = stream.Close()
				end, cancel := context.WithTimeout(context.WithoutCancel(ctx), issueTimeout)
				defer cancel()
				_ = ds.EndExec(end, box, exec.ID)
				return nil
			}
			return reviewStartedMsg{exec: exec, stream: stream, err: err}
		},
		tick(reviewStartTimeout, func(time.Time) tea.Msg { return reviewStartTimedOutMsg{} }),
	))
}

// closeReview ends the window, if there is one.
func (v *issuePane) closeReview() {
	if v.review != nil {
		v.review.end()
		v.review = nil
	}
}

// end ends the window's session. It is the window's own, which nothing else
// draws: closing the view is ending it, or the next listing would find it
// still running.
func (w *reviewWindow) end() {
	w.cancel()
	if w.stream != nil {
		_ = w.stream.Close()
	}
	w.endExec(w.execID)
	w.execID = ""
}

// endExec ends one session of the window's in the discobox, in the
// background: the window has already let go of it. It is counted until it is
// done, so a console on its way out waits for it rather than leaving the
// session running where nothing will ever list it.
func (w *reviewWindow) endExec(execID string) {
	if execID == "" {
		return
	}
	w.ends.Add(1)
	ds, box := w.ds, w.box
	// Not the window's context: that is the console's, which is canceled on
	// the way out this end is meant to outlast.
	go func() {
		defer w.ends.Done()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(w.ctx), issueTimeout)
		defer cancel()
		_ = ds.EndExec(ctx, box, execID)
	}()
}

// EndReviews ends every files changed still open and waits, for at most
// wait, for those ends and any already on their way to reach the discobox.
// The console calls it once its window has closed: quitting leaves every
// other session running, but these are its own.
func (m *Model) EndReviews(wait time.Duration) {
	for _, p := range m.allPanes() {
		if p.issue != nil {
			p.issue.closeReview()
		}
	}
	done := make(chan struct{})
	go func() {
		m.reviewEnds.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(wait):
	}
}

// updateReview hands the window what its session sent, and turns the tab back
// to the conversation when the session is over.
func (m *Model) updateReview(msg reviewMsg) tea.Cmd {
	p := m.paneByID(msg.pane)
	if p == nil || p.issue == nil || p.issue.review != msg.window {
		if started, ok := msg.msg.(reviewStartedMsg); ok && started.err == nil {
			// Started for a window since replaced or closed.
			_ = started.stream.Close()
			msg.window.endExec(started.exec.ID)
		}
		return nil
	}
	v, w := p.issue, msg.window
	switch msg := msg.msg.(type) {
	case reviewStartedMsg:
		if msg.err != nil {
			v.files, v.review = false, nil
			return m.report(true, "cannot show the files changed: %v", msg.err)
		}
		w.stream, w.execID = msg.stream, msg.exec.ID
		return fromReview(p.id, w, w.term.Attach(msg.stream))
	case reviewStartTimedOutMsg:
		if w.stream != nil {
			return nil
		}
		v.files = false
		v.closeReview()
		return m.report(true, "discobox-review did not start in the discobox within %s", reviewStartTimeout)
	case termpane.ClosedMsg:
		w.end()
		switch {
		case msg.Err != nil:
			// A stream that broke is the console's to report.
			v.files, v.review = false, nil
			return m.report(true, "files changed: %v", msg.Err)
		case !w.typed:
			// It ended on its own, and what it said is on its last screen.
			w.exited = true
			return status("discobox-review ended — any key goes back to the conversation")
		}
		// Quitting discobox-review is how you say you are done with it, as
		// it is for the diff tool.
		v.files, v.review = false, nil
		return nil
	case termpane.CopyMsg:
		return m.copyText(msg.Text, "copied")
	}
	return m.toReview(p, msg.msg)
}

// reviewKey is a key on the files changed. While the session is starting, or
// once it has ended on its own, there is nothing to send it to, and the keys
// that go back to the conversation are the tab's; every other key, then, is
// the session's.
func (m *Model) reviewKey(p *pane, msg tea.KeyPressMsg) tea.Cmd {
	v, w := p.issue, p.issue.review
	switch {
	case w.exited, w.stream == nil && (keyName(msg) == "q" || keyName(msg) == "esc"):
		v.files = false
		v.closeReview()
		return nil
	case w.stream == nil:
		return nil
	}
	return m.toReview(p, msg)
}

// toReview is one message for the tab's window: a key, a paste, or what its
// session sent. A key or a paste is the person at it, as a press is
// (reviewMouse).
func (m *Model) toReview(p *pane, msg tea.Msg) tea.Cmd {
	w := p.issue.review
	switch msg.(type) {
	case tea.KeyPressMsg, tea.PasteMsg:
		w.typed = true
	}
	term, cmd := w.term.Update(msg)
	w.term = term
	return fromReview(p.id, w, cmd)
}

// reviewMouse is the pointer over the window, read against where its grid was
// last drawn. Its gestures are held as a terminal pane's are, so a drag let go
// outside the tab still reaches it, and a selection it starts is the only one
// on screen.
func (m *Model) reviewMouse(p *pane, msg tea.MouseMsg) tea.Cmd {
	w := p.issue.review
	if w.stream == nil || w.exited {
		return nil
	}
	switch ev := msg.(type) {
	case tea.MouseClickMsg:
		w.typed = true
		if ev.Button == tea.MouseLeft {
			m.mouseCapture = p.id
			m.focusPane(p)
			m.chromeSel.Clear()
			m.clearPaneSelections(p)
		}
	case tea.MouseReleaseMsg:
		if ev.Button == tea.MouseLeft {
			m.mouseCapture = 0
		}
	}
	w.term.SetSeized(m.mouseSeized)
	return fromReview(p.id, w, w.term.HandleMouse(translateMouse(msg, w.x, w.y, w.width)))
}

// reviewView is the window drawn at width by height, from the tab's origin x,
// y on screen.
func (v *issuePane) reviewView(m *Model, width, height, x, y int) []string {
	w := v.review
	if w.width != width || w.height != height {
		w.term.SetSize(width, height)
		w.width, w.height = width, height
	}
	w.x, w.y = x, y
	if w.stream == nil {
		return fitRows([]string{m.st.dimText.Render(truncate("starting discobox-review in the discobox …", width))}, height)
	}
	return fitRows(w.term.View(), height)
}

// reviewCursor is where the window puts the cursor, when it shows one: in a
// comment being written.
func (v *issuePane) reviewCursor() *tea.Cursor {
	w := v.review
	if w == nil || w.stream == nil {
		return nil
	}
	return w.term.Cursor(w.x, w.y)
}
