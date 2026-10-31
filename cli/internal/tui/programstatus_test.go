package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/discobox-ai/discobox/pkg/programstatus"
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
	if m.statusTerminal != statusSpoken {
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
	m.statusTerminal = statusSpoken
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

// A terminal whose device attributes arrive with no answer to the probe
// before them does not know the protocol: it is sent no record, and the
// primary box's status leads the window title instead, since the program in
// the box was told the protocol is there and may have taken its activity out
// of its title.
func TestATerminalThatDoesNotKnowTheProtocolGetsTheStatusInItsTitle(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")
	term.send("\x1b]2;Claude Code\x07")
	d.wait("the title", func() bool { return m.View().WindowTitle == "Claude Code" })

	if _, cmd := m.Update(uv.PrimaryDeviceAttributesEvent{62, 22}); rawSent(t, cmd) != "" {
		t.Fatal("sent a record to a terminal that does not know the protocol")
	}
	if got := rawSent(t, m.syncProgramStatus()); got != "" {
		t.Fatalf("sent %q to a terminal that does not know the protocol", got)
	}
	if m.statusTerminal != statusTitled {
		t.Fatalf("support = %v after attributes with no answer, want titled", m.statusTerminal)
	}
	term.send("\x1b]7501;state=blocked:kind=permission\x1b\\")
	d.wait("the status in the title", func() bool { return m.View().WindowTitle == "◆ Claude Code" })
	term.send("\x1b]7501;state=working\x1b\\")
	d.wait("the new status in the title", func() bool { return m.View().WindowTitle == "▶ Claude Code" })
	term.send("\x1b]7501;state=clear\x1b\\")
	d.wait("the status gone from the title", func() bool { return m.View().WindowTitle == "Claude Code" })
}

// A terminal that knows the protocol shows the status its own way: the
// attributes after its answer change nothing, and the title carries no glyph.
func TestATerminalThatKnowsTheProtocolKeepsItsTitlePlain(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, term := openWorkspace(t, ds, "enter")
	d.dispatch(uv.UnknownOscEvent("\x1b]7501;?\x1b\\"))
	d.dispatch(uv.PrimaryDeviceAttributesEvent{62, 22})
	if m.statusTerminal != statusSpoken {
		t.Fatalf("support = %v, want spoken", m.statusTerminal)
	}
	term.send("\x1b]2;Claude Code\x1b\\\x1b]7501;state=working\x1b\\")
	d.wait("the title", func() bool { return m.View().WindowTitle == "Claude Code" })
	if rec, ok := m.focusedProgramStatus(); !ok || rec.State != programstatus.Working {
		t.Fatalf("status = %+v, %v; want working heard", rec, ok)
	}
	if got := m.View().WindowTitle; got != "Claude Code" {
		t.Fatalf("title = %q, want no glyph", got)
	}
}

// The record names the box without its program's words: the listing's name
// of a box nobody named is the msg itself (sandboxmeta.Title), which the
// record already carries beside the title.
func TestTheRecordsTitleIsNotTheMsgAgain(t *testing.T) {
	box := Sandbox{
		ID: "sbx_quiet", Name: "Fix the reaper race", ConfigName: "brave-otter", State: StateRunning,
		Agent: programstatus.Status{State: programstatus.Working, Msg: "Fix the reaper race"},
	}
	if got := recordTitle(box); got != "brave-otter" {
		t.Fatalf("title = %q, want the configured name", got)
	}
	box.GivenName = "Reaper"
	if got := recordTitle(box); got != "Reaper" {
		t.Fatalf("title = %q, want the given name", got)
	}
	box.GivenName, box.ConfigName = "", ""
	if got := recordTitle(box); got != "sbx_quiet" {
		t.Fatalf("title = %q, want the ID", got)
	}
}
