package tui

import (
	"errors"
	"fmt"

	"github.com/discobox-ai/discobox/cli/internal/console"

	tea "charm.land/bubbletea/v2"
)

// A refresh request asks for a new value of a token the project holds, because
// its value went stale or an upstream refused it (ADR 26-09-25-122). It sits in
// the inbox with every other ask, and is answered by writing a value rather
// than by approving: running the command the token suggests, here, or typing
// one in.
//
// The window is what offers to run the command, because it is where a person
// is — the command runs on their machine, with their login, and they see it
// before it runs. "Run for this session" remembers the token and the exact
// command for as long as this window runs (console.RenewalPermits), and
// answers that pair's next asks without a prompt; an edited command is a
// different pair and asks again.

// renewalAnsweredMsg reports a renewal coming back. auto is one a session
// permission made without a prompt; secret is the token, whose permission a
// failure revokes (console.Renewals.Finish).
type renewalAnsweredMsg struct {
	request console.CredentialRequest
	secret  console.Secret
	auto    bool
	err     error
}

// askAboutRefresh is the card for a refresh request: which token, why, and the
// ways to give it a new value (console.RefreshForm, console.RefreshCard).
func (m *Model) askAboutRefresh(req console.CredentialRequest, secrets []console.Secret) tea.Cmd {
	secret, ok := console.RefreshedSecret(req, secrets)
	if !ok {
		m.dialog = errorDialog("Refresh request", console.RefreshTokenGone)
		return nil
	}
	q := console.RefreshForm(secret)
	f := newForm(q.Fields...)
	var d *dialog
	d = approvalDialog(console.RefreshTitle(secret), f, console.CardActions(req, false), func(*form) tea.Cmd {
		if !d.chosenAction().Accepts() {
			f.set("value", "")
			return m.denyCredential(req)
		}
		refresh, ok := answer(f, q)
		if !ok {
			return nil
		}
		f.set("value", "")
		if refresh.Session {
			m.renewals.Permits.Allow(m.session.ServerName(req.Server), secret.ID, secret.RefreshCommand)
		}
		return m.renew(req, secret, refresh.Renewal(req, secret))
	})
	d.onCancel = func() tea.Cmd { f.set("value", ""); return nil }
	d.refreshSections = func() {
		d.sections = detailSections(console.RefreshCard(req, secret, f.answers(), m.secrets.now()))
	}
	d.refreshSections()
	m.dialog = d
	return nil
}

// renew sends a renewal a person answered, saying what it is doing while the
// command runs. One renewal per request at a time: a session's own, begun on
// the beat, may be running while a person answers the same request by hand.
func (m *Model) renew(req console.CredentialRequest, secret console.Secret, renewal console.Renewal) tea.Cmd {
	if !m.renewals.Start(req) {
		m.dialog = nil
		return m.report(false, "%s", console.RenewingAlready(secret.Name))
	}
	m.dialog = statusDialog("Refresh request", console.Renewing(renewal))
	return func() tea.Msg {
		return renewalAnsweredMsg{request: req, secret: secret, err: m.ds.RefreshSecret(m.ctx, req.Server, renewal)}
	}
}

// renewalAnswered closes the dialog and re-reads the inbox, saying what the
// core says came of it (console.Renewals.Finish). A failure a person asked
// for is shown as the command or the server said it; the request is still
// waiting.
func (m *Model) renewalAnswered(msg renewalAnsweredMsg) tea.Cmd {
	said := m.renewals.Finish(m.session.ServerName(msg.request.Server), msg.request, msg.secret, msg.auto, msg.err)
	if errors.Is(msg.err, console.ErrNotPermitted) {
		return nil
	}
	if msg.err != nil && !msg.auto && !errors.Is(msg.err, console.ErrAlreadyAnswered) {
		d := errorDialog("Could not renew", fmt.Sprintf("%v", msg.err))
		d.sections = detailSections([]console.DetailSection{console.RenewFailed(msg.secret.Name)})
		m.dialog = d
		return m.loadCredentialRequests()
	}
	if !msg.auto {
		m.dialog = nil
	}
	if !said.Said {
		return m.loadCredentialRequests()
	}
	return tea.Batch(m.loadCredentialRequests(), m.report(said.Warning, "%s", said.Text))
}

// renewBySession answers the refresh requests a session permission covers,
// once each, as the inbox lists them (console.RenewByPermit).
func (m *Model) renewBySession(requests []console.CredentialRequest) tea.Cmd {
	due, permits := m.renewals.Due(m.session, requests)
	var cmds []tea.Cmd
	for _, req := range due {
		server := m.session.ServerName(req.Server)
		cmds = append(cmds, func() tea.Msg {
			secret, err := console.RenewByPermit(m.ctx, m.ds, permits, server, req)
			return renewalAnsweredMsg{request: req, secret: secret, auto: true, err: err}
		})
	}
	return tea.Batch(cmds...)
}
