package tui

import (
	"io"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/discobox-ai/discobox/programstatus"
)

// The window speaks OSC 7501 to the terminal it runs in for the discobox it
// is focused on (ADR 26-10-16-424 §6), so that terminal's tab can say that
// box's agent is working, or blocked on you, while the window is behind
// another.
//
// It speaks only to a terminal that answered the probe Init sends. To one
// that did not, the box's status leads the window title as a glyph instead
// (statusTitlePrefix): the shim tells the program in the box the protocol is
// there whatever this terminal knows (§1), and a program that hears so may
// take its activity out of its title. It speaks for one box, as the terminal's root record: the window's terminal
// is showing one box, and a record per box would make one tab an inbox. The
// record is re-encoded from the parsed fields, never relayed as bytes, so
// nothing a program in a box writes reaches the window's terminal except as
// those fields. The window clears it as it exits (clearProgramStatus), and
// termguard after a crash.

// statusSupport is what the window's terminal answered the probe.
type statusSupport int

const (
	// statusUnanswered is a probe still out: nothing is sent, and the title
	// carries no glyph.
	statusUnanswered statusSupport = iota
	// statusSpoken is a terminal that knows the protocol, and is sent the
	// record.
	statusSpoken
	// statusTitled is a terminal that does not, whose title carries the
	// status instead.
	statusTitled
)

// probeProgramStatus asks the window's terminal whether it knows the
// protocol, and then for its primary device attributes, which every terminal
// answers. A terminal answers in order, so one that knows the protocol has
// said so before its attributes arrive, and attributes with no answer before
// them are a terminal that does not.
func probeProgramStatus() tea.Cmd {
	return tea.Raw(programstatus.QuerySequence + ansi.RequestPrimaryDeviceAttributes)
}

// isProgramStatusReply reports whether msg is the terminal's answer to the
// probe, which arrives as an OSC the input decoder does not know.
func isProgramStatusReply(msg tea.Msg) bool {
	osc, ok := msg.(uv.UnknownOscEvent)
	return ok && strings.HasPrefix(strings.TrimPrefix(string(osc), "\x1b]"), "7501;?")
}

// isProbeSettled reports whether msg is the terminal's device attributes, the
// answer that closes the probe.
func isProbeSettled(msg tea.Msg) bool {
	_, ok := msg.(uv.PrimaryDeviceAttributesEvent)
	return ok
}

// observeProgramStatus is the primary pane's watch on what its program
// reports, read off the stream as it is drawn, so the window speaks for the
// box without waiting for the next poll.
func (p *pane) observeProgramStatus(data []byte) {
	report, ok := programstatus.Parse(data)
	if !ok || report.Query {
		return
	}
	p.reportedAt = time.Now().UTC()
	p.reported.Apply(report, p.reportedAt, 0)
}

// observePrompt drops what the program said it was doing when a shell prompt
// begins (OSC 133 A), as the shim does: the job that said it is over.
func (p *pane) observePrompt(data []byte) {
	if strings.HasPrefix(string(data), "133;A") {
		p.reported.Drop(func(state programstatus.State, _ int) bool { return programstatus.Live(state) })
	}
}

// focusedProgramStatus is the status of the discobox the window is focused
// on, as one record to send: the workspace's box while one is open, the
// list's cursor row while the list has focus, and nothing otherwise.
//
// The workspace's box speaks from its primary pane's stream while the last
// report the pane heard is newer than the poll the listing carries — the
// stream is the shim's own words as they are said, and lags nothing — and
// from the poll otherwise. A repaint on attach carries no report, so a pane
// that has heard none yet defers to the poll; so does one whose session has
// ended. And the shim keeps lifetime rules the pane cannot, ending what a
// process said once its process group has left the foreground: a poll taken
// since the pane last heard anything carries that, so it wins. The two
// clocks compared are the sandbox's and this machine's.
func (m *Model) focusedProgramStatus() (programstatus.Record, bool) {
	var box Sandbox
	switch p := m.primary(); {
	case p != nil:
		box = m.currentBox()
		if !p.exited && !p.reportedAt.IsZero() && p.reportedAt.After(box.AgentObservedAt) {
			status, ok := p.reported.Effective()
			if !ok {
				return programstatus.Record{}, false
			}
			rec := status.Record()
			rec.Title = displayName(box)
			return rec, true
		}
	case m.focus == focusList && m.list.current() != nil:
		box = *m.list.current()
	default:
		return programstatus.Record{}, false
	}
	if box.Agent.State == "" {
		return programstatus.Record{}, false
	}
	rec := box.Agent.Record()
	rec.Title = displayName(box)
	return rec, true
}

// syncProgramStatus sends the focused box's status when it is not what was
// last sent: on a change of focus, and as that box's status changes. A box
// with none is sent as a clear, so the last box's status does not stay on
// the tab of a window that has moved on.
func (m *Model) syncProgramStatus() tea.Cmd {
	if m.statusTerminal != statusSpoken {
		return nil
	}
	seq := programstatus.ClearSequence
	if rec, ok := m.focusedProgramStatus(); ok {
		seq = rec.Sequence()
	}
	if seq == m.statusSent {
		return nil
	}
	m.statusSent = seq
	return tea.Raw(seq)
}

// statusTitlePrefix is what leads the window title on a terminal that does
// not know the protocol: the primary box's status as the glyph its list row
// draws (agentMark) and a space, or nothing with no status to show. A
// terminal that knows the protocol, or has not said yet, shows the status
// its own way, and the title carries none.
func (m *Model) statusTitlePrefix() string {
	if m.statusTerminal != statusTitled || m.primary() == nil {
		return ""
	}
	rec, ok := m.focusedProgramStatus()
	if !ok {
		return ""
	}
	return agentMark(rec.State) + " "
}

// clearProgramStatus takes back the status the window sent, once the program
// has ended and its terminal is the shell's again. A window that sent none,
// or whose last was a clear, writes nothing.
func (m *Model) clearProgramStatus(out io.Writer) {
	if m.statusSent == "" || m.statusSent == programstatus.ClearSequence {
		return
	}
	_, _ = io.WriteString(out, programstatus.ClearSequence)
	m.statusSent = programstatus.ClearSequence
}
