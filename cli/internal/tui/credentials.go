package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/console"

	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
)

// The credential inbox: what the window does about a request waiting on a
// person (ADR 0031).
//
// Two rules shape it. The list *marks* a discobox with a request and never
// takes the screen for one — an agent can ask at any moment, and a window that
// interrupts a sentence being typed teaches you to answer without reading. The
// workspace, where you are already looking at that one discobox, says it
// loudly and puts the answer one key away.
//
// Nothing here decides anything: the dialog collects which secret answers the
// ask and for how long, and the server does the rest, so the window and
// `discobox secret request approve` make the same binding from the same checks.

// credentialsKey opens the request waiting on a discobox, on its row in the
// list and on the secrets screen. C rather than g, because g is the top of a
// list on both of those screens, and capitalized because answering for a
// credential is not a keystroke to hit by accident.
var credentialsKey = bareKey("requests")

// credentialsLeaderKey is the same thing behind the leader in the workspace,
// where the banner names it.
//
// It is the one place the workspace does not carry the list's key on the list's
// letter. The leader's c opens another terminal, and a shift away from it sat a
// dialog that binds a credential — two commands one modifier apart, one of
// them consequential and reached for in a hurry, on a screen whose banner is
// asking you to hurry. There is no list here for g to be the top of, so g is
// free, and it is the letter of the thing it does.
var credentialsLeaderKey = leaderKey("requests")

// openCredentialsMsg is the leader plus that key inside a pane: the workspace's
// way to reach the request it is already telling you about.
type openCredentialsMsg struct{}

// credentialsLoadedMsg carries the pending requests read on the poll.
type credentialsLoadedMsg struct {
	requests []console.CredentialRequest
	err      error
}

// secretsLoadedMsg carries the secrets the picker offers, read when the dialog
// opens rather than polled: it is a list nothing changes behind your back.
type secretsLoadedMsg struct {
	requestID string
	secrets   []console.Secret
	// held is what applies in the use part's variable now, read with the
	// secrets: a variable a secret already holds is answered with that one.
	held []console.HeldGrant
	err  error
}

// standingLoadedMsg carries the approval with what it would replace read
// (console.ReadStanding), once the secret is chosen.
type standingLoadedMsg struct {
	wait     *dialog
	approval console.RequestApproval
	err      error
}

// credentialAnsweredMsg reports an approval or a denial coming back. ttl is
// how long the binding the approval made lasts, so the report says how long
// the credential is good for rather than only that it was handed over.
type credentialAnsweredMsg struct {
	request  console.CredentialRequest
	approved bool
	ttl      time.Duration
	err      error
	// applied is what had already been done to the project's secrets when an
	// approval failed: a credential typed in and stored ahead of the binding
	// that then did not happen. The failure says so rather than leave it
	// behind in silence.
	applied []string
}

func (m *Model) loadCredentialRequests() tea.Cmd {
	// One at a time, like the listing and the machine readout it is polled
	// beside: a server that is not answering collects one request rather than
	// one every tick for as long as the window is open. An approval's own
	// re-read is not lost to that — a read asked for while one is out goes as
	// soon as that one lands (credentialsLoadedMsg) — because what it is for
	// is taking the request just answered out of the inbox.
	if !m.inboxPoll.start(m.now()) {
		return nil
	}
	return func() tea.Msg {
		requests, err := m.ds.CredentialRequests(m.ctx)
		return credentialsLoadedMsg{requests: requests, err: err}
	}
}

// setCredentialRequests keeps the pending requests twice over: all of them, for
// the secrets screen, and indexed by discobox, for the row marks.
//
// A request no discobox owns — one made from the CLI, or by a person — has no
// row to mark and no workspace to raise it in, but it is still waiting on
// somebody. Dropping it on the way in made it invisible everywhere, including
// on the one screen an operator is looking at.
func (m *Model) setCredentialRequests(requests []console.CredentialRequest) {
	m.allRequests = requests
	byBox := map[string][]console.CredentialRequest{}
	for _, req := range requests {
		if req.SandboxID == "" {
			continue
		}
		byBox[req.SandboxID] = append(byBox[req.SandboxID], req)
	}
	had := m.bannerCost()
	m.requests = byBox
	m.list.SetPending(byBox)
	m.syncRequestRows()
	m.pruneDismissed()
	// The band takes a row from the panes rather than adding one to the
	// frame, so a request arriving — or being answered — resizes them.
	if m.bannerCost() != had {
		m.layout()
	}
}

// syncRequestRows puts the requests waiting on the secrets screen's server in
// its table (ADR 0131 §2): a request is answered with one of its own server's
// secrets, so the table under another server's secrets offers none of it.
func (m *Model) syncRequestRows() {
	rows := console.RequestsOn(m.session, m.configServer(), m.allRequests)
	had := len(m.requestRows.all)
	m.requestRows.setAll(rows)
	if len(rows) == 0 && m.table == tableRequests {
		m.table = tableSecrets
	}
	// The table is as tall as its rows, up to its share of the screen, and
	// layout is where that is counted. A poll is not a key: without this the
	// requests it brought waited behind one row for the next one pressed.
	if len(rows) != had {
		m.layout()
	}
}

// openCredentialDialog asks about the oldest request on the discobox the
// workspace is showing.
func (m *Model) openCredentialDialog(box console.Sandbox) tea.Cmd {
	pending := console.RequestsFor(m.session, box, m.requests[box.ID])
	if len(pending) == 0 {
		return m.report(false, "%s", console.NothingOnThisBox)
	}
	return m.openCredentialRequest(pending[0])
}

// openCredentialRequest asks about one request, whichever discobox it came from
// and whether it came from one at all. It reads the secrets first, because the
// question it asks is which of them answers this, and what holds the variable
// the use part names, because a secret that already does is the only answer
// the server takes (console.HeldBy).
func (m *Model) openCredentialRequest(req console.CredentialRequest) tea.Cmd {
	if req.APIAccess {
		return m.askAboutAPIAccess(req)
	}
	if req.Adoption != nil {
		return m.askAboutAdoption(req)
	}
	// A trust request is answered with a pin, not a secret: there is nothing
	// to read first.
	if req.Trust != nil {
		return m.askAboutTrust(req)
	}
	m.dialog = statusDialog("Credential request", console.ReadingSecrets)
	return func() tea.Msg {
		secrets, held, err := console.RequestSecrets(m.ctx, m.ds, req)
		return secretsLoadedMsg{requestID: req.ID, secrets: secrets, held: held, err: err}
	}
}

// credentialRequestByID finds a request across every discobox, since a dialog
// answers the one it was opened on however the poll has moved on.
func (m *Model) credentialRequestByID(requestID string) (console.CredentialRequest, bool) {
	for _, req := range m.allRequests {
		if req.ID == requestID {
			return req, true
		}
	}
	return console.CredentialRequest{}, false
}

// askAboutCredential opens the first part card, or the token refresh card.
// held is what applies in the use part's variable now.
func (m *Model) askAboutCredential(req console.CredentialRequest, secrets []console.Secret, held []console.HeldGrant) tea.Cmd {
	if req.Refresh != nil {
		return m.askAboutRefresh(req, secrets)
	}
	a := console.NewRequestApproval(req, secrets, held)
	return m.approvalPart(a, a.FirstPartDelegates())
}

// showRawRequest is the request as the server sent it, as JSON highlighted
// the way a code block in a comment is. back is the request card.
func (m *Model) showRawRequest(req console.CredentialRequest, back func() tea.Cmd) tea.Cmd {
	rendered := req.Raw
	if r, err := glamour.NewTermRenderer(glamour.WithStyles(markdownStyle(m.st)), glamour.WithWordWrap(0)); err == nil {
		if out, err := r.Render("```json\n" + req.Raw + "\n```"); err == nil {
			rendered = out
		}
	}
	plain := func(*styles) lipgloss.Style { return lipgloss.NewStyle() }
	var lines []line
	for _, row := range strings.Split(strings.Trim(rendered, "\n"), "\n") {
		lines = append(lines, line{spans: []span{{text: strings.TrimRight(row, " "), style: plain}}})
	}
	q := console.RawRequest(req)
	d := actionsDialog(q.Title, "", []action{{key: q.Choices[0].Key, press: "enter", label: q.Choices[0].Label, enabled: true}},
		func(string) tea.Cmd { return back() })
	d.titleRight = q.Subject
	d.sections = []section{{label: "as the server sent it", lines: lines}}
	d.keys = []hint{pressing("enter or esc goes back", "esc")}
	d.onCancel = back
	m.dialog = d
	return nil
}

// askNewCredentialForm stores a credential the project does not have yet and
// answers the request with it: the secrets screen's form, filled from the
// request (console.RequestApproval.NewCredentialAsk). It comes after the
// parts, so the token typed into it is never held by a card that can be gone
// back to: Esc here is the request, still waiting.
func (m *Model) askNewCredentialForm(a console.RequestApproval) tea.Cmd {
	q := console.NewSecretForm(a.NewCredentialAsk())
	d := formDialog(console.NewCredentialTitle, q, func(secret console.NewSecret) tea.Cmd { return m.createAndApprove(a, secret) })
	d.sections = detailSections(a.NewCredentialCard())
	// Esc second: a key line is cut from its tail, and the way back — the
	// request still waiting — is the one thing it must always say.
	d.keys = []hint{pressing("enter stores and approves", "enter"), pressing("esc back, nothing stored", "esc"), says("↑↓ moves"), says("←→ chooses")}
	d.onCancel = func() tea.Cmd { return m.askAboutCredential(a.Request, a.Secrets, a.Held) }
	m.dialog = d
	return nil
}

// createAndApprove stores the new credential and answers with it, for the
// lifetime chosen (console.ApproveWithNewSecret). A refusal after the secret
// was stored says so: the secret stays, and the request still waits.
func (m *Model) createAndApprove(a console.RequestApproval, secret console.NewSecret) tea.Cmd {
	m.dialog = statusDialog("Credential request", console.StoringNew(secret))
	return func() tea.Msg {
		_, applied, err := console.ApproveWithNewSecret(m.ctx, m.ds, a, secret)
		return credentialAnsweredMsg{request: a.Request, approved: true, ttl: a.TTL, err: err, applied: applied}
	}
}

// chooseSecret reads what the approval would replace before the review, so
// the review can say it.
func (m *Model) chooseSecret(a console.RequestApproval) tea.Cmd {
	if a.Request.SandboxID == "" {
		a, _ = console.ReadStanding(m.ctx, m.ds, a)
		return m.confirmCredentialApproval(a)
	}
	wait := statusDialog("Credential request", a.ReadingStanding())
	m.dialog = wait
	return func() tea.Msg {
		read, err := console.ReadStanding(m.ctx, m.ds, a)
		return standingLoadedMsg{wait: wait, approval: read, err: err}
	}
}

// standingLoaded opens the review with what the approval replaces.
func (m *Model) standingLoaded(msg standingLoadedMsg) tea.Cmd {
	if m.dialog != msg.wait {
		return nil
	}
	if msg.err != nil {
		m.dialog = errorDialog("Credential request", msg.err.Error())
		return nil
	}
	return m.confirmCredentialApproval(msg.approval)
}

// finishApproval makes the binding (console.ApproveRequest), its lifetime
// always said.
func (m *Model) finishApproval(a console.RequestApproval) tea.Cmd {
	m.dialog = statusDialog("Credential request", a.Approving())
	return func() tea.Msg {
		_, err := console.ApproveRequest(m.ctx, m.ds, a)
		return credentialAnsweredMsg{request: a.Request, approved: true, ttl: a.TTL, err: err}
	}
}

// denyCredential answers a request no (console.DenyRequest).
func (m *Model) denyCredential(req console.CredentialRequest) tea.Cmd {
	m.dialog = statusDialog(console.Denying(req))
	return func() tea.Msg {
		_, err := console.DenyRequest(m.ctx, m.ds, req)
		return credentialAnsweredMsg{request: req, approved: false, err: err}
	}
}

// credentialAnswered closes the dialog and re-reads the inbox, so the mark and
// the banner go with the request that is no longer waiting.
//
// A failure replaces the dialog rather than dropping to the status line. The
// server's refusals here are instructions — "GH_TOKEN already holds another
// credential in this discobox; approve the request with that secret, or ask
// for another variable" — and a line that clears itself after four seconds, on
// a screen the request has just left, is one that reads as nothing happening.
func (m *Model) credentialAnswered(msg credentialAnsweredMsg) tea.Cmd {
	if msg.err != nil {
		d := errorDialog(console.AnswerFailed(msg.approved), fmt.Sprintf("%v", msg.err))
		d.sections = detailSections(append([]console.DetailSection{console.StillWaiting(msg.request)}, console.AppliedBeforeFailing(msg.applied)...))
		m.dialog = d
		return m.loadCredentialRequests()
	}
	m.dialog = nil
	return tea.Batch(m.loadCredentialRequests(), m.report(false, "%s", console.Answered(msg.request, msg.approved, msg.ttl)))
}

// viewCredentialBanner is the workspace's line about a request waiting on the
// discobox it is showing. Empty when there is none.
//
// It is a band across the window rather than a sentence among sentences: the
// row above it is the header you stop seeing after a minute, and this is the
// thing that must not be stopped seeing — which is also why the chip in the
// middle of it throbs, and why it is the only thing in the window that moves
// without something having happened. See banner.go for the bar and its beat.
func (m *Model) viewCredentialBanner(width int) string {
	n := m.dismissed.NoticeOf(m.inbox(), console.NoticeRequest, m.Box)
	if n.Kind == console.NoNotice {
		return ""
	}
	st := m.st
	body := st.attentionText.Render(n.Subject) +
		st.attentionHint.Render("  ·  ") + st.attentionText.Render(n.Detail)
	call := m.noticeChip(n, colAlertChip)
	return bannerRow(st, alertPalette, width, body, call, m.leader()+" "+credentialsLeaderKey)
}
