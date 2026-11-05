package tui

import tea "charm.land/bubbletea/v2"

func approvalDialog(title string, f *form, submit func(*form) tea.Cmd) *dialog {
	d := formDialog(title, f, submit)
	d.formActions = []action{{key: "accept", label: "Accept", enabled: true}, {key: "deny", label: "Deny", enabled: true}}
	d.formActionAt = -1
	if len(f.rows) == 0 {
		d.formActionAt = 0
	}
	d.requireEnd = true
	d.decisionsBlocked = true
	d.keys = []hint{says("Tab / arrows move"), pressing("Enter selects", "enter"), pressing("Esc leaves waiting", "esc")}

	return d
}
func (m *Model) askAboutAPIAccess(req CredentialRequest) tea.Cmd {
	var d *dialog
	d = approvalDialog("Allow discobox API access?", newForm(), func(*form) tea.Cmd { return m.answerAPIAccess(req, d.formActions[d.formActionAt].key == "accept") })
	d.sections = []section{
		{fields: []field{{label: "Box", value: req.SandboxID, tone: toneAccent}, {label: "Server", value: req.Server}, {label: "Reason (box's words)", value: req.Justification}}},
		{label: "Project access", fields: []field{{label: "Discovery", value: "List boxes and watch project events"}, {label: "Create", value: "Launch boxes within its allowed egress, using project harness configs and their credentials"}}},
		{label: "Authority over boxes it leads", fields: []field{{label: "Terminals/files", value: "Run commands; read and write files; read output and type input"}, {label: "Lifecycle", value: "Start, stop, restart, repair, upgrade and archive"}, {label: "Source/settings", value: "Deliver source and edit metadata"}, {label: "Services/ports", value: "Inspect and control services; reach HTTP ports"}, {label: "Credentials", value: "Answer eligible requests within its delegation"}}},
		{label: "Limits", lines: []line{{text: "Management covers direct children in this project. Wider-egress children are read-only."}, {text: "Enabling API access grants no credential. Credential and egress restrictions still apply."}, {text: "Access stays on until a person turns it off in Settings. Deny leaves access unchanged."}}},
	}
	m.dialog = d
	return nil
}
func (m *Model) answerAPIAccess(req CredentialRequest, approve bool) tea.Cmd {
	m.dialog = statusDialog("API access request", "answering…")
	return func() tea.Msg {
		return credentialAnsweredMsg{request: req, approved: approve, err: m.ds.DecideAPIAccessRequest(m.ctx, req.Server, req.ID, approve)}
	}
}
