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

type HarnessChangedFile struct {
	Path    string
	Content string
	Remove  bool
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
	for _, f := range c.Files {
		if f.Remove {
			fmt.Fprintf(&body, "\nRemove %s\n", f.Path)
		} else {
			fmt.Fprintf(&body, "\nUpdate %s\n%s\n", f.Path, f.Content)
		}
	}
	for _, credential := range c.Credentials {
		fmt.Fprintf(&body, "\nCredential: %s\n", credential)
	}
	if len(c.Files)+len(c.Credentials) == 0 {
		body.WriteString("\nNo saveable settings or credentials.\n")
	}
	for _, diagnostic := range c.Diagnostics {
		fmt.Fprintf(&body, "\n%s\n", diagnostic)
	}
	body.WriteString("\nSave updates shared harness defaults and credentials. Discard leaves local files unchanged.")
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
	return nil
}

func (m *Model) harnessChangesDone(msg harnessChangesDoneMsg) tea.Cmd {
	if msg.err != nil {
		m.dialog = errorDialog("Harness changes", msg.err.Error()+"\n\nReopen the preview to inspect current changes. Conflicts require an explicit reconsider selection with discobox admin harness reconsider.")
		return nil
	}
	return tea.Batch(m.refresh(), m.report(false, "harness changes: %s", msg.state))
}
