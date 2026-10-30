package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

const harnessChangesKey = "H"

// HarnessChanges contains only the server's safe preview, never raw credentials.
type HarnessChanges struct {
	CandidateID string
	State       string
	Revision    int64
	Files       []HarnessChangedFile
	Credentials []string
	Diagnostics []string
}

// HarnessChangedFile is one file a candidate adds, updates or removes, shown
// as what changed in it rather than what it now holds.
type HarnessChangedFile struct {
	Path string
	// Action is add, update or remove.
	Action string
	// Diff is the change's unified diff hunks against the file it replaces.
	Diff string
}

type harnessChangesLoadedMsg struct {
	box     Sandbox
	changes HarnessChanges
	err     error
}

type harnessChangesDoneMsg struct {
	state string
	err   error
}

func (m *Model) reviewHarnessChanges(box Sandbox) tea.Cmd {
	m.busy = "reading harness changes…"
	return func() tea.Msg {
		changes, err := m.ds.HarnessChanges(m.ctx, box.ID)
		return harnessChangesLoadedMsg{box: box, changes: changes, err: err}
	}
}

func (m *Model) showHarnessChanges(msg harnessChangesLoadedMsg) tea.Cmd {
	m.busy = ""
	if msg.err != nil {
		m.dialog = errorDialog("Harness changes", msg.err.Error())
		return nil
	}
	c := msg.changes
	var body strings.Builder
	fmt.Fprintf(&body, "State: %s · revision %d\n", c.State, c.Revision)
	if len(c.Files)+len(c.Credentials) == 0 {
		body.WriteString("\nNo saveable settings or credentials.\n")
	}
	body.WriteString("\nSave updates shared harness defaults and credentials. Discard leaves local files unchanged.")
	var sections []section
	for _, f := range c.Files {
		sec := section{label: f.Action + " " + f.Path}
		for _, text := range strings.Split(f.Diff, "\n") {
			if text == "" {
				continue
			}
			l := line{text: untab(text)}
			if style := diffStyle(text); style != nil {
				l.spans = []span{{text: l.text, style: style}}
			}
			sec.lines = append(sec.lines, l)
		}
		sections = append(sections, sec)
	}
	if len(c.Credentials) > 0 {
		sec := section{label: "credentials"}
		for _, credential := range c.Credentials {
			sec.lines = append(sec.lines, line{text: credential, bullet: true})
		}
		sections = append(sections, sec)
	}
	if len(c.Diagnostics) > 0 {
		sec := section{label: "diagnostics"}
		for _, diagnostic := range c.Diagnostics {
			sec.lines = append(sec.lines, line{text: diagnostic, bullet: true, tone: toneAlert})
		}
		sections = append(sections, sec)
	}
	saveable := c.CandidateID != "" && ((c.State == "pending" && len(c.Files)+len(c.Credentials) > 0) || c.State == "saved-handoff-pending")
	items := []action{
		{key: "s", press: "s", label: "Save reviewed changes", enabled: saveable, why: "no saveable candidate"},
		{key: "d", press: "d", label: "Discard candidate", enabled: c.CandidateID != "" && c.State == "pending", why: "no pending candidate"},
		{key: "r", press: "r", label: "Refresh preview", enabled: true},
	}
	m.dialog = actionsDialog("Harness changes — "+msg.box.Name, body.String(), items, func(key string) tea.Cmd {
		if key == "r" {
			return m.reviewHarnessChanges(msg.box)
		}
		// The closure keeps the exact ID that was displayed, even if another
		// candidate arrives while the person reads this card.
		return func() tea.Msg {
			var state string
			var err error
			if key == "s" {
				state, err = m.ds.SaveHarnessChanges(m.ctx, msg.box.ID, c.CandidateID)
			} else {
				state, err = m.ds.DiscardHarnessChanges(m.ctx, msg.box.ID, c.CandidateID)
			}
			return harnessChangesDoneMsg{state: state, err: err}
		}
	})
	m.dialog.sections = sections
	return nil
}

func (m *Model) harnessChangesDone(msg harnessChangesDoneMsg) tea.Cmd {
	if msg.err != nil {
		m.dialog = errorDialog("Harness changes", msg.err.Error()+"\n\nReopen the preview to inspect current changes. Conflicts require an explicit reconsider selection with discobox admin harness reconsider.")
		return nil
	}
	return tea.Batch(m.refresh(), m.report(false, "harness changes: %s", msg.state))
}
