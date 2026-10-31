package tui

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/discobox-ai/discobox/cli/internal/clipimage"
	"github.com/discobox-ai/discobox/termpane"
)

// openWorkspace drives the window into the workspace on the first sandbox, on
// a console wide enough for the workspace tree beside it.
func openWorkspace(t *testing.T, ds *fakeSource, act string) (*driver, *Model, *fakeTerminal) {
	t.Helper()
	return openWorkspaceAt(t, ds, act, 120)
}

// openWorkspaceAt is openWorkspace on a console width cells wide.
func openWorkspaceAt(t *testing.T, ds *fakeSource, act string, width int) (*driver, *Model, *fakeTerminal) {
	t.Helper()
	m := New(t.Context(), ds)
	m.logo = logo{}
	// A test copy must not clobber the developer's actual clipboard, and a
	// test press on a header link must not open a browser on their screen.
	m.copyOS = func(string) error { return nil }
	m.openOS = func(string) error { return nil }
	m.readClipboard = func(context.Context) ([]clipimage.Image, error) { return nil, nil }
	// The runtime is what releases the terminal around an action; there is none
	// here, so the action is simply run and its result handed back.
	m.exec = func(c tea.ExecCommand, done tea.ExecCallback) tea.Cmd {
		return func() tea.Msg {
			c.SetStdin(strings.NewReader(""))
			c.SetStdout(io.Discard)
			c.SetStderr(io.Discard)
			return done(c.Run())
		}
	}
	d := newDriver(t, m)
	d.startAt(width, 40)
	// The window opens on the first discobox once the listing is in.
	d.wait("the list", func() bool { return m.focus == focusList })

	d.key(act)
	d.wait("the workspace", func() bool { return m.focus == focusPane })
	d.wait("the primary terminal", func() bool { return m.primary() != nil })
	return d, m, ds.execTerm(ExecPrimary)
}

// Attaching draws the sandbox's terminal in the window rather than handing the
// real terminal over to a command.
func TestAttachDrawsInTheWindow(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	term.send("hello from the sandbox")
	d.wait("output", func() bool { return strings.Contains(frameText(m), "hello from the sandbox") })

	frame := frameText(m)
	// Inside the border, under a banner identifying the discobox and saying how
	// to get out: beside the tree there is no list to detach to, so the way
	// out is quitting.
	if !strings.Contains(frame, "sbx_one") {
		t.Errorf("the pane should identify its discobox:\n%s", frame)
	}
	if !strings.Contains(frame, "ctrl+a q quit") {
		t.Errorf("the pane should say how to get out:\n%s", frame)
	}
	if strings.Contains(frame, "detach") {
		t.Errorf("beside the tree there is nothing to detach to, so it is not offered:\n%s", frame)
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
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "s")
	d.wait("the tab", func() bool { return m.shells.len() == 1 })

	if !m.onShells {
		t.Fatal("the fresh shell should have focus")
	}
	if m.shells.panes[0].execID != "exec_shell1" {
		t.Fatalf("tab = %q, want the created shell", m.shells.panes[0].execID)
	}
	// And the primary is there too, on the left.
	if m.primary() == nil {
		t.Fatal("the workspace should still hold the primary terminal")
	}
}

// The terminal is opened at the size of the box it is going into — the whole
// width, with no other session to share the screen with.
func TestPaneOpensAtTheSizeItWillBeDrawnAt(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	_, m, term := openWorkspace(t, ds, "enter")

	cols, rows := m.paneCells(m.paneWidthOf(m.primary()))
	if got := term.size(); got != [2]int{cols, rows} {
		t.Fatalf("opened at %v, want %dx%d", got, cols, rows)
	}
	// The grid is the whole window bar its chrome, so it grows with it: the
	// border and a cell of air inside it on each side, right of the tree.
	if m.width != 120-treeWidth {
		t.Fatalf("the window is %d cells beside the tree, want %d", m.width, 120-treeWidth)
	}
	if rows != m.height-4 || cols != m.width-2-2*boxPad {
		t.Fatalf("pane is %dx%d in a %d-row window", cols, rows, m.height)
	}
	if got := len(m.primary().term.View()); got != rows {
		t.Fatalf("the pane drew %d rows, want %d", got, rows)
	}
}

// A tab opening splits the screen: the terminal is resized to the left half,
// and the shell was opened at the right half's size to begin with.
func TestATabSplitsTheScreen(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return m.shells.len() == 1 })

	lcols, lrows := m.paneCells(m.width / 2)
	if got := ds.execTerm(ExecPrimary).size(); got != [2]int{lcols, lrows} {
		t.Fatalf("the terminal is %v, want the left half %dx%d", got, lcols, lrows)
	}
	rcols, rrows := m.paneCells(m.width - m.width/2)
	if got := ds.execTerm("exec_shell1").size(); got != [2]int{rcols, rrows} {
		t.Fatalf("the shell is %v, want the right half %dx%d", got, rcols, rrows)
	}
	// The new shell has focus: it is the thing you just asked for.
	if !m.onShells || m.shells.active != 0 {
		t.Fatalf("onShells=%v activeShell=%d, want the new tab focused", m.onShells, m.shells.active)
	}
}

// The leader plus z is the same toggle the boxes' [+] button is, so the
// workspace maximizes without a mouse: the focused column takes the window,
// and the hidden one is resized for it too — it keeps emulating off-screen,
// and flipping back to it must show a screen drawn at the size it is shown at.
func TestLeaderZMaximizesTheFocusedColumn(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return m.shells.len() == 1 })
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
	t.Parallel()
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
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return m.shells.len() == 1 })
	m.toggleMaximized(true)

	m.closeTab(m.shells.panes[0])
	if m.maximized {
		t.Fatal("the last tab closing should give the window back")
	}
	if got := m.paneWidthOf(m.primary()); got != m.width {
		t.Fatalf("the terminal is %d cells wide, want the whole window (%d)", got, m.width)
	}
}

// Resizing the window resizes every terminal with it.
func TestResizingTheWindowResizesTheTerminal(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	d.dispatch(sizeMsg(100, 30))
	d.settle()
	cols, rows := m.paneCells(m.paneWidthOf(m.primary()))
	if got := term.size(); got != [2]int{cols, rows} {
		t.Fatalf("resized to %v, want %dx%d", got, cols, rows)
	}
}

// Every key belongs to the sandbox while a pane is up — including the ones the
// window would otherwise use.
func TestKeysGoToTheSandbox(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openNarrowWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return m.shells.len() == 1 })

	d.key("ctrl+a")
	d.key("d")
	d.wait("the workspace to close", func() bool { return !m.inPanes() })
	// What detaching says is its own message, which may land after the
	// workspace has closed.
	d.wait("the detach to be reported", func() bool { return m.status != "" })
	if m.focus != focusList {
		t.Fatalf("focus = %v, want the list", m.focus)
	}
	if m.shells.len() != 0 {
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
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	// Beside the tree quit is the one way out: there is no list to detach to.
	header := windowLines(m)[0]
	header = ansi.Strip(header)
	if !strings.Contains(header, "ctrl+a q quit") {
		t.Fatalf("the header should offer quit: %q", header)
	}
	if strings.Contains(header, "detach") {
		t.Fatalf("the header should not offer a detach that does nothing: %q", header)
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
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	term.Close()
	d.wait("the workspace to close", func() bool { return !m.inPanes() })
	// What detaching says is its own message, which may land after the
	// workspace has closed.
	d.wait("the detach to be reported", func() bool { return m.status != "" })
	if m.focus != focusList {
		t.Fatalf("focus = %v, want the list", m.focus)
	}
}

// Opening the workspace joins every live TTY session the discobox already has,
// each on the side the server's own record puts it: the discobox's terminals
// on the left, the primary first, and its shells as tabs on the right, both in
// session order. A session that has exited or has no terminal is neither.
func TestAttachJoinsEveryLiveSession(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)
	ds := newFakeSource(testSandboxes()...)
	ds.execs = []Exec{
		{ID: "exec_pri", Harness: "claude", Primary: true, Tty: true, Live: true, CreatedAt: now},
		{ID: "exec_term", Command: []string{"claude"}, Harness: "claude", Tty: true, Live: true, CreatedAt: now.Add(2 * time.Minute)},
		{ID: "exec_a", Command: []string{"/bin/bash"}, Tty: true, Live: true, CreatedAt: now.Add(time.Minute)},
		{ID: "exec_b", Command: []string{"/bin/zsh"}, Tty: true, Live: true, CreatedAt: now.Add(3 * time.Minute)},
		{ID: "exec_gone", Command: []string{"/bin/sh"}, Tty: true, Live: false, CreatedAt: now},
		{ID: "exec_pipe", Command: []string{"make"}, Live: true, CreatedAt: now},
	}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the tabs", func() bool { return m.shells.len() == 2 && m.terminals.len() == 2 })

	// The primary leads the terminals whatever order the attaches landed in,
	// and the harness terminal is beside it rather than among the shells.
	if !m.terminals.panes[0].primary || m.terminals.panes[1].execID != "exec_term" {
		t.Fatalf("terminals = %s | %s, want the primary then the harness terminal",
			m.terminals.panes[0].execID, m.terminals.panes[1].execID)
	}
	if m.shells.panes[0].execID != "exec_a" || m.shells.panes[1].execID != "exec_b" {
		t.Fatalf("tabs = %s | %s, want session order", m.shells.panes[0].execID, m.shells.panes[1].execID)
	}
	if m.onShells {
		t.Fatal("focus starts on the primary terminal")
	}
	// Both strips name every pane, and the numbering runs across the screen:
	// the primary is 0 and the shells carry on from the terminals.
	frame := plainFrame(m)
	for _, want := range []string{"0 attach", "1 claude", "2 bash", "3 zsh"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("the strips should name every pane, missing %q:\n%s", want, frame)
		}
	}
}

// A session started from anywhere else appears as a tab while the workspace is
// up: the screen mirrors the server, not what was opened here.
func TestASessionStartedElsewhereBecomesATab(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	ds.addExec(Exec{
		ID: "exec_other", Command: []string{"/usr/bin/fish"}, Tty: true, Live: true,
		CreatedAt: time.Date(2026, 8, 7, 12, 30, 0, 0, time.UTC),
	})
	d.dispatch(workspaceTickMsg{gen: m.wsGen})
	d.wait("the tab", func() bool { return m.shells.len() == 1 })

	if m.shells.panes[0].execID != "exec_other" {
		t.Fatalf("tab = %q, want the session started elsewhere", m.shells.panes[0].execID)
	}
	if m.onShells {
		t.Fatal("a tab arriving on its own does not steal focus")
	}
}

// The poll never opens a second pane onto a session already on screen — the
// shell the leader created included, whose tab and listing entry share an id.
func TestThePollDoesNotReopenASessionAlreadyShown(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return m.shells.len() == 1 })

	d.dispatch(workspaceTickMsg{gen: m.wsGen})
	d.settle()
	if m.shells.len() != 1 {
		t.Fatalf("tabs = %d, want the poll to leave the shown session alone", m.shells.len())
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
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openNarrowWorkspace(t, ds, "enter")
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
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return m.shells.len() == 1 })

	shell := ds.execTerm("exec_shell1")
	shell.send("make: everything built\r\n")
	d.wait("output", func() bool { return strings.Contains(frameText(m), "everything built") })

	// The shell exits — and is gone from the listing, as an exited session
	// would be.
	ds.endExec("exec_shell1")
	shell.Close()
	d.wait("the pager", func() bool { return m.shells.len() == 1 && m.shells.panes[0].exited })

	if !strings.Contains(plainFrame(m), "everything built") {
		t.Errorf("the last screen should still be up:\n%s", plainFrame(m))
	}
	if !strings.Contains(hintLine(m.hints()), "finished") {
		t.Errorf("hints = %q, want them to say the shell is over", hintLine(m.hints()))
	}

	d.key("q")
	d.wait("the tab to close", func() bool { return m.shells.len() == 0 })
	if m.primary() == nil || m.focus != focusPane {
		t.Fatal("dismissing a tab should leave the workspace up")
	}
	if m.onShells {
		t.Fatal("with no tabs left, focus returns to the terminal")
	}
}

// A held screen is a pane, not a modal: the leader's chords still reach the
// window from it, so a shell you have finished reading does not have to be
// dismissed before opening another one or walking to the next pane.
func TestAHeldPaneStillAnswersTheLeader(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.services = []Service{{ID: "otel", Name: "OTEL", Status: "stopped"}}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return m.shells.len() == 1 })

	shell := ds.execTerm("exec_shell1")
	ds.endExec("exec_shell1")
	shell.Close()
	d.wait("the held screen", func() bool { return m.shells.len() == 1 && m.shells.panes[0].exited })

	if m.focusedPane() != m.shells.panes[0] {
		t.Fatal("the shell that exited should still have the keys")
	}

	// A second shell, asked for from the screen that is over.
	d.key("ctrl+a")
	d.key("s")
	d.wait("the second tab", func() bool { return m.shells.len() == 2 })
	if !m.shells.panes[0].exited {
		t.Fatal("the held screen should still be held")
	}

	// A chord too: its second keystroke goes to the pane that opened it,
	// rather than being read as a key on the held screen.
	m.focusOrdinal(1)
	if m.focusedPane() != m.shells.panes[0] {
		t.Fatal("the held screen should be the one the chord is typed on")
	}
	d.key("ctrl+a")
	d.key(paneServicesKey)
	d.key(paneServicesMenuKey)
	d.wait("the services menu", func() bool { return m.dialog != nil })
	d.key("esc")
	d.wait("the menu gone", func() bool { return m.dialog == nil })

	// Everything else is still the reader's, dismissal included.
	d.key("q")
	d.wait("the tab to close", func() bool { return m.shells.len() == 1 })
}

// A terminal that cannot be opened reports on the status line and leaves the
// window where it was, rather than stranding it in an empty pane.
func TestFailedOpenIsReported(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.openExecErr = errors.New("sandbox is not running")
	m := New(t.Context(), ds)
	m.logo = logo{}
	d := newDriver(t, m)
	d.start()
	// The window opens on the first discobox once the listing is in.
	d.wait("the list", func() bool { return m.focus == focusList })
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
	t.Parallel()
	now := time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)
	ds := newFakeSource(testSandboxes()...)
	ds.execs = []Exec{
		{ID: "exec_bad", Command: []string{"/bin/bash"}, Tty: true, Live: true, CreatedAt: now},
	}
	ds.openExecErrFor = map[string]error{"exec_bad": errors.New("session is sealed")}

	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the report", func() bool { return m.statusE })

	if m.primary() == nil || m.focus != focusPane {
		t.Fatal("the workspace should survive a tab that cannot open")
	}
	if m.shells.len() != 0 {
		t.Fatal("the failed session should not be a tab")
	}
	if !strings.Contains(m.status, "session is sealed") {
		t.Fatalf("status = %q, want the failure", m.status)
	}
}

// A reconnect never appears in the terminal's output — the stream simply
// carries on — so the pane says so itself.
func TestReconnectIsShownInThePane(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	term.events <- TerminalEvent{State: TerminalReconnecting}
	d.wait("the reconnect", func() bool { return m.primary().status != "" })
	if !strings.Contains(frameText(m), "reconnecting") {
		t.Errorf("the pane should say it is reconnecting:\n%s", frameText(m))
	}

	term.events <- TerminalEvent{State: TerminalReconnected}
	d.wait("the reconnection", func() bool { return m.primary().status == "" })
}

// The hardware cursor has to land on the cell the sandbox believes it is on.
// Every row of chrome above the grid is an offset, and getting it wrong puts
// the cursor a line off for the whole session.
func TestPaneCursorLandsOnTheGrid(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	term.send("abc")
	d.wait("output", func() bool { return strings.Contains(frameText(m), "abc") })

	// The cursor the terminal is sent, in screen cells: right of the tree.
	cursor := m.View().Cursor
	if cursor == nil {
		t.Fatal("an attached pane should place the cursor")
	}
	// The border and the air inside it across; the header, the line naming the
	// box, and the border's top edge down.
	originX, originY := screenOrigin(m, m.focusedPane())
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

// The same check with the credential band up. It is two rows of chrome that
// appear mid-session — one above the boxes and one below — and the boxes lose
// two rows of height while starting one row lower. A band that took its rows
// from the wrong end, or an offset that counted both rows as though both were
// above the grid, puts the cursor a line off the cell the sandbox believes it
// is on for the rest of the session.
func TestPaneCursorLandsOnTheGridUnderTheCredentialBand(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	term.send("abc")
	d.wait("output", func() bool { return strings.Contains(frameText(m), "abc") })

	ds.mu.Lock()
	ds.requests = []CredentialRequest{waitingRequest()}
	ds.mu.Unlock()
	d.dispatch(tickMsg{})
	d.wait("the band", func() bool { return m.bannerTop() == 1 })
	d.wait("the text again", func() bool { return strings.Contains(frameText(m), "abc") })

	// The cursor the terminal is sent, in screen cells: right of the tree.
	cursor := m.View().Cursor
	if cursor == nil {
		t.Fatal("an attached pane should place the cursor")
	}
	originX, originY := screenOrigin(m, m.focusedPane())
	if cursor.X != originX+3 || cursor.Y != originY {
		t.Fatalf("cursor at %d,%d, want %d,%d", cursor.X, cursor.Y, originX+3, originY)
	}
	// And it is where the frame actually put the text, which is the check that
	// survives the chrome changing.
	lines := strings.Split(rawFrame(m), "\n")
	if len(lines) != m.height {
		t.Fatalf("frame = %d rows, want the window's %d", len(lines), m.height)
	}
	cells := []rune(ansi.Strip(lines[cursor.Y]))
	if got := string(cells[originX : originX+3]); got != "abc" {
		t.Fatalf("the grid row reads %q where the cursor is pointing at cell %d", got, cursor.X)
	}
	// The band is on the rows either side of the boxes, and the cursor is on
	// neither of them.
	for _, row := range m.banner.rows {
		if row == cursor.Y {
			t.Fatalf("the cursor is on row %d, which is the band", row)
		}
		if !strings.Contains(ansi.Strip(lines[row]), "credential request") {
			t.Fatalf("row %d does not carry the band: %q", row, ansi.Strip(lines[row]))
		}
	}
}

// The window's own frame is drawn by hand rather than by a bordered style,
// because such a style re-wraps full-width lines — and a re-wrapped terminal
// grid shifts every row below the wrap, putting the cursor on the wrong line
// for the rest of the session.
func TestTheFrameNeverWraps(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	cols, _ := m.paneCells(m.paneWidthOf(m.primary()))
	term.send(strings.Repeat("X", cols))
	d.wait("a full-width row", func() bool { return strings.Contains(frameText(m), strings.Repeat("X", cols)) })

	lines := strings.Split(rawFrame(m), "\n")
	if got, want := len(lines), m.height; got > want {
		t.Fatalf("a full-width row made the frame %d lines in a %d-row window", got, want)
	}
	for i, line := range lines {
		if lipgloss.Width(line) != m.screenWidth {
			t.Fatalf("line %d is %d cells, want %d: %q", i, lipgloss.Width(line), m.screenWidth, ansi.Strip(line))
		}
	}
}

// A title an application sets is laid into the terminal's own top border: it
// names the terminal rather than the window, and a border is a line the eye
// already follows, so it costs no row.
func TestApplicationTitleIsSetIntoTheBorder(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	term.send("\x1b]2;go test ./...\x07")
	d.wait("the title", func() bool { return m.primary().term.Title() == "go test ./..." })

	border := ansi.Strip(windowLines(m)[1])
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
	if line := ansi.Strip(windowLines(m)[0]); strings.Contains(line, "go test ./...") {
		t.Errorf("the title is repeated in the banner: %q", line)
	}
}

// A title too long to sit in the line with rule either side is dropped rather
// than squeezing the border out; the terminal's own title bar has it too.
func TestALongTitleLeavesTheBorderAlone(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	long := strings.Repeat("very long title ", 12)
	term.send("\x1b]2;" + long + "\x07")
	d.wait("the title", func() bool { return m.primary().term.Title() == long })

	border := ansi.Strip(windowLines(m)[1])
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
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openNarrowWorkspace(t, ds, "enter")

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

// The terminal reports the mouse on every screen the window draws — selection
// and click-to-focus need the events — and a sandbox that asked for the mouse
// is forwarded them, translated out of screen space.
func TestMouseIsForwardedToTheSandboxThatAskedForIt(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	if m.View().MouseMode == tea.MouseModeNone {
		t.Fatal("a workspace should report the mouse for selection")
	}

	term.send("\x1b[?1000h\x1b[?1006hCLICKS")
	d.wait("mouse mode", func() bool {
		p := m.focusedPane()
		return p != nil && p.term.MouseMode() != termpane.MouseNone
	})

	// A click on the grid reaches the sandbox, translated out of screen space.
	originX, originY := screenOrigin(m, m.focusedPane())
	d.dispatch(tea.MouseClickMsg{X: originX + 4, Y: originY + 2, Button: tea.MouseLeft})
	if got := term.typed("\x1b[<"); !strings.Contains(got, "\x1b[<0;5;3") {
		t.Fatalf("typed %q, want a click at grid cell 4,2", got)
	}

	// A click on the window's own chrome is not the sandbox's.
	before := len(term.typed(""))
	d.windowMouse(tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft})
	if after := len(term.typed("")); after != before {
		t.Fatal("a click on the frame should not reach the sandbox")
	}
}

// A click on the box's air reaches a sandbox that asked for the mouse as a
// matched pair on the grid's edge column. The press is pulled onto the grid,
// and so is the release: forwarding one without the other leaves the
// application holding a button that never comes up.
func TestAClickOnTheAirForwardsBothHalvesToTheSandbox(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	term.send("\x1b[?1000h\x1b[?1006hCLICKS")
	d.wait("mouse mode", func() bool {
		p := m.focusedPane()
		return p != nil && p.term.MouseMode() != termpane.MouseNone
	})

	originX, originY := screenOrigin(m, m.focusedPane())
	d.dispatch(tea.MouseClickMsg{X: originX - boxPad, Y: originY, Button: tea.MouseLeft})
	d.dispatch(tea.MouseReleaseMsg{X: originX - boxPad, Y: originY, Button: tea.MouseLeft})
	if got := term.typed("\x1b[<0;1;1m"); !strings.Contains(got, "\x1b[<0;1;1M") ||
		!strings.Contains(got, "\x1b[<0;1;1m") {
		t.Fatalf("typed %q, want a press and a release at grid cell 0,0", got)
	}
}

// ctrl+a m takes the mouse from a sandbox that is using it, for when you
// would rather copy a stack trace than click on it. The terminal keeps
// reporting either way: the events drive selection while the mouse is taken.
func TestPrefixMSeizesTheMouse(t *testing.T) {
	t.Parallel()
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
	if m.View().MouseMode == tea.MouseModeNone {
		t.Fatal("the terminal keeps reporting while the mouse is taken")
	}
	if !strings.Contains(m.status, "mouse") {
		t.Fatalf("status = %q, want it to say what changed", m.status)
	}

	// Taken, a click drives selection rather than the sandbox.
	before := len(term.typed(""))
	originX, originY := screenOrigin(m, m.focusedPane())
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
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	term.send("hello world")
	d.wait("output", func() bool { return strings.Contains(frameText(m), "hello world") })

	originX, originY := screenOrigin(m, m.focusedPane())
	d.dispatch(tea.MouseClickMsg{X: originX, Y: originY, Button: tea.MouseLeft})
	d.dispatch(tea.MouseMotionMsg{X: originX + 4, Y: originY, Button: tea.MouseLeft})
	d.dispatch(tea.MouseReleaseMsg{X: originX + 4, Y: originY, Button: tea.MouseLeft})
	d.wait("the copy", func() bool { return m.status == "copied" })
	if got := m.focusedPane().term.SelectionText(); got != "hello" {
		t.Fatalf("selected %q, want %q", got, "hello")
	}
}

// The cell of air the box draws beside the grid belongs to the pane: a drag
// that starts on it selects from the edge of the text and goes on wrapping
// down the pane's own lines, rather than falling through to the chrome's
// selection across the whole frame.
func TestDragFromTheBoxAirSelectsInThePane(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	term.send("hello world\r\nsecond line")
	d.wait("output", func() bool { return strings.Contains(frameText(m), "second line") })

	originX, originY := screenOrigin(m, m.focusedPane())
	d.dispatch(tea.MouseClickMsg{X: originX - boxPad, Y: originY, Button: tea.MouseLeft})
	d.dispatch(tea.MouseMotionMsg{X: originX + 5, Y: originY + 1, Button: tea.MouseLeft})
	d.dispatch(tea.MouseReleaseMsg{X: originX + 5, Y: originY + 1, Button: tea.MouseLeft})
	d.wait("the copy", func() bool { return m.status == "copied" })
	if m.chromeSel.Active() {
		t.Fatal("the air beside the grid started a chrome selection")
	}
	if got := m.focusedPane().term.SelectionText(); got != "hello world\nsecond" {
		t.Fatalf("selected %q, want %q", got, "hello world\nsecond")
	}
}

// The air on the far side is the pane's too, and a press on it means the last
// cell of the row rather than nothing.
func TestPressInTheRightAirSelectsToTheEndOfTheRow(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	term.send("hello world")
	d.wait("output", func() bool { return strings.Contains(frameText(m), "hello world") })

	p := m.focusedPane()
	originX, originY := screenOrigin(m, p)
	cols, _ := m.paneCells(m.paneWidthOf(p))
	right := originX + cols - 1 + boxPad
	d.dispatch(tea.MouseClickMsg{X: right, Y: originY, Button: tea.MouseLeft})
	d.dispatch(tea.MouseMotionMsg{X: originX, Y: originY, Button: tea.MouseLeft})
	d.dispatch(tea.MouseReleaseMsg{X: originX, Y: originY, Button: tea.MouseLeft})
	d.wait("the copy", func() bool { return m.status == "copied" })
	if got := p.term.SelectionText(); got != "hello world" {
		t.Fatalf("selected %q, want %q", got, "hello world")
	}
}

// The air is one cell and no more. The border column just outside it is still
// the chrome's at a grid row, the same as it is on the rows where it carries
// the tabs and the title: a drag there runs as the chrome's selection, so
// border text stays drag-selectable.
func TestTheBorderBesideTheAirStaysTheChromes(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	term.send("hello world")
	d.wait("output", func() bool { return strings.Contains(frameText(m), "hello world") })

	p := m.focusedPane()
	originX, originY := screenOrigin(m, p)
	cols, _ := m.paneCells(m.paneWidthOf(p))
	for _, tc := range []struct {
		side string
		x    int
	}{
		{"left", originX - boxPad - 1},
		{"right", originX + cols + boxPad},
	} {
		d.dispatch(tea.MouseClickMsg{X: tc.x, Y: originY, Button: tea.MouseLeft})
		d.dispatch(tea.MouseMotionMsg{X: tc.x, Y: originY + 1, Button: tea.MouseLeft})
		d.dispatch(tea.MouseReleaseMsg{X: tc.x, Y: originY + 1, Button: tea.MouseLeft})
		d.settle()
		if !m.chromeSel.Active() {
			t.Fatalf("a drag on the %s border should be the chrome's selection", tc.side)
		}
		if p.term.HasSelection() {
			t.Fatalf("the %s border started a selection inside the pane", tc.side)
		}
	}
}

// Right-clicking a showing selection copies it and drops the highlight, the
// way Windows terminals do; with nothing selected the button does nothing.
func TestRightClickCopiesThePaneSelection(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")
	copies := make(chan string, 4)
	m.copyOS = func(text string) error { copies <- text; return nil }

	term.send("hello world")
	d.wait("output", func() bool { return strings.Contains(frameText(m), "hello world") })

	originX, originY := screenOrigin(m, m.focusedPane())
	d.dispatch(tea.MouseClickMsg{X: originX + 2, Y: originY, Button: tea.MouseRight})
	d.settle()
	if len(copies) != 0 {
		t.Fatal("a right click with nothing selected copied")
	}

	d.dispatch(tea.MouseClickMsg{X: originX, Y: originY, Button: tea.MouseLeft})
	d.dispatch(tea.MouseMotionMsg{X: originX + 4, Y: originY, Button: tea.MouseLeft})
	d.dispatch(tea.MouseReleaseMsg{X: originX + 4, Y: originY, Button: tea.MouseLeft})
	d.wait("the drag's own copy", func() bool { return len(copies) > 0 })
	<-copies

	d.dispatch(tea.MouseClickMsg{X: originX + 2, Y: originY, Button: tea.MouseRight})
	d.wait("the right click's copy", func() bool { return len(copies) > 0 })
	if got := <-copies; got != "hello" {
		t.Fatalf("copied %q, want %q", got, "hello")
	}
	if m.focusedPane().term.HasSelection() {
		t.Fatal("copying should clear the selection")
	}
}

// Clicking a pane focuses it: with a mouse in hand, pointing at the thing is
// how you say which one you mean.
func TestClickFocusesThePaneUnderIt(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key("s")
	d.wait("the first tab", func() bool { return m.shells.len() == 1 })
	d.wait("the tab to have focus", func() bool { return m.onShells })

	click := func(x, y int) {
		d.dispatch(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
		d.dispatch(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	}
	x, y := screenOrigin(m, m.primary())
	click(x, y)
	if m.onShells {
		t.Fatal("clicking the terminal should focus it")
	}
	x, y = screenOrigin(m, m.shells.panes[0])
	click(x, y)
	if !m.onShells {
		t.Fatal("clicking the tab should focus it back")
	}
}

// The primary terminal's title goes to the terminal's own title bar as well as
// the header: the header says what is in the window, and the title bar is how
// you find the window among the others you have open.
func TestTheTerminalTitleFollowsThePrimaryPane(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openNarrowWorkspace(t, ds, "enter")

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

// A name the discobox was given outranks its harness's title on the primary's
// tab and in the window title, and reaches both from the listing: one written
// into the box's meta shows within a poll, without the program retitling itself.
func TestAGivenNameFollowsTheListingOntoThePrimary(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")
	term.send("\x1b]2;go test ./...\x07")
	d.wait("the title", func() bool { return m.View().WindowTitle == "go test ./..." })

	// What the server reports once the box's meta file names it.
	if err := ds.Rename(t.Context(), m.paneBox.ID, "Reaper race"); err != nil {
		t.Fatal(err)
	}
	d.dispatch(tickMsg{})
	d.wait("the given name", func() bool { return m.View().WindowTitle == "Reaper race" })
	if frame := frameText(m); !strings.Contains(frame, "Reaper race") || strings.Contains(frame, "go test ./...") {
		t.Fatalf("the primary's edge should wear the given name in place of the title:\n%s", frame)
	}

	// The harness retitling itself does not take the name back.
	term.send("\x1b]2;go vet ./...\x07")
	d.wait("the new title", func() bool { return strings.TrimSpace(m.primary().term.Title()) == "go vet ./..." })
	if got := m.View().WindowTitle; got != "Reaper race" {
		t.Fatalf("title = %q, want the given name", got)
	}
	if frame := frameText(m); !strings.Contains(frame, "Reaper race") || strings.Contains(frame, "go vet ./...") {
		t.Fatalf("the primary's edge should keep the given name over a new title:\n%s", frame)
	}

	// With a second pane the primary's tab in the strip wears it too.
	d.key("ctrl+a")
	d.key("s")
	d.wait("the shell", func() bool { return m.shells.len() == 1 })
	d.wait("the primary's tab", func() bool { return strings.Contains(frameText(m), "0 Reaper race") })

	// Cleared, the harness's title is back.
	if err := ds.Rename(t.Context(), m.paneBox.ID, ""); err != nil {
		t.Fatal(err)
	}
	d.dispatch(tickMsg{})
	d.wait("the title back", func() bool { return m.View().WindowTitle == "go vet ./..." })
}

// What the primary's program reports as its msg (OSC 7501) stands where its
// window title would, since a harness that speaks the protocol stops putting
// its activity in the title, and follows the name the box was given as
// "name: msg". The rename dialog still offers the name alone.
func TestTheProgramsMsgFollowsTheGivenName(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")
	term.send("\x1b]2;Claude Code\x07")
	d.wait("the title", func() bool { return m.View().WindowTitle == "Claude Code" })

	msg := base64.RawStdEncoding.EncodeToString([]byte("Fix the reaper race"))
	term.send("\x1b]7501;state=working:app=claude-code:msg=" + msg + "\x1b\\")
	d.wait("the msg", func() bool { return m.View().WindowTitle == "Fix the reaper race" })

	if err := ds.Rename(t.Context(), m.paneBox.ID, "Reaper"); err != nil {
		t.Fatal(err)
	}
	d.dispatch(tickMsg{})
	d.wait("the given name and the msg", func() bool { return m.View().WindowTitle == "Reaper: Fix the reaper race" })
	if frame := frameText(m); !strings.Contains(frame, "Reaper: Fix the reaper race") {
		t.Fatalf("the primary's edge should wear the name and the msg:\n%s", frame)
	}
	if rec, ok := m.focusedProgramStatus(); !ok || rec.Title != "Reaper" || rec.Msg != "Fix the reaper race" {
		t.Fatalf("record = %+v, %v; want the given name as its title beside the msg", rec, ok)
	}

	// With nothing reported, the name alone.
	term.send("\x1b]7501;state=clear\x1b\\")
	d.wait("the name alone", func() bool { return m.View().WindowTitle == "Reaper" })
}

// A shell tab is something you are doing inside this window, so it never takes
// the window's name: the title bar is read from outside, where what matters is
// which discobox this is and what its agent is doing.
func TestTheTerminalTitleIgnoresTheFocusedTab(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "s")
	d.wait("the tab", func() bool { return m.shells.len() == 1 })
	d.wait("the tab to have focus", func() bool { return m.onShells })

	term.send("\x1b]2;go test ./...\x07")
	d.wait("the title", func() bool { return m.View().WindowTitle == "go test ./..." })

	// The poll can open a second stream before NewShell's answer arrives.
	// The pane keeps the first arrival and closes the other, while execTerm
	// records the latest open. Send to the stream this pane actually kept.
	m.shells.panes[0].stream.(*fakeTerminal).send("\x1b]2;less config.yaml\x07")
	d.wait("the tab's own title", func() bool {
		return strings.TrimSpace(m.shells.panes[0].term.Title()) == "less config.yaml"
	})
	if got, want := m.View().WindowTitle, "go test ./..."; got != want {
		t.Fatalf("title = %q, want the primary's %q", got, want)
	}
}

// The leader plus s always opens a fresh shell: the tabs are the server's
// sessions, and asking for another shell is asking for another session.
func TestLeaderSOpensAnotherShell(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key("s")
	d.wait("the first tab", func() bool { return m.shells.len() == 1 })
	d.key("ctrl+a")
	d.key("s")
	d.wait("the second tab", func() bool { return m.shells.len() == 2 })

	if !m.onShells || m.shells.visible().execID != "exec_shell2" {
		t.Fatal("the newest shell should have focus")
	}
	// Both are the same discobox.
	if m.shells.panes[0].sandbox.ID != m.shells.panes[1].sandbox.ID {
		t.Fatal("a shell belongs to the discobox it was opened in")
	}
}

// The leader plus a goes back to the terminal — there is always exactly one,
// so the key is a place, not an opener.
func TestLeaderAFocusesTheTerminal(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return m.shells.len() == 1 })
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
	t.Parallel()
	now := time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)
	ds := newFakeSource(testSandboxes()...)
	ds.execs = []Exec{
		{ID: "exec_a", Command: []string{"/bin/bash"}, Tty: true, Live: true, CreatedAt: now},
		{ID: "exec_b", Command: []string{"/bin/zsh"}, Tty: true, Live: true, CreatedAt: now.Add(time.Minute)},
	}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the tabs", func() bool { return m.shells.len() == 2 })

	if m.onShells {
		t.Fatal("focus starts on the terminal")
	}
	d.key("ctrl+a")
	d.key("l")
	if !m.onShells || m.shells.active != 0 {
		t.Fatalf("onShells=%v tab=%d, want the first tab", m.onShells, m.shells.active)
	}
	d.key("ctrl+a")
	d.key("l")
	if m.shells.active != 1 {
		t.Fatalf("tab = %d, want the second", m.shells.active)
	}
	d.key("ctrl+a")
	d.key("l")
	if m.shells.active != 1 {
		t.Fatal("the right edge should stop rather than wrap")
	}
	d.key("ctrl+a")
	d.key("h")
	if m.shells.active != 0 {
		t.Fatalf("tab = %d, want the first again", m.shells.active)
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
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, harness := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return m.shells.len() == 1 })
	shell := ds.execTerm("exec_shell1")

	// The shell has focus, and ctrl+c is its own.
	d.key("ctrl+c")
	if got := shell.typed("\x03"); !strings.Contains(got, "\x03") {
		t.Fatalf("shell typed %q, want the interrupt", got)
	}
	if !m.inPanes() || m.shells.len() != 1 {
		t.Fatal("ctrl+c in a shell should not detach anything")
	}
	// Beside the tree the way out is quitting, offered in the header.
	if !strings.Contains(hintLine(m.headerHints()), m.leader()+" q quit") {
		t.Fatalf("the keys should say the way out: %q", hintLine(m.headerHints()))
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
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return m.shells.len() == 1 })
	ds.execTerm("exec_shell1").send("in the shell")
	d.wait("output", func() bool { return strings.Contains(frameText(m), "in the shell") })

	for i, line := range windowLines(m) {
		if lipgloss.Width(line) != m.width {
			t.Fatalf("line %d is %d cells, want %d: %q", i, lipgloss.Width(line), m.width, ansi.Strip(line))
		}
	}
	// Each side names what it is running, and wears the number it answers to.
	frame := ansi.Strip(rawFrame(m))
	if !strings.Contains(frame, "[ 0 attach ]") || !strings.Contains(frame, "[ 1 zsh ]") {
		t.Fatalf("each side should name what it is running:\n%s", frame)
	}
}

// The primary session that ends is gone: the workspace was a view onto it, so
// it is not held.
func TestAnEndedSessionIsNotHeld(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	// The id leads the banner's middle; the git columns follow it.
	const middle = "sbx_one  main@a3f9c21*  dirty  +142 −38"

	nameRow := func() string { return ansi.Strip(windowLines(m)[0]) }
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
	m.primary().status = "reconnecting…"
	if after := strings.Index(nameRow(), "sbx_one"); before != after {
		t.Fatalf("the id moved when a status appeared: %d then %d", before, after)
	}
	if !strings.Contains(nameRow(), "reconnecting") {
		t.Fatalf("the status should still be shown: %q", nameRow())
	}

	// And one name over the whole workspace, not one per pane.
	m.primary().status = ""
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return m.shells.len() == 1 })
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
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	nameRow := func() string { return ansi.Strip(windowLines(m)[0]) }
	if !strings.Contains(nameRow(), "main@a3f9c21*  dirty") {
		t.Fatalf("the banner does not show the sandbox as dirty: %q", nameRow())
	}

	// The agent reports a clean tree with committed work beyond spawn.
	moved := testSandboxes()
	moved[0].Git = GitState{
		Known: true, Branch: "main", Commit: "b17e004",
	}
	moved[0].Diff = DiffStat{Known: true, Added: 12, Deleted: 3, Files: 2}
	ds.mu.Lock()
	ds.sandboxes = moved
	ds.mu.Unlock()
	d.dispatch(tickMsg{})
	d.wait("the refresh", func() bool { return strings.Contains(nameRow(), "ready") })

	if got := nameRow(); !strings.Contains(got, "main@b17e004⇡  ready  +12 −3") {
		t.Fatalf("the banner did not follow the listing: %q", got)
	}
}

// What the sandbox is serving rides the same banner, as protocol/number per
// port: the whole point of knowing a port is listening is knowing whether it is
// a page you could open, and the address it is bound on decides nothing a
// forward from inside the sandbox cares about.
func TestTheBannerCarriesTheListeningPorts(t *testing.T) {
	t.Parallel()
	serving := testSandboxes()
	serving[0].Ports = []Port{
		{Number: 22, Protocol: "tcp"},
		{Number: 5173, Protocol: "http"},
		{Number: 8443, Protocol: "https"},
	}
	ds := newFakeSource(serving...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.dispatch(sizeMsg(180, 40))

	nameRow := func() string { return ansi.Strip(windowLines(m)[0]) }
	if got := nameRow(); !strings.Contains(got, "http:5173 · https:8443 · tcp:22") {
		t.Fatalf("the banner does not carry the listening ports: %q", got)
	}
}

// A sandbox serving nothing says nothing: an empty list is not a field worth
// the width, and it reads the same as an agent that has not reported yet.
func TestTheBannerOmitsPortsWhenNothingIsListening(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	_, m, _ := openWorkspace(t, ds, "enter")

	row := ansi.Strip(windowLines(m)[0])
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
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.dispatch(sizeMsg(180, 40))

	nameRow := func() string { return ansi.Strip(windowLines(m)[0]) }
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
	t.Parallel()
	serving := testSandboxes()
	serving[0].Ports = []Port{{Number: 5173, Protocol: "http"}, {Number: 8443, Protocol: "https"}}
	ds := newFakeSource(serving...)
	d, m, _ := openWorkspace(t, ds, "enter")

	const middle = "sbx_one  main@a3f9c21*  dirty  +142 −38  http:5173 · https:8443"
	nameRow := func() string { return ansi.Strip(windowLines(m)[0]) }

	for _, tc := range []struct {
		width  int
		hidden bool
		what   string
		keys   bool
		brand  bool
		folder bool
	}{
		// Beside the tree the program's name is the tree's, and the row's left
		// edge is where the box was cut from: the keys go, then that.
		{width: 140, what: "beside the tree, room for everything", keys: true, folder: true},
		{width: 120, what: "beside the tree, the keys go first", folder: true},
		{width: 118, what: "beside the tree, then the source"},
		// With the tree put away the row is the whole window's again, and its
		// left edge is the brand and the list's filter.
		{width: 140, hidden: true, what: "room for everything", keys: true, brand: true, folder: true},
		{width: 110, hidden: true, what: "the keys go first", brand: true, folder: true},
		{width: 92, hidden: true, what: "then the program's own name", folder: true},
		{width: 80, hidden: true, what: "then the folder"},
	} {
		m.treeHidden = tc.hidden
		d.dispatch(sizeMsg(tc.width, 40))
		row := nameRow()
		// Whatever went, the middle is still whole: that is the point of the
		// order — the edges pay for it.
		if !strings.Contains(row, middle) {
			t.Fatalf("%s: the middle is not whole at %d columns: %q", tc.what, tc.width, row)
		}
		if got := strings.Contains(row, "quit"); got != tc.keys {
			t.Fatalf("%s: keys shown = %v at %d columns: %q", tc.what, got, tc.width, row)
		}
		// The folder path contains "disco2", so the brand is looked for where
		// only the brand can be: at the head of the row.
		if got := strings.HasPrefix(strings.TrimSpace(row), "discobox"); got != tc.brand {
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
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	m.primary().status = "reconnecting…"

	nameRow := func() string { return ansi.Strip(windowLines(m)[0]) }
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
// from the right — the ports first, then the diffstat, which the session output
// gives you anyway, then the word, whose mark the position carries regardless —
// so a narrow window loses a field rather than showing half of one. The id
// never goes: it is what identifies the window.
func TestTheBannersMiddleDropsFieldsWholeAsItNarrows(t *testing.T) {
	t.Parallel()
	serving := testSandboxes()
	serving[0].Ports = []Port{{Number: 5173, Protocol: "http"}, {Number: 8443, Protocol: "https"}}
	ds := newFakeSource(serving...)
	d, m, _ := openWorkspace(t, ds, "enter")

	nameRow := func() string { return ansi.Strip(windowLines(m)[0]) }
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

// busyWorkspace opens a workspace on a discobox serving two ports and measured
// on both schedules, on a machine whose readout has arrived.
func busyWorkspace(t *testing.T) (*driver, *Model, *fakeSource) {
	t.Helper()
	busy := testSandboxes()
	busy[0].Ports = []Port{{Number: 5173, Protocol: "http"}, {Number: 8443, Protocol: "https"}}
	busy[0].Usage = Usage{
		Known: true, CPUPercent: 12, MemoryBytes: 1_288_490_189, MemoryPercent: 4,
		DiskKnown: true, DiskBytes: 3_650_722_202, DiskPercent: 10,
	}
	ds := newFakeSource(busy...)
	ds.setResources(Resources{
		Known:    true,
		CPUVCPUs: 4.2, CPUCapacity: 24,
		MemoryBytes: 9_663_676_416, MemoryCapacity: 34_359_738_368,
		DiskKnown: true, DiskFreeBytes: 34_896_609_280,
	})
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the machine readout", func() bool { return m.resources.Known })
	return d, m, ds
}

// A banner with room to spare carries what this discobox is using and what the
// machine has of the same resource beside it, grouped by resource and written
// short, and follows both on the tick the way it follows the git state.
func TestTheBannerCarriesWhatTheDiscoboxAndMachineAreUsing(t *testing.T) {
	t.Parallel()
	d, m, ds := busyWorkspace(t)
	d.dispatch(sizeMsg(270, 40))

	nameRow := func() string { return ansi.Strip(windowLines(m)[0]) }
	const want = "cpu 12% (4.2/24)  mem 1.2G (9.0/32G)  disk 3.4G (32G free)"
	if got := nameRow(); !strings.Contains(got, want) {
		t.Fatalf("the banner does not carry the usage: %q", got)
	}

	busier := testSandboxes()
	busier[0].Ports = []Port{{Number: 5173, Protocol: "http"}, {Number: 8443, Protocol: "https"}}
	busier[0].Usage = Usage{Known: true, CPUPercent: 87, MemoryBytes: 1_288_490_189, DiskKnown: true, DiskBytes: 3_650_722_202}
	ds.mu.Lock()
	ds.sandboxes = busier
	ds.mu.Unlock()
	d.dispatch(tickMsg{})
	d.wait("the refresh", func() bool { return strings.Contains(nameRow(), "cpu 87%") })
}

// The readouts are ambient — nothing on this screen is done with them — so they
// ride only in room the rest of the row leaves over and are the first thing
// given up: the machine's halves, all of them at once, then this discobox's
// groups from the right, all before a single edge goes. At the width the rest
// of the row just fits, the row is exactly what it was without them.
func TestTheBannerGivesUpUsageBeforeAnyEdge(t *testing.T) {
	t.Parallel()
	d, m, _ := busyWorkspace(t)

	const middle = "sbx_one  main@a3f9c21*  dirty  +142 −38  http:5173 · https:8443"
	nameRow := func() string { return ansi.Strip(windowLines(m)[0]) }
	for _, tc := range []struct {
		width int
		want  []string
		gone  []string
	}{
		{280, []string{"cpu 12% (4.2/24)  mem 1.2G (9.0/32G)  disk 3.4G (32G free)"}, nil},
		{250, []string{"cpu 12%  mem 1.2G  disk 3.4G"}, []string{"(4.2/24)", "9.0/32G", "free"}},
		{190, []string{"cpu 12%  mem 1.2G"}, []string{"disk 3.4G", "(4.2/24)"}},
		{170, []string{"cpu 12%"}, []string{"mem 1.2G", "disk 3.4G"}},
		{134, nil, []string{"cpu", "mem 1.2G", "disk 3.4G"}},
	} {
		d.dispatch(sizeMsg(tc.width, 40))
		row := nameRow()
		if !strings.Contains(row, middle) {
			t.Fatalf("at %d columns the middle is not whole: %q", tc.width, row)
		}
		// Every edge is still there — beside the tree, the source on the left
		// and quit on the right: the readouts made way for them, never the
		// other way round.
		if !strings.Contains(row, "ctrl+a q quit") || !strings.HasPrefix(strings.TrimSpace(row), "/src/disco2") {
			t.Fatalf("at %d columns the readouts cost the row an edge: %q", tc.width, row)
		}
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

// Nothing measured says nothing, however much room there is: a zero would read
// as idle, and the dot the list holds a cell with has no cell to hold here.
func TestTheBannerOmitsUsageNothingHasMeasured(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.dispatch(sizeMsg(270, 40))

	row := ansi.Strip(windowLines(m)[0])
	for _, word := range []string{"cpu", "mem", "disk"} {
		if strings.Contains(row, word) {
			t.Fatalf("the banner carries %q with nothing measured: %q", word, row)
		}
	}
}

// The readouts change on the refresh — a cpu figure gains a digit, the
// machine's halves come and go in a block — and the middle is where the
// pressable things are: the git summary opens the diff and every forwarded port
// is a link. So the middle holds its place through all of it. A banner that
// re-centered itself every few seconds would walk its controls out from under
// the pointer between the look and the press.
func TestTheBannerHoldsItsMiddleAsTheReadoutsChange(t *testing.T) {
	t.Parallel()
	d, m, ds := busyWorkspace(t)
	d.dispatch(sizeMsg(270, 40))

	nameRow := func() string { return ansi.Strip(windowLines(m)[0]) }
	at := func() int { return strings.Index(nameRow(), "sbx_one") }
	before := at()
	if !strings.Contains(nameRow(), "(4.2/24)") {
		t.Fatalf("the machine's halves should be drawn to begin with: %q", nameRow())
	}

	// The machine stops reporting: the widest thing on the row goes.
	ds.setResources(Resources{})
	d.dispatch(tickMsg{})
	d.wait("the machine to go", func() bool { return !strings.Contains(nameRow(), "(4.2/24)") })
	if got := at(); got != before {
		t.Fatalf("the middle moved when the machine's halves went: %d then %d", before, got)
	}

	// And a figure that gains a digit does not move it either.
	busier := testSandboxes()
	busier[0].Ports = []Port{{Number: 5173, Protocol: "http"}, {Number: 8443, Protocol: "https"}}
	busier[0].Usage = Usage{Known: true, CPUPercent: 100, MemoryBytes: 1_288_490_189, DiskKnown: true, DiskBytes: 3_650_722_202}
	ds.mu.Lock()
	ds.sandboxes = busier
	ds.mu.Unlock()
	d.dispatch(tickMsg{})
	d.wait("the refresh", func() bool { return strings.Contains(nameRow(), "cpu 100%") })
	if got := at(); got != before {
		t.Fatalf("the middle moved when a figure grew: %d then %d", before, got)
	}
}

// A row narrow enough to give up its keys has no room to spare, whatever is now
// standing empty where they were: the readouts rank below every edge, so they
// are budgeted against the keys' own edge whether or not this row still draws
// it. Handing them the room an edge just vacated would rank them above it.
func TestTheBannerSpendsNoRoomAnEdgeGaveUp(t *testing.T) {
	t.Parallel()
	d, m, _ := busyWorkspace(t)
	// Narrow enough that the middle needs the keys' room and the keys go:
	// beside the tree, the window is this less the tree.
	d.dispatch(sizeMsg(130, 40))

	row := ansi.Strip(windowLines(m)[0])
	if strings.Contains(row, "quit") {
		t.Fatalf("this width is meant to be past the keys: %q", row)
	}
	if !strings.HasPrefix(strings.TrimSpace(row), "/src/disco2") {
		t.Fatalf("the source should still be there, or the width is wrong: %q", row)
	}
	if !strings.Contains(row, "sbx_one  main@a3f9c21*  dirty  +142 −38  http:5173 · https:8443") {
		t.Fatalf("the middle should be whole, which is what the keys paid for: %q", row)
	}
	for _, word := range []string{"cpu", "mem 1.2G", "disk 3.4G"} {
		if strings.Contains(row, word) {
			t.Fatalf("the banner spent the keys' room on %q: %q", word, row)
		}
	}
}

// The same holds for the edge on the other side. A wide brand and a key line
// with one hint in it — a one-shot window's — put the row in the band where the
// brand has been given up and the keys' room is what the budget still reserves:
// measured against the brand this row is no longer drawing, there is nothing
// spare, and measured against the folder that replaced it there would be just
// enough for a cpu figure. The edges the budget answers to are the row's at
// their widest, not the ones this concession kept.
func TestTheBannerSpendsNoRoomTheBrandGaveUp(t *testing.T) {
	t.Parallel()
	busy := testSandboxes()
	busy[0].Ports = []Port{{Number: 5173, Protocol: "http"}, {Number: 8443, Protocol: "https"}}
	busy[0].Usage = Usage{
		Known: true, CPUPercent: 12, MemoryBytes: 1_288_490_189, MemoryPercent: 4,
		DiskKnown: true, DiskBytes: 3_650_722_202, DiskPercent: 10,
	}
	ds := newFakeSource(busy...)
	// A project in the brand is what makes the left edge wide enough to matter.
	ds.session.Project = "backend-platform"
	d, m, _ := openWorkspace(t, ds, "enter")
	// The brand and the filter are the row's left edge only with the tree put
	// away: beside it the brand is the tree's. Put away, the key line is the
	// one hint — quit, with no list beside the box to detach to.
	m.treeHidden = true
	d.dispatch(sizeMsg(116, 40))
	if hints := hintLine(m.headerHints()); hints != "ctrl+a q quit" {
		t.Fatalf("header keys = %q, want the one hint", hints)
	}

	row := ansi.Strip(windowLines(m)[0])
	if strings.Contains(row, "discobox") {
		t.Fatalf("this width is meant to be past the brand: %q", row)
	}
	if !strings.Contains(row, "/src/disco2") {
		t.Fatalf("the folder should still be there, or the width is wrong: %q", row)
	}
	for _, word := range []string{"cpu", "mem 1.2G", "disk 3.4G"} {
		if strings.Contains(row, word) {
			t.Fatalf("the banner spent the brand's room on %q: %q", word, row)
		}
	}
}

// The machine's halves go together or not at all. Its free space comes from a
// statfs that answers before it has two cpu samples to difference, so the first
// report after the agent starts knows the disk and neither of the others — and
// a row framing the disk against the machine while the cpu beside it stands
// unframed makes half the comparison the parentheses are there to make.
func TestTheBannerDrawsTheMachineOnlyWhenAllOfItIsKnown(t *testing.T) {
	t.Parallel()
	d, m, ds := busyWorkspace(t)
	ds.setResources(Resources{DiskKnown: true, DiskFreeBytes: 34_896_609_280})
	d.dispatch(tickMsg{})
	d.dispatch(sizeMsg(270, 40))

	nameRow := func() string { return ansi.Strip(windowLines(m)[0]) }
	d.wait("the machine to go", func() bool { return !strings.Contains(nameRow(), "(4.2/24)") })
	row := nameRow()
	if !strings.Contains(row, "cpu 12%  mem 1.2G  disk 3.4G") {
		t.Fatalf("the discobox's own figures should be unaffected: %q", row)
	}
	if strings.Contains(row, "free") || strings.Contains(row, "(") {
		t.Fatalf("the banner framed one figure against a machine it only half knows: %q", row)
	}
}

// Streaming output continues below a viewport in scrollback without pulling
// the real window back to the live screen.
func TestStreamingOutputDoesNotMoveScrolledViewport(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")
	p := m.primary()

	_, rows := m.paneCells(m.paneWidthOf(p))
	var out strings.Builder
	for i := 1; i <= rows+10; i++ {
		if i > 1 {
			out.WriteString("\r\n")
		}
		fmt.Fprintf(&out, "stream line %d", i)
	}
	term.send(out.String())
	d.wait("stream output", func() bool { return p.term.ScrollbackLen() >= 10 })

	ox, oy := screenOrigin(m, p)
	d.dispatch(tea.MouseWheelMsg{X: ox + 1, Y: oy + 1, Button: tea.MouseWheelUp})
	if p.term.ScrollOffset() == 0 {
		t.Fatal("mouse wheel did not move the live pane into scrollback")
	}
	wantFrame := plainFrame(m)
	wantSeq := p.term.OutputSeq() + 1

	term.send("\r\nstream still running")
	d.wait("continued stream output", func() bool { return p.term.OutputSeq() >= wantSeq })
	if got := plainFrame(m); got != wantFrame {
		t.Fatalf("streaming output moved the scrolled TUI viewport:\n%s\nwant:\n%s", got, wantFrame)
	}
}

// Moving between panes takes the arrows as well as the letters, and holding
// Ctrl keeps the sequence open: the leader, then Ctrl-← Ctrl-→ walks across
// without pressing the leader again.
func TestMovingBetweenPanesRepeatsWhileCtrlIsHeld(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return m.shells.len() == 1 })

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
	t.Parallel()
	now := time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)
	ds := newFakeSource(testSandboxes()...)
	ds.execs = []Exec{
		{ID: "exec_a", Command: []string{"/bin/bash"}, Tty: true, Live: true, CreatedAt: now},
		{ID: "exec_b", Command: []string{"/bin/zsh"}, Tty: true, Live: true, CreatedAt: now.Add(time.Minute)},
	}
	d, m, term := openWorkspace(t, ds, "enter")
	d.wait("the tabs", func() bool { return m.shells.len() == 2 })

	d.key("ctrl+a")
	d.key("2")
	if !m.onShells || m.shells.active != 1 {
		t.Fatalf("onShells=%v tab=%d, want the second tab", m.onShells, m.shells.active)
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
	if !strings.Contains(m.status, "no pane 7") {
		t.Fatalf("status = %q, want it to say the pane is not there", m.status)
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
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, harness := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return m.shells.len() == 1 })

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

// Every command the list offers is here on the key it has there. Archiving is
// the exception that also detaches: the workspace belongs to the box being put
// away, so its result is reported on the list behind it.
func TestLeaderRunsTheListsVerbs(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key("x") // archive
	d.wait("the verb", func() bool { return len(ds.verbs()) > 0 })

	if ds.did[0] != "archive sbx_one" {
		t.Fatalf("did = %v, want the box on screen archived", ds.did)
	}
	if m.focus != focusList || m.inPanes() {
		t.Fatalf("focus = %v inPanes = %v, want the list", m.focus, m.inPanes())
	}
	select {
	case <-term.closed:
	default:
		t.Fatal("the workspace terminal is still attached after archive")
	}
}

// The leader's way out works from every pane, whether or not the pane has one
// of its own, on a console with no tree beside the workspace, where there is a
// list to detach to. It costs the application nothing, being behind the leader, and
// one key that always works beats remembering which pane took Ctrl-C.
func TestTheLeaderDetachesFromAnyPane(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openNarrowWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key("d")
	d.wait("the workspace to close", func() bool { return !m.inPanes() })
	// What detaching says is its own message, which may land after the
	// workspace has closed.
	d.wait("the detach to be reported", func() bool { return m.status != "" })
	if m.focus != focusList {
		t.Fatalf("focus = %v, want the list", m.focus)
	}
}

// ctrl+a v opens the workspace's own discobox in VS Code. Nothing on screen
// moves: the editor is another window, and the terminals stay attached — which
// is the whole point of binding it here rather than only in the list.
func TestTheToolsPickerOpensTheWorkspaceBoxInVSCode(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key("o")
	waitPicker(d, m)
	d.key("v")
	d.wait("the editor", func() bool { return len(ds.openedEditors()) == 1 })

	if got := ds.openedEditors(); got[0] != (editorOpen{id: "sbx_one", tool: "vscode"}) {
		t.Fatalf("editors = %v, want the box on screen in VS Code", got)
	}
	if !m.inPanes() {
		t.Fatal("opening an editor should leave the workspace up")
	}
	// The leader consumed the key; the sandbox never saw it.
	if got := term.typed("o"); strings.Contains(got, "o") {
		t.Fatalf("typed %q, want the leader to take the key", got)
	}
}

// A host tool that cannot run says why on the status line, not in a dialog:
// nothing about the box changed, and there is nothing to answer.
func TestTheToolsPickerReportsAHostToolFailureOnTheStatusLine(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	// The shape tools.ProgramError actually returns, so the fake cannot go on
	// asserting a sentence the code stopped producing.
	ds.hostToolErr = errors.New("looked for code, code-insiders, codium, cursor, windsurf on PATH and found none; install vscode, or name the program with --program or $DISCOBOX_VSCODE")
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key("o")
	waitPicker(d, m)
	d.key("v")
	d.wait("the report", func() bool { return strings.Contains(m.status, "found none; install vscode") })
	if !m.statusE {
		t.Fatalf("status = %q, want it reported as an error", m.status)
	}
}

// ctrl+a o z is the same row for the other editor: the picker is where the
// workspace reaches both, since neither is a key of its own there.
func TestTheToolsPickerOpensTheWorkspaceBoxInZed(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key("o")
	waitPicker(d, m)
	d.key("z")
	d.wait("the editor", func() bool { return len(ds.openedEditors()) == 1 })

	if got := ds.openedEditors(); got[0] != (editorOpen{id: "sbx_one", tool: "zed"}) {
		t.Fatalf("editors = %v, want the box on screen in Zed", got)
	}
	if !m.inPanes() {
		t.Fatal("opening an editor should leave the workspace up")
	}
}

// While a command is starting the workspace says so, in place of the keys: the
// screen it takes is not up yet, and a key that appears to have done nothing
// for as long as the server takes is the same key twice.
func TestTheWorkspaceShowsWhatItIsWaitingFor(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	m.busy = "configure…"
	d.dispatch(sizeMsg(m.width, m.height))
	if !strings.Contains(plainFrame(m), "configure…") {
		t.Fatalf("the workspace does not say what it is doing:\n%s", plainFrame(m))
	}
}

// Ctrl-L repaints, and is not consumed doing it. The window redraws itself,
// which is what clears clutter written over it from outside; the sandbox is
// asked for the screen again, because what the pane is showing is drawn from
// the emulator's grid and only the far end can put right what is in it; and the
// key goes on to the program as well.
//
// The size goes with the ask. Another client attached to the same terminal
// leaves it whatever size that client asked for, and this pane cannot tell that
// happened — so the one moment it says its size again is the moment somebody
// says the screen looks wrong. See termpane.Model.Repaint.
func TestRepaintRedrawsTheWindowAndReachesTheBox(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	_, m, term := openWorkspace(t, ds, "enter")
	cols, rows := m.focusedPane().term.Size()

	_, cmd := m.Update(keyPress("ctrl+l"))
	if !repaints(t, cmd) {
		t.Fatal("ctrl+l in a pane should redraw the window")
	}
	asked, size := term.asked()
	if asked != 1 {
		t.Fatalf("asked the box for %d repaints, want 1", asked)
	}
	if size != [2]int{cols, rows} {
		t.Fatalf("re-asserted %v, want the pane's own size %v", size, [2]int{cols, rows})
	}
	if got := term.typed("\x0c"); !strings.Contains(got, "\x0c") {
		t.Fatalf("typed %q, want ctrl+l to reach the box too", got)
	}
}

// The leader plus l moves the pane, and holding Ctrl through the pair is how
// everyone types it. That is a chord, not a repaint: asking the sandbox for its
// screen on the way past would fire on every step of a run of them.
func TestRepaintDoesNotFireOnTheLeaderChord(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, _, term := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key("ctrl+l")

	if asked, _ := term.asked(); asked != 0 {
		t.Fatalf("asked the box for %d repaints, want none for a chord", asked)
	}
}

// whereDrawn is where text is on the frame, in screen cells. It reads the
// frame rather than asking paneOrigin, so a test pressing there checks the
// drawing and the hit test against each other rather than the hit test
// against itself — which is how an off-by-one gets past every test.
func whereDrawn(t *testing.T, m *Model, text string) (x, y int) {
	t.Helper()
	for row, line := range strings.Split(plainFrame(m), "\n") {
		if i := strings.Index(line, text); i >= 0 {
			return len([]rune(line[:i])), row
		}
	}
	t.Fatalf("%q is not on screen:\n%s", text, plainFrame(m))
	return 0, 0
}

// A drag selects exactly what is drawn under the pointer, on the grid's first
// row and on its last, with every row under the boxes in play: the message row
// with an error standing on it, the keys, and the credential band. Rows below
// the boxes cost them height and must not move where they start; each past
// off-by-one here was one place counting a row another did not.
func TestDragSelectsWhatIsDrawnUnderThePointer(t *testing.T) {
	t.Parallel()
	for _, band := range []bool{false, true} {
		t.Run(fmt.Sprintf("band=%v", band), func(t *testing.T) {
			t.Parallel()
			ds := newFakeSource(testSandboxes()...)
			d, m, term := openWorkspace(t, ds, "enter")
			if band {
				ds.mu.Lock()
				ds.requests = []CredentialRequest{waitingRequest()}
				ds.mu.Unlock()
				d.dispatch(tickMsg{})
				d.wait("the band", func() bool { return m.bannerTop() == 1 })
			}
			d.dispatch(statusMsg{text: "cannot create the discobox: no space left on device", err: true})

			cols, rows := m.paneCells(m.paneWidthOf(m.primary()))
			d.wait("the pane at its size", func() bool { return term.size() == [2]int{cols, rows} })
			term.send("first" + strings.Repeat("\r\n", rows-1) + "final")
			d.wait("output", func() bool {
				s := plainFrame(m)
				return strings.Contains(s, "first") && strings.Contains(s, "final")
			})

			lines := strings.Split(plainFrame(m), "\n")
			if len(lines) != m.height {
				t.Fatalf("frame = %d rows, want the window's %d", len(lines), m.height)
			}
			// The foot, from the bottom up: the keys, the lower band when there
			// is one, and then the box's bottom edge, carrying the message,
			// directly under the grid's last row.
			if !strings.Contains(lines[len(lines)-1], m.leader()+" "+boxMenuKey+" actions") {
				t.Fatalf("last row = %q, want the keys", lines[len(lines)-1])
			}
			_, firstY := whereDrawn(t, m, "first")
			_, finalY := whereDrawn(t, m, "final")
			if finalY-firstY != rows-1 {
				t.Fatalf("grid drawn over rows %d..%d, want %d rows", firstY, finalY, rows)
			}
			edge := len(lines) - 2 - m.bannerTop()
			if finalY+1 != edge || !strings.Contains(lines[edge], "╰") {
				t.Fatalf("the grid's last row is %d and the bottom edge is %q on %d, want the edge right under it",
					finalY, lines[finalY+1], finalY+1)
			}
			if !strings.Contains(lines[edge], "no space left on device") {
				t.Fatalf("the bottom edge = %q, want the message on it", lines[edge])
			}
			// The keys row is not the pane's to take a press on.
			if p := m.paneBoxAt(5, len(lines)-1); p != nil {
				t.Fatalf("a press on row %d landed in a pane", len(lines)-1)
			}

			for _, word := range []string{"first", "final"} {
				x, y := whereDrawn(t, m, word)
				end := x + len(word) - 1
				d.dispatch(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
				d.dispatch(tea.MouseMotionMsg{X: end, Y: y, Button: tea.MouseLeft})
				d.dispatch(tea.MouseReleaseMsg{X: end, Y: y, Button: tea.MouseLeft})
				d.wait("the selection of "+word, func() bool { return m.focusedPane().term.SelectionText() == word })
			}
		})
	}
}
