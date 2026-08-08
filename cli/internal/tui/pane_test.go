package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// openPane drives the window into an attached pane on the first sandbox.
func openPane(t *testing.T, ds *fakeSource, act string) (*driver, *Model, *fakeTerminal) {
	t.Helper()
	t.Setenv("NO_COLOR", "1")
	m := New(t.Context(), ds)
	m.logo = logo{}
	m.expanded = true
	d := newDriver(t, m)
	d.start()
	d.wait("the listing", func() bool { return len(m.list.rows()) > 0 })

	d.key("tab")
	d.key(act)
	d.wait("the pane", func() bool { return m.focus == focusPane })
	if len(ds.terminals) != 1 {
		t.Fatalf("opened %d terminals, want 1", len(ds.terminals))
	}
	return d, m, ds.terminals[0]
}

// Attaching draws the sandbox's terminal in the window rather than handing the
// real terminal over to a command.
func TestAttachDrawsInTheWindow(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openPane(t, ds, "enter")

	if len(ds.interacts) != 0 {
		t.Fatalf("interacts = %v, want the window not to step aside", ds.interacts)
	}
	term.send("hello from the sandbox")
	d.wait("output", func() bool { return strings.Contains(frameText(m), "hello from the sandbox") })

	frame := frameText(m)
	// Inside the border, under a banner identifying the discobox and saying how
	// to get out.
	if !strings.Contains(frame, "sbx_one") {
		t.Errorf("the pane should identify its discobox:\n%s", frame)
	}
	if !strings.Contains(frame, "ctrl+c detach") {
		t.Errorf("the pane should say how to get out:\n%s", frame)
	}
	for _, line := range strings.Split(rawFrame(m), "\n") {
		if lipgloss.Width(line) != 120 {
			t.Fatalf("a pane row is %d cells, want 120: %q", lipgloss.Width(line), ansi.Strip(line))
		}
	}
}

// A shell is the other terminal a pane draws: a new interactive exec rather
// than the sandbox's own primary terminal.
func TestShellDrawsInTheWindow(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	_, _, _ = openPane(t, ds, "s")

	if len(ds.opens) != 1 || !strings.HasPrefix(ds.opens[0], "shell sbx_one ") {
		t.Fatalf("opens = %v", ds.opens)
	}
}

// The terminal is opened at the size of the pane it is going into: a terminal
// that starts at the wrong size draws itself wrong before anything can correct
// it.
func TestPaneOpensAtTheSizeItWillBeDrawnAt(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	_, m, term := openPane(t, ds, "enter")

	cols, rows := m.paneCells(len(m.panes))
	if got := term.size(); got != [2]int{cols, rows} {
		t.Fatalf("opened at %v, want %dx%d", got, cols, rows)
	}
	// The grid is the whole window bar its chrome, so it grows with it: the
	// border and a cell of air inside it on each side.
	if rows != m.height-4 || cols != m.width-2-2*boxPad {
		t.Fatalf("pane is %dx%d in a %d-row window", cols, rows, m.height)
	}
	if got := len(m.panes[0].term.View()); got != rows {
		t.Fatalf("the pane drew %d rows, want %d", got, rows)
	}
}

// Resizing the window resizes the terminal with it, both halves.
func TestResizingTheWindowResizesTheTerminal(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openPane(t, ds, "enter")

	d.dispatch(sizeMsg(100, 30))
	d.settle()
	cols, rows := m.paneCells(len(m.panes))
	if got := term.size(); got != [2]int{cols, rows} {
		t.Fatalf("resized to %v, want %dx%d", got, cols, rows)
	}
}

// Every key belongs to the sandbox while a pane is up — including the ones the
// window would otherwise use.
func TestKeysGoToTheSandbox(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, _, term := openPane(t, ds, "enter")

	// "t" is stop in the list, and must not stop anything here.
	d.key("t")
	if got := term.typed("t"); !strings.Contains(got, "t") {
		t.Fatalf("typed %q, want the key to reach the sandbox", got)
	}
	if len(ds.did) != 0 {
		t.Fatalf("did = %v, want the window not to act on the key", ds.did)
	}
}

// Detaching leaves the session running and puts the cursor back where it came
// from, on the sandbox it was opened on.
func TestDetachReturnsToTheList(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openPane(t, ds, "enter")

	d.key("ctrl+c")
	d.wait("the pane to close", func() bool { return len(m.panes) == 0 })
	if m.focus != focusList {
		t.Fatalf("focus = %v, want the list", m.focus)
	}
	if len(m.panes) > 0 {
		t.Fatal("the pane should be closed")
	}
	if !strings.Contains(m.status, "detached") {
		t.Fatalf("status = %q, want it to say the session is still running", m.status)
	}
	if got := m.list.current().ID; got != "sbx_one" {
		t.Fatalf("cursor on %s, want the sandbox the pane was opened on", got)
	}
}

// A session that ends on its own closes the pane and says so.
func TestEndedSessionClosesThePane(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openPane(t, ds, "enter")

	term.Close()
	d.wait("the pane to close", func() bool { return len(m.panes) == 0 })
	if m.focus != focusList {
		t.Fatalf("focus = %v, want the list", m.focus)
	}
}

// A terminal that cannot be opened reports on the status line and leaves the
// window where it was, rather than stranding it in an empty pane.
func TestFailedOpenIsReported(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.openErr = errors.New("sandbox is not running")
	t.Setenv("NO_COLOR", "1")
	m := New(t.Context(), ds)
	m.logo = logo{}
	d := newDriver(t, m)
	d.start()
	d.wait("the listing", func() bool { return len(m.list.rows()) > 0 })
	d.key("tab")
	d.key("enter")
	d.wait("the failure", func() bool { return m.statusE })

	if m.focus == focusPane {
		t.Fatal("a failed open should not leave the window in a pane")
	}
	if !m.statusE || !strings.Contains(m.status, "sandbox is not running") {
		t.Fatalf("status = %q (error %v), want the failure", m.status, m.statusE)
	}
}

// A reconnect never appears in the terminal's output — the stream simply
// carries on — so the pane says so itself.
func TestReconnectIsShownInThePane(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openPane(t, ds, "enter")

	term.events <- TerminalEvent{State: TerminalReconnecting}
	d.wait("the reconnect", func() bool { return m.panes[0].status != "" })
	if !strings.Contains(frameText(m), "reconnecting") {
		t.Errorf("the pane should say it is reconnecting:\n%s", frameText(m))
	}

	term.events <- TerminalEvent{State: TerminalReconnected}
	d.wait("the reconnection", func() bool { return m.panes[0].status == "" })
}

// The hardware cursor has to land on the cell the sandbox believes it is on.
// Every row of chrome above the grid is an offset, and getting it wrong puts
// the cursor a line off for the whole session.
func TestPaneCursorLandsOnTheGrid(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openPane(t, ds, "enter")

	term.send("abc")
	d.wait("output", func() bool { return strings.Contains(frameText(m), "abc") })

	cursor := m.paneCursor()
	if cursor == nil {
		t.Fatal("an attached pane should place the cursor")
	}
	// The border and the air inside it across; the header, the line naming the
	// box, and the border's top edge down.
	originX, originY := m.paneOrigin(0)
	if cursor.X != originX+3 || cursor.Y != originY {
		t.Fatalf("cursor at %d,%d, want %d,%d", cursor.X, cursor.Y, originX+3, originY)
	}

	// And it is where the frame actually put the text, which is the check that
	// survives the chrome changing.
	// Indexed in cells, not bytes: the border is a multi-byte rune, and slicing
	// the string by byte would land in the middle of it.
	lines := strings.Split(rawFrame(m), "\n")
	cells := []rune(ansi.Strip(lines[cursor.Y]))
	if got := string(cells[originX : originX+3]); got != "abc" {
		t.Fatalf("the grid row reads %q where the cursor is pointing at cell %d", got, cursor.X)
	}
	if cells[cursor.X] != ' ' {
		t.Fatalf("the cursor is on %q, want the empty cell after the text", string(cells[cursor.X]))
	}
}

// The window's own frame is drawn by hand rather than by a bordered style,
// because such a style re-wraps full-width lines — and a re-wrapped terminal
// grid shifts every row below the wrap, putting the cursor on the wrong line
// for the rest of the session.
func TestTheFrameNeverWraps(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openPane(t, ds, "enter")

	cols, _ := m.paneCells(len(m.panes))
	term.send(strings.Repeat("X", cols))
	d.wait("a full-width row", func() bool { return strings.Contains(frameText(m), strings.Repeat("X", cols)) })

	lines := strings.Split(rawFrame(m), "\n")
	if got, want := len(lines), m.height; got > want {
		t.Fatalf("a full-width row made the frame %d lines in a %d-row window", got, want)
	}
	for i, line := range lines {
		if lipgloss.Width(line) != m.width {
			t.Fatalf("line %d is %d cells, want %d: %q", i, lipgloss.Width(line), m.width, ansi.Strip(line))
		}
	}
}

// A title an application sets is laid into the terminal's own top border: it
// names the terminal rather than the window, and a border is a line the eye
// already follows, so it costs no row.
func TestApplicationTitleIsSetIntoTheBorder(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openPane(t, ds, "enter")

	term.send("\x1b]2;go test ./...\x07")
	d.wait("the title", func() bool { return m.panes[0].term.Title() == "go test ./..." })

	border := ansi.Strip(strings.Split(rawFrame(m), "\n")[1])
	if !strings.Contains(border, "go test ./...") {
		t.Fatalf("the border should carry the title: %q", border)
	}
	if !strings.HasPrefix(border, "╭") || !strings.HasSuffix(border, "╮") {
		t.Fatalf("the border should still be a border: %q", border)
	}
	// Centered in the line, so it does not shift as the terminal's contents do.
	cells := []rune(border)
	start := len([]rune(border[:strings.Index(border, "go test ./...")]))
	if want := (len(cells) - len("go test ./...")) / 2; start != want {
		t.Fatalf("title starts at cell %d, want %d", start, want)
	}
	// And it is nowhere else: the banner above says where you are and which
	// discobox this is.
	if line := ansi.Strip(strings.Split(rawFrame(m), "\n")[0]); strings.Contains(line, "go test ./...") {
		t.Errorf("the title is repeated in the banner: %q", line)
	}
}

// A title too long to sit in the line with rule either side is dropped rather
// than squeezing the border out; the terminal's own title bar has it too.
func TestALongTitleLeavesTheBorderAlone(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openPane(t, ds, "enter")

	long := strings.Repeat("very long title ", 12)
	term.send("\x1b]2;" + long + "\x07")
	d.wait("the title", func() bool { return m.panes[0].term.Title() == long })

	border := ansi.Strip(strings.Split(rawFrame(m), "\n")[1])
	if strings.Contains(border, "very long title very long") {
		t.Fatalf("a title with no room should be dropped: %q", border)
	}
	if lipgloss.Width(border) != m.width {
		t.Fatalf("the border is %d cells, want %d", lipgloss.Width(border), m.width)
	}
}

// Ctrl-C backs out of wherever you are: out of a pane to the sandboxes, and out
// of the window altogether from there.
func TestCtrlCDetachesThenQuits(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openPane(t, ds, "enter")

	d.key("ctrl+c")
	d.wait("the pane to close", func() bool { return len(m.panes) == 0 })
	if m.quit {
		t.Fatal("ctrl+c in a pane should detach, not quit")
	}
	if m.focus != focusList {
		t.Fatalf("focus = %v, want the list", m.focus)
	}
	if got := term.typed(""); strings.Contains(got, "\x03") {
		t.Fatalf("the detach key should not also reach the sandbox: %q", got)
	}

	d.key("ctrl+c")
	if !m.quit {
		t.Fatal("ctrl+c outside a pane should quit")
	}
}

// The sandbox still needs a real interrupt, and the prefix is how it gets the
// one key the pane has taken.
func TestPrefixSendsARealInterrupt(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openPane(t, ds, "enter")

	d.key("ctrl+a")
	d.key("ctrl+c")
	if got := term.typed("\x03"); !strings.Contains(got, "\x03") {
		t.Fatalf("typed %q, want ETX to reach the sandbox", got)
	}
	if len(m.panes) == 0 {
		t.Fatal("a prefixed interrupt should not detach")
	}
}

// The mouse goes to the sandbox only while something in it has asked for one,
// so the terminal's own selection is only lost while it is being used.
func TestMouseIsMirroredFromTheSandbox(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openPane(t, ds, "enter")

	if m.View().MouseMode != tea.MouseModeNone {
		t.Fatal("nothing has asked for the mouse yet")
	}

	term.send("\x1b[?1000h\x1b[?1006hCLICKS")
	d.wait("mouse mode", func() bool { return m.View().MouseMode == tea.MouseModeCellMotion })

	// A click on the grid reaches the sandbox, translated out of screen space.
	originX, originY := m.paneOrigin(0)
	d.dispatch(tea.MouseClickMsg{X: originX + 4, Y: originY + 2, Button: tea.MouseLeft})
	if got := term.typed("\x1b[<"); !strings.Contains(got, "\x1b[<0;5;3") {
		t.Fatalf("typed %q, want a click at grid cell 4,2", got)
	}

	// A click on the window's own chrome is not the sandbox's.
	before := len(term.typed(""))
	d.dispatch(tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft})
	if after := len(term.typed("")); after != before {
		t.Fatal("a click on the frame should not reach the sandbox")
	}
}

// ctrl+a m hands the mouse back, for when you would rather copy a stack trace
// than click on it.
func TestPrefixMTogglesTheMouse(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openPane(t, ds, "enter")

	term.send("\x1b[?1000h\x1b[?1006hCLICKS")
	d.wait("mouse mode", func() bool { return m.View().MouseMode == tea.MouseModeCellMotion })

	d.key("ctrl+a")
	d.key("m")
	if m.paneMouse {
		t.Fatal("ctrl+a m should hand the mouse back")
	}
	if m.View().MouseMode != tea.MouseModeNone {
		t.Fatal("with the mouse handed back the terminal should stop reporting it")
	}
	if !strings.Contains(m.status, "selection") {
		t.Fatalf("status = %q, want it to say what changed", m.status)
	}

	d.key("ctrl+a")
	d.key("m")
	if !m.paneMouse || m.View().MouseMode != tea.MouseModeCellMotion {
		t.Fatal("ctrl+a m again should give it back to the sandbox")
	}
}

// A pane's title goes to the terminal's own title bar as well as the header:
// the header says what is in the window, and the title bar is how you find the
// window among the others you have open.
func TestTheTerminalTitleFollowsThePane(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openPane(t, ds, "enter")

	// Until the application says otherwise, the sandbox is what to call it.
	if got, want := m.View().WindowTitle, "fix flaky pool reaper tests"; got != want {
		t.Fatalf("title = %q, want %q", got, want)
	}

	term.send("\x1b]2;go test ./...\x07")
	d.wait("the title", func() bool { return m.View().WindowTitle == "go test ./..." })

	// And with no pane the title is left exactly as the shell that started this
	// window left it: it is a guest in someone else's terminal.
	d.key("ctrl+c")
	d.wait("the pane to close", func() bool { return len(m.panes) == 0 })
	if got := m.View().WindowTitle; got != "" {
		t.Fatalf("title = %q, want the terminal's own left alone", got)
	}
}

// From an attached harness, the leader plus s opens a shell beside it — to the
// right, so what you were watching keeps the place your eye already has.
func TestSplitShellOpensToTheRight(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openPane(t, ds, "enter")

	d.key("ctrl+a")
	d.key("s")
	d.wait("the split", func() bool { return len(m.panes) == 2 })

	if m.panes[0].action != InteractAttach || m.panes[1].action != InteractShell {
		t.Fatalf("panes = %s | %s, want the shell on the right", m.panes[0].action, m.panes[1].action)
	}
	// The new one takes focus: it is the thing you just asked for.
	if m.focused != 1 {
		t.Fatalf("focused = %d, want the shell", m.focused)
	}
	// Both are the same discobox, and both were opened at half the screen.
	if m.panes[0].sandbox.ID != m.panes[1].sandbox.ID {
		t.Fatal("a split shell belongs to the discobox it was opened beside")
	}
	cols, _ := m.paneCells(2)
	for i, term := range ds.terminals {
		if got := term.size(); got[0] != cols {
			t.Fatalf("terminal %d opened at %d columns, want %d", i, got[0], cols)
		}
	}
	// And only one shell: asking again goes to the one that is open rather
	// than stacking a second beside it.
	d.key("ctrl+a")
	d.key("h")
	d.key("ctrl+a")
	d.key("s")
	if len(m.panes) != 2 {
		t.Fatalf("panes = %d, want the second ask to go to the one open", len(m.panes))
	}
	if m.focused != 1 {
		t.Fatalf("focused = %d, want the shell that was already there", m.focused)
	}
}

// The leader plus h and l move between them, and stop at the ends rather than
// wrapping: two panes have a left and a right, and focus that jumps the long way
// round is focus you have to look for.
func TestLeaderMovesBetweenPanes(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openPane(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the split", func() bool { return len(m.panes) == 2 })

	if m.focused != 1 {
		t.Fatalf("focused = %d, want the shell", m.focused)
	}
	d.key("ctrl+a")
	d.key("h")
	if m.focused != 0 {
		t.Fatalf("focused = %d, want the harness", m.focused)
	}
	d.key("ctrl+a")
	d.key("h")
	if m.focused != 0 {
		t.Fatal("the left edge should stop rather than wrap")
	}
	d.key("ctrl+a")
	d.key("l")
	if m.focused != 1 {
		t.Fatalf("focused = %d, want the shell again", m.focused)
	}
	d.key("ctrl+a")
	d.key("l")
	if m.focused != 1 {
		t.Fatal("the right edge should stop rather than wrap")
	}
}

// The sides are a preference, so the two can be exchanged — and focus goes with
// the terminal rather than staying on a side of the screen: you swapped the
// panes, not your place in them.
func TestSwapExchangesThePanes(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openPane(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the split", func() bool { return len(m.panes) == 2 })

	shell := m.panes[m.focused]
	d.key("ctrl+a")
	d.key("e")

	if m.panes[0].action != InteractShell || m.panes[1].action != InteractAttach {
		t.Fatalf("panes = %s | %s, want them exchanged", m.panes[0].action, m.panes[1].action)
	}
	if m.focusedPane() != shell {
		t.Fatal("focus should follow the terminal, not the side")
	}
	if m.focused != 0 {
		t.Fatalf("focused = %d, want the side the shell moved to", m.focused)
	}
}

// Keys reach whichever pane has focus, and the way out is that pane's own: the
// shell keeps ctrl+c for itself and detaches on the leader, the harness detaches
// on ctrl+c.
func TestEachPaneKeepsItsOwnKeys(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, harness := openPane(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the split", func() bool { return len(m.panes) == 2 })
	shell := ds.terminals[1]

	// The shell has focus, and ctrl+c is its own.
	d.key("ctrl+c")
	if got := shell.typed("\x03"); !strings.Contains(got, "\x03") {
		t.Fatalf("shell typed %q, want the interrupt", got)
	}
	if len(m.panes) != 2 {
		t.Fatal("ctrl+c in a shell should not detach it")
	}
	if !strings.Contains(m.hints(), m.leader()+" q detach") {
		t.Fatalf("the keys should say the shell's way out: %q", m.hints())
	}

	// The leader plus q closes the shell, leaving the harness.
	d.key("ctrl+a")
	d.key("q")
	d.wait("the shell to close", func() bool { return len(m.panes) == 1 })
	if m.panes[0].action != InteractAttach {
		t.Fatalf("left %s, want the harness", m.panes[0].action)
	}
	if m.focus != focusPane {
		t.Fatal("closing one pane should leave the other focused")
	}

	// And on the harness ctrl+c detaches, which is where the window ends up.
	if got := harness.typed(""); strings.Contains(got, "\x03") {
		t.Fatalf("the harness should not have seen an interrupt: %q", got)
	}
	d.key("ctrl+c")
	d.wait("the pane to close", func() bool { return len(m.panes) == 0 })
	if m.focus != focusList {
		t.Fatalf("focus = %v, want the list", m.focus)
	}
}

// A session that ends closes its own pane and leaves the other running.
func TestAnEndedShellLeavesTheHarness(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openPane(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the split", func() bool { return len(m.panes) == 2 })

	ds.terminals[1].Close() // the shell exits
	d.wait("the shell to close", func() bool { return len(m.panes) == 1 })
	if m.panes[0].action != InteractAttach {
		t.Fatalf("left %s, want the harness", m.panes[0].action)
	}
	if m.focus != focusPane {
		t.Fatal("the harness should still be up")
	}
}

// Two panes side by side each get half the screen, and the row still comes out
// exactly the terminal's width.
func TestSplitPanesShareTheScreen(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openPane(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the split", func() bool { return len(m.panes) == 2 })
	ds.terminals[1].send("in the shell")
	d.wait("output", func() bool { return strings.Contains(frameText(m), "in the shell") })

	for i, line := range strings.Split(rawFrame(m), "\n") {
		if lipgloss.Width(line) != m.width {
			t.Fatalf("line %d is %d cells, want %d: %q", i, lipgloss.Width(line), m.width, ansi.Strip(line))
		}
	}
	// The focused pane's border is lit and the other is not, because with every
	// key going to one of them, which one has to be visible without looking.
	frame := ansi.Strip(rawFrame(m))
	if !strings.Contains(frame, "[ shell ]") || !strings.Contains(frame, "[ attach ]") {
		t.Fatalf("each pane should name what it is running:\n%s", frame)
	}
}

// diff and status are drawn in a pane too. They are not terminals in the
// discobox — they are this CLI's own commands, given one of their own — but the
// window treats them the same way, and they keep ctrl+c for the pager.
func TestDiffAndStatusOpenInPanes(t *testing.T) {
	for _, act := range []struct {
		key    string
		action Interaction
	}{
		{"d", InteractDiff},
		{"i", InteractStatus},
	} {
		t.Run(string(act.action), func(t *testing.T) {
			ds := newFakeSource(testSandboxes()...)
			t.Setenv("NO_COLOR", "1")
			m := New(t.Context(), ds)
			m.logo = logo{}
			m.expanded = true
			d := newDriver(t, m)
			d.start()
			d.wait("the listing", func() bool { return len(m.list.rows()) > 0 })
			d.key("tab")
			d.key(act.key)
			d.wait("the pane", func() bool { return m.focus == focusPane })

			if len(ds.interacts) != 0 {
				t.Fatalf("interacts = %v, want the window not to step aside", ds.interacts)
			}
			if len(ds.opens) != 1 || !strings.HasPrefix(ds.opens[0], string(act.action)+" ") {
				t.Fatalf("opens = %v", ds.opens)
			}
			if m.overlay == nil || m.overlay.action != act.action {
				t.Fatalf("overlay = %v, want the command to have the screen", m.overlay)
			}
			// A pager wants ctrl+c, so the way out is behind the leader.
			if got := m.overlay.hint; got != m.leader()+" "+paneDetachAlt {
				t.Fatalf("detach hint = %q, want the leader's", got)
			}
		})
	}
}

// apply is not among them: it writes to the repository on this machine and can
// stop to ask about it, so the window still steps aside for it.
func TestApplyStillTakesTheRealTerminal(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	send(t, m, key("tab"), key("y"))

	if len(ds.opens) != 0 {
		t.Fatalf("opens = %v, want apply not drawn in a pane", ds.opens)
	}
	if len(ds.interacts) != 1 || ds.interacts[0] != "apply sbx_one" {
		t.Fatalf("interacts = %v", ds.interacts)
	}
}

// A command that ran, printed and returned leaves its last screen up to be
// read. `disco status` on a clean tree is over in a moment, and a pane that
// vanished with it would be a screen you never got to see.
func TestAFinishedCommandHoldsItsScreen(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	t.Setenv("NO_COLOR", "1")
	m := New(t.Context(), ds)
	m.logo = logo{}
	m.expanded = true
	d := newDriver(t, m)
	d.start()
	d.wait("the listing", func() bool { return len(m.list.rows()) > 0 })
	d.key("tab")
	d.key("i") // status
	d.wait("the pane", func() bool { return m.focus == focusPane })

	ds.terminals[0].send("On branch main\r\nnothing to commit\r\n")
	d.wait("output", func() bool { return strings.Contains(frameText(m), "nothing to commit") })

	// The command returns.
	ds.terminals[0].Close()
	d.wait("the pane to settle", func() bool { return m.overlay != nil && m.overlay.exited })

	// It is still there, and still readable.
	if m.overlay == nil {
		t.Fatal("the screen should have been kept")
	}
	if !strings.Contains(plainFrame(m), "nothing to commit") {
		t.Errorf("the last screen should still be up:\n%s", plainFrame(m))
	}
	if !strings.Contains(plainFrame(m), "finished") {
		t.Errorf("it should say the command is over:\n%s", plainFrame(m))
	}
	if !strings.Contains(m.hints(), "q closes") {
		t.Errorf("the keys should say how to take it away: %q", m.hints())
	}

	// A key that means nothing here leaves the screen alone: taking it away
	// mid-read is exactly the wrong moment.
	d.key("z")
	if m.overlay == nil {
		t.Fatal("a stray key should not close the screen")
	}

	// The ones that mean done take it away.
	d.key("q")
	d.wait("the pane to close", func() bool { return m.overlay == nil })
	if m.focus != focusList {
		t.Fatalf("focus = %v, want the list", m.focus)
	}
}

// A shell or a harness session that ends is gone: the pane has nothing left to
// show, so it is not held.
func TestAnEndedSessionIsNotHeld(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openPane(t, ds, "enter")

	term.Close()
	d.wait("the pane to close", func() bool { return len(m.panes) == 0 })
}

// The discobox is identified once, folded into the banner rather than given a
// line of its own, and centered in it. Its id rather than its name: that is
// what you would type at a shell to act on this one.
func TestTheDiscoboxIDIsCenteredInTheBanner(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openPane(t, ds, "enter")

	nameRow := func() string { return ansi.Strip(strings.Split(rawFrame(m), "\n")[0]) }
	centeredAt := func(row, name string) bool {
		start := len([]rune(row[:strings.Index(row, name)]))
		want := (len([]rune(row)) - len([]rune(name))) / 2
		return start >= want-1 && start <= want+1
	}

	row := nameRow()
	if !centeredAt(row, "sbx_one") {
		t.Fatalf("the id is not centered: %q", row)
	}
	if strings.Contains(row, "fix flaky pool reaper tests") {
		t.Fatalf("the banner should carry the id, not the name: %q", row)
	}

	// The transport's status displaces the keys rather than the name, and does
	// not move it: it is centered in the row, not in what is left of it.
	before := strings.Index(nameRow(), "sbx_one")
	m.panes[0].status = "reconnecting…"
	if after := strings.Index(nameRow(), "sbx_one"); before != after {
		t.Fatalf("the id moved when a status appeared: %d then %d", before, after)
	}
	if !strings.Contains(nameRow(), "reconnecting") {
		t.Fatalf("the status should still be shown: %q", nameRow())
	}

	// And one name over both panes, not one each.
	m.panes[0].status = ""
	d.key("ctrl+a")
	d.key("s")
	d.wait("the split", func() bool { return len(m.panes) == 2 })
	if got := strings.Count(nameRow(), "sbx_one"); got != 1 {
		t.Fatalf("the id appears %d times over two panes, want once", got)
	}
	if !centeredAt(nameRow(), "sbx_one") {
		t.Fatalf("the id is not centered over the split: %q", nameRow())
	}
}

// Output longer than the pane can be read back through: a screen you cannot
// scroll is a screen whose first half you never saw.
func TestAFinishedCommandCanBeScrolled(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	t.Setenv("NO_COLOR", "1")
	m := New(t.Context(), ds)
	m.logo = logo{}
	m.expanded = true
	d := newDriver(t, m)
	d.start()
	d.wait("the listing", func() bool { return len(m.list.rows()) > 0 })
	d.key("tab")
	d.key("i")
	d.wait("the pane", func() bool { return m.focus == focusPane })

	// More lines than the pane is tall.
	_, rows := m.paneCells(1)
	var out strings.Builder
	for i := 1; i <= rows+20; i++ {
		if i > 1 {
			out.WriteString("\r\n")
		}
		fmt.Fprintf(&out, "line %d", i)
	}
	ds.terminals[0].send(out.String())
	d.wait("output", func() bool { return strings.Contains(frameText(m), fmt.Sprintf("line %d", rows+20)) })
	ds.terminals[0].Close()
	d.wait("the pane to settle", func() bool { return m.overlay != nil && m.overlay.exited })

	if strings.Contains(plainFrame(m), "line 1 ") {
		t.Fatal("the first lines should have scrolled off the pane")
	}
	if !strings.Contains(m.hints(), "scroll") {
		t.Errorf("the keys should offer scrolling: %q", m.hints())
	}

	// Up walks back, home goes to the top, and the earliest output is there.
	d.key("up")
	if m.overlay.term.ScrollOffset() != 1 {
		t.Fatalf("offset = %d, want one line back", m.overlay.term.ScrollOffset())
	}
	d.key("g")
	if !strings.Contains(plainFrame(m), "line 1") {
		t.Errorf("the top of the output should be reachable:\n%s", plainFrame(m))
	}

	// And end comes back to where it finished.
	d.key("G")
	if m.overlay.term.ScrollOffset() != 0 {
		t.Fatalf("offset = %d, want the last screen", m.overlay.term.ScrollOffset())
	}
	if !strings.Contains(plainFrame(m), fmt.Sprintf("line %d", rows+20)) {
		t.Errorf("the end of the output should be back:\n%s", plainFrame(m))
	}
}

// Moving between panes takes the arrows as well as the letters, and holding
// Ctrl keeps the sequence open: the leader, then Ctrl-← Ctrl-→ walks across
// without pressing the leader again.
func TestMovingBetweenPanesRepeatsWhileCtrlIsHeld(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openPane(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the split", func() bool { return len(m.panes) == 2 })

	// The arrows work like h and l.
	d.key("ctrl+a")
	d.key("left")
	if m.focused != 0 {
		t.Fatalf("focused = %d, want the left pane", m.focused)
	}
	d.key("ctrl+a")
	d.key("right")
	if m.focused != 1 {
		t.Fatalf("focused = %d, want the right pane", m.focused)
	}

	// And one leader carries a run of them while Ctrl is down.
	d.key("ctrl+a")
	d.key("ctrl+left")
	if m.focused != 0 {
		t.Fatalf("focused = %d after the first step", m.focused)
	}
	d.key("ctrl+right")
	if m.focused != 1 {
		t.Fatalf("focused = %d, want the run to continue without the leader", m.focused)
	}
	d.key("ctrl+left")
	if m.focused != 0 {
		t.Fatalf("focused = %d, want the run to still be going", m.focused)
	}

	// Letting go of Ctrl ends it: the next key is the sandbox's again.
	d.key("left")
	shell := ds.terminals[1]
	d.key("left")
	if got := shell.typed(""); strings.Contains(got, "\x1b[D") {
		t.Fatal("the pane that was left should not have received the key")
	}
	if got := ds.terminals[0].typed("\x1b[D"); !strings.Contains(got, "\x1b[D") {
		t.Fatalf("the focused pane should get the arrow once the run has ended: %q", got)
	}
}

// The screen is two spots, one for each of the discobox's terminals, and an
// empty one is opened where it stands. Opening the harness from a screen that
// has only a shell puts it in its own spot — on the left, where it belongs —
// rather than beside it as a second shell would go.
func TestAnEmptySpotIsOpenedWhereItStands(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openPane(t, ds, "s")
	if m.panes[0].action != InteractShell {
		t.Fatalf("opened %s, want the shell", m.panes[0].action)
	}

	d.key("ctrl+a")
	d.key("a")
	d.wait("the harness", func() bool { return len(m.panes) == 2 })

	if m.panes[0].action != InteractAttach || m.panes[1].action != InteractShell {
		t.Fatalf("panes = %s | %s, want the harness in its own spot on the left",
			m.panes[0].action, m.panes[1].action)
	}
	if m.focused != 0 {
		t.Fatalf("focused = %d, want the one just opened", m.focused)
	}

	// And there is only ever one of each: asking again goes to the spot that
	// is open.
	d.key("ctrl+a")
	d.key("a")
	if len(m.panes) != 2 {
		t.Fatalf("panes = %d, want no second harness", len(m.panes))
	}
}

// Which side each spot is on outlasts the terminal in it: swap them, close one,
// open it again, and it comes back where you put it.
func TestTheSidesStaySwapped(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openPane(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the split", func() bool { return len(m.panes) == 2 })

	d.key("ctrl+a")
	d.key("e")
	if m.panes[0].action != InteractShell {
		t.Fatalf("panes[0] = %s, want the shell after the swap", m.panes[0].action)
	}

	// Close the shell and open it again.
	d.key("ctrl+a")
	d.key("q")
	d.wait("the shell to close", func() bool { return len(m.panes) == 1 })
	d.key("ctrl+a")
	d.key("s")
	d.wait("the shell", func() bool { return len(m.panes) == 2 })

	if m.panes[0].action != InteractShell || m.panes[1].action != InteractAttach {
		t.Fatalf("panes = %s | %s, want the sides still swapped",
			m.panes[0].action, m.panes[1].action)
	}
}

// A command that runs and finishes takes the whole screen for as long as it
// runs. What is under it is untouched — still connected, still running, still
// where it was — and is back the moment the command exits.
func TestACommandTakesTheScreenOverTheSpots(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, harness := openPane(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the split", func() bool { return len(m.panes) == 2 })
	harness.send("the harness is still working")
	d.wait("output", func() bool { return strings.Contains(frameText(m), "still working") })

	d.key("ctrl+a")
	d.key("d") // diff
	d.wait("the command", func() bool { return m.overlay != nil })

	if m.overlay.action != InteractDiff {
		t.Fatalf("overlay = %s, want the diff", m.overlay.action)
	}
	// Both terminals are still there, and neither was resized to make room:
	// nothing was made room for.
	if len(m.panes) != 2 {
		t.Fatalf("panes = %d, want both kept", len(m.panes))
	}
	cols, _ := m.paneCells(2)
	for i, term := range ds.terminals[:2] {
		if got := term.size(); got[0] != cols {
			t.Fatalf("terminal %d is %d columns, want the split's %d", i, got[0], cols)
		}
	}
	// The command has the screen: it is the only box drawn, at the full width.
	full, _ := m.paneCells(1)
	if got := ds.terminals[2].size(); got[0] != full {
		t.Fatalf("the command opened at %d columns, want the screen's %d", got[0], full)
	}
	frame := plainFrame(m)
	if strings.Contains(frame, "still working") {
		t.Fatalf("the spots should be under the command, not beside it:\n%s", frame)
	}
	if !strings.Contains(frame, "[ diff ]") {
		t.Fatalf("the command should name itself:\n%s", frame)
	}

	// It ends, and the two spots are back exactly as they were.
	ds.terminals[2].send("--- a/x\r\n+++ b/x\r\n")
	d.wait("the diff", func() bool { return strings.Contains(frameText(m), "+++ b/x") })
	ds.terminals[2].Close()
	d.wait("the command to settle", func() bool { return m.overlay.exited })
	d.key("q")
	d.wait("the spots", func() bool { return m.overlay == nil })

	if len(m.panes) != 2 {
		t.Fatalf("panes = %d, want both still up", len(m.panes))
	}
	if !strings.Contains(plainFrame(m), "still working") {
		t.Fatalf("what was underneath should be back as it was:\n%s", plainFrame(m))
	}
	if m.focus != focusPane {
		t.Fatalf("focus = %v, want the panes", m.focus)
	}
}

// Every command the list offers is here on the key it has there. A lifecycle
// verb runs against the server and reports, and the screen stays up — you did
// not leave the terminal to archive the box you are looking at.
func TestLeaderRunsTheListsVerbs(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openPane(t, ds, "enter")

	d.key("ctrl+a")
	d.key("x") // archive
	d.wait("the verb", func() bool { return len(ds.did) > 0 })

	if ds.did[0] != "archive sbx_one" {
		t.Fatalf("did = %v, want the box on screen archived", ds.did)
	}
	if m.focus != focusPane || len(m.panes) != 1 {
		t.Fatalf("focus = %v with %d panes, want the screen kept", m.focus, len(m.panes))
	}
}

// A command that cannot run says why, rather than opening a screen that reports
// the same thing less clearly. The checks are the list's, on the same discobox.
func TestTheLeaderKeepsTheListsChecks(t *testing.T) {
	boxes := testSandboxes()
	boxes[0].Diff = DiffStat{Known: true}
	ds := newFakeSource(boxes...)
	d, m, _ := openPane(t, ds, "enter")

	d.key("ctrl+a")
	d.key("d") // diff, with nothing to show
	d.settle()

	if m.overlay != nil {
		t.Fatal("a diff with nothing in it should not take the screen")
	}
	if m.dialog == nil || !strings.Contains(m.dialog.body, "nothing has changed") {
		t.Fatalf("dialog = %v, want it to say why", m.dialog)
	}
}

// The leader's way out works in every pane, whether or not the pane has one of
// its own. It costs the application nothing, being behind the leader, and one
// key that always works beats remembering which pane took Ctrl-C.
func TestTheLeaderClosesAnyPane(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openPane(t, ds, "enter") // an attach, which detaches on ctrl+c

	d.key("ctrl+a")
	d.key("q")
	d.wait("the pane to close", func() bool { return len(m.panes) == 0 })
	if m.focus != focusList {
		t.Fatalf("focus = %v, want the list", m.focus)
	}
}
