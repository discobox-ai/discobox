package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/pkg/sourceref"
	"github.com/discobox-ai/discobox/termpane"
)

// The workspace tree is the discobox list itself, squeezed into a column down
// the left of the workspace, so the box on the right is switched in place
// rather than by going back to the list screen. It is the same sandboxList —
// its sections, nesting, filter, selection and verbs — drawn narrow, which
// drops its columns the way a narrow window does. Expanded, it is the whole
// home screen again over the workspace, which stays attached underneath.
//
// A workspace switched away from is parked, not closed — its streams stay
// attached and its panes keep emulating off screen — so switching back is
// instant. Only the live one is drawn, and only the live one polls; a parked
// workspace picks its poll back up when it is shown again.
//
// Under the list are the window's own pages — the harnesses and the secrets —
// drawn right of the tree in place of the workspace, and the new
// discobox card. Beside the tree there is nothing to detach to: a box is left
// by switching to another, and the window by quitting (detaches).

const (
	// treeWidth is the cells the tree takes from the left of the window,
	// border included.
	treeWidth = 38
	// treeMinScreen is the narrowest window the tree is drawn in: below it the
	// terminals need every cell.
	treeMinScreen = 90
)

// switchBoxMsg is the walk along the tree from a pane — the next box and the
// previous box commands: move to the discobox above or below.
type switchBoxMsg struct{ delta int }

// treeShown reports whether the tree is drawn: in the workspace, or while one
// is opening, and on a window wide enough to give it the cells. A window
// opened to be one attach has no tree until it is asked for (showAllBoxes).
func (m *Model) treeShown() bool {
	// A column maximized over the other takes the tree's cells too: it is the
	// one thing on screen until it is put back.
	if m.maximized && m.workspaceShown() {
		return false
	}
	return m.treeMode()
}

// treeMode reports whether the window is laid out around the tree, whether
// or not a maximized column has its cells for now.
func (m *Model) treeMode() bool {
	return m.treeAvailable() && !m.treeHidden && !m.treeExpanded
}

// treeAvailable reports whether the window could draw the tree: a workspace
// open or opening, in a window opened on the list and wide enough for it.
func (m *Model) treeAvailable() bool {
	if m.attach != nil || m.screenWidth < treeMinScreen {
		return false
	}
	return m.hasWorkspace() || m.wsOpening || m.pageOpen()
}

// The window right of the tree shows one page: a discobox's workspace, or
// one of the window's own — the harnesses, the secrets — or the
// new discobox card standing in place of either. The workspace under a page
// stays live, just not drawn, and is back the moment the page is put away.

// pageOpen reports whether one of the window's own pages, or the new
// discobox card, is open.
func (m *Model) pageOpen() bool {
	return m.harnessesOpen || m.secretsOpen || m.newOpen
}

// closePages puts every page away.
func (m *Model) closePages() {
	m.harnessesOpen, m.secretsOpen, m.optionsOpen = false, false, false
	m.closeNew()
}

// openNew opens the new discobox card: the composer, framed as a dialog and
// modal like one.
func (m *Model) openNew() {
	m.commands.Use("new")
	m.closePages()
	m.newOpen = true
	m.listFront, m.treeExpanded = false, false
	m.backToPrompt()
	m.layout()
}

// closeNew puts the new discobox card away, its draft kept in the composer.
func (m *Model) closeNew() {
	if !m.newOpen {
		return
	}
	m.newOpen = false
	if m.focus == focusPrompt {
		m.prompt.Blur()
		m.focus = focusPane
	}
}

// openPage opens the page a tree entry names, the keys going with it. The
// audit screen it is opened over goes, so the page is what is in front.
func (m *Model) openPage(a treeAction) tea.Cmd {
	m.closeAudit()
	m.listFront, m.treeExpanded = false, false
	switch a {
	case treeNew:
		m.openNew()
		return nil
	case treeHarnesses:
		m.closeNew()
		m.focus = focusPane
		return m.openHarnesses()
	case treeSecrets:
		m.closeNew()
		m.focus = focusPane
		return m.openSecrets()
	}
	return nil
}

// treePages are the tree's fixed entries under the list, in order, each the
// registry command that opens it. The first is the new discobox card; the
// rest are pages, each on its function key.
var treePages = []struct {
	act       treeAction
	label, id string
}{
	{treeNew, "+ new discobox", "new"},
	{treeHarnesses, "  harnesses", "harnesses"},
	{treeSecrets, "  secrets", "credentials"},
}

// treePageOf is the tree entry a command opens, if any.
func treePageOf(id string) (treeAction, bool) {
	for _, p := range treePages {
		if p.id == id {
			return p.act, true
		}
	}
	return 0, false
}

// treeEntry opens a tree entry's page, for its bare key (treeKey) and for the
// leader's, which reaches it from a pane, the audit screen, a workspace on its
// way up, or another page. It reports false where there is no tree to hold the
// entry — a window too narrow or opened on one attach, a tree put away, the
// run options over it — and for the page already up, whose own key puts it
// away. A column maximized over the tree does not stop it: the page is not the
// workspace, so the tree is drawn beside it (treeShown).
func (m *Model) treeEntry(a treeAction) (tea.Cmd, bool) {
	if !m.treeAvailable() || m.treeHidden || m.optionsOpen {
		return nil, false
	}
	if cur, ok := m.pageShowing(); ok && cur == a {
		return nil, false
	}
	return m.openPage(a), true
}

// pageShowing is the tree entry whose page is up, if any.
func (m *Model) pageShowing() (treeAction, bool) {
	if !m.pageOpen() {
		return 0, false
	}
	switch {
	case m.newOpen:
		return treeNew, true
	case m.harnessesOpen:
		return treeHarnesses, true
	case m.secretsOpen:
		return treeSecrets, true
	}
	return 0, false
}

// treeAction is what one of the tree's controls does.
type treeAction int

const (
	treeToggleExpand treeAction = iota
	treeHide
	treeBack
	treeFocus
	treeNew
	treeHarnesses
	treeSecrets
)

// treeAct does what a tree control was pressed for.
func (m *Model) treeAct(a treeAction) tea.Cmd {
	switch a {
	case treeToggleExpand:
		if !m.listFront {
			m.focusTree()
		}
		m.toggleTreeExpanded()
	case treeHide:
		m.treeHidden = true
		m.backToPanes()
		return status("list hidden — %s h from the leftmost pane brings it back", m.leader())
	case treeBack:
		// From a page that has the keys, back is putting it away.
		if !m.listFront && m.pageOpen() {
			m.closePages()
		}
		m.backToPanes()
	case treeFocus:
		m.focusTree()
	case treeNew, treeHarnesses, treeSecrets:
		return m.openPage(a)
	}
	return nil
}

// treeControls draws the tree's [+][x] (or, expanded, [-][x]) for a top edge
// width cells wide starting at screen column left, marking each in the zones
// being drawn into. Nothing is drawn while there is no workspace to fold back
// into.
func (m *Model) treeControls(edge lipgloss.Style, width, left int) string {
	if !m.treeAvailable() || (!m.treeExpanded && m.treeHidden) {
		return ""
	}
	glyph := "+"
	if m.treeExpanded {
		glyph = "-"
	}
	// `…[+][x]─╮`: the last bracket sits a rule cell short of the corner.
	end := left + width - 2
	start := end - 6
	m.zones.mark(hit{kind: hitTree, idx: int(treeToggleExpand)}, start, 0, 3, 1)
	m.zones.mark(hit{kind: hitTree, idx: int(treeHide)}, start+3, 0, 3, 1)
	button := func(g string, x int) string {
		style := m.st.dimText
		if m.zones.hovering(x, 0, 3, 1) {
			style = m.st.hover
		}
		return edge.Render("[") + style.Render(g) + edge.Render("]")
	}
	return button(glyph, start) + button("x", start+3)
}

// hasWorkspace reports whether a workspace is open, whether or not the list is
// in front of it.
func (m *Model) hasWorkspace() bool {
	return m.Terminals.Len() > 0 || m.toolShown != nil
}

// workspaceShown reports whether the workspace is drawn: open, and not covered
// by the expanded list.
func (m *Model) workspaceShown() bool {
	return (m.hasWorkspace() || (m.wsOpening && m.attach == nil)) && !m.treeExpanded && !m.pageOpen()
}

// focusTree gives the keys to the list in the tree, its cursor on the box on
// screen.
func (m *Model) focusTree() {
	if m.attach != nil {
		return
	}
	// The list is not drawn beside a maximized column; asking for it puts
	// the column back.
	if m.maximized {
		m.maximized = false
		m.layout()
	}
	m.listFront = true
	m.prompt.Blur()
	m.focus = focusList
	m.list.visited = true
	m.syncListCursor()
}

// backToPanes takes the keys from the list back to the page right of it —
// the workspace, or the window's own page that is up — collapsing the list if
// it was expanded.
func (m *Model) backToPanes() {
	m.listFront, m.treeExpanded = false, false
	m.optionsOpen = false
	m.prompt.Blur()
	m.focus = focusPane
	// The new discobox page, or the home screen when there is nothing right
	// of the tree to go back to, is the composer's. A workspace still opening
	// is something to go back to: the keys are the leader's until its pane
	// arrives (openWorkspace).
	if m.newOpen || (!m.hasWorkspace() && !m.wsOpening && !m.pageOpen()) {
		m.backToPrompt()
	}
	m.syncListCursor()
	m.layout()
}

// showLive puts away whatever page covers the live workspace and gives it
// the keys.
func (m *Model) showLive() {
	m.closePages()
	m.backToPanes()
}

// toggleTreeExpanded swaps the compact list for the whole home screen and back.
func (m *Model) toggleTreeExpanded() {
	m.treeExpanded = !m.treeExpanded
	m.closePages()
	if m.focus == focusPrompt && !m.treeExpanded {
		m.prompt.Blur()
		m.focus = focusList
	}
	m.layout()
}

// syncListCursor puts the list's cursor on the box the workspace is showing,
// which is what the tree draws as picked out while the panes have the keys.
func (m *Model) syncListCursor() {
	if m.listCursorTo(m.Box) {
		m.cursorOn = m.boxRef(m.Box)
	}
}

// listCursorTo puts the list's cursor on one box's row, reporting whether the
// list has it.
func (m *Model) listCursorTo(box console.Sandbox) bool {
	for i, s := range m.list.Rows() {
		if m.boxRef(s) == m.boxRef(box) {
			m.list.moveTo(i)
			return true
		}
	}
	return false
}

// treeKey answers a key the list would otherwise take while it is in the
// compact tree: Esc goes back to the panes rather than to a prompt that is not
// drawn, and the function keys of the home screen expand it first so the
// screen they open is the one drawn. It reports whether it took the key.
func (m *Model) treeKey(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	if !m.treeAvailable() || m.treeHidden || m.dialog != nil || m.optionsOpen {
		return nil, false
	}
	// The pages' keys open them right of the tree, from the list compact or
	// expanded, and from a page — they are the way between the pages.
	for _, p := range treePages[1:] {
		if keyName(msg) == bareKey(p.id) && (m.listFront || m.treeExpanded || m.pageOpen()) {
			return m.treeEntry(p.act)
		}
	}
	// The new discobox card is modal: Esc puts it away, and Tab, which would
	// walk off the composer to the list, goes nowhere.
	if m.newOpen && m.pageOpen() && !m.listFront {
		switch keyName(msg) {
		case "esc":
			m.closeNew()
			if m.hasWorkspace() {
				m.backToPanes()
			}
			m.layout()
			return nil, true
		case "tab":
			return nil, true
		}
		return nil, false
	}
	if !m.listFront || m.treeExpanded {
		return nil, false
	}
	switch keyName(msg) {
	case "esc":
		if m.focus == focusList && m.list.visual {
			return nil, false
		}
		m.backToPanes()
		return nil, true
	case bareKey("new"):
		m.openNew()
		return nil, true
	case "enter":
		// The box a page covers is still the live one: choosing it puts the
		// page away rather than attaching again.
		if cur := m.list.current(); cur != nil && m.boxRef(*cur) == m.boxRef(m.Box) && m.pageOpen() && m.hasWorkspace() {
			m.showLive()
			return nil, true
		}
	}
	return nil, false
}

// treeCost is the cells the tree takes from the window's left.
func (m *Model) treeCost() int {
	if m.treeShown() {
		return treeWidth
	}
	return 0
}

// syncWidth makes m.width the width the rest of the window draws in: the
// terminal's, less the tree when there is one. It reports whether it changed.
func (m *Model) syncWidth() bool {
	if m.screenWidth <= 0 {
		return false
	}
	w := m.screenWidth - m.treeCost()
	if w == m.width {
		return false
	}
	m.width = w
	return true
}

// boxUpKey and boxDownKey walk the tree with no leader, from anywhere beside
// it: the previous and next box commands' chords (console's registry says
// why they are Ctrl-Shift chords). On a terminal that cannot encode them the
// walk is the leader's ↑↓ alone. Every terminal that speaks the Kitty
// keyboard protocol, which the window always asks for, sends them as
// themselves.
var (
	boxUpKey   = chordKey("previous box")
	boxDownKey = chordKey("next box")
)

// switchBox moves the workspace to the discobox delta rows away in the tree.
// It stops at the ends rather than wrapping, as moving between panes does.
// A window with no tree has nothing to move along — one that is one attach's
// own never lands on another box, and one too narrow for the tree would
// switch to a box nobody can see listed — so there it does nothing.
//
// A walk does not start a box. Attaching to one that is not live is what starts
// it (attachable), and passing over a row on the way to another is not asking
// for that, so the walk stops on such a row with only the tree's cursor:
// the list takes the keys, the workspace stays on screen, and Enter starts it
// as it would from the list. A walk carries on from the list's cursor while the
// list has the keys, so the leader's ↑↓ step past it as before.
func (m *Model) switchBox(delta int) tea.Cmd {
	if !m.treeAvailable() {
		return nil
	}
	if !m.listFront {
		m.syncListCursor()
	}
	m.list.move(delta)
	next := m.list.current()
	if next == nil {
		return nil
	}
	if m.boxRef(*next) == m.boxRef(m.Box) {
		if m.listFront && m.hasWorkspace() {
			m.backToPanes()
		}
		return nil
	}
	return m.landOn(*next)
}

// landOn moves to a box other than the one on screen the way a step along the
// tree does — a walk's step, or a single click on its row: a live or parked one
// is switched to, and any other is only pointed at, the list's cursor on its
// row and the list in front, since attaching to it would start it. Enter, or a
// double click, is what goes on to start it (switchTo).
func (m *Model) landOn(box console.Sandbox) tea.Cmd {
	if m.paletteReveal != nil && m.paletteReveal.target != m.boxRef(box) {
		m.restorePaletteView()
	}
	if !box.Live() && m.parked[m.boxRef(box)] == nil {
		m.focusTree()
		m.listCursorTo(box)
		if box.Startable() {
			return status("%s is %s — enter starts it", sandboxLabel(box), box.State)
		}
		return status("%s is %s — enter attaches", sandboxLabel(box), box.State)
	}
	return m.switchTo(box)
}

// switchTo shows one discobox's workspace: its parked one when it has one,
// and otherwise a fresh attach — refused, with the reason, on a box the list
// would not attach to either.
func (m *Model) switchTo(box console.Sandbox) tea.Cmd {
	if m.boxRef(box) == m.boxRef(m.Box) && m.hasWorkspace() && m.pageOpen() {
		m.showLive()
		return nil
	}
	if m.boxRef(box) == m.boxRef(m.Box) && (m.inPanes() || m.wsOpening) {
		return nil
	}
	// The box whose workspace is open needs no attach to be shown, whatever
	// it can do now (console.Context.Workspace): it is parked and brought back
	// like any parked one.
	open := m.boxRef(box) == m.boxRef(m.Box) && m.hasWorkspace()
	if m.parked[m.boxRef(box)] == nil && !open {
		if !box.Attachable() {
			return status("%s: %s", sandboxLabel(box), console.AttachWhy([]console.Sandbox{box}))
		}
	}
	return m.openFromList(console.InteractAttach, box)
}

// parkWorkspace puts the open workspace aside with its streams attached. A
// workspace whose primary has not arrived yet has nothing worth keeping, and
// is closed instead.
//
// What only the screen runs goes as it does on a detach (endLiveScreen), and
// so do its local ports: they live exactly as long as the screen that shows
// them, and come back with it. Its push loop is the exception, and goes on
// under its PushGen while it is attached.
func (m *Model) parkWorkspace() {
	if !m.Parkable() {
		m.closeWorkspace()
		return
	}
	m.endLiveScreen()
	m.Park()
	ws := m.workspace
	m.workspace = workspace{Box: m.Box}
	if m.parked == nil {
		m.parked = map[console.BoxRef]*workspace{}
	}
	m.parked[m.boxRef(ws.Box)] = &ws
}

// unparkWorkspace puts a parked workspace back on screen and restarts what
// only the screen runs. Its push loop is already running, and is not started
// a second time.
func (m *Model) unparkWorkspace(ws *workspace) tea.Cmd {
	m.recordPaletteVisit(ws.Box)
	delete(m.parked, m.boxRef(ws.Box))
	m.workspace = *ws
	m.harnessesOpen = false
	m.connecting = map[string]bool{}
	m.listFront, m.treeExpanded = false, false
	m.closePages()
	m.focus = focusPane
	m.prompt.Blur()
	m.syncWidth()
	m.layout()
	gen := m.wsGen
	return tea.Batch(m.listExecs(gen), m.listServices(gen), m.workspaceTick(gen), m.startForward(gen))
}

// parkedPane finds a pane by id among the parked workspaces.
func (m *Model) parkedPane(id int) (*workspace, *pane) {
	for _, ws := range m.parked {
		for _, p := range ws.Attached() {
			if p.id == id {
				return ws, p
			}
		}
	}
	return nil, nil
}

// updateParkedPane keeps a parked pane emulating: its output goes into its
// grid and its read is re-armed, and nothing is drawn. A pane that ends is
// settled as paneClosed settles one on screen, so switching back shows what
// it would have: a primary takes its workspace with it, a tool its tab, and a
// shell or terminal that finished keeps its last screen to be read.
func (m *Model) updateParkedPane(ws *workspace, p *pane, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case termpane.ClosedMsg:
		box := sandboxLabel(ws.Box)
		switch p.End(msg.Err) {
		case console.EndsWorkspace:
			delete(m.parked, m.boxRef(ws.Box))
			ws.Release()
			if msg.Err != nil {
				return m.report(true, "%s: %s: %v", box, p.Action, msg.Err)
			}
			return status("%s: the session ended", box)
		case console.EndsHeld:
			return nil
		}
		if i := ws.Terminals.Index(p); i >= 0 {
			ws.Terminals.Close(i)
		} else if i := ws.Shells.Index(p); i >= 0 {
			ws.Shells.Close(i)
		}
		if ws.toolShown == p {
			ws.toolShown = nil
		}
		if msg.Err != nil {
			return m.report(true, "%s: %s: %v", box, p.Action, msg.Err)
		}
		return nil
	case paneEventMsg:
		return fromPane(p.id, m.paneEvents(p.stream))
	case termpane.DetachMsg, termpane.CopyMsg:
		return nil
	}
	term, cmd := p.term.Update(msg)
	p.term = term
	return fromPane(p.id, cmd)
}

// closeParked ends every parked workspace's streams, for a window on its way
// out.
func (m *Model) closeParked() {
	for id, ws := range m.parked {
		ws.Release()
		delete(m.parked, id)
	}
}

// shiftMouse moves a mouse event dx cells to the left, so the window under
// the tree reads it in its own coordinates.
func shiftMouse(msg tea.Msg, dx int) tea.Msg {
	switch ev := msg.(type) {
	case tea.MouseClickMsg:
		ev.X = max(ev.X-dx, 0)
		return ev
	case tea.MouseReleaseMsg:
		ev.X = max(ev.X-dx, 0)
		return ev
	case tea.MouseMotionMsg:
		ev.X = max(ev.X-dx, 0)
		return ev
	case tea.MouseWheelMsg:
		ev.X = max(ev.X-dx, 0)
		return ev
	}
	return msg
}

// treeMouse answers a mouse event while the tree is drawn. Over the tree it
// is the list's, read against the zones the tree was drawn with: the wheel
// moves its cursor wherever the keys are, and a press on a row switches to
// that discobox. Right of it, it goes on to the window in the window's
// coordinates, and a press there gives the keys back to the panes.
func (m *Model) treeMouse(msg tea.MouseMsg) (tea.Msg, tea.Cmd, bool) {
	mouse := msg.Mouse()
	// A resting pointer lights the tree's controls or the workspace's, never
	// both: each side keeps its own idea of where it is.
	if ev, ok := msg.(tea.MouseMotionMsg); ok && ev.Button == tea.MouseNone {
		if mouse.X < treeWidth {
			m.treeHoverX, m.treeHoverY = mouse.X, mouse.Y
			m.hoverX, m.hoverY = -1, -1
			return nil, nil, true
		}
		m.treeHoverX, m.treeHoverY = -1, -1
	}
	// A gesture the window holds — a drag in a pane, over its chrome, or in
	// the new discobox card's composer — is the window's to the end, wherever
	// the pointer has wandered: its release must reach what took the press.
	if mouse.X >= treeWidth || m.mouseCapture != 0 || m.chromeCapture || m.promptCapture {
		if _, ok := msg.(tea.MouseClickMsg); ok && m.listFront && !m.modalUp() {
			m.backToPanes()
		}
		return shiftMouse(msg, treeWidth), nil, false
	}
	if m.treeBlocked() {
		return nil, nil, true
	}
	where, ok := m.treeZones.beneathLinks(mouse.X, mouse.Y)
	if !ok {
		return nil, nil, true
	}
	switch ev := msg.(type) {
	case tea.MouseWheelMsg:
		if where.what.kind == hitRow {
			m.list.move(-wheelLines(ev))
		}
		return nil, nil, true
	case tea.MouseClickMsg:
		// The right button is the row's menu, as it is on the list screen,
		// without switching to it.
		if ev.Button == tea.MouseRight && where.what.kind == hitRow {
			rows := m.list.Rows()
			if i := where.what.idx; i >= 0 && i < len(rows) {
				m.list.moveTo(i)
				return nil, m.boxMenu(rows[i]), true
			}
		}
		if ev.Button != tea.MouseLeft {
			return nil, nil, true
		}
		switch where.what.kind {
		case hitTree:
			return nil, m.treeAct(treeAction(where.what.idx)), true
		case hitRow:
			rows := m.list.Rows()
			if where.what.idx < 0 || where.what.idx >= len(rows) {
				m.focusTree()
				return nil, nil, true
			}
			box := rows[where.what.idx]
			m.list.moveTo(where.what.idx)
			// A double click is Enter on the row: it attaches, starting a
			// box the first click only pointed at, and on the box on screen
			// — the one the first click just switched to, opened or still
			// opening — it gives that box the keys rather than the list.
			clicks, prev := m.countClick(mouse.X, mouse.Y), m.clickBox
			m.clickBox = m.boxRef(box)
			if clicks > 1 && prev == m.boxRef(box) {
				if m.boxRef(box) == m.boxRef(m.Box) {
					if m.hasWorkspace() || m.wsOpening {
						m.showLive()
					}
					return nil, nil, true
				}
				return nil, m.switchTo(box), true
			}
			if m.boxRef(box) == m.boxRef(m.Box) && m.pageOpen() && m.hasWorkspace() {
				m.showLive()
				return nil, nil, true
			}
			if m.boxRef(box) == m.boxRef(m.Box) {
				m.focusTree()
				return nil, nil, true
			}
			return nil, m.landOn(box), true
		default:
			// The filter, the band's keys: what they do on the list screen.
			m.focusTree()
			cmd, _ := m.press(where.what, 1)
			return nil, cmd, true
		}
	}
	return nil, nil, true
}

// viewTree draws the tree, m.height rows tall: the brand above its box, then
// in the box the filter, the list at the tree's width, disco when there is
// room, the pages, and the keys that move between it and the panes. It is
// drawn against zones of its own, since the window beside it reads the mouse
// in coordinates the tree is not in.
func (m *Model) viewTree() string {
	inner := treeWidth - 2
	saved := m.zones
	m.zones = m.treeZones
	m.zones.reset()
	m.zones.setHover(m.treeHoverX, m.treeHoverY)
	// The box sits a row in from the top and the bottom: its top edge level
	// with the workspace's panes.
	h := m.height - 2
	m.zones.push(0, 1)

	edge := m.st.rule
	if m.listFront {
		edge = m.st.frame
	}
	side := edge.Render("│")
	out := []string{titledEdge(m.st, edge, "discoboxes", m.treeControls(edge, treeWidth, 0), inner)}

	label := m.listFilterLabel()
	filter := m.styleFilter(label, false, m.focus == focusFilter)
	if filter != "" {
		fw := min(lipgloss.Width(filter)+1, inner)
		m.zones.mark(hit{kind: hitFilter}, 1, len(out), fw, 1)
		if m.zones.hovering(1, len(out), fw, 1) {
			filter = m.styleFilter(label, true, false)
		}
		out = append(out, side+padANSI(" "+filter, inner)+side)
	}

	// The pages are fixed under the list: a rule, then one row each.
	pagesRows := len(treePages) + 1
	m.list.width, m.list.height = inner, max(h-len(out)-2-pagesRows, 1)
	m.list.inTree = true
	m.list.clamp()
	m.zones.push(1, len(out))
	// The cursor is drawn whoever has the keys: with the panes it is the box
	// on screen, and with the list it is where the list is.
	// A page up is what the right side shows, so the box under it is not
	// picked out unless the list has the keys.
	body := m.list.view(m.st, &m.zones, m.listFront || !m.pageOpen())
	m.zones.pop()
	lines := strings.Split(body, "\n")
	for len(lines) > 0 && strings.TrimSpace(ansi.Strip(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	for _, row := range lines {
		if len(out) >= h-2-pagesRows {
			break
		}
		out = append(out, side+padANSI(row, inner)+side)
	}
	// Disco stands at the foot of the list, over the pages, when the rows the
	// boxes leave can hold him with a row of air above; otherwise he sits out.
	free := h - 2 - pagesRows - len(out)
	var disco []string
	if rows := m.logo.centeredRows(inner); len(rows) > 0 && free >= len(rows)+1 {
		disco = rows
	}
	for len(out) < h-2-pagesRows-len(disco) {
		out = append(out, side+strings.Repeat(" ", inner)+side)
	}
	for _, row := range disco {
		out = append(out, side+padANSI(row, inner)+side)
	}
	out = append(out, edge.Render("├"+strings.Repeat("─", inner)+"┤"))
	showing, up := m.pageShowing()
	for _, p := range treePages {
		y := len(out)
		m.zones.mark(hit{kind: hitTree, idx: int(p.act)}, 1, y, inner, 1)
		label := m.st.dimText
		if up && showing == p.act {
			label = m.st.headerBar
		}
		if m.zones.hovering(1, y, inner, 1) {
			label = m.st.hover
		}
		k := keyLabel(bareKey(p.id))
		// A key is shown only where it works: all of them with the list's
		// keys, and the function keys from a page too. See treeKey.
		if !m.listFront && (!m.pageOpen() || p.act == treeNew) {
			k = ""
		}
		key := m.st.dimText.Render(k)
		room := inner - 2 - lipgloss.Width(k)
		line := " " + padANSI(label.Render(p.label), room) + key + " "
		if up && showing == p.act {
			line = highlight(m.st, line, colHighlightBG)
		}
		out = append(out, side+line+side)
	}
	// The keys are buttons for themselves, as they are on every other screen.
	type offer struct {
		key, label string
		act        treeAction
	}
	offers := []offer{{m.leader() + " h", "list", treeFocus}, {m.leader() + " ↑↓", "switch", treeFocus}}
	if m.listFront {
		offers = []offer{{"esc", "back", treeBack}, {m.leader() + " z", "expand", treeToggleExpand}}
	}
	if m.newOpen && m.pageOpen() && !m.listFront {
		offers = []offer{{"esc", "close", treeBack}}
	}
	hint, x := " ", 2
	for i, o := range offers {
		if i > 0 {
			hint += m.st.dimText.Render(" · ")
			x += 3
		}
		text := o.key + " " + o.label
		w := lipgloss.Width(text)
		m.zones.mark(hit{kind: hitTree, idx: int(o.act)}, x, len(out), w, 1)
		style := m.st.dimText
		if m.zones.hovering(x, len(out), w, 1) {
			style = m.st.hover
		}
		hint += style.Render(text)
		x += w
	}
	out = append(out, side+padANSI(hint, inner)+side)
	out = append(out, edge.Render("╰"+strings.Repeat("─", inner)+"╯"))

	m.zones.pop()
	m.treeZones = m.zones
	m.zones = saved
	// A modal has the keys and the pointer, so the tree beside it is drawn
	// as what it is for now: something to read, not to press.
	// The row above names the window, level with the workspace header that
	// names the box.
	out = append(append([]string{padANSI(" "+m.viewHeaderBrand(), treeWidth)}, out...), strings.Repeat(" ", treeWidth))
	if m.treeBlocked() {
		for i, line := range out {
			out[i] = m.st.greyedOut.Render(ansi.Strip(line))
		}
	}
	return lipgloss.NewStyle().Width(treeWidth).MaxHeight(m.height).Render(strings.Join(out, "\n"))
}

// viewOpening is the workspace's body while it has no terminal yet: the box
// it is attaching to, and what the attach is waiting on.
func (m *Model) viewOpening(width, height int) string {
	inner := max(width-2, 1)
	what := m.busy
	if what == "" {
		what = "attach…"
	}
	out := []string{titledEdge(m.st, m.st.rule, sandboxLabel(m.Box), "", inner)}
	side := m.st.rule.Render("│")
	for i := 0; i < max(height-2, 0); i++ {
		row := ""
		if i == height/2-1 {
			row = m.st.dimText.Render(lipgloss.PlaceHorizontal(inner, lipgloss.Center, what))
		}
		out = append(out, side+padANSI(row, inner)+side)
	}
	out = append(out, m.st.rule.Render("╰"+strings.Repeat("─", inner)+"╯"))
	return strings.Join(out, "\n")
}

// focusEdge is the edge of a workspace box that has the window to itself:
// bright unless the list beside it has focus.
func (m *Model) focusEdge() lipgloss.Style {
	if m.listFront {
		return m.st.rule
	}
	return m.st.frame
}

// newCardInner is the width inside the new discobox card: a dialog's.
func newCardInner(window int) int {
	return max(dialogWidth(window)-dialogChromeWidth, 16)
}

// viewNewCard draws the new discobox card: the composer, framed as a dialog.
// Enter asks where it is cut from on the create card, then the new box takes
// the right side the way switching to one does.
func (m *Model) viewNewCard() string {
	inner := newCardInner(m.width)
	title := m.st.dialogTitle.Render("New discobox")
	// The dialog style's border and padding put the body at (3, 2); the
	// composer is under the title and the blank after it.
	m.zones.push(3, 4)
	composer := m.viewComposer(inner)
	m.zones.pop()
	parts := []string{title, "", composer, ""}
	if msg := m.statusMessage(inner); msg != "" {
		parts = append(parts, msg)
	}
	parts = append(parts, m.st.dimText.Render(truncate("Enter creates · Shift-Tab harness · "+keyLabel(optionsKey)+" options · Alt-E editor · Esc cancels", inner)))
	return m.st.dialog.Width(dialogWidth(m.width)).Render(strings.Join(parts, "\n"))
}

// detaches reports whether leader-d leaves the workspace for the list. Beside
// the tree it does not: the list is on screen already, a box is left by
// switching to another, and the window by quitting.
func (m *Model) detaches() bool {
	return !m.treeAvailable()
}

// boxMenuKey, behind the leader, is the actions menu for the box on screen —
// the menu `.` opens on a row of the list.
var boxMenuKey = bareKey("actions")

// boxMenuMsg is the leader's boxMenuKey in a pane.
type boxMenuMsg struct{}

// boxMenu opens the actions menu for one discobox, read from the list when it
// is there so the menu answers for the box as it is now.
func (m *Model) boxMenu(box console.Sandbox) tea.Cmd {
	for _, s := range m.list.Rows() {
		if m.boxRef(s) == m.boxRef(box) {
			box = s
			break
		}
	}
	targets := []console.Sandbox{box}
	// Any other box's menu is the list's own, attach and all.
	if m.boxRef(box) != m.boxRef(m.Box) || !m.hasWorkspace() {
		m.dialog = actionsDialog(console.TargetsTitle(targets), "", actions(targets), chooseAction(targets))
		return nil
	}
	// The box is already on screen, so attaching to it is not on offer; what
	// is, is what the workspace itself can open for it.
	ctx := m.commandContext()
	entry := func(id string) action {
		e, _ := m.commands.Entry(ctx, id, targets)
		return commandAction(e)
	}
	onScreen := []action{entry("services"), entry("tools"), entry("audit")}
	var items []action
	for _, a := range actions(targets) {
		switch a.key {
		case "attach":
		case "shell":
			items = append(append(items, a), onScreen...)
		default:
			items = append(items, a)
		}
	}
	if m.attach != nil {
		// A window opened on this one box can become the whole window, with
		// every discobox in the tree beside it and this one picked out.
		items = append(items, entry("all"))
	}
	// Every row is a registry command, dispatched on this box like the rest.
	m.dialog = actionsDialog(console.TargetsTitle(targets), "", items, chooseAction(targets))
	return nil
}

// allBoxesKey, in the box menu of a window opened on one box, turns it into
// the whole window: see showAllBoxes.
var allBoxesKey = menuKey("all")

// showAllBoxes turns a window opened on one discobox — `discobox attach`, or
// the run that made it — into the console: the tree beside the workspace,
// with its cursor on the box. The workspace is not reopened; it is the same
// one, now with the list beside it, and leaving it no longer leaves the
// window.
//
// The console's draft was kept out of the composer while the window was one
// command's own (restoreDraft); it is put back now, or the first save would
// write the empty composer over it.
func (m *Model) showAllBoxes() tea.Cmd {
	m.attach, m.oneRun = nil, false
	m.treeHidden, m.treeExpanded = false, false
	if m.syncWidth() {
		m.layout()
	}
	m.focusTree()
	return tea.Batch(m.refresh(), m.restoreDraft(m.draft))
}

// boxSourceLabel is where the box on screen was cut from, as the list spells
// a source.
func (m *Model) boxSourceLabel() string {
	box := m.Box
	if box.Source == "" {
		return console.NoSourceChoice
	}
	return sourceref.Display(box.Source, m.session.Home)
}

// edgeMessage lays the window's message into a box's bottom edge, `╰─ … ─╯`,
// a cell of rule in from the corner and a space either side of the words. An
// edge with no message is left as it is.
func (m *Model) edgeMessage(edge string, width int) string {
	room := width - 6
	if room < 4 {
		return edge
	}
	msg := m.statusMessage(room)
	if msg == "" {
		return edge
	}
	w := lipgloss.Width(msg)
	return ansi.Truncate(edge, 2, "") + " " + msg + " " + ansi.TruncateLeft(edge, 4+w, "")
}

// pageHeight is the height a page's own layout is measured against. Beside
// the tree a page is framed as the workspace is (pageFrame): the quit row
// above its box, the keys below, the message in its bottom edge and one blank
// row over the body. That leaves the body one row more than the full-window
// screen leaves it, which is this, less the screen's own chrome.
func (m *Model) pageHeight() int {
	if m.treeShown() {
		return m.height + 1
	}
	return m.height
}

// pageBodyTop is the row, inside the box, a page's body starts on.
func (m *Model) pageBodyTop() int {
	if m.treeShown() {
		return headerTop + 1
	}
	return headerTop + 2
}

// pageFrame draws a page beside the tree the way the workspace is drawn: quit
// above the box, where the workspace header has it; the body alone in the
// box, with the message laid into its bottom edge; and the page's keys under
// it, where the workspace's are. view() has pushed the origin a row down, to
// the box's top edge.
func (m *Model) pageFrame(body []string) string {
	pad := strings.Repeat(" ", boxPad)
	room := max(m.inner(), 1)

	quit := pressing(m.leader()+" "+paneQuitKey+" quit", m.leader(), paneQuitKey)
	if m.focus == focusPrompt {
		quit = keyed(keyLabel(quitAnywhereKey), quitAnywhereKey, "quit")
	}
	qw := lipgloss.Width(quit.text)
	qx := max(m.width-1-boxPad-qw, 0)
	m.zones.push(0, -1)
	// The server the page shows, where the full window's header has it (ADR
	// 0131 §2): the tree's filter is the list's, not the page's.
	left := " " + pad
	if server := m.viewFilter(false); server != "" {
		x, w := lipgloss.Width(left), lipgloss.Width(server)
		m.zones.mark(hit{kind: hitFilter}, x, 0, w, 1)
		if m.zones.hovering(x, 0, w, 1) {
			server = m.viewFilter(true)
		}
		left += server
	}
	left = ansi.Truncate(left, max(qx-1, 0), "")
	top := padANSI(padANSI(left, qx)+viewHints(m.st, &m.zones, []hint{quit}, qx, headerSep), m.width)
	m.zones.pop()

	box := strings.Split(m.box(append([]string{""}, body...)), "\n")
	box[len(box)-1] = m.edgeMessage(box[len(box)-1], m.width)

	m.zones.push(1+boxPad, len(box))
	keys := m.statusKeys(room)
	m.zones.pop()
	out := append([]string{top}, box...)
	out = append(out, " "+pad+padANSI(keys, room)+pad+" ")
	return strings.Join(out, "\n")
}

// listHasKeys reports whether the list in the tree drawn beside the window
// has the keys, so nothing right of it does: what the window's routing,
// hints and status line read.
func (m *Model) listHasKeys() bool {
	return m.listFront && m.treeShown()
}

// dropStaleListFront ends the list's hold on the keys once there is no tree
// for it to be in front in: a window narrowed past it, the last page closed
// with no workspace behind (the expanded home screen closes the pages). With
// something right of it, that gets the keys back — a list nobody can see
// keeps none, or every key typed would go to it. With nothing, the list is
// just the home screen's, and keeps them. layout runs it, after every key,
// click and resize, so no path leaves the hold stale.
func (m *Model) dropStaleListFront() {
	if !m.listFront || m.treeAvailable() {
		return
	}
	if m.hasWorkspace() || m.wsOpening || m.pageOpen() {
		m.backToPanes()
		return
	}
	m.listFront = false
}

// listInFront reports whether the list has the keys at all, drawn beside the
// right side or expanded over it (or with a column maximized over it): what
// the registry's rules read (console.Context.ListHasKeys), and what the next
// pane hands back from. It is listHasKeys wherever the tree is drawn.
func (m *Model) listInFront() bool {
	return m.listFront && m.treeAvailable()
}

// treeBlocked reports whether something modal has the input: the tree is
// drawn grey and takes no clicks.
func (m *Model) treeBlocked() bool {
	return m.modalUp() || m.newOpen
}
