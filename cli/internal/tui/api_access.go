package tui

import (
	tea "charm.land/bubbletea/v2"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

// approvalDialog is a request's card as a form, answered with its actions as
// the core offers them (console.CardActions).
func approvalDialog(title string, f *form, offers []console.CardOffer, submit func(*form) tea.Cmd) *dialog {
	d := &dialog{kind: dlgForm, title: title, form: f, submit: submit}
	for _, o := range offers {
		d.formActions = append(d.formActions, action{key: string(o.Action), label: o.Label, enabled: true})
	}
	d.formActionAt = -1
	if len(f.rows) == 0 {
		d.formActionAt = 0
	}
	d.requireEnd = true
	d.decisionsBlocked = true
	d.keys = []hint{says("Tab / arrows move"), pressing("Enter selects", "enter"), pressing("Esc "+console.LeaveWaiting, "esc")}

	return d
}

// chosenAction is the card action a request's card was submitted with.
func (d *dialog) chosenAction() console.CardAction {
	return console.CardAction(d.formActions[d.formActionAt].key)
}

// askAboutAPIAccess is an API access request's card (console.APIAccessCard),
// accepted or denied.
func (m *Model) askAboutAPIAccess(req console.CredentialRequest) tea.Cmd {
	var d *dialog
	d = approvalDialog(console.APIAccessTitle, newForm(), console.CardActions(req, false), func(*form) tea.Cmd { return m.answerAccess(req, d.chosenAction().Accepts()) })
	d.sections = detailSections(console.APIAccessCard(req))
	m.dialog = d
	return nil
}

// answerAccess answers an adoption or an API access request.
func (m *Model) answerAccess(req console.CredentialRequest, approve bool) tea.Cmd {
	if !approve {
		return m.denyCredential(req)
	}
	title, _ := console.Denying(req)
	m.dialog = statusDialog(title, "answering…")
	return func() tea.Msg {
		_, err := console.ApproveAccess(m.ctx, m.ds, req)
		return credentialAnsweredMsg{request: req, approved: true, err: err}
	}
}
