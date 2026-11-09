package tui

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

// The settings form: `discobox admin box update` as one card. It edits what
// that command edits — the configured name, the harness, the discobox's own
// egress level and its discobox API access — reading what is stored when it
// opens, and saving only the parts that were changed, so a save never writes
// back a value somebody else changed meanwhile that the card did not touch.

// settingsKey is the letter the settings form answers to in the list and in
// the box menu. Lower-case e is rename, which is the name a person reads; this
// is everything else a discobox is configured with, the configured name
// among it.
var settingsKey = bareKey("settings")

// boxSettingsReadMsg is what the settings form opens on, read off the server,
// with the card that was put up to wait for it.
type boxSettingsReadMsg struct {
	wait      *dialog
	box       console.Sandbox
	settings  console.BoxSettings
	harnesses []console.Harness
	err       error
}

// boxSettingsSaveMsg is an answered settings form on its way back to the live
// model, the way renameMsg carries a name. It carries the form's own card, so
// a save the server refuses can put it back with everything typed.
type boxSettingsSaveMsg struct {
	form   *dialog
	box    console.Sandbox
	update console.BoxSettingsUpdate
}

type boxSettingsDoneMsg struct {
	form, wait *dialog
	err        error
}

// askSettings reads what the box is configured with and the harnesses on its
// server, and opens the form once both are in.
//
// The read is waited on behind a card of its own, not on the busy line: the
// answer opens a modal, and one that arrived after the person had opened a
// workspace or another dialog would be drawn over it and take its keys. The
// form replaces only that card; Esc on it gives the read up.
func (m *Model) askSettings(box console.Sandbox) tea.Cmd {
	wait := statusDialog("Settings", "reading the settings of "+boxTitle(box, box.TerminalTitle)+"…")
	m.dialog = wait
	return func() tea.Msg {
		settings, err := m.ds.ForServer(box.Server).BoxSettings(m.ctx, box.ID)
		if err != nil {
			return boxSettingsReadMsg{wait: wait, box: box, err: err}
		}
		// The harnesses only fill the harness row. A listing that fails
		// leaves the row on the box's own harness, and the rest of the form
		// as editable as ever.
		harnesses, _ := m.ds.Harnesses(m.ctx, box.Server)
		return boxSettingsReadMsg{wait: wait, box: box, settings: settings, harnesses: harnesses}
	}
}

func (m *Model) settingsRead(msg boxSettingsReadMsg) tea.Cmd {
	if m.dialog != msg.wait {
		// Given up, or something else has the screen now.
		return nil
	}
	if msg.err != nil {
		m.dialog = errorDialog("Settings", fmt.Sprintf("%v", msg.err))
		return nil
	}
	m.dialog = settingsDialog(msg.box, msg.settings, msg.harnesses)
	return nil
}

// settingsDialog is the core's settings form (console.SettingsForm) as a card.
func settingsDialog(box console.Sandbox, was console.BoxSettings, harnesses []console.Harness) *dialog {
	q := console.SettingsForm(box, was, harnesses)
	// Named without the msg its program puts after the title (boxTitle), as
	// rename's dialog is: the card names the box, not what it is doing.
	var d *dialog
	d = formDialog("Settings · "+boxTitle(box, box.TerminalTitle), q, func(update console.BoxSettingsUpdate) tea.Cmd {
		if update.Empty() {
			return status("settings unchanged")
		}
		form := d
		return func() tea.Msg { return boxSettingsSaveMsg{form: form, box: box, update: update} }
	})
	d.sections = []section{{lines: []line{
		{text: "what `discobox admin box update` changes · egress here is the box's own level, one of four with its project, pool and harness", tone: toneDim},
	}}}
	d.keys = []hint{says("↑↓ moves"), pressing("enter saves", "enter"), pressing("esc leaves it alone", "esc")}
	return d
}

// saveSettings sends the form's answers, waiting behind a card as the read
// did, for the same reason.
func (m *Model) saveSettings(msg boxSettingsSaveMsg) tea.Cmd {
	wait := statusDialog("Settings", "saving…")
	m.dialog = wait
	return func() tea.Msg {
		return boxSettingsDoneMsg{form: msg.form, wait: wait, err: m.ds.ForServer(msg.box.Server).UpdateBox(m.ctx, msg.box.ID, msg.update)}
	}
}

// settingsDone takes the wait down. A refusal puts the form back, holding
// everything typed and saying why on it, since the server refuses for reasons
// a person fixes in one row — a harness that is not there, a name already taken —
// and retyping six rows for that is the alternative. A form given up meanwhile
// is not brought back; the refusal goes to the status line instead.
func (m *Model) settingsDone(msg boxSettingsDoneMsg) tea.Cmd {
	waiting := m.dialog == msg.wait
	if msg.err != nil {
		if !waiting {
			return m.report(true, "settings: %v", msg.err)
		}
		msg.form.form.err = fmt.Sprintf("not saved: %v", msg.err)
		m.dialog = msg.form
		return nil
	}
	if waiting {
		m.dialog = nil
	}
	return tea.Batch(m.refresh(), m.report(false, "settings saved"))
}
