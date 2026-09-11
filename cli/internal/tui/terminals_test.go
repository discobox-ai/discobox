package tui

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The leader plus c opens another of the discobox's own terminals, beside the
// primary rather than among the shells — and focused, since it is the thing
// just asked for. It is a tab in the box the primary already has, so the
// screen is not split by it.
func TestLeaderCOpensATerminalBesideThePrimary(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key(paneTerminalKey)
	d.wait("the terminal", func() bool { return m.terminals.len() == 2 })

	if m.onShells || m.shells.len() != 0 {
		t.Fatalf("onShells=%v shells=%d, want a terminal on the left and no tabs",
			m.onShells, m.shells.len())
	}
	if m.terminals.active != 1 || m.terminals.panes[1].execID != "exec_term1" {
		t.Fatalf("active=%d id=%q, want the new terminal focused",
			m.terminals.active, m.terminals.panes[1].execID)
	}
	// It runs the harness the discobox already runs: the sandbox answers which
	// one, so the request names none.
	if m.terminals.panes[1].action != InteractTerminal {
		t.Fatalf("action = %q, want a terminal", m.terminals.panes[1].action)
	}
	// One box still, at the full width, with both terminals opened for it.
	full, rows := m.paneCells(m.width)
	for _, id := range []string{ExecPrimary, "exec_term1"} {
		if got := ds.execTerm(id).size(); got != [2]int{full, rows} {
			t.Fatalf("%s is %v, want the whole window %dx%d", id, got, full, rows)
		}
	}
	// And the strip names them both, numbered from the primary.
	frame := plainFrame(m)
	if !strings.Contains(frame, "0 attach") || !strings.Contains(frame, "1 claude") {
		t.Fatalf("the strip should name both terminals:\n%s", frame)
	}
}

// The primary is pane 0 whatever order the attaches land in: it is opened
// under a virtual id with no creation time, so it sorts to the head of the
// column even when another terminal's attach arrives first.
func TestThePrimaryIsAlwaysTheFirstTerminal(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key(paneTerminalKey)
	d.wait("the terminal", func() bool { return m.terminals.len() == 2 })

	// Start over with the primary's attach held up behind the other one: the
	// window is left, and what comes back is a fresh workspace.
	primary := m.primary()
	m.closeWorkspace()
	gen := m.wsGen
	d.dispatch(workspaceTermMsg{
		gen:  gen,
		exec: Exec{ID: "exec_late", Harness: "claude", Tty: true, Live: true, CreatedAt: time.Date(2026, 8, 7, 14, 0, 0, 0, time.UTC)},
		term: newFakeTerminal(),
	})
	d.dispatch(workspaceTermMsg{gen: gen, exec: Exec{ID: ExecPrimary, Primary: true}, term: newFakeTerminal()})

	if m.terminals.len() != 2 {
		t.Fatalf("terminals = %d, want both", m.terminals.len())
	}
	if got := m.terminals.panes[0]; !got.primary || got == primary {
		t.Fatalf("terminal 0 is %q, want the primary that just arrived", got.execID)
	}
	if m.terminals.panes[1].execID != "exec_late" {
		t.Fatalf("terminal 1 = %q, want the one that arrived first", m.terminals.panes[1].execID)
	}
}

// The digits count across the whole screen rather than per column: the
// terminals from the primary at 0, and the shells carrying on from them.
func TestDigitsCountAcrossBothColumns(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key(paneTerminalKey)
	d.wait("the terminal", func() bool { return m.terminals.len() == 2 })
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return m.shells.len() == 1 })

	for _, tc := range []struct {
		key    string
		shells bool
		at     int
	}{
		{"0", false, 0},
		{"1", false, 1},
		{"2", true, 0},
	} {
		d.key("ctrl+a")
		d.key(tc.key)
		if m.onShells != tc.shells || m.column().active != tc.at {
			t.Fatalf("leader %s: onShells=%v active=%d, want %v/%d",
				tc.key, m.onShells, m.column().active, tc.shells, tc.at)
		}
	}
	// And past the end it says so rather than moving anything.
	d.key("ctrl+a")
	d.key("3")
	if !strings.Contains(m.status, "no pane 3") {
		t.Fatalf("status = %q, want it to say the pane is not there", m.status)
	}
}

// The arrows walk the terminals and the shells as one strip, left to right,
// stopping at its ends.
func TestMovingWalksTheTerminalsThenTheShells(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key(paneTerminalKey)
	d.wait("the terminal", func() bool { return m.terminals.len() == 2 })
	d.key("ctrl+a")
	d.key("s")
	d.wait("the tab", func() bool { return m.shells.len() == 1 })

	m.focusOrdinal(0)
	for _, want := range []int{1, 2, 2} {
		d.key("ctrl+a")
		d.key("l")
		if got := m.paneOrdinal(); got != want {
			t.Fatalf("moving right landed on %d, want %d", got, want)
		}
	}
	for _, want := range []int{1, 0, 0} {
		d.key("ctrl+a")
		d.key("h")
		if got := m.paneOrdinal(); got != want {
			t.Fatalf("moving left landed on %d, want %d", got, want)
		}
	}
}

// A terminal beside the primary is a session like any other tab: it holds its
// last screen when it ends, and dismissing it leaves the workspace up. Only
// the primary's ending ends the workspace.
func TestATerminalThatEndsIsHeldAndDismissed(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key(paneTerminalKey)
	d.wait("the terminal", func() bool { return m.terminals.len() == 2 })

	ds.execTerm("exec_term1").Close()
	d.wait("the held screen", func() bool { return m.terminals.len() == 2 && m.terminals.panes[1].exited })
	if !m.inPanes() {
		t.Fatal("a terminal ending should not take the workspace with it")
	}

	d.key("q")
	d.wait("the terminal dismissed", func() bool { return m.terminals.len() == 1 })
	if !m.inPanes() || m.onShells {
		t.Fatal("the workspace should be back on the primary")
	}
}

// A harness terminal started from anywhere else joins the left column on its
// own while the workspace is up: the screen mirrors the server, and which side
// a session is drawn on is the server's own answer.
func TestATerminalStartedElsewhereJoinsTheLeftColumn(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	ds.addExec(Exec{
		ID: "exec_other", Command: []string{"claude"}, Harness: "claude-code",
		Tty: true, Live: true, CreatedAt: time.Date(2026, 8, 7, 12, 30, 0, 0, time.UTC),
	})
	d.dispatch(workspaceTickMsg{gen: m.wsGen})
	d.wait("the terminal", func() bool { return m.terminals.len() == 2 })

	if m.shells.len() != 0 {
		t.Fatalf("shells = %d, want a harness terminal on the left instead", m.shells.len())
	}
	if m.terminals.panes[1].execID != "exec_other" {
		t.Fatalf("terminal 1 = %q, want the session started elsewhere", m.terminals.panes[1].execID)
	}
	// It arrived on its own, so it does not take the keys off the primary.
	if m.onShells || m.terminals.active != 0 {
		t.Fatalf("onShells=%v active=%d, want the primary still focused", m.onShells, m.terminals.active)
	}
}

// A terminal that cannot be created is a report, not a closed workspace: the
// screen is still a workspace without it.
func TestAFailedTerminalReports(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	ds.newTerminalErr = errors.New("the harness would not start")

	d.key("ctrl+a")
	d.key(paneTerminalKey)
	d.wait("the report", func() bool { return strings.Contains(m.status, "terminal:") })
	if !m.inPanes() || m.terminals.len() != 1 {
		t.Fatalf("terminals = %d, want the workspace up with just the primary", m.terminals.len())
	}
}

// The leader plus X ends the shell in front of you: the session is killed in
// the discobox and its tab goes with it.
func TestLeaderXEndsTheFocusedShell(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the shell", func() bool { return m.shells.len() == 1 })
	execID := m.shells.panes[0].execID

	d.key("ctrl+a")
	d.key(paneEndKey)
	d.wait("the session ended", func() bool { return len(ds.endedExecs()) == 1 })
	if got := ds.endedExecs(); got[0] != execID {
		t.Fatalf("ended = %v, want the focused shell %q", got, execID)
	}
	if m.shells.len() != 0 {
		t.Fatalf("shells = %d, want the tab gone with the session", m.shells.len())
	}
	// The window goes back to the terminals, which take the width back.
	if m.onShells {
		t.Error("focus should leave a column with nothing left in it")
	}
	if got := m.paneWidthOf(m.primary()); got != m.width {
		t.Fatalf("the terminal is %d cells wide, want the whole window (%d)", got, m.width)
	}
}

// And only that one: the other tabs in the column are other sessions, still
// running and still attached.
func TestEndingAShellLeavesTheTabsBesideIt(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	for range 3 {
		d.key("ctrl+a")
		d.key("s")
	}
	d.wait("the shells", func() bool { return m.shells.len() == 3 })
	m.shells.active = 1
	ended := m.shells.panes[1].execID
	kept := []string{m.shells.panes[0].execID, m.shells.panes[2].execID}

	d.key("ctrl+a")
	d.key(paneEndKey)
	d.wait("the session ended", func() bool { return len(ds.endedExecs()) == 1 })
	if got := ds.endedExecs(); got[0] != ended {
		t.Fatalf("ended = %v, want only the visible shell %q", got, ended)
	}
	d.settle()
	if m.shells.len() != 2 {
		t.Fatalf("shells = %d, want the other two still open", m.shells.len())
	}
	for i, id := range kept {
		if got := m.shells.panes[i].execID; got != id {
			t.Fatalf("shell %d is %q, want %q left where it was", i, got, id)
		}
	}
}

// The poll runs behind the kill, so a listing already in flight still reports a
// session this window has just ended. The tab must not come back up.
func TestAnEndedShellIsNotReopenedByThePoll(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the shell", func() bool { return m.shells.len() == 1 })
	execID := m.shells.panes[0].execID
	// The answer a poll started before the kill comes back with.
	stale, _ := ds.Execs(t.Context(), "")

	d.key("ctrl+a")
	d.key(paneEndKey)
	d.wait("the session ended", func() bool { return len(ds.endedExecs()) == 1 })

	d.dispatch(workspaceExecsMsg{gen: m.wsGen, execs: stale})
	d.settle()
	if m.shells.len() != 0 {
		t.Fatalf("shells = %d, want the tab to stay closed", m.shells.len())
	}
	if !m.ending[execID] {
		t.Fatalf("ending = %v, want the killed session remembered", m.ending)
	}

	// It is remembered for as long as the workspace is, rather than until some
	// answer says the session is gone: the answers overlap, so one of them
	// saying so is no proof a later one will. Exec ids are not reused, and
	// leaving the workspace drops the map whole.
	current, _ := ds.Execs(t.Context(), "")
	d.dispatch(workspaceExecsMsg{gen: m.wsGen, execs: current})
	d.dispatch(workspaceExecsMsg{gen: m.wsGen, execs: stale})
	d.settle()
	if m.shells.len() != 0 {
		t.Fatalf("shells = %d, want an answer older than the kill still ignored", m.shells.len())
	}
	m.closeWorkspace()
	if m.ending != nil {
		t.Fatalf("ending = %v, want it dropped with the workspace", m.ending)
	}
}

// A kill the server refuses gives the tab back: the session is still running,
// and a workspace that hid it would be one there is no way back to it from.
func TestAShellTheServerWillNotEndComesBack(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the shell", func() bool { return m.shells.len() == 1 })
	execID := m.shells.panes[0].execID
	ds.endExecErr = errors.New("still going")

	d.key("ctrl+a")
	d.key(paneEndKey)
	d.wait("the refusal", func() bool { return strings.Contains(m.status, "could not end") })
	if m.ending[execID] {
		t.Fatalf("ending = %v, want a refused kill forgotten", m.ending)
	}

	listing, _ := ds.Execs(t.Context(), "")
	d.dispatch(workspaceExecsMsg{gen: m.wsGen, execs: listing})
	d.wait("the tab back", func() bool { return m.shells.len() == 1 })
	if got := m.shells.panes[0].execID; got != execID {
		t.Fatalf("shell = %q, want the session that is still running (%q)", got, execID)
	}
}

// The primary is the workspace and a service is the discobox's own: neither is
// ended this way, and neither wears the button that would say it is.
func TestThePrimaryAndServicesHaveNoEndButton(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.services = []Service{runningService("discobox-api", "Discobox API", "exec_svc1")}
	ds.execs = []Exec{serviceExecRecord("exec_svc1", "discobox-api", "Discobox API")}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the service tab", func() bool { return m.terminals.len() == 2 })

	// The primary, which the workspace is a view onto.
	d.key("ctrl+a")
	d.key(paneEndKey)
	d.wait("the report", func() bool { return strings.Contains(m.status, "primary terminal") })
	if _, ok := m.endablePane(m.primary()); ok {
		t.Error("the primary should wear no [x]")
	}

	// And the service, which the discobox starts and stops on its own.
	service := m.terminals.panes[0]
	if service.service == "" {
		t.Fatalf("pane 0 is %q, want the service tab", service.name())
	}
	m.focusPane(service)
	d.key("ctrl+a")
	d.key(paneEndKey)
	d.wait("the report", func() bool { return strings.Contains(m.status, "stopped rather than ended") })
	if _, ok := m.endablePane(service); ok {
		t.Error("a service should wear no [x]")
	}

	// Neither press reached the server.
	if got := ds.endedExecs(); len(got) != 0 {
		t.Fatalf("ended = %v, want both left alone", got)
	}
	// Nor does the box drawing one offer the button: the service is what the
	// left column is showing, so its border is where an [x] would be.
	_ = rawFrame(m)
	if term, _ := endButtons(t, m); term != -1 {
		t.Fatalf("the service's box drew an [x] at column %d, want none", term)
	}
}

// A tool is ended the same way and has the same race behind it: the listing is
// a poll behind the kill, and a tool picked back up off it would arrive put
// away, into a strip the press just emptied.
func TestAnEndedToolIsNotReopenedByThePoll(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.execs = []Exec{{
		ID: "exec_diff", Command: []string{"discobox-review"}, Tool: "diff",
		Tty: true, Live: true, CreatedAt: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC),
	}}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the running tool", func() bool { return m.toolPane("diff") != nil })
	// The answer a poll started before the kill comes back with.
	stale, _ := ds.Execs(t.Context(), "")

	m.showTool(m.toolPane("diff"))
	d.key("ctrl+a")
	d.key(toolCloseKey)
	d.wait("the session ended", func() bool { return len(ds.endedExecs()) == 1 })

	d.dispatch(workspaceExecsMsg{gen: m.wsGen, execs: stale})
	d.settle()
	if p := m.toolPane("diff"); p != nil {
		t.Fatalf("the tool came back as %q, want a closed tool to stay closed", p.name())
	}
}
