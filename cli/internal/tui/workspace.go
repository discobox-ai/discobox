package tui

import (
	"path"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/discobox-ai/discobox/termpane"
)

// The workspace screen is the discobox as the server has it, not as this
// window remembers it. Opening it attaches to the primary terminal and to
// every other live TTY session the discobox has; while it is open, a poll of
// the exec listing keeps the tabs following the server, so a shell started
// from another window appears here on its own. Detaching leaves the workspace:
// every stream is closed at once, and every session keeps running.
//
// Which side a session is drawn on is the server's own answer rather than a
// layout this window remembers: a harness terminal is a terminal and goes on
// the left beside the primary, and everything else is a shell and goes on the
// right. See terminalExec.
//
// The poll is a poll rather than a subscription because the control plane has
// no exec event stream yet — exec state lives on the sandbox and is proxied
// through — and it only has to notice *new* sessions: the attach streams
// themselves already deliver every exit. The seam (DataSource.Execs) is a
// snapshot so that a stream, when one exists, replaces the tick loop and
// nothing else.

// workspacePollEvery is how often the exec listing is re-read while the
// workspace is open. It only has to catch sessions started elsewhere, so it is
// paced like a refresh rather than a keystroke.
const workspacePollEvery = 2 * time.Second

// workspaceExecsMsg is one answer from the exec listing: the first one opens
// the workspace's terminals, and every one after reconciles the tabs.
type workspaceExecsMsg struct {
	gen   int
	execs []Exec
	err   error
}

// workspaceTermMsg carries one connected workspace session back to the model:
// the primary, an existing session's attach, or a terminal or shell the leader
// asked for — which arrives with focus.
type workspaceTermMsg struct {
	gen  int
	exec Exec
	term Terminal
	err  error
	// asked is what the leader asked for, empty for a session the poll opened
	// on its own. A create that failed carries no session record to say what
	// it would have been, so this is what such a failure is reported as.
	asked Interaction
	focus bool
}

// workspaceTickMsg asks for the next poll of the exec listing.
type workspaceTickMsg struct{ gen int }

// workspaceForwardMsg carries the port forward the workspace opened with, or
// the reason there is none.
type workspaceForwardMsg struct {
	gen     int
	forward Forward
	err     error
}

// workspaceForwardChangedMsg is the forward saying its bindings differ. It
// carries nothing: the header redraws from the forward itself.
type workspaceForwardChangedMsg struct{ gen int }

// openFromList opens a terminal screen from the list, on the discobox under
// the cursor. It replaces whatever was open: this is a different discobox, or
// the same one started over, and either way what was on screen was about
// something else.
func (m *Model) openFromList(act Interaction, sandbox Sandbox) tea.Cmd {
	m.closeWorkspace()
	m.paneBox = sandbox
	// The workspace is the screen either way; asking for a shell opens it
	// with a fresh one, focused.
	return m.openWorkspace(sandbox, act == InteractShell)
}

// openWorkspace attaches to a discobox's workspace: list what is running,
// then open the primary and a tab for every live session the listing reports.
// The poll starts alongside and follows the server for as long as the screen
// is up.
func (m *Model) openWorkspace(sandbox Sandbox, freshShell bool) tea.Cmd {
	m.paneBox = sandbox
	// A terminal wants the whole screen, so opening one opens the window out
	// even when nothing has asked for the list yet — and nothing else can still
	// have it. The prompt can start a discobox and be on the harnesses screen by
	// the time it is created, which is the one way the two ever meet.
	m.expanded = true
	m.harnessesOpen = false
	m.busy = "attach…"
	m.connecting = map[string]bool{}
	gen := m.wsGen
	// The attach waits for the discobox to become attachable, which behind a
	// cold image pull is minutes (ADR 0039). Say what it is waiting for while
	// it does; the watch reports nothing for a discobox that is already up, so
	// attaching to a running one still shows only "attach…" (ADR 0060).
	cmds := []tea.Cmd{m.listExecs(gen), m.workspaceTick(gen), m.startForward(gen), m.watchProvisioning(sandbox.ID)}
	if freshShell {
		cmds = append(cmds, m.newShell())
	}
	return tea.Batch(cmds...)
}

// startForward opens the workspace's port forward. It is opened with the
// workspace rather than on a key: the header already lists what the discobox is
// serving, and a port you can see and cannot open is the gap this closes — so
// the reachable form of that list is what the screen shows from the moment it
// is up.
func (m *Model) startForward(gen int) tea.Cmd {
	ctx, ds, id := m.ctx, m.ds, m.paneBox.ID
	return func() tea.Msg {
		forward, err := ds.Forward(ctx, id)
		return workspaceForwardMsg{gen: gen, forward: forward, err: err}
	}
}

// workspaceForward takes ownership of the forward, or reports why there is
// none. A workspace that was left while it was opening closes it here: nothing
// else holds it, and the local ports it bound are this window's.
func (m *Model) workspaceForward(msg workspaceForwardMsg) tea.Cmd {
	if msg.gen != m.wsGen {
		if msg.forward != nil {
			_ = msg.forward.Close()
		}
		return nil
	}
	if msg.err != nil {
		// The terminals are the screen; the ports are what rides on its
		// header. Losing them is worth saying once and is not worth closing
		// anything over.
		return status("ports are not being forwarded: %v", msg.err)
	}
	if msg.forward == nil {
		return nil
	}
	m.forward = msg.forward
	return m.forwardEvents(msg.gen, msg.forward)
}

// forwardEvents waits for the next change to what is bound. Like the panes'
// connection events it re-arms itself, and like them it stops when the window's
// context ends or the channel closes.
func (m *Model) forwardEvents(gen int, forward Forward) tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-forward.Events():
			if !ok {
				return nil
			}
			return workspaceForwardChangedMsg{gen: gen}
		}
	}
}

// forwardedPorts is what the header draws its arrows from: the sandbox port a
// local one stands in for. Empty while nothing is forwarded, which is also what
// every screen but the workspace sees.
func (m *Model) forwardedPorts() map[int]int {
	if m.forward == nil {
		return nil
	}
	bindings := m.forward.Bindings()
	if len(bindings) == 0 {
		return nil
	}
	forwarded := make(map[int]int, len(bindings))
	for _, binding := range bindings {
		forwarded[binding.Port] = binding.Local
	}
	return forwarded
}

// listExecs asks the server what is running in the workspace's discobox.
func (m *Model) listExecs(gen int) tea.Cmd {
	ctx, ds, id := m.ctx, m.ds, m.paneBox.ID
	return func() tea.Msg {
		execs, err := ds.Execs(ctx, id)
		return workspaceExecsMsg{gen: gen, execs: execs, err: err}
	}
}

// workspaceTick schedules the next poll. The generation is the loop's own
// off-switch: detaching bumps it, and a tick from a workspace that is gone is
// dropped rather than answered.
func (m *Model) workspaceTick(gen int) tea.Cmd {
	return tea.Tick(workspacePollEvery, func(time.Time) tea.Msg {
		return workspaceTickMsg{gen: gen}
	})
}

// workspaceExecs reconciles the tabs against one answer from the listing:
// every live TTY session that is not the primary and not already on screen is
// opened as a tab. Nothing is ever closed here — the attach streams deliver
// their own exits, and a held pane is a local artifact the listing knows
// nothing about — so each transition has exactly one writer.
//
// A listing that failed is not reported. The workspace is opened on a sandbox
// that may still be provisioning, where the listing is answered "not yet" and
// only the attach waits for the sandbox to become reachable (ADR 0039) — so
// the listing's error would be noise the user cannot act on, and the poll
// retries it seconds later. The attach is the reporter: it waits, and if it
// fails the workspace closes saying why.
func (m *Model) workspaceExecs(msg workspaceExecsMsg) tea.Cmd {
	if msg.gen != m.wsGen {
		return nil
	}
	var cmds []tea.Cmd
	first := m.primary() == nil && !m.connecting[ExecPrimary]
	if first {
		// The primary is opened from the first answer whatever it says: a
		// workspace is above all a view onto that session, and the sandbox
		// resolves — and revives — it under the virtual id.
		m.connecting[ExecPrimary] = true
	}

	var open []Exec
	newShells := 0
	if msg.err == nil {
		for _, exec := range msg.execs {
			// A service is drawn like any other tab but is not a TTY session:
			// it runs on pipes, because its output is read rather than typed
			// at (ADR 0063 §3). That is the one place the tab strip stops
			// being "every live TTY session" (ADR 0054 §2).
			if !exec.Live || exec.Primary || exec.ID == "" {
				continue
			}
			if !exec.Tty && exec.Service == "" {
				continue
			}
			if m.paneByExec(exec.ID) != nil || m.connecting[exec.ID] {
				continue
			}
			open = append(open, exec)
			if !terminalExec(exec) {
				newShells++
			}
		}
		sort.SliceStable(open, func(i, j int) bool { return execBefore(open[i], open[j]) })
	}

	// Sized for the box they will be drawn in, counting the shells about to
	// arrive as well as the ones already here: the full window when there is
	// one box, the halves when there are two. Only the shells decide that —
	// another terminal is a tab in the box the primary already has. See
	// columns.
	term, shells := m.columns(newShells + m.shells.len())
	if first {
		cmds = append(cmds, m.openExec(msg.gen, Exec{ID: ExecPrimary, Primary: true}, term))
	}
	for _, exec := range open {
		m.connecting[exec.ID] = true
		width := shells
		if terminalExec(exec) {
			width = term
		}
		cmds = append(cmds, m.openExec(msg.gen, exec, width))
	}
	return tea.Batch(cmds...)
}

// terminalExec reports whether a session belongs on the workspace's left: the
// primary, or another of the discobox's harness terminals. Everything else is
// a shell and goes on the right.
//
// It is the server's own record that answers, not a layout this window keeps:
// a terminal is created in harness mode and carries the harness it runs, so
// reopening the workspace draws the same two columns anyone else's window
// would.
func terminalExec(exec Exec) bool {
	// A service is a right-hand tab whatever else its record says: it is a
	// process the discobox runs for you, not a session of the harness you are
	// talking to.
	if exec.Service != "" {
		return false
	}
	return exec.Primary || exec.Harness != ""
}

// serviceExec reports whether a session is one of the discobox's declared
// services, and so whether its pane is read-only and its lifecycle keys apply.
func serviceExec(exec Exec) bool { return exec.Service != "" }

// openExec connects one workspace terminal. The pane is sized before it is
// opened: the size is what the far end is told, and a terminal that starts at
// the wrong size draws itself wrong before anything can correct it.
func (m *Model) openExec(gen int, exec Exec, width int) tea.Cmd {
	cols, rows := m.paneCells(width)
	ctx, ds, id := m.ctx, m.ds, m.paneBox.ID
	return func() tea.Msg {
		term, err := ds.OpenExec(ctx, id, exec.ID, cols, rows)
		return workspaceTermMsg{gen: gen, exec: exec, term: term, err: err}
	}
}

// newShell creates a fresh shell in the discobox and opens it as a focused
// tab. The tab is keyed by the exec id the server hands back, which is what
// keeps the poll from opening a second pane onto it when the listing catches
// up.
func (m *Model) newShell() tea.Cmd {
	m.busy = "shell…"
	gen := m.wsGen
	_, shells := m.columns(m.shells.len() + 1)
	cols, rows := m.paneCells(shells)
	ctx, ds, id := m.ctx, m.ds, m.paneBox.ID
	return func() tea.Msg {
		exec, term, err := ds.NewShell(ctx, id, cols, rows)
		return workspaceTermMsg{gen: gen, exec: exec, term: term, err: err, asked: InteractShell, focus: true}
	}
}

// newTerminal opens another of the discobox's own terminals — the same harness
// the primary runs — as a focused tab beside it. It is the left column's
// counterpart of newShell, and like it the tab is keyed by the exec id the
// server hands back, which is what keeps the poll from opening a second pane
// onto the same session.
//
// The left box does not change width for it: another terminal is a tab in the
// box the primary already has, not a third column.
func (m *Model) newTerminal() tea.Cmd {
	m.busy = "terminal…"
	gen := m.wsGen
	term, _ := m.columns(m.shells.len())
	cols, rows := m.paneCells(term)
	ctx, ds, id := m.ctx, m.ds, m.paneBox.ID
	return func() tea.Msg {
		exec, term, err := ds.NewTerminal(ctx, id, cols, rows)
		return workspaceTermMsg{gen: gen, exec: exec, term: term, err: err, asked: InteractTerminal, focus: true}
	}
}

// workspaceTermOpened starts drawing one connected workspace session, or
// reports why there is none. A failed terminal or shell degrades to a report —
// the workspace is still a workspace without it, and the poll retries while
// the listing still says it is live — but a failed primary closes the screen:
// a workspace without its primary terminal is not one.
func (m *Model) workspaceTermOpened(msg workspaceTermMsg) tea.Cmd {
	if msg.gen != m.wsGen {
		// From a workspace that has since been left; its stream must not leak.
		if msg.term != nil {
			_ = msg.term.Close()
		}
		return nil
	}
	// A connected session is the end of the wait this screen narrates: the
	// discobox agent accepts the attach only once the terminal is launched and
	// installed, so there is nothing left to say about getting here.
	m.endNarration()
	m.busy = ""
	delete(m.connecting, msg.exec.ID)
	primary := msg.exec.ID == ExecPrimary
	terminal := primary || terminalExec(msg.exec) || msg.asked == InteractTerminal

	// What this pane is, in the words the list uses: the primary is the
	// attach, another harness session is a terminal, and everything else is a
	// shell. A create that failed carries no session record, so there what was
	// asked for is what is reported.
	action := InteractShell
	switch {
	case primary:
		action = InteractAttach
	case serviceExec(msg.exec):
		action = InteractService
	case msg.asked != "":
		action = msg.asked
	case terminal:
		action = InteractTerminal
	}

	if msg.err != nil {
		if primary {
			m.closeWorkspace()
		}
		// Re-fit what did open: a primary sized for a split whose tabs never
		// arrived should take the width back.
		m.layout()
		return m.report(true, "%s: %v", action, msg.err)
	}

	if !primary {
		if existing := m.paneByExec(msg.exec.ID); existing != nil {
			// The poll and a leader key race onto the same session; the tab
			// that arrived first is the tab.
			_ = msg.term.Close()
			if msg.focus {
				m.focusPane(existing)
			}
			return nil
		}
	}

	m.nextPaneID++
	// A service runs on pipes, so its pane draws and is navigated but is never
	// typed at: there is no stdin at the far end to reach (ADR 0063 §7).
	service := serviceExec(msg.exec)
	// The primary is attached under the virtual id, which carries no session
	// record to name it by: until its harness titles its own terminal it is
	// what it is, the attach.
	title := ""
	if !primary {
		title = execTitle(msg.exec)
	}
	p := &pane{
		id:      m.nextPaneID,
		term:    termpane.New(m.paneOptions(false, service)...),
		stream:  msg.term,
		action:  action,
		sandbox: m.paneBox,
		execID:  msg.exec.ID,
		title:   title,
		primary: primary,
		service: msg.exec.Service,
	}

	col := &m.shells
	if terminal {
		col = &m.terminals
	}
	at := col.insert(p, msg.exec, m.column() == col)
	if msg.focus {
		m.onShells = col == &m.shells
		col.active = at
	}
	// Focus moves to the screen only once there is a screen: a tab that lands
	// before the primary keeps emulating off-screen until it does.
	if m.inPanes() {
		m.focus = focusPane
		m.prompt.Blur()
	}
	m.layout()
	return tea.Batch(
		fromPane(p.id, p.term.Attach(msg.term)),
		fromPane(p.id, m.paneEvents(msg.term)),
	)
}

// execBefore orders tabs: by when their sessions were created, oldest first,
// with the id as the tie-break so the order is stable whatever the listing's.
func execBefore(a, b Exec) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.ID < b.ID
}

// paneByExec finds the pane drawing the given session on either side of the
// workspace, held panes included.
func (m *Model) paneByExec(execID string) *pane {
	if p := m.terminals.byExec(execID); p != nil {
		return p
	}
	return m.shells.byExec(execID)
}

// closeTab takes one pane off the screen: its stream is closed and it leaves
// its column. It is how a held pane is dismissed and how an errored one is
// dropped — never how a running session is ended, which is the session's own
// to do.
func (m *Model) closeTab(p *pane) {
	col, i := m.paneColumn(p)
	if col == nil {
		return
	}
	col.close(i)
	if m.shells.len() == 0 {
		// Nothing left to share the window with, so there is nothing left to
		// maximize over either; the terminals take it back on their own.
		m.onShells = false
		m.maximized = false
	}
	m.layout()
}

// closeWorkspace leaves the workspace: every stream is closed at once — the
// overlay, the terminal, every tab — and every session keeps running. Bumping
// the generation is what ends the poll and orphans any open still in flight.
func (m *Model) closeWorkspace() {
	m.wsGen++
	m.connecting = nil
	m.endNarration()
	m.busy = ""
	if m.forward != nil {
		// The local ports go with the screen that opened them. A forward left
		// running behind a closed workspace is a listener nothing on screen
		// accounts for.
		_ = m.forward.Close()
		m.forward = nil
	}
	if m.overlay != nil {
		_ = m.overlay.term.Close()
		m.overlay = nil
	}
	m.terminals.closeAll()
	m.shells.closeAll()
	m.onShells = false
	m.maximized = false
	m.leavePanes()
}

// panes is the whole screen as one strip, left to right: the terminals, the
// primary first, then the shells. It is what the leader's movement and its
// digits count along, so a pane wears the same number wherever the focus
// happens to be.
func (m *Model) panes() []*pane {
	return append(append([]*pane{}, m.terminals.panes...), m.shells.panes...)
}

// paneOrdinal is where the focused pane sits in that strip, or -1 when nothing
// on the workspace has the keys.
func (m *Model) paneOrdinal() int {
	if m.column().visible() == nil {
		return -1
	}
	if m.onShells {
		return m.terminals.len() + m.shells.active
	}
	return m.terminals.active
}

// focusOrdinal puts the keys on the pane wearing that number, clamped to the
// strip's ends.
func (m *Model) focusOrdinal(n int) {
	n = min(max(n, 0), max(m.terminals.len()+m.shells.len()-1, 0))
	if n < m.terminals.len() {
		m.onShells, m.terminals.active = false, n
		return
	}
	m.onShells, m.shells.active = true, n-m.terminals.len()
}

// movePane shifts focus along that strip. It stops at the ends rather than
// wrapping — the screen has a left and a right, and a focus that jumps the
// long way round is one you have to look for.
func (m *Model) movePane(delta int) tea.Cmd {
	at := m.paneOrdinal()
	if at < 0 {
		return nil
	}
	m.focusOrdinal(at + delta)
	return nil
}

// jumpPane focuses a pane by the number its label wears: 0 is the primary
// terminal, all the way on the left, and the rest are counted across the
// screen from it. A number with no pane under it is answered rather than
// swallowed — a jump that lands nowhere should say so.
func (m *Model) jumpPane(n int) tea.Cmd {
	if n >= m.terminals.len()+m.shells.len() {
		return status("no pane %d", n)
	}
	m.focusOrdinal(n)
	return nil
}

// execTitle is what to call a tab before its application names itself: the
// program its session is running, the harness it is, or as a last resort the
// tail of its id.
func execTitle(exec Exec) string {
	// A service's name is the one thing about it a person chose, and its argv
	// is the login shell every service shares.
	if name := strings.TrimSpace(exec.ServiceName); name != "" && exec.Service != "" {
		return name
	}
	if exec.Service != "" {
		return exec.Service
	}
	if len(exec.Command) > 0 {
		if base := path.Base(exec.Command[0]); base != "" && base != "." && base != "/" {
			return base
		}
	}
	if exec.Harness != "" {
		return exec.Harness
	}
	if len(exec.ID) > 6 {
		return exec.ID[len(exec.ID)-6:]
	}
	return exec.ID
}

// tabBase is where a column's numbering starts: the terminals from 0, the
// primary among them, and the shells straight after them. One press of the
// leader and a digit therefore means one pane on the whole screen.
func (m *Model) tabBase(shells bool) int {
	if shells {
		return m.terminals.len()
	}
	return 0
}

// tabbedEdge is a column's top border with its tab strip laid into it — a
// titledEdge with several titles: `╭─[ 1 zsh ]─[ 2 claude ]───╮`. The border is
// a line the eye already follows, so the strip costs no row, and both grids
// come out the same height.
//
// The visible tab is lit and never clipped; when the strip outgrows the line,
// a window of tabs around it is shown with an ellipsis at the clipped end.
//
// left is the screen column the box starts at, so a click on a label can be
// routed back to the tab that drew it whichever side it is on, and control is
// the box's already-drawn maximize button, which shares the line.
func (m *Model) tabbedEdge(col *column, shells bool, left int, control string, edge lipgloss.Style, inner int) string {
	rule := func(n int) string { return strings.Repeat("─", max(n, 0)) }
	// The maximize button shares the line, at the far end; the strip gets what
	// it leaves. See zoomControl.
	reserve := 0
	if control != "" {
		reserve = lipgloss.Width(control) + 1
	}
	tail := func(cells int) string {
		out := edge.Render(rule(inner - cells - reserve))
		if control != "" {
			out += control + edge.Render("─")
		}
		return out + edge.Render("╮")
	}
	// The strip is a click target as well as a label row, so where each tab
	// lands is recorded as it is drawn — the drawing loop is the one place
	// that knows. Columns are box-relative: the corner is cell zero.
	base := m.tabBase(shells)
	labels := make([]string, col.len())
	widths := make([]int, col.len())
	for i := range col.panes {
		labels[i] = col.label(i, base)
		widths[i] = len([]rune(labels[i])) + 4 // brackets and their padding
	}

	active := min(max(col.active, 0), len(labels)-1)
	avail := inner - 2 - reserve // a cell of rule survives at each end
	if widths[active] > avail {
		// Even alone the visible tab does not fit: shorten its label as a last
		// resort, and below any readable size give the row back to the rule.
		keep := avail - 4
		if keep < 1 {
			return edge.Render("╭") + tail(0)
		}
		labels[active] = string([]rune(labels[active])[:keep])
		widths[active] = keep + 4
	}

	// The window grows outward from the visible tab while there is room, a
	// cell of rule between neighbors.
	lo, hi, used := active, active, widths[active]
	for {
		grew := false
		if lo > 0 && used+1+widths[lo-1] <= avail {
			lo--
			used += 1 + widths[lo]
			grew = true
		}
		if hi < len(labels)-1 && used+1+widths[hi+1] <= avail {
			hi++
			used += 1 + widths[hi]
			grew = true
		}
		if !grew {
			break
		}
	}

	var out strings.Builder
	cells := 0
	out.WriteString(edge.Render("╭"))
	if lo > 0 {
		out.WriteString(edge.Render("…"))
	} else {
		out.WriteString(edge.Render("─"))
	}
	cells++
	for i := lo; i <= hi; i++ {
		if i > lo {
			out.WriteString(edge.Render("─"))
			cells++
		}
		st := m.st.dimText
		if i == active {
			st = m.st.headerBar
		}
		out.WriteString(edge.Render("["))
		out.WriteString(st.Render(" " + labels[i] + " "))
		out.WriteString(edge.Render("]"))
		m.tabSpans = append(m.tabSpans, tabSpan{
			shells: shells,
			index:  i,
			start:  left + 1 + cells,
			end:    left + cells + widths[i],
		})
		cells += widths[i]
	}
	if hi < len(labels)-1 {
		out.WriteString(edge.Render("…"))
		cells++
	}
	out.WriteString(tail(cells))
	return out.String()
}
