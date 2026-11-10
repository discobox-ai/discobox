package tui

import (
	"github.com/discobox-ai/discobox/cli/internal/console"

	tea "charm.land/bubbletea/v2"
)

// The trust inbox (ADR 0149): an agent asking for a host whose certificate the
// pool's egress refuses to be trusted for its discobox. It arrives in the same
// inbox as a credential request and is marked, bannered, and listed the same
// way; only the question differs. There is no secret to choose: the answer is
// which certificate from the chain the pool observed to pin, and for how long.

// askAboutTrust is the trust card: who is asking to reach which host, why, what
// the pool was shown there, and one row per certificate that may be pinned —
// the server's default first (console.TrustForm, console.TrustCard).
func (m *Model) askAboutTrust(req console.CredentialRequest) tea.Cmd {
	q := console.TrustForm(req)
	f := newForm(q.Fields...)
	var d *dialog
	d = approvalDialog(console.TrustTitle(req), f, console.CardActions(req, false), func(*form) tea.Cmd {
		if !d.chosenAction().Accepts() {
			return m.denyCredential(req)
		}
		trust, ok := answer(f, q)
		if !ok {
			return nil
		}
		return m.finishTrustApproval(req, trust)
	})
	d.refreshSections = func() {
		if card := console.TrustCard(req, f.answers()); card != nil {
			d.sections = detailSections(card)
		}
	}
	d.refreshSections()
	m.dialog = d
	return nil
}

func (m *Model) finishTrustApproval(req console.CredentialRequest, trust console.TrustAnswer) tea.Cmd {
	m.dialog = statusDialog("Trust request", console.Trusting(req, trust))
	return func() tea.Msg {
		_, err := console.TrustRequest(m.ctx, m.ds, req, trust)
		return credentialAnsweredMsg{request: req, approved: true, ttl: trust.TTL, err: err}
	}
}
