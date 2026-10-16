package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/discobox-ai/discobox/programstatus"
)

// rawSent is the sequence a command writes to the window's terminal, or ""
// when it writes none.
func rawSent(t *testing.T, cmd tea.Cmd) string {
	t.Helper()
	if cmd == nil {
		return ""
	}
	raw, ok := cmd().(tea.RawMsg)
	if !ok {
		t.Fatalf("the command did not write to the terminal")
	}
	seq, _ := raw.Msg.(string)
	return seq
}

func statusModel(t *testing.T) *Model {
	t.Helper()
	m := newTestModel(t, newFakeSource())
	// The list with no folder chosen, which is every box.
	m.list = agentList()
	m.focus = focusList
	return m
}

// A terminal that never answered the probe is never sent a record.
func TestNoRecordIsSentToATerminalThatDidNotAnswer(t *testing.T) {
	m := statusModel(t)
	if got := rawSent(t, m.syncProgramStatus()); got != "" {
		t.Fatalf("sent %q to a terminal that never said it knows the protocol", got)
	}
}

// Once the terminal answers, the focused box's status is sent, again only
// when it changes, and a box with none is sent as a clear.
func TestTheFocusedBoxsStatusIsSentAsFocusMoves(t *testing.T) {
	m := statusModel(t)
	m.list.moveTo(0) // blocked-box
	_, cmd := m.Update(uv.UnknownOscEvent("\x1b]7501;?\x1b\\"))
	if !m.statusTerminal {
		t.Fatal("the probe's answer was not taken")
	}
	want := programstatus.Record{State: programstatus.Blocked, Progress: -1, Title: "blocked-box", Msg: "Allow rm -rf build?"}.Sequence()
	if got := rawSent(t, cmd); got != want {
		t.Fatalf("sent %q, want %q", got, want)
	}
	if got := rawSent(t, m.syncProgramStatus()); got != "" {
		t.Fatalf("sent %q again with nothing changed", got)
	}

	m.list.moveTo(5) // quiet-box
	if got := rawSent(t, m.syncProgramStatus()); got != programstatus.ClearSequence {
		t.Fatalf("sent %q for a box with no status, want a clear", got)
	}

	// The prompt is focused on no box.
	m.list.moveTo(1)
	m.focus = focusPrompt
	if got := rawSent(t, m.syncProgramStatus()); got != "" {
		t.Fatalf("sent %q with no box focused and the clear already sent", got)
	}
}

// The workspace's box speaks from its primary pane's stream once that has
// heard a report, the polled status standing in until then.
func TestTheWorkspacesBoxSpeaksFromItsStream(t *testing.T) {
	m := statusModel(t)
	m.statusTerminal = true
	box := m.list.all[1] // working-box, polled working
	p := &pane{id: 1, primary: true, sandbox: box}
	m.paneBox = box
	m.terminals.panes = append(m.terminals.panes, p)

	rec, ok := m.focusedProgramStatus()
	if !ok || rec.State != programstatus.Working {
		t.Fatalf("before the stream reported: %+v, %v; want the polled working", rec, ok)
	}
	p.observeProgramStatus([]byte("7501;state=blocked:kind=question:app=claude-code"))
	rec, ok = m.focusedProgramStatus()
	if !ok || rec.State != programstatus.Blocked || rec.Kind != programstatus.Question || rec.Title != "working-box" {
		t.Fatalf("after the stream reported: %+v, %v; want blocked on a question", rec, ok)
	}
	p.observeProgramStatus([]byte("7501;state=clear"))
	if rec, ok := m.focusedProgramStatus(); ok {
		t.Fatalf("after the stream cleared: %+v; the older poll must not come back", rec)
	}

	// A prompt ends what the job before it said it was doing.
	p.observeProgramStatus([]byte("7501;state=working"))
	p.observePrompt([]byte("133;A"))
	if rec, ok := m.focusedProgramStatus(); ok {
		t.Fatalf("after a prompt: %+v, want nothing", rec)
	}

	// A poll observed since the stream last spoke carries what the shim
	// dropped that the pane could not see: it wins.
	p.observeProgramStatus([]byte("7501;state=working"))
	for i := range m.list.all {
		if m.list.all[i].ID == box.ID {
			m.list.all[i].Agent = programstatus.Status{State: programstatus.Idle}
			m.list.all[i].AgentObservedAt = p.reportedAt.Add(time.Second)
		}
	}
	if rec, ok := m.focusedProgramStatus(); !ok || rec.State != programstatus.Idle {
		t.Fatalf("with a newer poll: %+v, %v; want the poll's idle", rec, ok)
	}
}

// The window takes back what it sent once it has ended, and writes nothing
// when it sent nothing that stands.
func TestTheWindowClearsWhatItSentOnItsWayOut(t *testing.T) {
	m := statusModel(t)
	var out strings.Builder
	m.clearProgramStatus(&out)
	if out.Len() != 0 {
		t.Fatalf("wrote %q having sent nothing", out.String())
	}
	m.statusSent = programstatus.Record{State: programstatus.Working, Progress: -1}.Sequence()
	m.clearProgramStatus(&out)
	if out.String() != programstatus.ClearSequence {
		t.Fatalf("wrote %q, want the clear", out.String())
	}
}
