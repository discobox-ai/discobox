package tui

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
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
const settingsKey = "E"

// boxSettingsReadMsg is what the settings form opens on, read off the server,
// with the card that was put up to wait for it.
type boxSettingsReadMsg struct {
	wait      *dialog
	box       Sandbox
	settings  BoxSettings
	harnesses []Harness
	err       error
}

// boxSettingsSaveMsg is an answered settings form on its way back to the live
// model, the way renameMsg carries a name. It carries the form's own card, so
// a save the server refuses can put it back with everything typed.
type boxSettingsSaveMsg struct {
	form   *dialog
	id     string
	update BoxSettingsUpdate
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
func (m *Model) askSettings(box Sandbox) tea.Cmd {
	wait := statusDialog("Settings", "reading the settings of "+boxTitle(box, box.TerminalTitle)+"…")
	m.dialog = wait
	return func() tea.Msg {
		settings, err := m.ds.BoxSettings(m.ctx, box.ID)
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

// settingsDialog is the card. The harness row is drawn on a box that is not
// stopped, with why it cannot be changed, because the server refuses a harness
// change on anything else and the card should say so before Enter does.
func settingsDialog(box Sandbox, was BoxSettings, harnesses []Harness) *dialog {
	name := textRow("name", "name", "the name it is configured with", was.Name)
	name.required = "a discobox needs a configured name"
	name.hint = "the handle it was created with, which commands take · the list shows the name rename gives it, or its agent's title"

	harness := pickRow("harness", "harness", harnessChoices(was.HarnessID, box, harnesses)...)
	harness.hint = "the new harness provisions on the next start · conversation state is not carried over"
	harness.when = func(*form) bool { return box.State == StateStopped }
	harness.why = "stop the box to change its harness"

	deny := pickRow("deny", "restrict",
		choice{key: "no", label: "no", hint: "this level restricts nothing · the project, pool or harness may still"},
		choice{key: "yes", label: "deny by default", hint: "reach only every level's allow entries and the hosts its harness image declares"})
	deny.section = "Egress"
	allow := textRow("allow", "allow", "hosts, *.wildcards, IPs or CIDRs, comma-separated", strings.Join(was.Egress.Allow, ", "))
	allow.section = "Egress"
	allow.hint = "added to the allow list of a restricted box · an allow list alone restricts nothing · empty is none"
	passthrough := textRow("passthrough", "passthrough", "hosts whose HTTPS is passed through unread", strings.Join(was.Egress.Passthrough, ", "))
	passthrough.section = "Egress"
	passthrough.hint = "spliced to the origin unread, TLS end to end: no credential is swapped in · it admits no host"

	api := pickRow("api", "discobox API",
		choice{key: "no", label: "no", hint: "the box cannot call the discobox API"},
		choice{key: "yes", label: "yes", hint: "the box may call the discobox API through its pool, acting only on the discoboxes it created"})
	api.section = "Access"

	f := newForm(name, harness, deny, allow, passthrough, api)
	// The name is compared with what the field opened holding, not with what
	// was read: the field drops a control character a stored name can carry,
	// and a save of anything else would then rename the box as well.
	opened := f.value("name")
	if was.Egress.DenyByDefault {
		f.choose("deny", "yes")
	}
	if was.DiscoboxAPI {
		f.choose("api", "yes")
	}

	// Named without the msg its program puts after the title (boxTitle), as
	// rename's dialog is: the card names the box, not what it is doing.
	var d *dialog
	d = formDialog("Settings · "+boxTitle(box, box.TerminalTitle), f, func(f *form) tea.Cmd {
		update := settingsUpdate(f, was, opened, box.State == StateStopped)
		if update.empty() {
			return status("settings unchanged")
		}
		id, form := box.ID, d
		return func() tea.Msg { return boxSettingsSaveMsg{form: form, id: id, update: update} }
	})
	d.sections = []section{{lines: []line{
		{text: "what `discobox admin box update` changes · egress here is the box's own level, one of four with its project, pool and harness", tone: toneDim},
	}}}
	d.keys = []hint{says("↑↓ moves"), pressing("enter saves", "enter"), pressing("esc leaves it alone", "esc")}
	return d
}

// harnessChoices is every enabled harness on the box's server, and the box's
// own harness whatever state it is in, so the row opens on what the box has.
func harnessChoices(current string, box Sandbox, harnesses []Harness) []choice {
	var choices []choice
	found := false
	for _, h := range harnesses {
		if h.ID != current && h.State != HarnessEnabled {
			continue
		}
		found = found || h.ID == current
		label := h.flagName()
		hint := h.Name
		if h.ID == current {
			hint = "what the box has now"
		}
		choices = append(choices, choice{key: h.ID, label: label, hint: hint})
	}
	if !found {
		// A harness the listing does not hold — deleted, or the listing
		// failed — is still the box's, and the row has to open on it.
		label := box.Harness
		if label == "" {
			label = current
		}
		choices = append([]choice{{key: current, label: label, hint: "what the box has now"}}, choices...)
	}
	for i, c := range choices {
		if c.key == current {
			// The current harness leads, so the row opens on it.
			choices = append(append([]choice{c}, choices[:i]...), choices[i+1:]...)
			break
		}
	}
	return choices
}

// settingsUpdate is what the answered form changes against what it opened on.
// The name is measured against opened, what its field started as.
func settingsUpdate(f *form, was BoxSettings, opened string, stopped bool) BoxSettingsUpdate {
	var update BoxSettingsUpdate
	if name := f.value("name"); name != opened {
		update.Name = &name
	}
	if harness := f.chosen("harness"); stopped && harness != "" && harness != was.HarnessID {
		update.Harness = &harness
	}
	if deny := f.chosen("deny") == "yes"; deny != was.Egress.DenyByDefault {
		update.EgressDeny = &deny
	}
	if allow := hostList(f.value("allow")); !slices.Equal(allow, was.Egress.Allow) {
		update.EgressAllow = &allow
	}
	if passthrough := hostList(f.value("passthrough")); !slices.Equal(passthrough, was.Egress.Passthrough) {
		update.EgressPassthrough = &passthrough
	}
	if api := f.chosen("api") == "yes"; api != was.DiscoboxAPI {
		update.DiscoboxAPI = &api
	}
	return update
}

// hostList is a typed list of egress entries: separated by commas or spaces,
// since no entry holds either, with the empty ones a trailing comma leaves
// dropped. Nothing typed is the empty list, never nil, so it compares equal to
// a level that stores none.
func hostList(text string) []string {
	return append([]string{}, strings.FieldsFunc(text, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })...)
}

// saveSettings sends the form's answers, waiting behind a card as the read
// did, for the same reason.
func (m *Model) saveSettings(msg boxSettingsSaveMsg) tea.Cmd {
	wait := statusDialog("Settings", "saving…")
	m.dialog = wait
	return func() tea.Msg {
		return boxSettingsDoneMsg{form: msg.form, wait: wait, err: m.ds.UpdateBox(m.ctx, msg.id, msg.update)}
	}
}

// settingsDone takes the wait down. A refusal puts the form back, holding
// everything typed and saying why on it, since the server refuses for reasons
// a person fixes in one row — a harness not configured, a name already taken —
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
