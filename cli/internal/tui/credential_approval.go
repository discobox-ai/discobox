package tui

import (
	tea "charm.land/bubbletea/v2"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

// Each part has its own decision and scope. Nothing is sent until both have
// been reviewed; canceling leaves the whole request pending on the server.
// What the card asks, accepts and says, and where its answer goes next, are
// the core's (console.RequestApproval).
func (m *Model) approvalPart(a console.RequestApproval, delegate bool) tea.Cmd {
	ask := a.PartAsk(delegate)
	q := console.ApprovalPartForm(ask)
	f := newForm(q.Fields...)
	var d *dialog
	d = approvalDialog(console.PartTitle(delegate), f, console.CardActions(a.Request, true), func(f *form) tea.Cmd {
		if d.chosenAction() == console.ActionRaw {
			return m.showRawRequest(a.Request, func() tea.Cmd { m.dialog = d; return nil })
		}
		decision, part := console.PartApprove, console.ReadApprovalPart(ask, f.answers())
		if !d.chosenAction().Accepts() {
			decision = console.PartDeny
		} else {
			p, ok := answer(f, q)
			if !ok {
				return nil
			}
			part = p
		}
		n, step := a.Decide(delegate, decision, part)
		switch step {
		case console.StepDelegatePart:
			return m.approvalPart(n, true)
		case console.StepDeny:
			return m.denyCredential(a.Request)
		case console.StepNewSecret:
			return m.askNewCredentialForm(n)
		}
		return m.chooseSecret(n)
	})
	d.titleRight = a.PartCount(delegate)
	d.refreshSections = func() { d.sections = detailSections(a.PartCard(delegate, f.answers())) }
	d.refreshSections()
	m.dialog = d
	return nil
}

func (m *Model) confirmCredentialApproval(a console.RequestApproval) tea.Cmd {
	var d *dialog
	d = approvalDialog(console.ReviewTitle, newForm(), console.CardActions(a.Request, false), func(*form) tea.Cmd {
		if !d.chosenAction().Accepts() {
			return m.denyCredential(a.Request)
		}
		return m.finishApproval(a)
	})
	d.sections = detailSections(a.Review())
	m.dialog = d
	return nil
}
