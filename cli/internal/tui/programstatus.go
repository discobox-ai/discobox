package tui

import (
	"io"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/pkg/programstatus"
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

// observeProgram is the primary pane's watch on what its program reports
// (OSC 7501), and on a shell prompt beginning (OSC 133 A), read off the
// stream as it is drawn, so the window speaks for the box without waiting
// for the next poll. The protocol's rules are programstatus.Set.Observe's.
func (p *pane) observeProgram(data []byte) {
	p.reported.Observe(data, time.Now().UTC(), 0)
}

// focusedProgramStatus is the status of the discobox the window is focused
// on, as one record to send: the workspace's box while one is open, the
// list's cursor row while the list has focus, and nothing otherwise.
//
// The workspace's box speaks from its primary pane's stream or from the
// poll, whichever the core's rule says (console.Sandbox.AgentHeard), the
// rule the desktop's header is drawn by as well.
func (m *Model) focusedProgramStatus() (programstatus.Record, bool) {
	var box console.Sandbox
	switch p := m.Primary(); {
	case p != nil:
		box = m.currentBox()
		heard, ended := m.primaryHeard()
		status, ok := box.AgentHeard(heard, ended)
		if !ok {
			return programstatus.Record{}, false
		}
		rec := status.Record()
		rec.Title = recordTitle(box)
		return rec, true
	case m.focus == focusList && m.list.current() != nil:
		box = *m.list.current()
	default:
		return programstatus.Record{}, false
	}
	if box.Agent.State == "" {
		return programstatus.Record{}, false
	}
	rec := box.Agent.Record()
	rec.Title = recordTitle(box)
	return rec, true
}

// recordTitle is the title the window's record carries for box: what the box
// is called without its program's words — the name it was given, else the
// name it was configured with, else its ID (ADR 26-10-31-618). Never the
// listing's name, which holds the msg (sandboxmeta.Title) or, with none, a
// title the program wrote; the record carries the msg beside it already, and
// would say it twice, or say it once from a poll that lags the stream.
func recordTitle(box console.Sandbox) string {
	if box.GivenName != "" {
		return box.GivenName
	}
	return box.Unnamed()
}

// primaryTitle is what the box in the workspace is called from the primary
// terminal's side (console.Sandbox.PrimaryTitle): its resolved title
// followed by the msg its program reports while it has one, as "title: msg".
// The msg is focusedProgramStatus's, so it is the one the program said last,
// from the stream or the poll, whichever is newer — the listing's name
// carries the poll's, which lags what the pane has drawn; the title is the
// pane's own, for the same reason, and the one the box last reported until
// the pane has drawn one. Never empty: a workspace always has a box, and a
// box always has an ID.
func (m *Model) primaryTitle() string {
	var msg string
	if rec, ok := m.focusedProgramStatus(); ok {
		msg = rec.Msg
	}
	return m.currentBox().PrimaryTitle(m.primaryTermTitle(), msg)
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
// draws (console.AgentGlyph), uncolored, and a space, or nothing with no status to show. A
// terminal that knows the protocol, or has not said yet, shows the status
// its own way, and the title carries none.
func (m *Model) statusTitlePrefix() string {
	if m.statusTerminal != statusTitled || m.Primary() == nil {
		return ""
	}
	rec, ok := m.focusedProgramStatus()
	if !ok {
		return ""
	}
	return console.AgentGlyph(rec.State).Glyph + " "
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

// primaryHeard is what the workspace's primary pane has heard its program
// say since it attached, and whether its session has ended: nil with no
// primary, which leaves the poll to speak (console.Sandbox.AgentHeard).
func (m *Model) primaryHeard() (*programstatus.Set, bool) {
	p := m.Primary()
	if p == nil {
		return nil, false
	}
	return &p.reported, p.Exited
}

// primaryTermTitle is the title the workspace's primary terminal last set:
// empty before it has set one, or with no primary.
func (m *Model) primaryTermTitle() string {
	p := m.Primary()
	if p == nil || p.term == nil {
		return ""
	}
	return p.term.Title()
}
