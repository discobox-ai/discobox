package tui

import (
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/obot-platform/discobox/cli/internal/keys"
	"github.com/obot-platform/discobox/termpane"
)

// A pane is a discobox's terminal drawn in the window rather than by handing
// the real terminal over.
//
// The screen is the discobox's workspace: its primary terminal on the left,
// and every other live session as a tab on the right — one visible at a time.
// The workspace mirrors the server rather than remembering what was opened
// here: attaching draws every session the discobox has, and a session started
// from anywhere appears as a tab on its own. See workspace.go.
//
// Over them, a command that runs and finishes — apply — takes the
// whole screen for as long as it runs. The terminals underneath are untouched:
// still connected, still running, still where they were when it comes back.
//
// Every command the list offers is reachable from here behind the leader, on
// the key it has there, and acts on the one discobox the screen is showing.

const (
	paneMouseKey = "m"
	paneLeftKey  = "h"
	paneRightKey = "l"
	// paneDetachAlt is the detach key behind the leader, for a pane whose
	// application needs Ctrl-C more than the window does. It is the d screen,
	// tmux, and a plain `disco attach` all detach on. The leader also carries
	// the list's own keys, but none of them is d anymore — it was diff, until
	// diff left the CLI.
	paneDetachAlt = "d"
	// paneQuitKey quits the whole window from behind the leader. Ctrl-C quits
	// everywhere else but belongs to the application inside a pane, so the
	// leader carries the exit here — beside d, which only detaches.
	paneQuitKey = "q"
	// paneInterruptKey is the application's everywhere, and never the window's.
	// The one exception is a pane whose command has finished, where there is
	// nothing left to interrupt and it means done like the rest of them.
	paneInterruptKey = keys.Interrupt
)

// pane is one terminal in the window.
type pane struct {
	// id addresses this pane in the messages its own commands produce. Without
	// it a pane closing would be reported as "a pane closed", and with several
	// of them the window would have to guess which — and a guess that lands on
	// the wrong one closes a session nobody asked to end.
	id int

	term    *termpane.Model
	stream  Terminal
	action  Interaction
	sandbox Sandbox
	status  string

	// execID keys the pane to the server session it is drawing, which is what
	// lets the workspace's poll tell a session it already shows from one it
	// has to open. Empty for the overlay, which is no session at all.
	execID string
	// title is what to call the tab until the application names itself.
	title string
	// created is when the session was created, which is the tab order.
	created time.Time

	// exited is set when what was running in the pane finished and the pane
	// was kept anyway, so its last screen can be read.
	exited bool
}

// detachHint is how to get out of the workspace, as the key lists spell it.
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

// toggleMouseMsg is the leader plus m: take the mouse from the box, or give
// it back.
//
// While nothing in the box has asked for the mouse there is nothing to take —
// drag selects and copies on its own. The toggle is for the programs that did
// ask, vim and htop and their kind, when you would rather copy a stack trace
// than click on it. Taken, the box sees no mouse at all.
type toggleMouseMsg struct{}

// paneActionMsg is the leader plus one of the list's own action keys: the same
// command, on the discobox the screen is showing.
type paneActionMsg struct{ key string }

// movePaneMsg is the leader plus h or l: move focus that way, between the
// terminal and the shell tabs.
type movePaneMsg struct{ delta int }

// jumpPaneMsg is the leader plus a digit: focus the pane wearing that number —
// 0 the terminal, 1 through 9 the tab whose label carries the ordinal.
type jumpPaneMsg struct{ n int }

// quitPaneMsg is the leader plus q: quit the whole window, every session left
// running. It is the same exit Ctrl-C is everywhere else, spelled behind the
// leader because inside a pane Ctrl-C belongs to the application.
type quitPaneMsg struct{}

// paneOpenedMsg carries a connected overlay terminal back to the model. The
// workspace's own terminals arrive as workspaceTermMsg instead; see
// workspace.go.
type paneOpenedMsg struct {
	action  Interaction
	sandbox Sandbox
	term    Terminal
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

// paneByID finds the pane a message came from: the overlay, the terminal, or
// one of the shell tabs.
func (m *Model) paneByID(id int) *pane {
	if m.overlay != nil && m.overlay.id == id {
		return m.overlay
	}
	if m.terminal != nil && m.terminal.id == id {
		return m.terminal
	}
	for _, p := range m.shells {
		if p.id == id {
			return p
		}
	}
	return nil
}

// inPanes reports whether the window is showing terminals rather than the list.
func (m *Model) inPanes() bool { return m.terminal != nil || m.overlay != nil }

// focusedPane is the pane every key goes to: the overlay while one is up, since
// it has the screen, and otherwise the terminal or the visible shell tab.
func (m *Model) focusedPane() *pane {
	if m.overlay != nil {
		return m.overlay
	}
	if m.onShells && m.activeShell >= 0 && m.activeShell < len(m.shells) {
		return m.shells[m.activeShell]
	}
	return m.terminal
}

// currentBox is the discobox the workspace is showing, as the list last saw
// it. The workspace was opened on a snapshot, and what a command may do to it
// — a diffstat that has since arrived, a state that has since changed — moves
// on without it.
func (m *Model) currentBox() Sandbox {
	for _, s := range m.list.all {
		if s.ID == m.paneBox.ID {
			return s
		}
	}
	return m.paneBox
}

// openOverlay runs a command over the workspace, in a screen of its own.
//
// It has the screen because it is the thing you asked for and it is over when
// it is over — a report you opened to read is not something to read in half a
// window beside a harness scrolling past. What is underneath keeps running,
// unresized and unredrawn, and is exactly where it was when the command exits.
func (m *Model) openOverlay(act Interaction, sandbox Sandbox) tea.Cmd {
	if m.overlay != nil {
		return status("%s is still up — close it first", m.overlay.action)
	}
	// A terminal wants the whole screen, so opening one opens the window out
	// even when nothing has asked for the list yet. The overlay is sized
	// before it is opened: the size is what the far end is told, and a
	// terminal that starts at the wrong size draws itself wrong before
	// anything can correct it.
	m.expanded = true
	m.busy = string(act) + "…"
	cols, rows := m.paneCells(m.width)
	ctx, ds, id := m.ctx, m.ds, sandbox.ID
	return func() tea.Msg {
		term, err := ds.Open(ctx, act, id, cols, rows)
		if err != nil {
			return statusMsg{text: string(act) + ": " + err.Error(), err: true}
		}
		return paneOpenedMsg{action: act, sandbox: sandbox, term: term}
	}
}

// paneOpened starts drawing a connected overlay terminal.
func (m *Model) paneOpened(msg paneOpenedMsg) tea.Cmd {
	m.busy = ""
	m.nextPaneID++
	p := &pane{
		id:      m.nextPaneID,
		term:    termpane.New(m.paneOptions(true)...),
		stream:  msg.term,
		action:  msg.action,
		sandbox: msg.sandbox,
	}
	m.overlay = p
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
// A workspace terminal carries the whole key map, because every one of them is
// about the discobox on screen and the screen is where you are. The overlay
// carries only its way out and the mouse: it is one command running to
// completion, and a key that opened something else over it would be a key that
// lost it.
func (m *Model) paneOptions(overlay bool) []termpane.Option {
	opts := []termpane.Option{
		// No bare detach key: nothing the window reserves stands between a
		// program and its own interrupt. The way out is behind the leader, and
		// it is the same one in every pane. See detachHint.
		termpane.WithPrefix(m.leader(), ""),
		termpane.WithPrefixBinding(paneMouseKey, toggleMouseMsg{}),
		termpane.WithPrefixBinding(paneDetachAlt, termpane.DetachMsg{}),
		termpane.WithPrefixBinding(paneQuitKey, quitPaneMsg{}),
	}
	if overlay {
		return opts
	}
	opts = append(opts,
		// Moving between the terminal and the tabs is something you do in
		// runs, so it holds the leader open while Ctrl is down: leader, then
		// Ctrl-← Ctrl-← walks across without pressing the leader again. The
		// arrows and the letters are the same binding, so neither has to be
		// learned.
		termpane.WithRepeatingPrefixBinding(paneLeftKey, movePaneMsg{delta: -1}),
		termpane.WithRepeatingPrefixBinding(paneRightKey, movePaneMsg{delta: 1}),
		termpane.WithRepeatingPrefixBinding("left", movePaneMsg{delta: -1}),
		termpane.WithRepeatingPrefixBinding("right", movePaneMsg{delta: 1}),
	)
	// The digits jump straight to a pane by the number its label wears, the
	// way tmux selects windows: 0 is the terminal, 1 through 9 the tabs.
	for n := 0; n <= 9; n++ {
		opts = append(opts, termpane.WithPrefixBinding(strconv.Itoa(n), jumpPaneMsg{n: n}))
	}
	// Every command the list offers, on the key it has there. One key map for
	// the two screens is the point: the workspace is a discobox with the
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

// closeOverlay ends the command that had the screen and gives it back to the
// workspace, which has been running underneath the whole time.
func (m *Model) closeOverlay() {
	if m.overlay == nil {
		return
	}
	_ = m.overlay.term.Close()
	m.overlay = nil
	if m.terminal != nil {
		m.layout()
		return
	}
	// It was opened from the list rather than over anything, so there is
	// nothing to come back to.
	m.leavePanes()
}

// leavePanes returns focus to where the screen was opened from.
func (m *Model) leavePanes() {
	if m.focus != focusPane {
		return
	}
	// Back to the list, which is where the screen was opened from, with the
	// cursor still on the discobox it was opened on. Landing in the prompt would
	// mean two presses to act on the one you were just looking at.
	m.prompt.Blur()
	m.focus = focusList
	if len(m.list.rows()) == 0 {
		m.backToPrompt()
	}
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
	// The mouse routes by position rather than focus — a pointer names the
	// pane it is over — and works on finished panes too, where the last
	// screen still selects and scrolls.
	if mouse, ok := msg.(tea.MouseMsg); ok {
		return m.routeMouse(mouse)
	}
	// A pane whose command has finished is a screen to read, not a terminal to
	// type at. Its keys are the reader's: the arrows walk back through output
	// longer than the pane, and the ones that mean "done" take it away.
	if p.exited {
		if key, ok := msg.(tea.KeyPressMsg); ok {
			return m.readFinished(p, key)
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
	p := m.paneByID(tagged.id)
	if p == nil {
		// Its pane is already gone; nothing left to tell.
		return nil
	}
	switch msg := tagged.msg.(type) {
	case toggleMouseMsg:
		m.mouseSeized = !m.mouseSeized
		if m.mouseSeized {
			return status("mouse taken from the box — drag selects; %s gives it back", m.paneMouseHint())
		}
		return status("mouse handed back to the box")

	case termpane.CopyMsg:
		// The OS clipboard first, and OSC 52 only when there is none to
		// write — an SSH session, a box with no clipboard tool. Not both:
		// they can land on the same clipboard (WSL's is Windows'), the last
		// writer wins, and OSC 52 is the path terminals are known to
		// mis-decode. See osClipboard.
		text, copyOS := msg.Text, m.copyOS
		return tea.Batch(
			func() tea.Msg {
				if copyOS(text) == nil {
					return nil
				}
				return tea.SetClipboard(text)()
			},
			status("copied"),
		)

	case paneActionMsg:
		// The list's dispatcher, on the one discobox this screen is showing:
		// the same enabled checks, the same confirmations, the same reports.
		return m.actOn(msg.key, []Sandbox{m.currentBox()})

	case movePaneMsg:
		if p == m.overlay {
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

	case jumpPaneMsg:
		if p == m.overlay {
			return nil
		}
		return m.jumpPane(msg.n)

	case quitPaneMsg:
		// The same exit Ctrl-C is outside a pane: the window goes, and every
		// session keeps running without it.
		m.quit = true
		return tea.Quit

	case termpane.DetachMsg:
		if p == m.overlay {
			// The overlay is this CLI's own command, so leaving it closes it.
			m.closeOverlay()
			m.layout()
			return tea.Batch(m.refresh(), status("%s closed", p.action))
		}
		// Detaching leaves every session running: they are the discobox's
		// terminals, and closing the window onto them is not the same as
		// ending them.
		m.closeWorkspace()
		return tea.Batch(m.refresh(), status("detached — the discobox is still running"))

	case termpane.ClosedMsg:
		return m.paneClosed(p, msg)

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

// paneClosed handles a session or command ending on its own.
//
// Where the pane sits decides what happens: the primary terminal ending ends
// the workspace, since a workspace is a view onto that session; a shell tab
// and a finished command hold their last screen to be read — an apply with
// little to say is over in a moment, and a pane that vanished with it would be
// a screen you never got to read. Only an error closes a pane unread, and is
// reported instead.
func (m *Model) paneClosed(p *pane, msg termpane.ClosedMsg) tea.Cmd {
	action := p.action
	switch {
	case p == m.terminal:
		m.closeWorkspace()
		if msg.Err != nil {
			return tea.Batch(m.refresh(), m.report(true, "%s: %v", action, msg.Err))
		}
		return tea.Batch(m.refresh(), status("the session ended"))

	case msg.Err == nil:
		// Keep the last screen up to be read; the keys that mean done take it
		// away.
		p.exited = true
		p.status = "finished"
		return m.refresh()

	case p == m.overlay:
		m.closeOverlay()
		m.layout()
		return tea.Batch(m.refresh(), m.report(true, "%s: %v", action, msg.Err))

	default:
		m.closeShell(m.shellIndex(p))
		m.layout()
		return tea.Batch(m.refresh(), m.report(true, "%s: %v", action, msg.Err))
	}
}

// readFinished handles a key on a pane whose command has finished.
//
// Only the keys that mean "done" close it. Anything else would take the screen
// away mid-read, which for output longer than the pane is exactly when you are
// still working through it — except left and right, which on a shell tab keep
// meaning what they mean everywhere on the workspace: go look at another pane,
// leaving this one held.
func (m *Model) readFinished(p *pane, key tea.KeyPressMsg) tea.Cmd {
	_, rows := m.paneCells(m.paneWidthOf(p))
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
	case "left", "h":
		if p != m.overlay {
			return m.movePane(-1)
		}
	case "right", "l":
		if p != m.overlay {
			return m.movePane(1)
		}
	case "q", "esc", "enter", paneInterruptKey:
		m.dismissPane(p)
	}
	return nil
}

// dismissPane takes a finished pane off the screen: the overlay back to the
// workspace, a shell tab out of the strip.
func (m *Model) dismissPane(p *pane) {
	if p == m.overlay {
		m.closeOverlay()
	} else if i := m.shellIndex(p); i >= 0 {
		m.closeShell(i)
	}
	m.layout()
}

// routeMouse sends a mouse event to the pane it belongs to: the one a
// left-button gesture started in while one is open, and otherwise the pane
// under the pointer. A left press also focuses the pane it landed in — with a
// mouse in hand, pointing at the thing is how you say which one you mean. The
// pane's own HandleMouse decides what the event does: selection, forwarding
// to an application that asked for the mouse, or the wheel.
func (m *Model) routeMouse(msg tea.MouseMsg) tea.Cmd {
	var x, y int
	p := m.paneByID(m.mouseCapture)
	if p != nil {
		x, y = m.paneOrigin(p)
	} else {
		mouse := msg.Mouse()
		p, x, y = m.paneAt(mouse.X, mouse.Y)
	}
	if p == nil {
		return nil
	}
	switch ev := msg.(type) {
	case tea.MouseClickMsg:
		if ev.Button == tea.MouseLeft {
			m.mouseCapture = p.id
			m.focusPane(p)
		}
	case tea.MouseReleaseMsg:
		if ev.Button == tea.MouseLeft {
			m.mouseCapture = 0
		}
	case tea.MouseWheelMsg:
		// A finished pane has no application left to consult about the
		// wheel; its held screen scrolls directly.
		if p.exited {
			p.term.Scroll(wheelLines(ev))
			return nil
		}
	}
	p.term.SetSeized(m.mouseSeized)
	return fromPane(p.id, p.term.HandleMouse(translateMouse(msg, x, y)))
}

// paneAt is the pane whose grid is under a screen position, with the origin
// that grid was drawn at. The overlay owns the whole screen while it is up;
// otherwise the terminal is on the left and the visible shell tab on the
// right. The chrome between and around them belongs to no pane.
func (m *Model) paneAt(x, y int) (*pane, int, int) {
	candidates := []*pane{m.overlay}
	if m.overlay == nil {
		candidates = candidates[:0]
		candidates = append(candidates, m.terminal)
		if m.activeShell >= 0 && m.activeShell < len(m.shells) {
			candidates = append(candidates, m.shells[m.activeShell])
		}
	}
	for _, p := range candidates {
		if p == nil {
			continue
		}
		ox, oy := m.paneOrigin(p)
		cols, rows := m.paneCells(m.paneWidthOf(p))
		if x >= ox && x < ox+cols && y >= oy && y < oy+rows {
			return p, ox, oy
		}
	}
	return nil, 0, 0
}

// focusPane moves focus to a pane the mouse chose.
func (m *Model) focusPane(p *pane) {
	if p == m.overlay || p == m.terminal {
		m.onShells = false
		return
	}
	if i := m.shellIndex(p); i >= 0 {
		m.onShells, m.activeShell = true, i
	}
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

// paneRows is the height every pane gets: the banner, the border's own two
// edges, and the status line at the bottom.
func (m *Model) paneRows() int { return m.height - 4 }

// paneCells is the terminal size a pane of the given box width implies: the
// box, less its border and a cell of air inside it on each side.
func (m *Model) paneCells(width int) (cols, rows int) {
	return max(width-2-2*boxPad, 1), max(m.paneRows(), 1)
}

// paneWidthOf is the width of the box a pane is drawn in: the whole window for
// the overlay and for a terminal with no tabs beside it, half of it for each
// side of the split. The right half takes any odd cell, so the two are never
// more than one apart.
func (m *Model) paneWidthOf(p *pane) int {
	if p == m.overlay || len(m.shells) == 0 {
		return max(m.width, 4)
	}
	if p == m.terminal {
		return max(m.width/2, 4)
	}
	return max(m.width-m.width/2, 4)
}

// paneOrigin is where a pane's grid was drawn: past its border's left edge
// and the cell of air inside it, and down past the banner and the border's
// top edge.
//
// The cursor and every mouse event are placed against this, so it is worked
// out once rather than counted twice.
func (m *Model) paneOrigin(p *pane) (x, y int) {
	if p != nil && p != m.overlay && p != m.terminal && len(m.shells) > 0 {
		// A shell tab: its box starts where the terminal's ends.
		return m.width/2 + 1 + boxPad, 2
	}
	return 1 + boxPad, 2
}

// viewPaneWindow draws the whole screen for the open workspace: the captions
// above, the bordered terminals side by side, and the keys below.
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
	var body string
	switch {
	case m.overlay != nil:
		body = m.viewPaneBox(m.overlay, m.width, true)
	case len(m.shells) == 0:
		body = m.viewPaneBox(m.terminal, m.width, true)
	default:
		// The terminal on the left, the tabbed shells on the right; the right
		// half takes any cell the division left over so the row comes out
		// exactly the width.
		left := m.viewPaneBox(m.terminal, m.width/2, !m.onShells)
		right := m.viewShellBox(m.width-m.width/2, m.onShells)
		body = lipgloss.JoinHorizontal(lipgloss.Top, left, right)
	}
	rows = append(rows, strings.Split(body, "\n")...)
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
	x, y := m.paneOrigin(p)
	return p.term.Cursor(x, y)
}

// paneMouseMode is what the window asks the real terminal to report. While
// panes are up the mouse is always reported: selection and click-to-focus
// need the events even when nothing in the box asked for a mouse — which is
// the bargain every multiplexer strikes, native selection traded for the
// panes' own. All-motion is requested only when the focused application wants
// it and the mouse has not been seized away from it.
func (m *Model) paneMouseMode() tea.MouseMode {
	if !m.inPanes() {
		return tea.MouseModeNone
	}
	if p := m.focusedPane(); p != nil && !m.mouseSeized &&
		p.term.MouseMode() == termpane.MouseAllMotion {
		return tea.MouseModeAllMotion
	}
	return tea.MouseModeCellMotion
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
