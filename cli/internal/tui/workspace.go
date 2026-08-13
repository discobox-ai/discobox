package tui

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/obot-platform/discobox/termpane"
)

// The workspace screen is the discobox as the server has it, not as this
// window remembers it. Opening it attaches to the primary terminal and to
// every other live TTY session the discobox has; while it is open, a poll of
// the exec listing keeps the tabs following the server, so a shell started
// from another window appears here on its own. Detaching leaves the workspace:
// every stream is closed at once, and every session keeps running.
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

// workspaceTermMsg carries one connected workspace terminal back to the
// model: the primary, an existing session's attach, or a shell the leader
// asked for — which arrives with focus.
type workspaceTermMsg struct {
	gen   int
	exec  Exec
	term  Terminal
	err   error
	focus bool
}

// workspaceTickMsg asks for the next poll of the exec listing.
type workspaceTickMsg struct{ gen int }

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
	// even when nothing has asked for the list yet.
	m.expanded = true
	m.busy = "attach…"
	m.connecting = map[string]bool{}
	gen := m.wsGen
	cmds := []tea.Cmd{m.listExecs(gen), m.workspaceTick(gen)}
	if freshShell {
		cmds = append(cmds, m.newShell())
	}
	return tea.Batch(cmds...)
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
func (m *Model) workspaceExecs(msg workspaceExecsMsg) tea.Cmd {
	if msg.gen != m.wsGen {
		return nil
	}
	var cmds []tea.Cmd
	first := m.terminal == nil && !m.connecting[ExecPrimary]
	if first {
		// The primary is opened from the first answer whatever it says: a
		// workspace is above all a view onto that session, and the sandbox
		// resolves — and revives — it under the virtual id.
		m.connecting[ExecPrimary] = true
	}

	var open []Exec
	if msg.err == nil {
		for _, exec := range msg.execs {
			if !exec.Live || !exec.Tty || exec.Primary || exec.ID == "" {
				continue
			}
			if m.shellByExec(exec.ID) != nil || m.connecting[exec.ID] {
				continue
			}
			open = append(open, exec)
		}
		sort.SliceStable(open, func(i, j int) bool { return execBefore(open[i], open[j]) })
	}

	if first {
		// Sized for the box it will be drawn in: the full window when the
		// workspace has no tabs, the left half when it does.
		width := m.width
		if len(open) > 0 || len(m.shells) > 0 {
			width = m.width / 2
		}
		cmds = append(cmds, m.openExec(msg.gen, Exec{ID: ExecPrimary, Primary: true}, width))
		if msg.err != nil {
			cmds = append(cmds, m.report(true, "cannot list sessions: %v", msg.err))
		}
	}
	for _, exec := range open {
		m.connecting[exec.ID] = true
		cmds = append(cmds, m.openExec(msg.gen, exec, m.width-m.width/2))
	}
	return tea.Batch(cmds...)
}

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
	cols, rows := m.paneCells(m.width - m.width/2)
	ctx, ds, id := m.ctx, m.ds, m.paneBox.ID
	return func() tea.Msg {
		exec, term, err := ds.NewShell(ctx, id, cols, rows)
		return workspaceTermMsg{gen: gen, exec: exec, term: term, err: err, focus: true}
	}
}

// workspaceTermOpened starts drawing one connected workspace terminal, or
// reports why there is none. A failed shell degrades to a report — the
// workspace is still a workspace without it, and the poll retries while the
// listing still says it is live — but a failed primary closes the screen: a
// workspace without its terminal is not one.
func (m *Model) workspaceTermOpened(msg workspaceTermMsg) tea.Cmd {
	if msg.gen != m.wsGen {
		// From a workspace that has since been left; its stream must not leak.
		if msg.term != nil {
			_ = msg.term.Close()
		}
		return nil
	}
	m.busy = ""
	delete(m.connecting, msg.exec.ID)
	primary := msg.exec.ID == ExecPrimary

	if msg.err != nil {
		action := InteractShell
		if primary {
			m.closeWorkspace()
			action = InteractAttach
		}
		// Re-fit what did open: a primary sized for a split whose tabs never
		// arrived should take the width back.
		m.layout()
		return m.report(true, "%s: %v", action, msg.err)
	}

	if !primary {
		if existing := m.shellByExec(msg.exec.ID); existing != nil {
			// The poll and a leader-s race onto the same session; the tab that
			// arrived first is the tab.
			_ = msg.term.Close()
			if msg.focus {
				m.onShells = true
				m.activeShell = m.shellIndex(existing)
			}
			return nil
		}
	}

	m.nextPaneID++
	action := InteractShell
	if primary {
		action = InteractAttach
	}
	p := &pane{
		id:      m.nextPaneID,
		term:    termpane.New(m.paneOptions(false)...),
		stream:  msg.term,
		action:  action,
		sandbox: m.paneBox,
		execID:  msg.exec.ID,
		title:   execTitle(msg.exec),
	}

	if primary {
		m.terminal = p
	} else {
		at := m.insertShell(p, msg.exec)
		if msg.focus {
			m.onShells = true
			m.activeShell = at
		}
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

// insertShell puts a tab where its session's age says it goes, so the strip
// holds its order as the listing changes around it, and returns where it
// landed.
func (m *Model) insertShell(p *pane, exec Exec) int {
	p.created = exec.CreatedAt
	at := len(m.shells)
	for i, s := range m.shells {
		if execBefore(exec, Exec{ID: s.execID, CreatedAt: s.created}) {
			at = i
			break
		}
	}
	m.shells = append(m.shells, nil)
	copy(m.shells[at+1:], m.shells[at:])
	m.shells[at] = p
	if at <= m.activeShell && m.onShells {
		// The tab being worked in stays the tab being worked in; an arrival
		// must not move the keys onto a different session mid-word.
		m.activeShell++
	}
	m.activeShell = min(m.activeShell, len(m.shells)-1)
	return at
}

// execBefore orders tabs: by when their sessions were created, oldest first,
// with the id as the tie-break so the order is stable whatever the listing's.
func execBefore(a, b Exec) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.ID < b.ID
}

// shellByExec finds the tab drawing the given session, held panes included.
func (m *Model) shellByExec(execID string) *pane {
	for _, p := range m.shells {
		if p.execID == execID {
			return p
		}
	}
	return nil
}

// shellIndex is where a tab sits in the strip, or -1 when it is not one.
func (m *Model) shellIndex(p *pane) int {
	for i, s := range m.shells {
		if s == p {
			return i
		}
	}
	return -1
}

// closeShell ends one tab's stream and takes it out of the strip. It is how a
// held pane is dismissed and how an errored one is dropped — never how a
// running session is ended: that is the session's own to do.
func (m *Model) closeShell(i int) {
	if i < 0 || i >= len(m.shells) {
		return
	}
	_ = m.shells[i].term.Close()
	m.shells = append(m.shells[:i], m.shells[i+1:]...)
	if i < m.activeShell {
		m.activeShell--
	}
	m.activeShell = min(m.activeShell, max(len(m.shells)-1, 0))
	if len(m.shells) == 0 {
		m.onShells = false
	}
	m.layout()
}

// closeWorkspace leaves the workspace: every stream is closed at once — the
// overlay, the terminal, every tab — and every session keeps running. Bumping
// the generation is what ends the poll and orphans any open still in flight.
func (m *Model) closeWorkspace() {
	m.wsGen++
	m.connecting = nil
	m.busy = ""
	if m.overlay != nil {
		_ = m.overlay.term.Close()
		m.overlay = nil
	}
	if m.terminal != nil {
		_ = m.terminal.term.Close()
		m.terminal = nil
	}
	for _, p := range m.shells {
		_ = p.term.Close()
	}
	m.shells = nil
	m.activeShell = 0
	m.onShells = false
	m.leavePanes()
}

// movePane shifts focus along the strip the screen is: the terminal, then the
// shell tabs one by one. It stops at the ends rather than wrapping — the
// screen has a left and a right, and a focus that jumps the long way round is
// one you have to look for.
func (m *Model) movePane(delta int) tea.Cmd {
	if !m.onShells {
		if delta > 0 && len(m.shells) > 0 {
			m.onShells = true
		}
		return nil
	}
	next := m.activeShell + delta
	if next < 0 {
		m.onShells = false
		return nil
	}
	if next >= len(m.shells) {
		return nil
	}
	m.activeShell = next
	return nil
}

// jumpPane focuses a pane by the number its label wears: 0 is the terminal,
// and 1 through 9 the tab strip's ordinals. A number with no tab under it is
// answered rather than swallowed — a jump that lands nowhere should say so.
func (m *Model) jumpPane(n int) tea.Cmd {
	if n == 0 {
		m.onShells = false
		return nil
	}
	if n > len(m.shells) {
		return status("no tab %d", n)
	}
	m.onShells = true
	m.activeShell = n - 1
	return nil
}

// execTitle is what to call a tab before its application names itself: the
// program its session is running, the harness it is, or as a last resort the
// tail of its id.
func execTitle(exec Exec) string {
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

// shellTabTitle is one tab's label: its place in the strip, what it is
// running, and whether it is over.
func (m *Model) shellTabTitle(i int) string {
	p := m.shells[i]
	title := strings.TrimSpace(p.term.Title())
	if title == "" {
		title = p.title
	}
	if title == "" {
		title = string(p.action)
	}
	title = fmt.Sprintf("%d %s", i+1, title)
	if p.exited {
		title += " ·done"
	}
	return title
}

// viewShellBox draws the workspace's right half: the visible tab's grid, with
// the whole strip laid into the top border. Only the visible tab is drawn; the
// others keep emulating off-screen, so flipping to one shows where it is now.
func (m *Model) viewShellBox(width int, focused bool) string {
	p := m.shells[min(m.activeShell, len(m.shells)-1)]
	edge := m.st.frame
	if !focused {
		edge = m.st.rule
	}
	inner := max(width-2, 1)
	grid := max(inner-2*boxPad, 1)
	side := edge.Render("│")
	pad := strings.Repeat(" ", boxPad)

	rows := []string{m.tabbedEdge(edge, inner)}
	for _, line := range p.term.View() {
		rows = append(rows, side+pad+padANSI(line, grid)+pad+side)
	}
	rows = append(rows, edge.Render("╰"+strings.Repeat("─", inner)+"╯"))
	return strings.Join(rows, "\n")
}

// tabbedEdge is the shell box's top border with the tab strip laid into it —
// a titledEdge with several titles: `╭─[ 1 zsh ]─[ 2 claude ]───╮`. The border
// is a line the eye already follows, so the strip costs no row, and both grids
// come out the same height.
//
// The visible tab is lit and never clipped; when the strip outgrows the line,
// a window of tabs around it is shown with an ellipsis at the clipped end.
func (m *Model) tabbedEdge(edge lipgloss.Style, inner int) string {
	rule := func(n int) string { return strings.Repeat("─", max(n, 0)) }
	labels := make([]string, len(m.shells))
	widths := make([]int, len(m.shells))
	for i := range m.shells {
		labels[i] = m.shellTabTitle(i)
		widths[i] = len([]rune(labels[i])) + 4 // brackets and their padding
	}

	active := min(m.activeShell, len(labels)-1)
	avail := inner - 2 // a cell of rule survives at each end
	if widths[active] > avail {
		// Even alone the visible tab does not fit: shorten its label as a last
		// resort, and below any readable size give the row back to the rule.
		keep := avail - 4
		if keep < 1 {
			return edge.Render("╭" + rule(inner) + "╮")
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
		cells += widths[i]
	}
	if hi < len(labels)-1 {
		out.WriteString(edge.Render("…"))
		cells++
	}
	out.WriteString(edge.Render(rule(inner-cells) + "╮"))
	return out.String()
}
