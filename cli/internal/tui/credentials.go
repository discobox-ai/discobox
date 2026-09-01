package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/hostscope"

	tea "charm.land/bubbletea/v2"
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
// ask and the server does the rest, so the window and `discobox secret request
// approve` mint the same grant from the same checks.

// credentialsKey opens the request waiting on a discobox: on its row in the
// list, and behind the leader in the workspace, which is the same key map the
// two screens already share. C rather than g — g is the top of a list
// everywhere in this window — and capitalized because answering for a
// credential is not a keystroke to hit by accident.
const credentialsKey = "C"

// openCredentialsMsg is the leader plus that key inside a pane: the workspace's
// way to reach the request it is already telling you about.
type openCredentialsMsg struct{}

// credentialsLoadedMsg carries the pending requests read on the poll.
type credentialsLoadedMsg struct {
	requests []CredentialRequest
	err      error
}

// secretsLoadedMsg carries the secrets the picker offers, read when the dialog
// opens rather than polled: it is a list nothing changes behind your back.
type secretsLoadedMsg struct {
	requestID string
	secrets   []Secret
	err       error
}

// credentialAnsweredMsg reports an approval or a denial coming back.
type credentialAnsweredMsg struct {
	request  CredentialRequest
	approved bool
	err      error
}

func (m *Model) loadCredentialRequests() tea.Cmd {
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
func (m *Model) setCredentialRequests(requests []CredentialRequest) {
	m.allRequests = requests
	byBox := map[string][]CredentialRequest{}
	for _, req := range requests {
		if req.SandboxID == "" {
			continue
		}
		byBox[req.SandboxID] = append(byBox[req.SandboxID], req)
	}
	had := m.credentialBannerRows()
	m.requests = byBox
	m.list.setPending(byBox)
	m.requestRows.setAll(requests)
	// The banner takes a row from the panes rather than adding one to the
	// frame, so a request arriving — or being answered — resizes them.
	if m.credentialBannerRows() != had {
		m.layout()
	}
}

// pendingFor is the requests waiting on one discobox, oldest first: the one
// that has been waiting longest is the one to answer.
func (m *Model) pendingFor(sandboxID string) []CredentialRequest {
	out := append([]CredentialRequest(nil), m.requests[sandboxID]...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// openCredentialDialog asks about the oldest request on the discobox the
// workspace is showing.
func (m *Model) openCredentialDialog(sandboxID string) tea.Cmd {
	pending := m.pendingFor(sandboxID)
	if len(pending) == 0 {
		return m.report(false, "nothing waiting on this discobox")
	}
	return m.openCredentialRequest(pending[0])
}

// openCredentialRequest asks about one request, whichever discobox it came from
// and whether it came from one at all. It reads the secrets first, because the
// question it asks is which of them answers this.
func (m *Model) openCredentialRequest(req CredentialRequest) tea.Cmd {
	m.dialog = statusDialog("Credential request", "reading the project's secrets…")
	return func() tea.Msg {
		secrets, err := m.ds.Secrets(m.ctx)
		return secretsLoadedMsg{requestID: req.ID, secrets: secrets, err: err}
	}
}

// credentialRequestByID finds a request across every discobox, since a dialog
// answers the one it was opened on however the poll has moved on.
func (m *Model) credentialRequestByID(requestID string) (CredentialRequest, bool) {
	for _, req := range m.allRequests {
		if req.ID == requestID {
			return req, true
		}
	}
	return CredentialRequest{}, false
}

// askAboutCredential is the dialog itself: what was asked, and the ways to
// answer it.
func (m *Model) askAboutCredential(req CredentialRequest, secrets []Secret) tea.Cmd {
	items := make([]action, 0, len(secrets)+3)
	// Every secret is offered. A secret bound to a neighboring host is the
	// likeliest answer, not the least: a GitHub token is inferred as
	// api.github.com and asked for as github.com, and greying it out leaves the
	// one secret that plainly answers the request unpickable while an unrelated
	// one is offered. Choosing it asks about the binding instead — the remedy
	// the server names when it refuses the grant.
	for _, secret := range secretsForRequest(secrets, req.Host) {
		items = append(items, action{
			key:     "secret:" + secret.ID,
			label:   secret.Name,
			detail:  secretDetail(secret, req.Host),
			enabled: true,
		})
	}
	items = append(items,
		action{key: "new", press: "n", label: "New credential…", detail: "store it as a project secret and approve with it", enabled: true},
		action{key: "deny", press: "d", label: "Deny", detail: "answer no; the agent is waiting on one", enabled: true},
	)

	d := actionsDialog("Credential request", "", items, func(result string) tea.Cmd {
		switch {
		case result == "deny":
			return m.denyCredential(req)
		case result == "new":
			return m.askForNewCredential(req)
		case strings.HasPrefix(result, "secret:"):
			return m.chooseSecret(req, secrets, strings.TrimPrefix(result, "secret:"))
		}
		return nil
	})
	d.sections = credentialAsk(req, m.secrets.now())
	d.answerLabel = "which secret answers this?"
	d.footer = "enter approves with the highlighted secret · esc leaves it waiting"
	m.dialog = d
	return nil
}

// credentialAsk is the request as a person needs to read it: who is asking, for
// what, where it may go, and what they said they would do with it.
//
// It is a column of facts rather than a paragraph containing them. Every field
// here is one an approval turns on — the host it may be sent to most of all —
// and a fact that has to be found inside a sentence is a fact that gets skipped
// by somebody answering their fourth request of the morning.
func credentialAsk(req CredentialRequest, now time.Time) []section {
	asked := "the agent in this discobox"
	if !req.FromAgent() {
		// Not a question an agent composed: the proxy met a sentinel nothing
		// resolves, and the ask is what it saw rather than what anyone means
		// to do.
		asked = "the proxy — this discobox used a credential it has no grant for"
	}
	if age := since(req.Created, now); age != "" {
		asked = age + " ago by " + asked
	}
	fields := []field{{label: "credential", value: credentialName(req), tone: toneAccent}}
	if req.Type != "" {
		fields = append(fields, field{label: "kind", value: req.Type})
	}
	if req.EnvVar != "" {
		fields = append(fields, field{label: "delivered as", value: req.EnvVar})
	}
	if req.Host != "" {
		fields = append(fields, field{label: "may be sent to", value: req.Host, tone: toneAccent})
	}
	fields = append(fields, field{label: "asked", value: asked, tone: toneDim})
	sections := []section{{label: "asked for", fields: fields}}

	if len(req.Uses) == 0 && req.Justification == "" {
		return sections
	}
	what := section{label: "what for"}
	for _, use := range req.Uses {
		what.lines = append(what.lines, line{text: use, bullet: true})
	}
	if req.Justification != "" {
		if len(what.lines) > 0 {
			what.lines = append(what.lines, line{})
		}
		what.lines = append(what.lines, line{text: req.Justification, tone: toneDim})
	}
	return append(sections, what)
}

func credentialName(req CredentialRequest) string {
	if req.Name != "" {
		return req.Name
	}
	if req.EnvVar != "" {
		return req.EnvVar
	}
	return "a credential"
}

// secretsForRequest orders the secrets by how likely each is to be the answer:
// the host asked for, then a host of the same site, then the unbound ones,
// then the rest. Order is the whole of the opinion here — every one of them
// can be chosen.
func secretsForRequest(secrets []Secret, host string) []Secret {
	out := append([]Secret(nil), secrets...)
	sort.SliceStable(out, func(i, j int) bool {
		return hostRank(out[i].Host, host) < hostRank(out[j].Host, host)
	})
	return out
}

// hostRank orders the picker: the secret bound to exactly this host, then one
// whose binding covers it, then the unbound ones, then a binding that does not
// answer for this host at all and will be asked about.
func hostRank(bound, want string) int {
	bound, want = normalizeHostName(bound), normalizeHostName(want)
	switch {
	case bound == want && want != "":
		return 0
	case bound != "" && hostscope.Covers(bound, want):
		return 1
	case bound == "":
		return 2
	default:
		return 3
	}
}

func normalizeHostName(host string) string { return hostscope.Normalize(host) }

// secretDetail says what a secret is and, when its binding is not the host
// being asked about, what choosing it will mean.
func secretDetail(secret Secret, host string) string {
	bound := normalizeHostName(secret.Host)
	switch {
	case bound == "":
		return secret.Type + " · any host"
	case hostscope.Covers(bound, host):
		return secret.Type + " · " + secret.Host
	default:
		return secret.Type + " · bound to " + secret.Host + ", asks before using it here"
	}
}

// askForNewCredential collects a credential the project does not have yet. The
// value is typed masked, and goes straight to the server: this window never
// writes it anywhere, and the dialog holding it is replaced the moment it is
// answered.
func (m *Model) askForNewCredential(req CredentialRequest) tea.Cmd {
	fields := []field{{label: "stored as", value: credentialName(req), tone: toneAccent}}
	if req.Host != "" {
		fields = append(fields, field{label: "bound to", value: req.Host, tone: toneAccent})
	}
	d := inputDialog("New credential", "", "token", "", func(value string) tea.Cmd {
		value = strings.TrimSpace(value)
		if value == "" {
			return m.report(true, "no token entered; the request is still waiting")
		}
		return m.createAndApprove(req, value)
	})
	d.sections = []section{{label: "the new project secret", fields: fields}}
	d.answerLabel = "paste the token"
	d.footer = "it is stored encrypted and this request approved with it · Enter accepts · Esc cancels"
	d.input.EchoMode = 1 // textinput.EchoPassword: a credential is not drawn back.
	m.dialog = d
	return nil
}

func (m *Model) createAndApprove(req CredentialRequest, value string) tea.Cmd {
	m.dialog = statusDialog("Credential request", "storing the credential…")
	return func() tea.Msg {
		secret, err := m.ds.CreateSecret(m.ctx, NewSecret{
			Name:  credentialName(req),
			Type:  req.Type,
			Host:  req.Host,
			Value: SecretValue{Token: value},
		})
		if err != nil {
			return credentialAnsweredMsg{request: req, approved: true, err: err}
		}
		err = m.ds.ApproveCredentialRequest(m.ctx, Approval{RequestID: req.ID, SecretID: secret.ID})
		return credentialAnsweredMsg{request: req, approved: true, err: err}
	}
}

// chooseSecret approves with the chosen secret, or — when that secret is bound
// to another host — asks the one question standing between them first.
//
// The server refuses a grant that would point a host-bound secret somewhere
// else, and it is right to: that check is what stops an approval typo sending a
// real credential to a host it was never meant for. But when the two hosts are
// the same site, the mismatch is usually the inference being narrower than the
// credential, and the remedy is the secret's binding rather than the grant. So
// the window asks for exactly that, in the words the server would use.
func (m *Model) chooseSecret(req CredentialRequest, secrets []Secret, secretID string) tea.Cmd {
	var chosen Secret
	for _, secret := range secrets {
		if secret.ID == secretID {
			chosen = secret
		}
	}
	bound := normalizeHostName(chosen.Host)
	if bound == "" || hostscope.Covers(bound, req.Host) {
		return m.approveCredential(req, secretID)
	}
	// The binding that would cover both, when there is one: a credential asked
	// for at github.com and bound to api.github.com belongs to the site, and
	// the site covers what is beneath it. Otherwise the only way through is to
	// release the binding entirely.
	widened := commonParent(chosen.Host, req.Host)
	question := fmt.Sprintf("release %s's binding, so it may be sent anywhere a grant says?", chosen.Name)
	if widened != "" {
		question = fmt.Sprintf("bind %s to %s instead, so it covers both?", chosen.Name, widened)
	}
	d := confirmDialog("Bound to another host", "", func(string) tea.Cmd {
		return m.rebindAndApprove(req, chosen, widened)
	})
	d.sections = []section{{
		label: "the conflict",
		fields: []field{
			{label: "secret", value: chosen.Name},
			{label: "bound to", value: chosen.Host, tone: toneAccent},
			{label: "asked for", value: req.Host, tone: toneAccent},
		},
		lines: []line{{text: "a credential may only be sent to its own host and the hosts beneath it", tone: toneDim}},
	}}
	d.answerLabel = question
	d.footer = "no leaves the request waiting, and nothing is changed"
	// The costly answer is yes: it widens where a credential may be sent.
	d.defaultNo = true
	// Saying no goes back to the question rather than nowhere. A dialog that
	// closes onto the list, having done nothing and said nothing, is how a
	// deliberate "not that one" reads as the window ignoring the keypress.
	d.onCancel = func() tea.Cmd { return m.askAboutCredential(req, secrets) }
	m.dialog = d
	return nil
}

// commonParent is the scope that would cover both hosts, mirroring the server's
// own answer when it refuses: the deeper of the two when one is beneath the
// other, the shared site when they are siblings, and nothing when they share no
// site at all.
func commonParent(a, b string) string {
	return hostscope.CommonParent(a, b)
}

// rebindAndApprove moves the binding to what covers both — or releases it when
// nothing does — and answers the request with the secret.
func (m *Model) rebindAndApprove(req CredentialRequest, secret Secret, host string) tea.Cmd {
	what := "releasing " + secret.Name + "'s binding…"
	if host != "" {
		what = "binding " + secret.Name + " to " + host + "…"
	}
	m.dialog = statusDialog("Credential request", what)
	return func() tea.Msg {
		if err := m.ds.UpdateSecret(m.ctx, secret.ID, SecretUpdate{Host: &host}); err != nil {
			return credentialAnsweredMsg{request: req, approved: true, err: err}
		}
		err := m.ds.ApproveCredentialRequest(m.ctx, Approval{RequestID: req.ID, SecretID: secret.ID})
		return credentialAnsweredMsg{request: req, approved: true, err: err}
	}
}

func (m *Model) approveCredential(req CredentialRequest, secretID string) tea.Cmd {
	m.dialog = statusDialog("Credential request", "approving…")
	return func() tea.Msg {
		err := m.ds.ApproveCredentialRequest(m.ctx, Approval{RequestID: req.ID, SecretID: secretID})
		return credentialAnsweredMsg{request: req, approved: true, err: err}
	}
}

func (m *Model) denyCredential(req CredentialRequest) tea.Cmd {
	m.dialog = statusDialog("Credential request", "denying…")
	return func() tea.Msg {
		err := m.ds.DenyCredentialRequest(m.ctx, req.ID)
		return credentialAnsweredMsg{request: req, approved: false, err: err}
	}
}

// credentialAnswered closes the dialog and re-reads the inbox, so the mark and
// the banner go with the request that is no longer waiting.
//
// A failure replaces the dialog rather than dropping to the status line. The
// server's refusals here are instructions — "bound to api.github.com and cannot
// be granted for github.com; pick a secret for github.com, or clear the
// secret's host" — and a line that clears itself after four seconds, on a
// screen the request has just left, is one that reads as nothing happening.
func (m *Model) credentialAnswered(msg credentialAnsweredMsg) tea.Cmd {
	if msg.err != nil {
		verb := "Could not approve"
		if !msg.approved {
			verb = "Could not deny"
		}
		d := errorDialog(verb, fmt.Sprintf("%v", msg.err))
		d.sections = []section{credentialErrorSection(msg.request)}
		m.dialog = d
		return m.loadCredentialRequests()
	}
	m.dialog = nil
	if msg.approved {
		return tea.Batch(m.loadCredentialRequests(), m.report(false, "approved %s", credentialName(msg.request)))
	}
	return tea.Batch(m.loadCredentialRequests(), m.report(false, "denied %s", credentialName(msg.request)))
}

// credentialSpan is where the banner sits on screen, in absolute cells, both
// ends inclusive. It is recorded as the banner is drawn — the same way the tabs
// and the maximize controls record theirs — so a press can be matched against
// what is actually on the frame rather than against where it ought to be.
type credentialSpan struct {
	row        int
	start, end int
	live       bool
}

// credentialBannerRows is how many rows the workspace is spending on the
// banner: one while a request is waiting on the discobox it is showing, none
// otherwise.
//
// Every measurement of the workspace goes through it. The panes are sized from
// the rows left over, and the chrome's hit tests are offset by it, so a banner
// that appears does not push the boxes out from under the mouse.
func (m *Model) credentialBannerRows() int {
	if !m.inPanes() || len(m.requests[m.paneBox.ID]) == 0 {
		return 0
	}
	return 1
}

// credentialBannerAt reports whether a press landed on the banner.
func (m *Model) credentialBannerAt(x, y int) bool {
	s := m.credentials
	return s.live && y == s.row && x >= s.start && x <= s.end
}

// viewCredentialBanner is the workspace's line about a request waiting on the
// discobox it is showing. Empty when there is none.
//
// It is drawn in the error color and reversed, so it reads as a bar rather than
// a sentence among sentences: the row above it is the header you stop seeing
// after a minute, and this is the thing that must not be stopped seeing.
func (m *Model) viewCredentialBanner(width int) string {
	pending := m.pendingFor(m.paneBox.ID)
	if len(pending) == 0 {
		return ""
	}
	req := pending[0]
	what := credentialName(req)
	if req.Host != "" {
		what += " for " + req.Host
	}
	label := fmt.Sprintf(" ⚠  the agent is waiting on %s — %s %s or click here to answer ", what, m.leader(), credentialsKey)
	if len(pending) > 1 {
		label = fmt.Sprintf(" ⚠  %d credential requests waiting — %s %s or click here to answer ",
			len(pending), m.leader(), credentialsKey)
	}
	// The whole band is the target, not the words in it: a bar this size that
	// only answered on its text would be a bar that mostly does nothing.
	m.credentials = credentialSpan{row: 1, start: 0, end: max(m.width-1, 0), live: true}
	return m.st.attention.Render(padANSI(label, width))
}

// credentialErrorSection says which request is still waiting, under the
// server's own wording of the refusal: that wording is the half that says what
// to do next, so it is left whole as the dialog's body and this stands under it.
func credentialErrorSection(req CredentialRequest) section {
	fields := []field{{label: "credential", value: credentialName(req), tone: toneAccent}}
	if req.Host != "" {
		fields = append(fields, field{label: "for", value: req.Host, tone: toneAccent})
	}
	return section{label: "still waiting", fields: fields}
}
