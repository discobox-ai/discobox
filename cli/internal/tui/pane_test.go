package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/obot-platform/discobox/termpane"
)

// openWorkspace drives the window into the workspace on the first sandbox.
func openWorkspace(t *testing.T, ds *fakeSource, act string) (*driver, *Model, *fakeTerminal) {
	t.Helper()
	t.Setenv("NO_COLOR", "1")
	m := New(t.Context(), ds)
	m.logo = logo{}
	m.expanded = true
	// A test copy must not clobber the developer's actual clipboard.
	m.copyOS = func(string) error { return nil }
	d := newDriver(t, m)
	d.start()
	d.wait("the listing", func() bool { return len(m.list.rows()) > 0 })

	d.key("tab")
	d.key(act)
	d.wait("the workspace", func() bool { return m.focus == focusPane })
	d.wait("the primary terminal", func() bool { return m.terminal != nil })
	return d, m, ds.execTerm(ExecPrimary)
}

// Attaching draws the sandbox's terminal in the window rather than handing the
// real terminal over to a command.
func TestAttachDrawsInTheWindow(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

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
	if !strings.Contains(frame, "ctrl+a d detach") {
		t.Errorf("the pane should say how to get out:\n%s", frame)
	}
	for _, line := range strings.Split(rawFrame(m), "\n") {
		if lipgloss.Width(line) != 120 {
			t.Fatalf("a pane row is %d cells, want 120: %q", lipgloss.Width(line), ansi.Strip(line))
		}
	}
}

// s from the list opens the workspace too — with a fresh shell already in a
// tab, focused: the workspace is the screen, and a shell was what was asked
// for.
func TestShellOpensTheWorkspaceWithAFreshTab(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "s")
	d.wait("the tab", func() bool { return len(m.shells) == 1 })

	if !m.onShells {
		t.Fatal("the fresh shell should have focus")
	}
	if m.shells[0].execID != "exec_shell1" {
		t.Fatalf("tab = %q, want the created shell", m.shells[0].execID)
	}
	// And the primary is there too, on the left.
	if m.terminal == nil {
		t.Fatal("the workspace should still hold the primary terminal")
	}
}

// The terminal is opened at the size of the box it is going into — the whole
// width, with no other session to share the screen with.
func TestPaneOpensAtTheSizeItWillBeDrawnAt(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	_, m, term := openWorkspace(t, ds, "enter")

	cols, rows := m.paneCells(m.paneWidthOf(m.terminal))
	if got := term.size(); got != [2]int{cols, rows} {
		t.Fatalf("opened at %v, want %dx%d", got, cols, rows)
	}
	// The grid is the whole window bar its chrome, so it grows with it: the
	// border and a cell of air inside it on each side.
	if rows != m.height-4 || cols != m.width-2-2*boxPad {
		t.Fatalf("pane is %dx%d in a %d-row window", cols, rows, m.height)
	}
	if got := len(m.terminal.term.View()); got != rows {
		t.Fatalf("the pane drew %d rows, want %d", got, rows)
	}
}

// A tab opening splits the screen: the terminal is resized to the left half,
// and the shell was opened at the right half's size to begin with.
func TestATabSplitsTheScreen(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return len(m.shells) == 1 })

	lcols, lrows := m.paneCells(m.width / 2)
	if got := ds.execTerm(ExecPrimary).size(); got != [2]int{lcols, lrows} {
		t.Fatalf("the terminal is %v, want the left half %dx%d", got, lcols, lrows)
	}
	rcols, rrows := m.paneCells(m.width - m.width/2)
	if got := ds.execTerm("exec_shell1").size(); got != [2]int{rcols, rrows} {
		t.Fatalf("the shell is %v, want the right half %dx%d", got, rcols, rrows)
	}
	// The new shell has focus: it is the thing you just asked for.
	if !m.onShells || m.activeShell != 0 {
		t.Fatalf("onShells=%v activeShell=%d, want the new tab focused", m.onShells, m.activeShell)
	}
}

// The leader plus z is the same toggle the boxes' [+] button is, so the
// workspace maximizes without a mouse: the focused column takes the window,
// and the hidden one is resized for it too — it keeps emulating off-screen,
// and flipping back to it must show a screen drawn at the size it is shown at.
func TestLeaderZMaximizesTheFocusedColumn(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return len(m.shells) == 1 })
	d.wait("the tab focused", func() bool { return m.onShells })

	d.key("ctrl+a")
	d.key("z")
	d.wait("the tab maximized", func() bool { return m.maximized })

	full, fullRows := m.paneCells(m.width)
	for what, term := range map[string]*fakeTerminal{
		"the tab":      ds.execTerm("exec_shell1"),
		"the terminal": ds.execTerm(ExecPrimary),
	} {
		if got := term.size(); got != [2]int{full, fullRows} {
			t.Fatalf("%s is %v, want the whole window %dx%d", what, got, full, fullRows)
		}
	}
	if !strings.Contains(frameText(m), "ctrl+a z restore") {
		t.Fatalf("the hints should offer the way back:\n%s", frameText(m))
	}

	// And back to the split.
	d.key("ctrl+a")
	d.key("z")
	d.wait("the split back", func() bool { return !m.maximized })
	cols, rows := m.paneCells(m.width / 2)
	if got := ds.execTerm(ExecPrimary).size(); got != [2]int{cols, rows} {
		t.Fatalf("the terminal is %v, want the left half %dx%d", got, cols, rows)
	}
}

// With nothing beside it there is nothing to maximize over, so the key says so
// rather than looking broken.
func TestLeaderZWithNoTabsSaysSo(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key("z")
	d.wait("the report", func() bool { return strings.Contains(m.status, "nothing to maximize") })
	if m.maximized {
		t.Fatal("a lone box should not maximize over nothing")
	}
}

// The last tab closing gives the window back on its own: there is nothing left
// to maximize over, and a workspace stuck maximized would hide the next tab.
func TestClosingTheLastTabDropsTheMaximize(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return len(m.shells) == 1 })
	m.toggleMaximized(true)

	m.closeShell(0)
	if m.maximized {
		t.Fatal("the last tab closing should give the window back")
	}
	if got := m.paneWidthOf(m.terminal); got != m.width {
		t.Fatalf("the terminal is %d cells wide, want the whole window (%d)", got, m.width)
	}
}

// Resizing the window resizes every terminal with it.
func TestResizingTheWindowResizesTheTerminal(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	d.dispatch(sizeMsg(100, 30))
	d.settle()
	cols, rows := m.paneCells(m.paneWidthOf(m.terminal))
	if got := term.size(); got != [2]int{cols, rows} {
		t.Fatalf("resized to %v, want %dx%d", got, cols, rows)
	}
}

// Every key belongs to the sandbox while a pane is up — including the ones the
// window would otherwise use.
func TestKeysGoToTheSandbox(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, _, term := openWorkspace(t, ds, "enter")

	// "t" is stop in the list, and must not stop anything here.
	d.key("t")
	if got := term.typed("t"); !strings.Contains(got, "t") {
		t.Fatalf("typed %q, want the key to reach the sandbox", got)
	}
	if len(ds.did) != 0 {
		t.Fatalf("did = %v, want the window not to act on the key", ds.did)
	}
}

// Detaching leaves the whole workspace: every stream is closed at once, every
// session keeps running, and the cursor is back on the sandbox it was opened
// on.
func TestDetachLeavesTheWholeWorkspace(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return len(m.shells) == 1 })

	d.key("ctrl+a")
	d.key("d")
	d.wait("the workspace to close", func() bool { return !m.inPanes() })
	if m.focus != focusList {
		t.Fatalf("focus = %v, want the list", m.focus)
	}
	if len(m.shells) != 0 {
		t.Fatal("detach should take the tabs with it")
	}
	for i, term := range ds.terminals {
		select {
		case <-term.closed:
		default:
			t.Fatalf("terminal %d is still open after detach", i)
		}
	}
	if !strings.Contains(m.status, "detached") {
		t.Fatalf("status = %q, want it to say the discobox is still running", m.status)
	}
	if got := m.list.current().ID; got != "sbx_one" {
		t.Fatalf("cursor on %s, want the sandbox the workspace was opened on", got)
	}
}

// The leader plus q quits the whole window from inside a pane — the exit
// Ctrl-C is everywhere else — and the header's top right says so.
func TestLeaderQQuitsTheWindow(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	header := ansi.Strip(strings.Split(rawFrame(m), "\n")[0])
	if !strings.Contains(header, "ctrl+a d detach") || !strings.Contains(header, "ctrl+a q quit") {
		t.Fatalf("the header should offer both ways out: %q", header)
	}

	d.key("ctrl+a")
	d.key("q")
	if !m.quit {
		t.Fatal("leader q should quit the window")
	}
}

// The primary session ending ends the workspace: it is above all a view onto
// that session.
func TestEndedPrimaryClosesTheWorkspace(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	term.Close()
	d.wait("the workspace to close", func() bool { return !m.inPanes() })
	if m.focus != focusList {
		t.Fatalf("focus = %v, want the list", m.focus)
	}
}

// Opening the workspace joins every live TTY session the discobox already has:
// the primary on the left, the rest as tabs in session order — and only those.
// A session that has exited or has no terminal is not a tab.
func TestAttachJoinsEveryLiveSession(t *testing.T) {
	now := time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)
	ds := newFakeSource(testSandboxes()...)
	ds.execs = []Exec{
		{ID: "exec_pri", Harness: "claude", Primary: true, Tty: true, Live: true, CreatedAt: now},
		{ID: "exec_b", Command: []string{"claude"}, Harness: "claude", Tty: true, Live: true, CreatedAt: now.Add(2 * time.Minute)},
		{ID: "exec_a", Command: []string{"/bin/bash"}, Tty: true, Live: true, CreatedAt: now.Add(time.Minute)},
		{ID: "exec_gone", Command: []string{"/bin/sh"}, Tty: true, Live: false, CreatedAt: now},
		{ID: "exec_pipe", Command: []string{"make"}, Live: true, CreatedAt: now},
	}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the tabs", func() bool { return len(m.shells) == 2 })

	if m.shells[0].execID != "exec_a" || m.shells[1].execID != "exec_b" {
		t.Fatalf("tabs = %s | %s, want session order", m.shells[0].execID, m.shells[1].execID)
	}
	if m.onShells {
		t.Fatal("focus starts on the terminal")
	}
	// The strip names both, the visible one and the other.
	frame := plainFrame(m)
	if !strings.Contains(frame, "1 bash") || !strings.Contains(frame, "2 claude") {
		t.Fatalf("the strip should name every tab:\n%s", frame)
	}
}

// A session started from anywhere else appears as a tab while the workspace is
// up: the screen mirrors the server, not what was opened here.
func TestASessionStartedElsewhereBecomesATab(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	ds.addExec(Exec{
		ID: "exec_other", Command: []string{"/usr/bin/fish"}, Tty: true, Live: true,
		CreatedAt: time.Date(2026, 8, 7, 12, 30, 0, 0, time.UTC),
	})
	d.dispatch(workspaceTickMsg{gen: m.wsGen})
	d.wait("the tab", func() bool { return len(m.shells) == 1 })

	if m.shells[0].execID != "exec_other" {
		t.Fatalf("tab = %q, want the session started elsewhere", m.shells[0].execID)
	}
	if m.onShells {
		t.Fatal("a tab arriving on its own does not steal focus")
	}
}

// The poll never opens a second pane onto a session already on screen — the
// shell the leader created included, whose tab and listing entry share an id.
func TestThePollDoesNotReopenASessionAlreadyShown(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return len(m.shells) == 1 })

	d.dispatch(workspaceTickMsg{gen: m.wsGen})
	d.settle()
	if len(m.shells) != 1 {
		t.Fatalf("tabs = %d, want the poll to leave the shown session alone", len(m.shells))
	}
	attaches := 0
	for _, open := range ds.execOpened() {
		if strings.Contains(open, "exec_shell1") {
			attaches++
		}
	}
	if attaches != 1 {
		t.Fatalf("the shell was attached %d times, want once", attaches)
	}
}

// A tick from a workspace that has been left is stale, and must not reopen
// anything.
func TestAStaleTickIsDropped(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	gen := m.wsGen

	d.key("ctrl+a")
	d.key("d")
	d.wait("the workspace to close", func() bool { return !m.inPanes() })

	before := len(ds.execOpened())
	d.dispatch(workspaceTickMsg{gen: gen})
	d.settle()
	if m.inPanes() {
		t.Fatal("a stale tick must not reopen the workspace")
	}
	if got := len(ds.execOpened()); got != before {
		t.Fatalf("a stale tick opened %d attaches", got-before)
	}
}

// A shell that exits keeps its last screen as a tab to be read; dismissing it
// closes the tab and leaves the workspace up.
func TestAnExitedShellStaysReadableUntilDismissed(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return len(m.shells) == 1 })

	shell := ds.execTerm("exec_shell1")
	shell.send("make: everything built\r\n")
	d.wait("output", func() bool { return strings.Contains(frameText(m), "everything built") })

	// The shell exits — and is gone from the listing, as an exited session
	// would be.
	ds.endExec("exec_shell1")
	shell.Close()
	d.wait("the pager", func() bool { return len(m.shells) == 1 && m.shells[0].exited })

	if !strings.Contains(plainFrame(m), "everything built") {
		t.Errorf("the last screen should still be up:\n%s", plainFrame(m))
	}
	if !strings.Contains(m.hints(), "finished") {
		t.Errorf("hints = %q, want them to say the shell is over", m.hints())
	}

	d.key("q")
	d.wait("the tab to close", func() bool { return len(m.shells) == 0 })
	if m.terminal == nil || m.focus != focusPane {
		t.Fatal("dismissing a tab should leave the workspace up")
	}
	if m.onShells {
		t.Fatal("with no tabs left, focus returns to the terminal")
	}
}

// A terminal that cannot be opened reports on the status line and leaves the
// window where it was, rather than stranding it in an empty pane.
func TestFailedOpenIsReported(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.openExecErr = errors.New("sandbox is not running")
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

// One session's attach failing does not take the workspace down: the tab is
// reported and dropped, and the terminal keeps the screen.
func TestAFailedTabDegradesToAReport(t *testing.T) {
	now := time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)
	ds := newFakeSource(testSandboxes()...)
	ds.execs = []Exec{
		{ID: "exec_bad", Command: []string{"/bin/bash"}, Tty: true, Live: true, CreatedAt: now},
	}
	ds.openExecErrFor = map[string]error{"exec_bad": errors.New("session is sealed")}

	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the report", func() bool { return m.statusE })

	if m.terminal == nil || m.focus != focusPane {
		t.Fatal("the workspace should survive a tab that cannot open")
	}
	if len(m.shells) != 0 {
		t.Fatal("the failed session should not be a tab")
	}
	if !strings.Contains(m.status, "session is sealed") {
		t.Fatalf("status = %q, want the failure", m.status)
	}
}

// A reconnect never appears in the terminal's output — the stream simply
// carries on — so the pane says so itself.
func TestReconnectIsShownInThePane(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	term.events <- TerminalEvent{State: TerminalReconnecting}
	d.wait("the reconnect", func() bool { return m.terminal.status != "" })
	if !strings.Contains(frameText(m), "reconnecting") {
		t.Errorf("the pane should say it is reconnecting:\n%s", frameText(m))
	}

	term.events <- TerminalEvent{State: TerminalReconnected}
	d.wait("the reconnection", func() bool { return m.terminal.status == "" })
}

// The hardware cursor has to land on the cell the sandbox believes it is on.
// Every row of chrome above the grid is an offset, and getting it wrong puts
// the cursor a line off for the whole session.
func TestPaneCursorLandsOnTheGrid(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	term.send("abc")
	d.wait("output", func() bool { return strings.Contains(frameText(m), "abc") })

	cursor := m.paneCursor()
	if cursor == nil {
		t.Fatal("an attached pane should place the cursor")
	}
	// The border and the air inside it across; the header, the line naming the
	// box, and the border's top edge down.
	originX, originY := m.paneOrigin(m.focusedPane())
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
	d, m, term := openWorkspace(t, ds, "enter")

	cols, _ := m.paneCells(m.paneWidthOf(m.terminal))
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
	d, m, term := openWorkspace(t, ds, "enter")

	term.send("\x1b]2;go test ./...\x07")
	d.wait("the title", func() bool { return m.terminal.term.Title() == "go test ./..." })

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
	d, m, term := openWorkspace(t, ds, "enter")

	long := strings.Repeat("very long title ", 12)
	term.send("\x1b]2;" + long + "\x07")
	d.wait("the title", func() bool { return m.terminal.term.Title() == long })

	border := ansi.Strip(strings.Split(rawFrame(m), "\n")[1])
	if strings.Contains(border, "very long title very long") {
		t.Fatalf("a title with no room should be dropped: %q", border)
	}
	if lipgloss.Width(border) != m.width {
		t.Fatalf("the border is %d cells, want %d", lipgloss.Width(border), m.width)
	}
}

// Ctrl-C never quits the window from inside a pane: there it belongs to the
// program. It quits from everywhere else, which is where nothing is running to
// take it.
func TestCtrlCNeverQuitsFromAPane(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	d.key("ctrl+c")
	d.settle()
	if m.quit {
		t.Fatal("ctrl+c in a pane should reach the program, not quit")
	}
	if !m.inPanes() {
		t.Fatal("ctrl+c in a pane should not detach either")
	}
	if got := term.typed("\x03"); !strings.Contains(got, "\x03") {
		t.Fatalf("typed %q, want the interrupt to reach the sandbox", got)
	}

	// Out of the workspace, and it is the window's again.
	d.key("ctrl+a")
	d.key("d")
	d.wait("the workspace to close", func() bool { return !m.inPanes() })
	d.key("ctrl+c")
	if !m.quit {
		t.Fatal("ctrl+c outside a pane should quit")
	}
}

// While panes are up the terminal always reports the mouse — selection and
// click-to-focus need the events — and a sandbox that asked for the mouse is
// forwarded them, translated out of screen space.
func TestMouseIsForwardedToTheSandboxThatAskedForIt(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	if m.View().MouseMode != tea.MouseModeCellMotion {
		t.Fatal("a workspace should report the mouse for selection")
	}

	term.send("\x1b[?1000h\x1b[?1006hCLICKS")
	d.wait("mouse mode", func() bool {
		p := m.focusedPane()
		return p != nil && p.term.MouseMode() != termpane.MouseNone
	})

	// A click on the grid reaches the sandbox, translated out of screen space.
	originX, originY := m.paneOrigin(m.focusedPane())
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

// ctrl+a m takes the mouse from a sandbox that is using it, for when you
// would rather copy a stack trace than click on it. The terminal keeps
// reporting either way: the events drive selection while the mouse is taken.
func TestPrefixMSeizesTheMouse(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	term.send("\x1b[?1000h\x1b[?1006hCLICKS")
	d.wait("mouse mode", func() bool {
		p := m.focusedPane()
		return p != nil && p.term.MouseMode() != termpane.MouseNone
	})

	d.key("ctrl+a")
	d.key("m")
	if !m.mouseSeized {
		t.Fatal("ctrl+a m should take the mouse")
	}
	if m.View().MouseMode != tea.MouseModeCellMotion {
		t.Fatal("the terminal keeps reporting while the mouse is taken")
	}
	if !strings.Contains(m.status, "mouse") {
		t.Fatalf("status = %q, want it to say what changed", m.status)
	}

	// Taken, a click drives selection rather than the sandbox.
	before := len(term.typed(""))
	originX, originY := m.paneOrigin(m.focusedPane())
	d.dispatch(tea.MouseClickMsg{X: originX, Y: originY, Button: tea.MouseLeft})
	d.dispatch(tea.MouseReleaseMsg{X: originX, Y: originY, Button: tea.MouseLeft})
	if after := len(term.typed("")); after != before {
		t.Fatal("a seized click reached the sandbox")
	}

	d.key("ctrl+a")
	d.key("m")
	if m.mouseSeized {
		t.Fatal("ctrl+a m again should give the mouse back")
	}
}

// Dragging over a pane whose sandbox never asked for the mouse selects, and
// releasing copies: the clipboard commands run and the status line says so.
func TestDragSelectsAndCopiesFromThePane(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	term.send("hello world")
	d.wait("output", func() bool { return strings.Contains(frameText(m), "hello world") })

	originX, originY := m.paneOrigin(m.focusedPane())
	d.dispatch(tea.MouseClickMsg{X: originX, Y: originY, Button: tea.MouseLeft})
	d.dispatch(tea.MouseMotionMsg{X: originX + 4, Y: originY, Button: tea.MouseLeft})
	d.dispatch(tea.MouseReleaseMsg{X: originX + 4, Y: originY, Button: tea.MouseLeft})
	d.wait("the copy", func() bool { return m.status == "copied" })
	if got := m.focusedPane().term.SelectionText(); got != "hello" {
		t.Fatalf("selected %q, want %q", got, "hello")
	}
}

// Clicking a pane focuses it: with a mouse in hand, pointing at the thing is
// how you say which one you mean.
func TestClickFocusesThePaneUnderIt(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key("s")
	d.wait("the first tab", func() bool { return len(m.shells) == 1 })
	d.wait("the tab to have focus", func() bool { return m.onShells })

	click := func(x, y int) {
		d.dispatch(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
		d.dispatch(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	}
	x, y := m.paneOrigin(m.terminal)
	click(x, y)
	if m.onShells {
		t.Fatal("clicking the terminal should focus it")
	}
	x, y = m.paneOrigin(m.shells[0])
	click(x, y)
	if !m.onShells {
		t.Fatal("clicking the tab should focus it back")
	}
}

// The primary terminal's title goes to the terminal's own title bar as well as
// the header: the header says what is in the window, and the title bar is how
// you find the window among the others you have open.
func TestTheTerminalTitleFollowsThePrimaryPane(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	// Until the application says otherwise, the sandbox is what to call it.
	if got, want := m.View().WindowTitle, "fix flaky pool reaper tests"; got != want {
		t.Fatalf("title = %q, want %q", got, want)
	}

	term.send("\x1b]2;go test ./...\x07")
	d.wait("the title", func() bool { return m.View().WindowTitle == "go test ./..." })

	// And with no pane the title is left exactly as the shell that started this
	// window left it: it is a guest in someone else's terminal.
	d.key("ctrl+a")
	d.key("d")
	d.wait("the workspace to close", func() bool { return !m.inPanes() })
	if got := m.View().WindowTitle; got != "" {
		t.Fatalf("title = %q, want the terminal's own left alone", got)
	}
}

// A shell tab is something you are doing inside this window, so it never takes
// the window's name: the title bar is read from outside, where what matters is
// which discobox this is and what its agent is doing.
func TestTheTerminalTitleIgnoresTheFocusedTab(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "s")
	d.wait("the tab", func() bool { return len(m.shells) == 1 })
	d.wait("the tab to have focus", func() bool { return m.onShells })

	term.send("\x1b]2;go test ./...\x07")
	d.wait("the title", func() bool { return m.View().WindowTitle == "go test ./..." })

	ds.execTerm("exec_shell1").send("\x1b]2;less config.yaml\x07")
	d.wait("the tab's own title", func() bool {
		return strings.TrimSpace(m.shells[0].term.Title()) == "less config.yaml"
	})
	if got, want := m.View().WindowTitle, "go test ./..."; got != want {
		t.Fatalf("title = %q, want the primary's %q", got, want)
	}
}

// The leader plus s always opens a fresh shell: the tabs are the server's
// sessions, and asking for another shell is asking for another session.
func TestLeaderSOpensAnotherShell(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key("s")
	d.wait("the first tab", func() bool { return len(m.shells) == 1 })
	d.key("ctrl+a")
	d.key("s")
	d.wait("the second tab", func() bool { return len(m.shells) == 2 })

	if !m.onShells || m.shells[m.activeShell].execID != "exec_shell2" {
		t.Fatal("the newest shell should have focus")
	}
	// Both are the same discobox.
	if m.shells[0].sandbox.ID != m.shells[1].sandbox.ID {
		t.Fatal("a shell belongs to the discobox it was opened in")
	}
}

// The leader plus a goes back to the terminal — there is always exactly one,
// so the key is a place, not an opener.
func TestLeaderAFocusesTheTerminal(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return len(m.shells) == 1 })
	if !m.onShells {
		t.Fatal("the fresh shell should have focus")
	}

	before := len(ds.execOpened())
	d.key("ctrl+a")
	d.key("a")
	if m.onShells {
		t.Fatal("leader a should focus the terminal")
	}
	if got := len(ds.execOpened()); got != before {
		t.Fatal("leader a should open nothing")
	}
}

// The leader plus h and l walk the strip — the terminal, then the tabs — and
// stop at the ends rather than wrapping.
func TestLeaderMovesBetweenTerminalAndTabs(t *testing.T) {
	now := time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)
	ds := newFakeSource(testSandboxes()...)
	ds.execs = []Exec{
		{ID: "exec_a", Command: []string{"/bin/bash"}, Tty: true, Live: true, CreatedAt: now},
		{ID: "exec_b", Command: []string{"/bin/zsh"}, Tty: true, Live: true, CreatedAt: now.Add(time.Minute)},
	}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the tabs", func() bool { return len(m.shells) == 2 })

	if m.onShells {
		t.Fatal("focus starts on the terminal")
	}
	d.key("ctrl+a")
	d.key("l")
	if !m.onShells || m.activeShell != 0 {
		t.Fatalf("onShells=%v tab=%d, want the first tab", m.onShells, m.activeShell)
	}
	d.key("ctrl+a")
	d.key("l")
	if m.activeShell != 1 {
		t.Fatalf("tab = %d, want the second", m.activeShell)
	}
	d.key("ctrl+a")
	d.key("l")
	if m.activeShell != 1 {
		t.Fatal("the right edge should stop rather than wrap")
	}
	d.key("ctrl+a")
	d.key("h")
	if m.activeShell != 0 {
		t.Fatalf("tab = %d, want the first again", m.activeShell)
	}
	d.key("ctrl+a")
	d.key("h")
	if m.onShells {
		t.Fatal("left from the first tab should reach the terminal")
	}
	d.key("ctrl+a")
	d.key("h")
	if m.onShells {
		t.Fatal("the left edge should stop rather than wrap")
	}
}

// Keys reach whichever pane has focus, and ctrl+c is not one the window takes:
// it reaches the program in a shell tab exactly as it does in the terminal,
// and the way out — the whole workspace's — is the leader's in both.
func TestEachPaneKeepsItsOwnKeys(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, harness := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return len(m.shells) == 1 })
	shell := ds.execTerm("exec_shell1")

	// The shell has focus, and ctrl+c is its own.
	d.key("ctrl+c")
	if got := shell.typed("\x03"); !strings.Contains(got, "\x03") {
		t.Fatalf("shell typed %q, want the interrupt", got)
	}
	if !m.inPanes() || len(m.shells) != 1 {
		t.Fatal("ctrl+c in a shell should not detach anything")
	}
	if !strings.Contains(m.hints(), m.leader()+" d detach") {
		t.Fatalf("the keys should say the way out: %q", m.hints())
	}

	// Back on the terminal, ctrl+c reaches the harness too. Someone who types
	// it to stop an agent has to have stopped the agent.
	d.key("ctrl+a")
	d.key("h")
	d.key("ctrl+c")
	if got := harness.typed("\x03"); !strings.Contains(got, "\x03") {
		t.Fatalf("the harness typed %q, want the interrupt", got)
	}
	if !m.inPanes() || m.focus != focusPane {
		t.Fatal("ctrl+c in a harness should not detach it")
	}
}

// The workspace splits the row exactly: the terminal's box plus the shell box
// come out at the window's width, whatever the division left over.
func TestSplitPanesShareTheScreen(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return len(m.shells) == 1 })
	ds.execTerm("exec_shell1").send("in the shell")
	d.wait("output", func() bool { return strings.Contains(frameText(m), "in the shell") })

	for i, line := range strings.Split(rawFrame(m), "\n") {
		if lipgloss.Width(line) != m.width {
			t.Fatalf("line %d is %d cells, want %d: %q", i, lipgloss.Width(line), m.width, ansi.Strip(line))
		}
	}
	// The terminal names what it is, and the strip names the tab.
	frame := ansi.Strip(rawFrame(m))
	if !strings.Contains(frame, "[ attach ]") || !strings.Contains(frame, "[ 1 zsh ]") {
		t.Fatalf("each side should name what it is running:\n%s", frame)
	}
}

// apply writes to the repository on this machine and can stop to ask about it,
// so from the list the window steps aside for it rather than drawing it in a
// pane.
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
// read. An apply with little to say is over in a moment, and a pane that
// vanished with it would be a screen you never got to see.
func TestAFinishedCommandHoldsItsScreen(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("y") // apply
	d.wait("the command", func() bool { return m.overlay != nil })

	apply := ds.terminals[len(ds.terminals)-1]
	apply.send("applied 2 commits\r\nnothing left behind\r\n")
	d.wait("output", func() bool { return strings.Contains(frameText(m), "nothing left behind") })

	// The command returns.
	apply.Close()
	d.wait("the pane to settle", func() bool { return m.overlay != nil && m.overlay.exited })

	// It is still there, and still readable.
	if m.overlay == nil {
		t.Fatal("the screen should have been kept")
	}
	if !strings.Contains(plainFrame(m), "nothing left behind") {
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
	if m.focus != focusPane {
		t.Fatalf("focus = %v, want the workspace it was over", m.focus)
	}
}

// The primary session that ends is gone: the workspace was a view onto it, so
// it is not held.
func TestAnEndedSessionIsNotHeld(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	term.Close()
	d.wait("the workspace to close", func() bool { return !m.inPanes() })
}

// The discobox is identified once, folded into the banner rather than given a
// line of its own, and centered in it along with where its work sits. Its id
// rather than its name: that is what you would type at a shell to act on this
// one.
func TestTheDiscoboxIDIsCenteredInTheBanner(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	// The id leads the banner's middle; the git columns follow it.
	const middle = "sbx_one  main@a3f9c21*  dirty  +142 −38"

	nameRow := func() string { return ansi.Strip(strings.Split(rawFrame(m), "\n")[0]) }
	centeredAt := func(row, name string) bool {
		start := len([]rune(row[:strings.Index(row, name)]))
		want := (len([]rune(row)) - len([]rune(name))) / 2
		return start >= want-1 && start <= want+1
	}

	row := nameRow()
	if !strings.Contains(row, middle) {
		t.Fatalf("the banner does not carry %q: %q", middle, row)
	}
	if !centeredAt(row, middle) {
		t.Fatalf("the banner's middle is not centered: %q", row)
	}
	if strings.Contains(row, "fix flaky pool reaper tests") {
		t.Fatalf("the banner should carry the id, not the name: %q", row)
	}

	// The transport's status displaces the keys rather than the name, and does
	// not move it: it is centered in the row, not in what is left of it.
	before := strings.Index(nameRow(), "sbx_one")
	m.terminal.status = "reconnecting…"
	if after := strings.Index(nameRow(), "sbx_one"); before != after {
		t.Fatalf("the id moved when a status appeared: %d then %d", before, after)
	}
	if !strings.Contains(nameRow(), "reconnecting") {
		t.Fatalf("the status should still be shown: %q", nameRow())
	}

	// And one name over the whole workspace, not one per pane.
	m.terminal.status = ""
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return len(m.shells) == 1 })
	if got := strings.Count(nameRow(), "sbx_one"); got != 1 {
		t.Fatalf("the id appears %d times over the split, want once", got)
	}
	if !centeredAt(nameRow(), middle) {
		t.Fatalf("the banner's middle is not centered over the split: %q", nameRow())
	}
}

// The banner's git status is the listing's, live: the workspace was opened on
// a snapshot, and a session that commits while you watch it must not leave the
// header saying what was true when you attached.
func TestTheBannerFollowsTheListingsGitStatus(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	nameRow := func() string { return ansi.Strip(strings.Split(rawFrame(m), "\n")[0]) }
	if !strings.Contains(nameRow(), "main@a3f9c21*  dirty") {
		t.Fatalf("the banner does not show the sandbox as dirty: %q", nameRow())
	}

	// The agent reports: the work is committed, and an apply has landed it on
	// the host under a commit of its own.
	moved := testSandboxes()
	moved[0].Git = GitState{
		Known: true, Branch: "main", Commit: "b17e004",
		Applied: true, AppliedCommit: "9dd21fa",
	}
	moved[0].Diff = DiffStat{Known: true, Added: 12, Deleted: 3, Files: 2}
	ds.mu.Lock()
	ds.sandboxes = moved
	ds.mu.Unlock()
	d.dispatch(tickMsg{})
	d.wait("the refresh", func() bool { return strings.Contains(nameRow(), "applied") })

	if got := nameRow(); !strings.Contains(got, "main@9dd21fa✓  applied  +12 −3") {
		t.Fatalf("the banner did not follow the listing: %q", got)
	}
}

// What the sandbox is serving rides the same banner, as protocol/number per
// port: the whole point of knowing a port is listening is knowing whether it is
// a page you could open, and the address it is bound on decides nothing a
// forward from inside the sandbox cares about.
func TestTheBannerCarriesTheListeningPorts(t *testing.T) {
	serving := testSandboxes()
	serving[0].Ports = []Port{
		{Number: 22, Protocol: "tcp"},
		{Number: 5173, Protocol: "http"},
		{Number: 8443, Protocol: "https"},
	}
	ds := newFakeSource(serving...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.dispatch(sizeMsg(180, 40))

	nameRow := func() string { return ansi.Strip(strings.Split(rawFrame(m), "\n")[0]) }
	if got := nameRow(); !strings.Contains(got, "http:5173 · https:8443 · tcp:22") {
		t.Fatalf("the banner does not carry the listening ports: %q", got)
	}
}

// A sandbox serving nothing says nothing: an empty list is not a field worth
// the width, and it reads the same as an agent that has not reported yet.
func TestTheBannerOmitsPortsWhenNothingIsListening(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	_, m, _ := openWorkspace(t, ds, "enter")

	row := ansi.Strip(strings.Split(rawFrame(m), "\n")[0])
	for _, group := range []string{"http:", "https:", "tcp:", "?:"} {
		if strings.Contains(row, group) {
			t.Fatalf("the banner carries %q for a sandbox serving nothing: %q", group, row)
		}
	}
	if !strings.Contains(row, "sbx_one  main@a3f9c21*  dirty") {
		t.Fatalf("the rest of the banner should be unchanged: %q", row)
	}
}

// The ports follow the listing the way the git fields do, so a dev server
// started in one of these panes shows up in the header above it.
func TestTheBannerFollowsTheListingsPorts(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.dispatch(sizeMsg(180, 40))

	nameRow := func() string { return ansi.Strip(strings.Split(rawFrame(m), "\n")[0]) }
	if strings.Contains(nameRow(), "http/") {
		t.Fatalf("the banner should start with nothing listening: %q", nameRow())
	}

	served := testSandboxes()
	served[0].Ports = []Port{{Number: 5173, Protocol: "http"}}
	ds.mu.Lock()
	ds.sandboxes = served
	ds.mu.Unlock()
	d.dispatch(tickMsg{})
	d.wait("the refresh", func() bool { return strings.Contains(nameRow(), "http:5173") })
}

// The banner's middle is what the window is about; its edges are context the
// screen carries elsewhere. So a row too narrow for all three gives the edges
// up first, in the order they are worth least here: the keys, which the hints
// line under the grid repeats; then the program's own name, which you can see
// you are in; then the folder, which every row of the list already shared.
func TestTheBannerGivesUpItsEdgesBeforeItsMiddle(t *testing.T) {
	serving := testSandboxes()
	serving[0].Ports = []Port{{Number: 5173, Protocol: "http"}, {Number: 8443, Protocol: "https"}}
	ds := newFakeSource(serving...)
	d, m, _ := openWorkspace(t, ds, "enter")

	const middle = "sbx_one  main@a3f9c21*  dirty  +142 −38  http:5173 · https:8443"
	nameRow := func() string { return ansi.Strip(strings.Split(rawFrame(m), "\n")[0]) }

	for _, tc := range []struct {
		width  int
		what   string
		keys   bool
		brand  bool
		folder bool
	}{
		{width: 140, what: "room for everything", keys: true, brand: true, folder: true},
		{width: 120, what: "the keys go first", brand: true, folder: true},
		{width: 92, what: "then the program's own name", folder: true},
		{width: 80, what: "then the folder"},
	} {
		d.dispatch(sizeMsg(tc.width, 40))
		row := nameRow()
		// Whatever went, the middle is still whole: that is the point of the
		// order — the edges pay for it.
		if !strings.Contains(row, middle) {
			t.Fatalf("%s: the middle is not whole at %d columns: %q", tc.what, tc.width, row)
		}
		if got := strings.Contains(row, "detach"); got != tc.keys {
			t.Fatalf("%s: keys shown = %v at %d columns: %q", tc.what, got, tc.width, row)
		}
		// The folder path contains "disco2", so the brand is looked for where
		// only the brand can be: at the head of the row.
		if got := strings.HasPrefix(strings.TrimSpace(row), "disco"); got != tc.brand {
			t.Fatalf("%s: brand shown = %v at %d columns: %q", tc.what, got, tc.width, row)
		}
		if got := strings.Contains(row, "/src/disco2"); got != tc.folder {
			t.Fatalf("%s: folder shown = %v at %d columns: %q", tc.what, got, tc.width, row)
		}
	}
}

// What the transport is doing holds its place all the way down. It displaces
// the keys while it is happening, and unlike them it is written down nowhere
// else on the screen, so it is not one of the things the row gives up.
func TestTheBannerKeepsATransportStatusAsItNarrows(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	m.terminal.status = "reconnecting…"

	nameRow := func() string { return ansi.Strip(strings.Split(rawFrame(m), "\n")[0]) }
	for _, width := range []int{140, 100, 80, 60} {
		d.dispatch(sizeMsg(width, 40))
		row := nameRow()
		if !strings.Contains(row, "reconnecting") {
			t.Fatalf("at %d columns the status was given up: %q", width, row)
		}
		if !strings.Contains(row, "sbx_one") {
			t.Fatalf("at %d columns the id was given up: %q", width, row)
		}
	}
}

// Once there are no edges left to give, the middle drops its own fields whole
// from the right — the ports first, then the diffstat, which the apply report
// gives you anyway, then the word, whose mark the position carries regardless —
// so a narrow window loses a field rather than showing half of one. The id
// never goes: it is what identifies the window.
func TestTheBannersMiddleDropsFieldsWholeAsItNarrows(t *testing.T) {
	serving := testSandboxes()
	serving[0].Ports = []Port{{Number: 5173, Protocol: "http"}, {Number: 8443, Protocol: "https"}}
	ds := newFakeSource(serving...)
	d, m, _ := openWorkspace(t, ds, "enter")

	nameRow := func() string { return ansi.Strip(strings.Split(rawFrame(m), "\n")[0]) }
	for _, tc := range []struct {
		width int
		want  []string
		gone  []string
	}{
		{70, []string{"sbx_one", "main@a3f9c21*", "dirty", "+142 −38", "http:5173"}, nil},
		{60, []string{"sbx_one", "main@a3f9c21*", "dirty", "+142 −38"}, []string{"http:5173"}},
		{44, []string{"sbx_one", "main@a3f9c21*", "dirty"}, []string{"http:5173", "+142"}},
		{34, []string{"sbx_one", "main@a3f9c21*"}, []string{"dirty", "+142"}},
		{27, []string{"sbx_one"}, []string{"a3f9c21", "dirty", "+142"}},
	} {
		d.dispatch(sizeMsg(tc.width, 40))
		row := nameRow()
		for _, want := range tc.want {
			if !strings.Contains(row, want) {
				t.Fatalf("at %d columns the banner dropped %q: %q", tc.width, want, row)
			}
		}
		for _, gone := range tc.gone {
			if strings.Contains(row, gone) {
				t.Fatalf("at %d columns the banner still carries %q: %q", tc.width, gone, row)
			}
		}
	}
}

// Output longer than the pane can be read back through: a screen you cannot
// scroll is a screen whose first half you never saw.
func TestAFinishedCommandCanBeScrolled(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("y") // apply
	d.wait("the command", func() bool { return m.overlay != nil })
	apply := ds.terminals[len(ds.terminals)-1]

	// More lines than the pane is tall.
	_, rows := m.paneCells(m.width)
	var out strings.Builder
	for i := 1; i <= rows+20; i++ {
		if i > 1 {
			out.WriteString("\r\n")
		}
		fmt.Fprintf(&out, "line %d", i)
	}
	apply.send(out.String())
	d.wait("output", func() bool { return strings.Contains(frameText(m), fmt.Sprintf("line %d", rows+20)) })
	apply.Close()
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
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return len(m.shells) == 1 })

	// The arrows work like h and l.
	d.key("ctrl+a")
	d.key("left")
	if m.onShells {
		t.Fatal("left should land on the terminal")
	}
	d.key("ctrl+a")
	d.key("right")
	if !m.onShells {
		t.Fatal("right should land on the tab")
	}

	// And one leader carries a run of them while Ctrl is down.
	d.key("ctrl+a")
	d.key("ctrl+left")
	if m.onShells {
		t.Fatal("the first step should reach the terminal")
	}
	d.key("ctrl+right")
	if !m.onShells {
		t.Fatal("the run should continue without the leader")
	}
	d.key("ctrl+left")
	if m.onShells {
		t.Fatal("the run should still be going")
	}

	// Letting go of Ctrl ends it: the next key is the sandbox's again.
	d.key("left")
	shell := ds.execTerm("exec_shell1")
	d.key("left")
	if got := shell.typed(""); strings.Contains(got, "\x1b[D") {
		t.Fatal("the pane that was left should not have received the key")
	}
	if got := ds.execTerm(ExecPrimary).typed("\x1b[D"); !strings.Contains(got, "\x1b[D") {
		t.Fatalf("the focused pane should get the arrow once the run has ended: %q", got)
	}
}

// The leader plus a digit jumps straight to the pane wearing that number: 0
// the terminal, 1 through 9 the tabs — no walking. A number with no tab under
// it says so instead of moving anything.
func TestLeaderDigitsJumpToPanes(t *testing.T) {
	now := time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)
	ds := newFakeSource(testSandboxes()...)
	ds.execs = []Exec{
		{ID: "exec_a", Command: []string{"/bin/bash"}, Tty: true, Live: true, CreatedAt: now},
		{ID: "exec_b", Command: []string{"/bin/zsh"}, Tty: true, Live: true, CreatedAt: now.Add(time.Minute)},
	}
	d, m, term := openWorkspace(t, ds, "enter")
	d.wait("the tabs", func() bool { return len(m.shells) == 2 })

	d.key("ctrl+a")
	d.key("2")
	if !m.onShells || m.activeShell != 1 {
		t.Fatalf("onShells=%v tab=%d, want the second tab", m.onShells, m.activeShell)
	}
	d.key("ctrl+a")
	d.key("0")
	if m.onShells {
		t.Fatal("leader 0 should focus the terminal")
	}

	// A number with no tab under it reports and stays put.
	d.key("ctrl+a")
	d.key("7")
	if m.onShells {
		t.Fatal("a jump that lands nowhere should not move focus")
	}
	if !strings.Contains(m.status, "no tab 7") {
		t.Fatalf("status = %q, want it to say the tab is not there", m.status)
	}

	// A bare digit is the application's, as every unprefixed key is.
	d.key("3")
	if got := term.typed("3"); !strings.Contains(got, "3") {
		t.Fatalf("typed %q, want the digit to reach the sandbox", got)
	}
}

// A key typed after a run of moves is the application's, not a command: the
// prefix was held open for the run, never re-pressed, so ctrl+a ctrl+← ctrl+←
// followed by a bare letter types the letter rather than firing the binding it
// happens to be bound to.
func TestAKeyAfterARunIsNotACommand(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, harness := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return len(m.shells) == 1 })

	// A run back to the terminal, then a bare letter with Ctrl let go.
	d.key("ctrl+a")
	d.key("ctrl+left")
	if m.onShells {
		t.Fatal("the run should have reached the terminal")
	}
	d.key("x") // archive in the binding table — and the whole point
	if got := harness.typed("x"); !strings.Contains(got, "x") {
		t.Fatalf("typed %q, want the letter to reach the sandbox", got)
	}
	if len(ds.did) != 0 {
		t.Fatalf("did = %v, want no verb fired by a letter after a run", ds.did)
	}
}

// A command that runs and finishes takes the whole screen for as long as it
// runs. What is under it is untouched — still connected, still running, still
// where it was — and is back the moment the command exits.
func TestACommandTakesTheScreenOverTheWorkspace(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, harness := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return len(m.shells) == 1 })
	harness.send("the harness is still working")
	d.wait("output", func() bool { return strings.Contains(frameText(m), "still working") })

	d.key("ctrl+a")
	d.key("y") // apply
	d.wait("the command", func() bool { return m.overlay != nil })

	if m.overlay.action != InteractApply {
		t.Fatalf("overlay = %s, want apply", m.overlay.action)
	}
	// Both terminals are still there, and neither was resized to make room:
	// nothing was made room for.
	if m.terminal == nil || len(m.shells) != 1 {
		t.Fatal("the workspace should be kept under the command")
	}
	lcols, _ := m.paneCells(m.width / 2)
	if got := ds.execTerm(ExecPrimary).size(); got[0] != lcols {
		t.Fatalf("the terminal is %d columns, want the split's %d", got[0], lcols)
	}
	rcols, _ := m.paneCells(m.width - m.width/2)
	if got := ds.execTerm("exec_shell1").size(); got[0] != rcols {
		t.Fatalf("the shell is %d columns, want the split's %d", got[0], rcols)
	}
	// The command has the screen: it is the only box drawn, at the full width.
	full, _ := m.paneCells(m.width)
	if got := ds.terminals[len(ds.terminals)-1].size(); got[0] != full {
		t.Fatalf("the command opened at %d columns, want the screen's %d", got[0], full)
	}
	frame := plainFrame(m)
	if strings.Contains(frame, "still working") {
		t.Fatalf("the workspace should be under the command, not beside it:\n%s", frame)
	}
	if !strings.Contains(frame, "[ apply ]") {
		t.Fatalf("the command should name itself:\n%s", frame)
	}

	// It ends, and the workspace is back exactly as it was.
	apply := ds.terminals[len(ds.terminals)-1]
	apply.send("applied 2 commits\r\n")
	d.wait("the output", func() bool { return strings.Contains(frameText(m), "applied 2 commits") })
	apply.Close()
	d.wait("the command to settle", func() bool { return m.overlay != nil && m.overlay.exited })
	d.key("q")
	d.wait("the workspace", func() bool { return m.overlay == nil })

	if m.terminal == nil || len(m.shells) != 1 {
		t.Fatal("the workspace should be back as it was")
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
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key("x") // archive
	d.wait("the verb", func() bool { return len(ds.did) > 0 })

	if ds.did[0] != "archive sbx_one" {
		t.Fatalf("did = %v, want the box on screen archived", ds.did)
	}
	if m.focus != focusPane || !m.inPanes() {
		t.Fatalf("focus = %v, want the screen kept", m.focus)
	}
}

// A command that cannot run says why, rather than opening a screen that reports
// the same thing less clearly. The checks are the list's, on the same discobox.
func TestTheLeaderKeepsTheListsChecks(t *testing.T) {
	boxes := testSandboxes()
	boxes[0].Diff = DiffStat{Known: true}
	ds := newFakeSource(boxes...)
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key("y") // apply, with nothing to bring back
	d.settle()

	if m.overlay != nil {
		t.Fatal("an apply with nothing in it should not take the screen")
	}
	if m.dialog == nil || !strings.Contains(m.dialog.body, "nothing has changed") {
		t.Fatalf("dialog = %v, want it to say why", m.dialog)
	}
}

// The leader's way out works from every pane, whether or not the pane has one
// of its own. It costs the application nothing, being behind the leader, and
// one key that always works beats remembering which pane took Ctrl-C.
func TestTheLeaderDetachesFromAnyPane(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key("d")
	d.wait("the workspace to close", func() bool { return !m.inPanes() })
	if m.focus != focusList {
		t.Fatalf("focus = %v, want the list", m.focus)
	}
}
