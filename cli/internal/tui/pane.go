package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/obot-platform/discobox/cli/internal/keys"
	"github.com/obot-platform/discobox/termpane"
)

// A pane is a discobox's terminal drawn in the window rather than by handing
// the real terminal over.
//
// The screen is two spots side by side, one for each of the discobox's
// terminals: the harness you attached to, and a shell to do something while it
// works. Either may be empty, and an empty one is opened where it stands rather
// than by leaving for the list — so there is never a second shell instead of
// the attach you wanted, and the two spots are always the same two spots.
//
// Over them, a command that runs and finishes — diff, status, apply — takes the
// whole screen for as long as it runs. The terminals underneath are untouched:
// still connected, still running, still where they were when it comes back.
//
// Every command the list offers is reachable from here behind the leader, on
// the key it has there, and acts on the one discobox the screen is showing.

const (
	paneMouseKey = "m"
	paneSwapKey  = "e"
	paneLeftKey  = "h"
	paneRightKey = "l"
	// paneDetachAlt is the detach key behind the leader, for a pane whose
	// application needs Ctrl-C more than the window does. It is q rather than
	// the d screen, tmux, and a plain `disco attach` all use, because the leader
	// here also carries the list's own keys and d among them is diff.
	paneDetachAlt = "q"
	// paneInterruptKey is the application's everywhere, and never the window's.
	// The one exception is a pane whose command has finished, where there is
	// nothing left to interrupt and it means done like the rest of them.
	paneInterruptKey = keys.Interrupt
)

// pane is one terminal in the window.
type pane struct {
	// id addresses this pane in the messages its own commands produce. Without
	// it a pane closing would be reported as "a pane closed", and with two of
	// them the window would have to guess which — and a guess that lands on the
	// wrong one closes a session nobody asked to end.
	id int

	term    *termpane.Model
	stream  Terminal
	action  Interaction
	sandbox Sandbox
	status  string

	// exited is set when what was running in the pane finished and the pane was
	// kept anyway, so its last screen can be read. See Interaction.holdsOnExit.
	exited bool
}

// detachHint is how to get out of a pane, as the key lists spell it.
//
// It is the same in every pane, and Ctrl-C is not it. A harness attach used to
// take Ctrl-C as "back out of this", which is a fine reading of the key right
// up until it is wrong: someone who types it to stop an agent and gets a
// detached session instead has not stopped anything, and has no way to tell
// from the screen they are looking at. The key belongs to whatever is running,
// in a shell and in a harness alike, and the window asks for the leader.
func (m *Model) detachHint() string { return m.leader() + " " + paneDetachAlt }

// leader is the pane's prefix key, as a Bubble Tea key name. It is the same key
// a plain `disco attach` detaches behind; see [keys].
func (m *Model) leader() string {
	if m.leaderKey == "" {
		return keys.DefaultLeader
	}
	return m.leaderKey
}

// paneMouseHint is the mouse toggle, as the key lists spell it.
func (m *Model) paneMouseHint() string { return m.leader() + " " + paneMouseKey }

// toggleMouseMsg is the leader plus m: hand the mouse to the box, or keep it.
//
// While the terminal is reporting the mouse you lose your own selection in it,
// which is the bargain every multiplexer makes and usually the right one — but
// only you know when you would rather copy a stack trace than click on it.
type toggleMouseMsg struct{}

// paneActionMsg is the leader plus one of the list's own action keys: the same
// command, on the discobox the screen is showing.
type paneActionMsg struct{ key string }

// movePaneMsg is the leader plus h or l: focus the pane that way.
type movePaneMsg struct{ delta int }

// swapPanesMsg is the leader plus e: exchange the two spots, for when the one
// you are working in would rather be on the other side.
type swapPanesMsg struct{}

// paneOpenedMsg carries a connected terminal back to the model.
type paneOpenedMsg struct {
	action  Interaction
	sandbox Sandbox
	term    Terminal
	// at is where the pane goes among the ones already open.
	at int
	// overlay puts it over the two spots instead of into one of them.
	overlay bool
}

// paneEventMsg is a connection state change under an open pane.
type paneEventMsg struct{ event TerminalEvent }

// paneMsg is anything a pane's own commands produced, addressed to the pane it
// came from. See pane.id.
type paneMsg struct {
	id  int
	msg tea.Msg
}

// fromPane tags a pane's commands so what comes back says where it came from.
// A batch is tagged through to its parts, since each half arrives separately.
func fromPane(id int, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		msg := cmd()
		if msg == nil {
			return nil
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			tagged := make([]tea.Cmd, 0, len(batch))
			for _, sub := range batch {
				tagged = append(tagged, fromPane(id, sub))
			}
			return tea.BatchMsg(tagged)
		}
		return paneMsg{id: id, msg: msg}
	}
}

// paneByID finds the pane a message came from, and where it is: the index of
// its spot, or overlayAt when it is the one over them.
func (m *Model) paneByID(id int) (*pane, int) {
	if m.overlay != nil && m.overlay.id == id {
		return m.overlay, overlayAt
	}
	for i, p := range m.panes {
		if p.id == id {
			return p, i
		}
	}
	return nil, -1
}

// overlayAt is where the overlay is, which is not among the spots.
const overlayAt = -2

// inPanes reports whether the window is showing terminals rather than the list.
func (m *Model) inPanes() bool { return len(m.panes) > 0 || m.overlay != nil }

// focusedPane is the pane every key goes to: the overlay while one is up, since
// it has the screen, and otherwise the spot with focus.
func (m *Model) focusedPane() *pane {
	if m.overlay != nil {
		return m.overlay
	}
	if m.focused < 0 || m.focused >= len(m.panes) {
		return nil
	}
	return m.panes[m.focused]
}

// slotFor is the spot running act, if it is open.
func (m *Model) slotFor(act Interaction) (*pane, int) {
	for i, p := range m.panes {
		if p.action == act {
			return p, i
		}
	}
	return nil, -1
}

// currentBox is the discobox the pane screen is showing, as the list last saw
// it. The pane was opened on a snapshot, and what a command may do to it — a
// diffstat that has since arrived, a state that has since changed — moves on
// without it.
func (m *Model) currentBox() Sandbox {
	for _, s := range m.list.all {
		if s.ID == m.paneBox.ID {
			return s
		}
	}
	return m.paneBox
}

// openPane opens a terminal from the list, on the discobox under the cursor. It
// replaces whatever was open: this is a different discobox, or the same one
// started over, and either way what was on screen was about something else.
func (m *Model) openPane(act Interaction, sandbox Sandbox) tea.Cmd {
	m.closePanes()
	m.paneBox = sandbox
	// Which side each spot is on is a preference that lasts as long as the
	// screen does, and this is a new one.
	m.paneOrder = [2]Interaction{InteractAttach, InteractShell}
	return m.connectPane(act, sandbox, 0, !act.slotted())
}

// openSlot fills one of the two spots, or moves to it when it is already full.
//
// Going to the one that is open rather than saying so is the answer to the same
// keystroke either way: you pressed the key for the harness, and the harness is
// what you get.
func (m *Model) openSlot(act Interaction) tea.Cmd {
	if _, at := m.slotFor(act); at >= 0 {
		m.focused = at
		return nil
	}
	return m.connectPane(act, m.currentBox(), m.slotIndex(act), false)
}

// slotIndex is which side a spot goes on: the order the two are in, which swap
// exchanges, applied to whichever of them is already up.
func (m *Model) slotIndex(act Interaction) int {
	if len(m.panes) == 0 {
		return 0
	}
	if act == m.paneOrder[0] {
		return 0
	}
	return len(m.panes)
}

// openOverlay runs a command over the two spots, in a screen of its own.
//
// It has the screen because it is the thing you asked for and it is over when
// it is over — a diff you opened to read is not something to read in half a
// window beside a harness scrolling past. What is underneath keeps running,
// unresized and unredrawn, and is exactly where it was when the command exits.
func (m *Model) openOverlay(act Interaction, sandbox Sandbox) tea.Cmd {
	if m.overlay != nil {
		return status("%s is still up — close it first", m.overlay.action)
	}
	return m.connectPane(act, sandbox, 0, true)
}

// swapPanes exchanges the two, carrying focus with the terminal rather than
// leaving it on the side of the screen: you swapped the panes, not your place
// in them.
//
// The order it swaps is the screen's, not just this pair's, so a spot closed
// and opened again comes back on the side you put it.
func (m *Model) swapPanes() tea.Cmd {
	m.paneOrder[0], m.paneOrder[1] = m.paneOrder[1], m.paneOrder[0]
	if len(m.panes) < 2 {
		// There is no side to be on with one pane, but there will be: the
		// order is what the next one to open goes by.
		return status("%s opens on the left now", m.paneOrder[0])
	}
	m.panes[0], m.panes[1] = m.panes[1], m.panes[0]
	m.focused = 1 - m.focused
	return nil
}

// connectPane opens a terminal for a spot, or for the screen over them. The
// pane is sized before it is opened: the size is what the far end is told, and
// a terminal that starts at the wrong size draws itself wrong before anything
// can correct it.
func (m *Model) connectPane(act Interaction, sandbox Sandbox, at int, overlay bool) tea.Cmd {
	// A terminal wants the whole screen, so opening one opens the window out
	// even when nothing has asked for the list yet.
	m.expanded = true
	m.busy = string(act) + "…"

	// An overlay has the screen to itself; a spot has its share of it.
	boxes := len(m.panes) + 1
	if overlay {
		boxes = 1
	}
	cols, rows := m.paneCells(boxes)
	ctx, ds, id := m.ctx, m.ds, sandbox.ID
	return func() tea.Msg {
		term, err := ds.Open(ctx, act, id, cols, rows)
		if err != nil {
			return statusMsg{text: fmt.Sprintf("%s: %v", act, err), err: true}
		}
		return paneOpenedMsg{action: act, sandbox: sandbox, term: term, at: at, overlay: overlay}
	}
}

// paneOpened starts drawing a connected terminal.
func (m *Model) paneOpened(msg paneOpenedMsg) tea.Cmd {
	m.busy = ""
	m.nextPaneID++
	p := &pane{
		id:      m.nextPaneID,
		term:    termpane.New(m.paneOptions(msg.overlay)...),
		stream:  msg.term,
		action:  msg.action,
		sandbox: msg.sandbox,
	}

	if msg.overlay {
		m.overlay = p
	} else {
		at := min(max(msg.at, 0), len(m.panes))
		m.panes = append(m.panes, nil)
		copy(m.panes[at+1:], m.panes[at:])
		m.panes[at] = p
		m.focused = at
	}

	m.focus = focusPane
	m.prompt.Blur()
	m.layout()
	return tea.Batch(
		fromPane(p.id, p.term.Attach(msg.term)),
		fromPane(p.id, m.paneEvents(msg.term)),
	)
}

// paneOptions are the keys a pane keeps for the window rather than passing on.
//
// A spot carries the whole key map, because every one of them is about the
// discobox on screen and the screen is where you are. The overlay carries only
// its way out and the mouse: it is one command running to completion, and a key
// that opened something else over it would be a key that lost it.
func (m *Model) paneOptions(overlay bool) []termpane.Option {
	opts := []termpane.Option{
		// No bare detach key: nothing the window reserves stands between a
		// program and its own interrupt. The way out is behind the leader, and
		// it is the same one in every pane. See detachHint.
		termpane.WithPrefix(m.leader(), ""),
		termpane.WithPrefixBinding(paneMouseKey, toggleMouseMsg{}),
		termpane.WithPrefixBinding(paneDetachAlt, termpane.DetachMsg{}),
	}
	if overlay {
		return opts
	}
	opts = append(opts,
		termpane.WithPrefixBinding(paneSwapKey, swapPanesMsg{}),
		// Moving between panes is something you do in runs, so it holds
		// the leader open while Ctrl is down: leader, then Ctrl-← Ctrl-←
		// walks across without pressing the leader again. The arrows and
		// the letters are the same binding, so neither has to be learned.
		termpane.WithRepeatingPrefixBinding(paneLeftKey, movePaneMsg{delta: -1}),
		termpane.WithRepeatingPrefixBinding(paneRightKey, movePaneMsg{delta: 1}),
		termpane.WithRepeatingPrefixBinding("left", movePaneMsg{delta: -1}),
		termpane.WithRepeatingPrefixBinding("right", movePaneMsg{delta: 1}),
	)
	// Every command the list offers, on the key it has there. One key map for
	// the two screens is the point: the pane screen is a discobox with the
	// cursor on it, and what you can do to it does not change with where you
	// are looking at it from.
	for key := range interactions {
		opts = append(opts, termpane.WithPrefixBinding(key, paneActionMsg{key: key}))
	}
	for key := range verbs {
		opts = append(opts, termpane.WithPrefixBinding(key, paneActionMsg{key: key}))
	}
	return opts
}

// paneEvents waits for the next connection state change. A reconnect never
// appears in the output — the stream simply carries on — so it is reported here
// or not at all.
func (m *Model) paneEvents(term Terminal) tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-term.Events():
			if !ok {
				return nil
			}
			return paneEventMsg{event: event}
		}
	}
}

// closePane ends one session and takes it off the screen. Whatever is left
// keeps running.
func (m *Model) closePane(at int) {
	if at == overlayAt {
		m.closeOverlay()
		return
	}
	if at < 0 || at >= len(m.panes) {
		return
	}
	_ = m.panes[at].term.Close()
	m.panes = append(m.panes[:at], m.panes[at+1:]...)
	m.focused = min(m.focused, max(len(m.panes)-1, 0))
	if len(m.panes) > 0 {
		m.layout()
		return
	}
	m.leavePanes()
}

// closeOverlay ends the command that had the screen and gives it back to the
// two spots, which have been running underneath the whole time.
func (m *Model) closeOverlay() {
	if m.overlay == nil {
		return
	}
	_ = m.overlay.term.Close()
	m.overlay = nil
	if len(m.panes) > 0 {
		m.layout()
		return
	}
	// It was opened from the list rather than over anything, so there is
	// nothing to come back to.
	m.leavePanes()
}

// closePanes ends every session and puts the window back the way it was.
func (m *Model) closePanes() {
	if m.overlay != nil {
		_ = m.overlay.term.Close()
		m.overlay = nil
	}
	for _, p := range m.panes {
		_ = p.term.Close()
	}
	m.panes = nil
	m.focused = 0
	m.leavePanes()
}

// leavePanes returns focus to where a pane was opened from.
func (m *Model) leavePanes() {
	if m.focus != focusPane {
		return
	}
	// Back to the list, which is where the pane was opened from, with the
	// cursor still on the discobox it was opened on. Landing in the prompt would
	// mean two presses to act on the one you were just looking at.
	m.prompt.Blur()
	m.focus = focusList
	if len(m.list.rows()) == 0 {
		m.backToPrompt()
	}
}

// movePane shifts focus between panes. It stops at the ends rather than
// wrapping: two panes side by side have a left and a right, and a focus that
// jumps the long way round is one you have to look for.
func (m *Model) movePane(delta int) tea.Cmd {
	next := m.focused + delta
	if next < 0 || next >= len(m.panes) {
		return nil
	}
	m.focused = next
	return nil
}

// updatePane routes a key or a mouse event to the pane with focus. Everything a
// pane's own commands produced comes back addressed, and goes through
// updatePaneMsg instead.
func (m *Model) updatePane(msg tea.Msg) tea.Cmd {
	p := m.focusedPane()
	if p == nil {
		return nil
	}
	if tagged, ok := msg.(paneMsg); ok {
		return m.updatePaneMsg(tagged)
	}
	// A pane whose command has finished is a screen to read, not a terminal to
	// type at. Its keys are the reader's: the arrows walk back through output
	// longer than the pane, and the ones that mean "done" take it away.
	if p.exited {
		if key, ok := msg.(tea.KeyPressMsg); ok {
			return m.readFinished(p, key)
		}
		if wheel, ok := msg.(tea.MouseWheelMsg); ok {
			p.term.Scroll(wheelLines(wheel))
			return nil
		}
		return nil
	}
	if mouse, ok := msg.(tea.MouseMsg); ok {
		// In cells relative to the focused pane's grid. A click on the window's
		// own chrome, or on the other pane, lands outside it and is dropped.
		if m.paneMouse {
			x, y := m.focusedOrigin()
			p.term.SendMouse(translateMouse(mouse, x, y))
		}
		return nil
	}
	term, cmd := p.term.Update(msg)
	p.term = term
	return fromPane(p.id, cmd)
}

// updatePaneMsg handles what one pane's commands produced, against that pane.
//
// Which pane it came from is the whole point: a pane that has just been closed
// still has a read in flight, and its parting message must not be taken for the
// survivor's.
func (m *Model) updatePaneMsg(tagged paneMsg) tea.Cmd {
	p, at := m.paneByID(tagged.id)
	if p == nil {
		// Its pane is already gone; nothing left to tell.
		return nil
	}
	switch msg := tagged.msg.(type) {
	case toggleMouseMsg:
		m.paneMouse = !m.paneMouse
		if m.paneMouse {
			return status("mouse goes to the box when it asks for it")
		}
		return status("mouse stays with your terminal — selection works again")

	case paneActionMsg:
		// The list's dispatcher, on the one discobox this screen is showing:
		// the same enabled checks, the same confirmations, the same reports.
		return m.actOn(msg.key, []Sandbox{m.currentBox()})

	case swapPanesMsg:
		return m.swapPanes()

	case movePaneMsg:
		if at == overlayAt {
			return nil
		}
		// A run of these is one chord: the pane it fired in is left armed while
		// Ctrl is down, and moving focus has to carry that to the pane it moved
		// to, or the sequence would end on its own first step.
		armed := p.term.PrefixArmed()
		p.term.SetPrefixArmed(false)
		cmd := m.movePane(msg.delta)
		if armed {
			if next := m.focusedPane(); next != nil {
				next.term.SetPrefixArmed(true)
			}
		}
		return cmd

	case termpane.DetachMsg:
		// Detaching leaves the session running: it is the discobox's terminal,
		// and closing the window onto it is not the same as ending it.
		m.closePane(at)
		m.layout()
		return tea.Batch(m.refresh(), status("detached — the terminal is still running"))

	case termpane.ClosedMsg:
		action := p.action
		if action.holdsOnExit() && msg.Err == nil {
			// Keep the last screen up to be read; any key takes it away.
			p.exited = true
			p.status = "finished"
			return m.refresh()
		}
		m.closePane(at)
		m.layout()
		if msg.Err != nil {
			return tea.Batch(m.refresh(), m.report(true, "%s: %v", action, msg.Err))
		}
		return tea.Batch(m.refresh(), status("%s session ended", action))

	case paneEventMsg:
		switch msg.event.State {
		case TerminalReconnecting:
			p.status = "reconnecting…"
		case TerminalReconnected:
			p.status = ""
		}
		return fromPane(p.id, m.paneEvents(p.stream))
	}

	term, cmd := p.term.Update(tagged.msg)
	p.term = term
	return fromPane(p.id, cmd)
}

// readFinished handles a key on a pane whose command has finished.
//
// Only the keys that mean "done" close it. Anything else would take the screen
// away mid-read, which for output longer than the pane is exactly when you are
// still working through it.
func (m *Model) readFinished(p *pane, key tea.KeyPressMsg) tea.Cmd {
	_, rows := m.paneCells(m.paneBoxes())
	switch keyName(key) {
	case "up", "k":
		p.term.Scroll(1)
	case "down", "j":
		p.term.Scroll(-1)
	case "pgup", "ctrl+b":
		p.term.Scroll(rows - 1)
	case "pgdown", "ctrl+f", " ":
		p.term.Scroll(-(rows - 1))
	case "home", "g":
		p.term.Scroll(p.term.ScrollbackLen())
	case "end", "G":
		p.term.Scroll(-p.term.ScrollbackLen())
	case "q", "esc", "enter", paneInterruptKey:
		_, at := m.paneByID(p.id)
		m.closePane(at)
		m.layout()
	}
	return nil
}

// wheelLines is how far one turn of the wheel moves the view.
func wheelLines(wheel tea.MouseWheelMsg) int {
	switch wheel.Button {
	case tea.MouseWheelUp:
		return 3
	case tea.MouseWheelDown:
		return -3
	default:
		return 0
	}
}

// paneBoxes is how many terminals are drawn side by side: one when the overlay
// has the screen, and otherwise a box per spot.
func (m *Model) paneBoxes() int {
	if m.overlay != nil {
		return 1
	}
	return len(m.panes)
}

// paneRows is the height every pane gets: the banner, the border's own two
// edges, and the status line at the bottom.
func (m *Model) paneRows() int { return m.height - 4 }

// paneCells is the terminal size one of n panes implies: its share of the
// screen, less its border and a cell of air inside it on each side.
func (m *Model) paneCells(n int) (cols, rows int) {
	return max(m.paneWidth(n)-2-2*boxPad, 1), max(m.paneRows(), 1)
}

// paneWidth is the width of one box when n are side by side. The leftmost takes
// any odd cell, so the two are never more than one apart.
func (m *Model) paneWidth(n int) int {
	if n < 1 {
		n = 1
	}
	return max(m.width/n, 4)
}

// paneOrigin is where a pane's grid begins on screen: past its border's left
// edge and the cell of air inside it, and down past the banner and the border's
// top edge.
//
// The cursor and every mouse event are placed against this, so it is worked out
// once rather than counted twice.
func (m *Model) paneOrigin(i int) (x, y int) {
	width := m.paneWidth(m.paneBoxes())
	return i*width + 1 + boxPad, 2
}

// focusedOrigin is where the pane every key goes to was drawn. The overlay is
// the only box on screen, so it starts where the first one would.
func (m *Model) focusedOrigin() (x, y int) {
	if m.overlay != nil {
		return m.paneOrigin(0)
	}
	return m.paneOrigin(m.focused)
}

// viewPaneWindow draws the whole screen for the open panes: the captions above,
// the bordered terminals side by side, and the keys below.
//
// The grid rows come back from the library already fitted to the cell and are
// put between the border's sides without passing through anything that could
// re-wrap them.
func (m *Model) viewPaneWindow() string {
	inner := max(m.width-2, 1)
	pad := strings.Repeat(" ", boxPad)

	// Every pane is the same discobox, so it is identified once — folded into
	// the banner rather than given a line of its own, and centered in it.
	//
	// The id rather than the name, because the id is what you would type at a
	// shell to act on this one, and muted rather than foreground, because it is
	// there to be looked up when wanted rather than read on every glance. What
	// each pane is running is in its own border.
	name := m.st.dimText.Render(m.paneBox.ID)
	right := m.viewHeaderRight()
	// What the transport is doing displaces the keys while it is doing it: it
	// is the more urgent of the two, and the keys are on the status line too.
	if p := m.focusedPane(); p != nil && p.status != "" {
		right = m.st.statusWA.Render(p.status)
	}

	rows := []string{
		" " + pad + spreadCenter(m.viewHeaderLeft(), name, right, max(inner-2*boxPad, 1)) + pad + " ",
	}

	// The overlay has the screen while it is up. What is under it is not drawn
	// half-covered or dimmed behind it: it is not there, and it is all still
	// there when the command exits.
	drawn, focused := m.panes, m.focused
	if m.overlay != nil {
		drawn, focused = []*pane{m.overlay}, 0
	}

	// Each pane is a block of its own, joined side by side; the last takes any
	// cell the division left over so the row comes out exactly the width.
	blocks := make([]string, 0, len(drawn))
	width := m.paneWidth(len(drawn))
	for i, p := range drawn {
		w := width
		if i == len(drawn)-1 {
			w = m.width - i*width
		}
		blocks = append(blocks, m.viewPaneBox(p, w, i == focused))
	}
	rows = append(rows, strings.Split(lipgloss.JoinHorizontal(lipgloss.Top, blocks...), "\n")...)
	rows = append(rows, " "+pad+padANSI(m.st.dimText.Render(m.hints()), max(inner-2*boxPad, 1))+pad+" ")
	return strings.Join(rows, "\n")
}

// viewPaneBox draws one pane: its border, with what it is running set into the
// top of it, around its grid.
//
// The border of the pane with focus is lit and the others are not. With two
// terminals side by side and every key going to one of them, which one that is
// has to be visible without looking for it.
func (m *Model) viewPaneBox(p *pane, width int, focused bool) string {
	edge := m.st.frame
	if !focused {
		edge = m.st.rule
	}
	inner := max(width-2, 1)
	grid := max(inner-2*boxPad, 1)
	side := edge.Render("│")
	pad := strings.Repeat(" ", boxPad)

	// Until the application says what it is, what it is is what was asked for:
	// a shell, or the harness you attached to.
	title := strings.TrimSpace(p.term.Title())
	if title == "" {
		title = string(p.action)
	}
	rows := []string{titledEdge(m.st, edge, title, inner)}
	for _, line := range p.term.View() {
		rows = append(rows, side+pad+padANSI(line, grid)+pad+side)
	}
	rows = append(rows, edge.Render("╰"+strings.Repeat("─", inner)+"╯"))
	return strings.Join(rows, "\n")
}

// paneCursor is where the hardware cursor goes: the focused pane's own idea of
// it, offset by where that pane's grid was drawn.
func (m *Model) paneCursor() *tea.Cursor {
	p := m.focusedPane()
	if p == nil {
		return nil
	}
	x, y := m.focusedOrigin()
	return p.term.Cursor(x, y)
}

// paneMouseMode mirrors what the focused pane's application has asked for, so
// the terminal reports the mouse only while something is using it — and only
// while the mouse has been handed over at all.
func (m *Model) paneMouseMode() tea.MouseMode {
	p := m.focusedPane()
	if p == nil || !m.paneMouse {
		return tea.MouseModeNone
	}
	switch p.term.MouseMode() {
	case termpane.MouseAllMotion:
		return tea.MouseModeAllMotion
	case termpane.MouseCellMotion:
		return tea.MouseModeCellMotion
	default:
		return tea.MouseModeNone
	}
}

// translateMouse moves an event from the screen into a pane's grid.
func translateMouse(msg tea.MouseMsg, originX, originY int) tea.MouseMsg {
	switch event := msg.(type) {
	case tea.MouseClickMsg:
		event.X, event.Y = event.X-originX, event.Y-originY
		return event
	case tea.MouseReleaseMsg:
		event.X, event.Y = event.X-originX, event.Y-originY
		return event
	case tea.MouseWheelMsg:
		event.X, event.Y = event.X-originX, event.Y-originY
		return event
	case tea.MouseMotionMsg:
		event.X, event.Y = event.X-originX, event.Y-originY
		return event
	}
	return msg
}

// displayName is what to call a discobox in the pane's caption: its name, or its
// ID when it has none worth reading.
func displayName(s Sandbox) string {
	if s.Name != "" {
		return s.Name
	}
	return s.ID
}
