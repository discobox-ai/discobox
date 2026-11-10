package tui

import (
	tea "charm.land/bubbletea/v2"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

// The refused credential: what the window says about a credential an upstream
// will not accept and the server cannot renew (ADR 0132).
//
// Its first job is attribution. From inside a discobox this failure looks
// exactly like Discobox failing to deliver a credential — the variable is set,
// the request went out, something on the other side said no — and the most
// useful sentence anybody can be handed is which credential was refused, and
// where. The second job is the remedy, and there are two. A harness's shared
// login is replaced the way it was made (ADR 26-11-02-152): sign in again with
// the harness's own login inside a discobox, and save that box's harness
// change, which replaces the refused login in place for every box on the
// harness. Any other secret is replaced by giving it a new value out here.
//
// The remedy names a fresh discobox, not the one the refusal showed in: a box
// can provoke a refusal, and a login typed into it is readable there. And the
// refusal is never taken down by a change the box reports (console.Inbox.Wanted).
//
// It outranks every other band, including a credential request (see
// console.NoticeKind), because an agent that has just taken a 401 asks for a credential
// — so the request and the refusal are one event, and the request is the half
// that cannot be usefully answered.

// rejectedKey is the band's key behind the leader: K, for the key that is not
// working — capital, because k and j move along the workspace tree. The
// letters this window already spends are elsewhere — g is the credential
// request.
var rejectedKey = bareKey("rejections")

// openRejectedMsg is the leader plus that key inside a pane.
type openRejectedMsg struct{}

// secretRejectionsLoadedMsg carries the refused credentials read on the poll.
type secretRejectionsLoadedMsg struct {
	rejections []console.SecretRejection
	err        error
}

// rejectionRemedyMsg carries what a press needs to act: the secret to
// replace. A harness's login needs nothing read to say how to replace it.
type rejectionRemedyMsg struct {
	rejection console.SecretRejection
	secret    *console.Secret
	err       error
}

// loadSecretRejections re-reads the refused credentials, one read at a time —
// the rule every polled listing here follows, so a server that has stopped
// answering collects one request rather than one a tick.
func (m *Model) loadSecretRejections() tea.Cmd {
	if !m.alertPoll.start(m.now()) {
		return nil
	}
	return func() tea.Msg {
		rejections, err := m.ds.SecretRejections(m.ctx)
		return secretRejectionsLoadedMsg{rejections: rejections, err: err}
	}
}

// setSecretRejections keeps what came back, oldest first: the credential that
// has been failing longest is the one somebody has been living with.
func (m *Model) setSecretRejections(rejections []console.SecretRejection) {
	console.SortRejections(rejections)
	had := m.bannerCost()
	m.rejections = rejections
	// The secrets screen says which credential does not work from the same
	// read: one poll answers the band and the rows, the way the credential
	// inbox already does.
	m.secrets.setRefused(console.RefusedSecrets(m.session, m.configServer(), rejections))
	m.pruneDismissed()
	// The band takes a row from the panes rather than adding one to the frame,
	// so a credential dying — or being replaced — resizes them.
	if m.bannerCost() != had {
		m.layout()
	}
}

// viewRejectedBanner is the workspace's line about a credential that does not
// work. Empty when this discobox has none.
//
// The chip does not throb, where the credential request's does. That animation
// is for an agent stopped mid-sentence with a person in the loop; this is a
// credential that has been dead for a while and will still be dead in a minute,
// and a second moving bar would spend the attention the first one is for.
func (m *Model) viewRejectedBanner(width int) string {
	// The listing's row, as bannerShowing reads it: the band that is chosen
	// and the band that is drawn must be about the same row, or the panes give
	// up two rows to a band that draws nothing.
	box := m.currentBox()
	n := m.dismissed.NoticeOf(m.inbox(), console.NoticeRejected, box)
	if n.Kind == console.NoNotice {
		return ""
	}
	st := m.st
	subject := st.attentionText.Render(n.Subject)
	if n.Detail != "" {
		subject += st.attentionHint.Render("  ·  ") + st.attentionText.Render(n.Detail)
	}
	// Last in the sentence, because it is the first thing a narrow window
	// should drop.
	if n.Behind != "" {
		subject += st.attentionHint.Render("  ·  " + n.Behind)
	}
	call := m.noticeChip(n, colAlertChip)
	return bannerRow(st, alertPalette, width, subject, call, m.leader()+" "+rejectedKey)
}

// openRejectedRemedy answers a press on the band: how to sign in again for a
// harness's shared login (console.SignInAgain), and the secret's card for
// anything else.
//
// Both open over the workspace rather than taking you to another screen, and
// both end back on the discobox you were watching: the session you were in the
// middle of is still there when it is done.
func (m *Model) openRejectedRemedy(box console.Sandbox) tea.Cmd {
	rejection, ok := m.dismissed.ShownRejection(m.inbox(), box)
	if !ok {
		return m.report(false, "%s", console.NothingRefusedHere)
	}
	if rejection.HarnessOwned() {
		title, body := console.SignInAgain(rejection)
		m.dialog = &dialog{kind: dlgMessage, title: title, body: body}
		return nil
	}
	m.busy = "reading " + rejection.Name() + "…"
	// The credential's own server, not the one the header names: the fix goes
	// where the credential lives (ADR 0131 §2).
	ctx, ds, session := m.ctx, m.ds, m.session
	return func() tea.Msg {
		secret, err := console.RefusedSecret(ctx, ds, session, rejection)
		if err != nil {
			return rejectionRemedyMsg{rejection: rejection, err: err}
		}
		return rejectionRemedyMsg{rejection: rejection, secret: &secret}
	}
}

// rejectionRemedy opens whichever remedy came back.
func (m *Model) rejectionRemedy(msg rejectionRemedyMsg) tea.Cmd {
	m.busy = ""
	switch {
	case msg.err != nil:
		return m.report(true, "%s: %v", msg.rejection.Name(), msg.err)
	case msg.secret != nil:
		return m.editSecretForm(m.session.ServerName(msg.rejection.Server), *msg.secret, &msg.rejection)
	}
	return nil
}
