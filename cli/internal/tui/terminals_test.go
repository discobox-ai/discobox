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
