package tui

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/lifetime"
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
// ask and for how long, and the server does the rest, so the window and
// `discobox secret request approve` mint the same grant from the same checks.

// credentialsKey opens the request waiting on a discobox, on its row in the
// list and on the secrets screen. C rather than g, because g is the top of a
// list on both of those screens, and capitalized because answering for a
// credential is not a keystroke to hit by accident.
const credentialsKey = "C"

// credentialsLeaderKey is the same thing behind the leader in the workspace,
// where the banner names it.
//
// It is the one place the workspace does not carry the list's key on the list's
// letter. The leader's c opens another terminal, and a shift away from it sat a
// dialog that grants a credential — two commands one modifier apart, one of
// them consequential and reached for in a hurry, on a screen whose banner is
// asking you to hurry. There is no list here for g to be the top of, so g is
// free, and it is the letter of the thing it does.
const credentialsLeaderKey = "g"

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

// credentialAnsweredMsg reports an approval or a denial coming back. ttl is
// what the approval granted, so the report says how long the credential is
// good for rather than only that it was handed over.
type credentialAnsweredMsg struct {
	request  CredentialRequest
	approved bool
	ttl      time.Duration
	err      error
	// applied is what had already been done to the project's secrets when an
	// approval failed: a limit raised, a binding moved, a credential stored.
	// Each was agreed to as part of a grant that then did not happen, so the
	// failure has to say it rather than leave a changed credential behind in
	// silence.
	applied []string
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
	had := m.bannerCost()
	m.requests = byBox
	m.list.setPending(byBox)
	m.requestRows.setAll(requests)
	// The band takes a row from the panes rather than adding one to the
	// frame, so a request arriving — or being answered — resizes them.
	if m.bannerCost() != had {
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
// answer it. Choosing a secret, or a new credential, is the first of two steps;
// the second is how long the grant lives (askLifetime).
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
		a := approval{req: req, secrets: secrets}
		switch {
		case result == "deny":
			return m.denyCredential(req)
		case result == "new":
			a.fresh = true
			return m.askLifetime(a, m.toRequest(a))
		case strings.HasPrefix(result, "secret:"):
			return m.chooseSecret(a, strings.TrimPrefix(result, "secret:"))
		}
		return nil
	})
	d.sections = credentialAsk(req, m.secrets.now())
	d.answerLabel = "which secret answers this?"
	d.keys = []hint{pressing("enter chooses the highlighted secret", "enter"), pressing("esc leaves it waiting", "esc")}
	m.dialog = d
	return nil
}

// lifetimeCustom is the lifetime menu's way out to one the presets do not
// have. The presets' own keys are their lifetimes in seconds, so reading one
// back is parsing what was chosen rather than remembering which row meant what.
const lifetimeCustom = "custom"

// askLifetime is the second step of every approval: how long the grant lives.
//
// It is a step of its own, and a required one, because it is the half of an
// approval nobody thinks to look for. Leaving the lifetime out of the call
// asks the server for the credential's own ceiling — a ceiling most
// credentials do not have — so a window that did would hand out permanent
// credentials without ever saying the word. A card that has to be answered is
// one that gets read. It opens on lifetime.Default, an hour.
//
// back is the dialog before this one, which Esc returns to: the request card,
// or the binding question when the secret chosen raised one.
func (m *Model) askLifetime(a approval, back func() tea.Cmd) tea.Cmd {
	items := make([]action, 0, len(lifetime.Presets)+1)
	for _, d := range lifetime.Presets {
		items = append(items, action{
			key:     strconv.FormatInt(lifetime.Seconds(d), 10),
			label:   lifetime.Label(d),
			detail:  lifetimeDetail(a.secret, d),
			enabled: true,
		})
	}
	items = append(items, action{key: lifetimeCustom, label: "custom…", detail: "type one: 90m, 3d, 6mo", enabled: true})

	again := func() tea.Cmd { return m.askLifetime(a, back) }
	opens := strconv.FormatInt(lifetime.Seconds(lifetime.Default), 10)
	d := actionsDialog("How long?", "", items, func(result string) tea.Cmd {
		if result == lifetimeCustom {
			return m.askCustomLifetime(a, again, "")
		}
		seconds, err := strconv.ParseInt(result, 10, 64)
		if err != nil {
			return nil
		}
		next := a
		next.ttl = time.Duration(seconds) * time.Second
		return m.lifetimeChosen(next, again)
	})
	// It opens on the default, whatever order the presets are offered in, so
	// Enter is the answer nobody had to choose.
	for i, item := range items {
		if item.key == opens {
			d.cursor = i
		}
	}
	d.sections = []section{grantSection(a)}
	d.answerLabel = "how long may " + credentialName(a.req) + " be used for?"
	d.footer = "when it lapses the credential stops working, and the agent asks again"
	d.keys = []hint{pressing("enter chooses the highlighted lifetime", "enter"), pressing("esc goes back", "esc")}
	d.onCancel = back
	m.dialog = d
	return nil
}

// lifetimeDetail says what a lifetime means beyond its name: that forever never
// lapses, and that one longer than the credential allows will be asked about —
// said before it is chosen rather than discovered after.
func lifetimeDetail(secret Secret, d time.Duration) string {
	if limit := secret.MaxTTL; limit > 0 && (d <= 0 || d > limit) {
		return "longer than " + secret.Name + " allows (" + lifetime.Label(limit) + "), asks first"
	}
	if d <= 0 {
		return "never expires"
	}
	return ""
}

// askCustomLifetime takes a lifetime the presets do not offer. refused is the
// last answer's refusal, which the card keeps rather than closing onto the
// screen behind it: what is wrong with "3 weeks-ish" has to be said where it
// was typed. back is the lifetime menu.
func (m *Model) askCustomLifetime(a approval, back func() tea.Cmd, refused string) tea.Cmd {
	d := inputDialog("How long?", "", "e.g. 90m, 3d, 6mo", "", func(value string) tea.Cmd {
		chosen, err := lifetime.Parse(value)
		if err != nil {
			return m.askCustomLifetime(a, back, fmt.Sprintf("%v", err))
		}
		next := a
		next.ttl = chosen
		return m.lifetimeChosen(next, func() tea.Cmd { return m.askCustomLifetime(a, back, "") })
	})
	d.sections = []section{grantSection(a)}
	d.answerLabel = "how long may " + credentialName(a.req) + " be used for?"
	d.footer = "1h, 90m, 3d, 2w, 1mo — or forever"
	d.emphasis = refused
	d.onCancel = back
	m.dialog = d
	return nil
}

// grantSection is what the lifetime is being chosen for, so the card asking
// "how long" says how long *what*: the credential, what answers it, and where
// it may be sent.
func grantSection(a approval) section {
	answered := a.secret.Name
	if a.fresh {
		answered = "a new credential, stored as " + credentialName(a.req)
	}
	fields := []field{
		{label: "credential", value: credentialName(a.req), tone: toneAccent},
		{label: "answered with", value: answered},
	}
	if a.req.Host != "" {
		fields = append(fields, field{label: "may be sent to", value: a.req.Host, tone: toneAccent})
	}
	return section{label: "the grant", fields: fields}
}

// lifetimeChosen is where the two kinds of answer part: a new credential still
// needs its value, and an existing one may need its limit raised. back is the
// dialog the lifetime was chosen on, which either of them returns to.
func (m *Model) lifetimeChosen(a approval, back func() tea.Cmd) tea.Cmd {
	if a.fresh {
		return m.askForNewCredential(a, back)
	}
	return m.confirmGrantLimit(a, back)
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
// being asked about, what choosing it will mean. A credential that caps how
// long its grants may live says so here, beside the row that would be asked
// about on the way through if the lifetime on the card is longer.
func secretDetail(secret Secret, host string) string {
	bound := normalizeHostName(secret.Host)
	detail := secret.Type + " · bound to " + secret.Host + ", asks before using it here"
	switch {
	case bound == "":
		detail = secret.Type + " · any host"
	case hostscope.Covers(bound, host):
		detail = secret.Type + " · " + secret.Host
	}
	if secret.MaxTTL > 0 {
		detail += " · at most " + lifetime.Label(secret.MaxTTL)
	}
	return detail
}

// askForNewCredential collects a credential the project does not have yet. The
// value is typed masked, and goes straight to the server: this window never
// writes it anywhere, and the dialog holding it is replaced the moment it is
// answered.
//
// It comes after the lifetime rather than before it, so that going back never
// has to hold a token that was already typed: Esc here returns to the lifetime,
// and nothing typed survives it.
func (m *Model) askForNewCredential(a approval, back func() tea.Cmd) tea.Cmd {
	req := a.req
	fields := []field{{label: "stored as", value: credentialName(req), tone: toneAccent}}
	if req.Host != "" {
		fields = append(fields, field{label: "bound to", value: req.Host, tone: toneAccent})
	}
	fields = append(fields, field{label: "granted for", value: lifetime.Label(a.ttl), tone: toneAccent})
	d := inputDialog("New credential", "", "token", "", func(value string) tea.Cmd {
		value = strings.TrimSpace(value)
		if value == "" {
			return m.report(true, "no token entered; the request is still waiting")
		}
		return m.createAndApprove(a, value)
	})
	d.sections = []section{{label: "the new project secret", fields: fields}}
	d.answerLabel = "paste the token"
	d.footer = "it is stored encrypted and this request approved with it"
	d.keys = []hint{pressing("Enter accepts", "enter"), pressing("Esc goes back", "esc")}
	d.input.EchoMode = 1 // textinput.EchoPassword: a credential is not drawn back.
	d.onCancel = back
	m.dialog = d
	return nil
}

// createAndApprove stores the typed credential and answers with it. A secret
// stored here has no grant limit of its own, so the lifetime chosen is the
// whole of what bounds the grant, and there is nothing to ask about on the way
// through.
func (m *Model) createAndApprove(a approval, value string) tea.Cmd {
	req, ttl := a.req, a.ttl
	m.dialog = statusDialog("Credential request", "storing the credential…")
	return func() tea.Msg {
		secret, err := m.ds.CreateSecret(m.ctx, NewSecret{
			Name:  credentialName(req),
			Type:  req.Type,
			Host:  req.Host,
			Value: SecretValue{Token: value},
		})
		if err != nil {
			return credentialAnsweredMsg{request: req, approved: true, ttl: ttl, err: err}
		}
		err = m.ds.ApproveCredentialRequest(m.ctx, Approval{
			RequestID:  req.ID,
			SecretID:   secret.ID,
			TTLSeconds: lifetime.Seconds(ttl),
		})
		msg := credentialAnsweredMsg{request: req, approved: true, ttl: ttl, err: err}
		if err != nil {
			msg.applied = []string{"the token was stored as the project secret " + secret.Name}
		}
		return msg
	}
}

// approval is what answering a request will do, gathered one step at a time:
// the secret chosen, how long its grant lives, and the one change to the secret
// those two need first.
//
// It is carried whole, and copied rather than changed at each step, because
// every step can be gone back from: the dialog before one is rebuilt from the
// approval as it stood when that dialog was asked, and has to find it that way.
type approval struct {
	req     CredentialRequest
	secrets []Secret
	secret  Secret
	// fresh is an answer typed in on the spot rather than a secret the project
	// holds: secret is empty, and the value is asked for after the lifetime.
	fresh bool
	ttl   time.Duration
	// update is what must change on the secret before the grant can be minted.
	// One update, not one per question: a card whose rows were saved by a call
	// each half-applies when the second fails.
	update SecretUpdate
	// changes is what that update does, in the words the status dialog says
	// while it happens.
	changes []string
}

// toRequest is the way back to the request card: where Esc goes from the first
// step after it.
func (m *Model) toRequest(a approval) func() tea.Cmd {
	return func() tea.Cmd { return m.askAboutCredential(a.req, a.secrets) }
}

// chooseSecret is the first step's answer: the secret, then whatever it raises
// — a binding pointing somewhere else — then how long.
func (m *Model) chooseSecret(a approval, secretID string) tea.Cmd {
	for _, secret := range a.secrets {
		if secret.ID == secretID {
			a.secret = secret
		}
	}
	return m.confirmGrantHost(a)
}

// confirmGrantHost asks the one question standing between a secret bound to
// another host and the request in front of it, before the lifetime: it is a
// question about the secret just chosen, and is asked while that choice is the
// thing being looked at.
//
// The server refuses a grant that would point a host-bound secret somewhere
// else, and it is right to: that check is what stops an approval typo sending a
// real credential to a host it was never meant for. But when the two hosts are
// the same site, the mismatch is usually the inference being narrower than the
// credential, and the remedy is the secret's binding rather than the grant. So
// the window asks for exactly that, in the words the server would use.
func (m *Model) confirmGrantHost(a approval) tea.Cmd {
	bound := normalizeHostName(a.secret.Host)
	if bound == "" || hostscope.Covers(bound, a.req.Host) {
		return m.askLifetime(a, m.toRequest(a))
	}
	// The binding that would cover both, when there is one: a credential asked
	// for at github.com and bound to api.github.com belongs to the site, and
	// the site covers what is beneath it. Otherwise the only way through is to
	// release the binding entirely.
	widened := commonParent(a.secret.Host, a.req.Host)
	question := fmt.Sprintf("release %s's binding, so it may be sent anywhere a grant says?", a.secret.Name)
	if widened != "" {
		question = fmt.Sprintf("bind %s to %s instead, so it covers both?", a.secret.Name, widened)
	}
	d := confirmDialog("Bound to another host", "", func(string) tea.Cmd {
		next := a
		host := widened
		next.update.Host = &host
		what := "releasing " + a.secret.Name + "'s binding"
		if host != "" {
			what = "binding " + a.secret.Name + " to " + host
		}
		next.changes = append(slices.Clip(a.changes), what)
		return m.askLifetime(next, func() tea.Cmd { return m.confirmGrantHost(a) })
	})
	d.sections = []section{{
		label: "the conflict",
		fields: []field{
			{label: "secret", value: a.secret.Name},
			{label: "bound to", value: a.secret.Host, tone: toneAccent},
			{label: "asked for", value: a.req.Host, tone: toneAccent},
		},
		lines: []line{{text: "a credential may only be sent to its own host and the hosts beneath it", tone: toneDim}},
	}}
	d.answerLabel = question
	d.footer = "no goes back to the request, and nothing is changed"
	// The costly answer is yes: it widens where a credential may be sent.
	d.defaultNo = true
	// Saying no goes back to the question rather than nowhere. A dialog that
	// closes onto the list, having done nothing and said nothing, is how a
	// deliberate "not that one" reads as the window ignoring the keypress.
	d.onCancel = m.toRequest(a)
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

// confirmGrantLimit asks about a lifetime the credential does not allow.
//
// The limit is the one place a credential says how long consent to it may
// last, and the server refuses a grant that would outlive it. Raising it is a
// decision about the credential rather than about this one grant, so it is
// asked for as one — and No goes back to where the lifetime was chosen, where a
// shorter one can be picked instead.
func (m *Model) confirmGrantLimit(a approval, back func() tea.Cmd) tea.Cmd {
	limit := a.secret.MaxTTL
	if limit <= 0 || (a.ttl > 0 && a.ttl <= limit) {
		return m.finishApproval(a)
	}
	d := confirmDialog("Longer than the credential allows", "", func(string) tea.Cmd {
		next := a
		seconds := lifetime.Seconds(a.ttl)
		next.update.MaxTTLSeconds = &seconds
		next.changes = append(slices.Clip(a.changes), "raising "+a.secret.Name+"'s limit to "+lifetime.Label(a.ttl))
		return m.finishApproval(next)
	})
	d.sections = []section{{
		label: "the conflict",
		fields: []field{
			{label: "secret", value: a.secret.Name},
			{label: "grants last at most", value: lifetime.Label(limit), tone: toneAccent},
			{label: "asked for", value: lifetime.Label(a.ttl), tone: toneAccent},
		},
		lines: []line{{text: "the limit is how long consent to this credential may last, and it holds for every grant minted on it from now on", tone: toneDim}},
	}}
	d.answerLabel = fmt.Sprintf("raise %s's limit to %s?", a.secret.Name, lifetime.Label(a.ttl))
	d.footer = "no goes back, where a shorter lifetime can be chosen instead"
	// The costly answer is yes: it lengthens how long every grant on this
	// credential may live.
	d.defaultNo = true
	d.onCancel = back
	m.dialog = d
	return nil
}

// finishApproval applies whatever the questions agreed to and mints the grant.
//
// The lifetime rides on the approval itself rather than being left out for the
// server to default: the default is the secret's own ceiling, which most
// credentials do not have, and a window that stayed quiet about it handed out
// permanent credentials without ever saying the word.
func (m *Model) finishApproval(a approval) tea.Cmd {
	what := "approving for " + lifetime.Label(a.ttl) + "…"
	if len(a.changes) > 0 {
		what = strings.Join(a.changes, ", then ") + ", then approving…"
	}
	m.dialog = statusDialog("Credential request", what)
	changing := a.update.Host != nil || a.update.MaxTTLSeconds != nil
	return func() tea.Msg {
		if changing {
			if err := m.ds.UpdateSecret(m.ctx, a.secret.ID, a.update); err != nil {
				return credentialAnsweredMsg{request: a.req, approved: true, ttl: a.ttl, err: err}
			}
		}
		err := m.ds.ApproveCredentialRequest(m.ctx, Approval{
			RequestID:  a.req.ID,
			SecretID:   a.secret.ID,
			TTLSeconds: lifetime.Seconds(a.ttl),
		})
		msg := credentialAnsweredMsg{request: a.req, approved: true, ttl: a.ttl, err: err}
		if err != nil && changing {
			msg.applied = appliedChanges(a)
		}
		return msg
	}
}

// appliedChanges says what an approval's secret update did, as done rather than
// being done. It is not put back when the approval then fails: undoing one
// write with another that can fail too leaves a state nobody described, while
// saying what stands leaves it to the person who agreed to it.
func appliedChanges(a approval) []string {
	var done []string
	if host := a.update.Host; host != nil {
		if *host == "" {
			done = append(done, a.secret.Name+"'s binding was released: it may be sent anywhere a grant says")
		} else {
			done = append(done, a.secret.Name+" is now bound to "+*host)
		}
	}
	if seconds := a.update.MaxTTLSeconds; seconds != nil {
		limit := time.Duration(*seconds) * time.Second
		if limit <= 0 {
			done = append(done, a.secret.Name+"'s limit was lifted: grants on it may now live forever")
		} else {
			done = append(done, a.secret.Name+"'s limit was raised: grants on it may now live "+lifetime.Label(limit))
		}
	}
	return done
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
		if len(msg.applied) > 0 {
			done := section{label: "already done, and still in place"}
			for _, change := range msg.applied {
				done.lines = append(done.lines, line{text: change, bullet: true, tone: toneAlert})
			}
			done.lines = append(done.lines, line{}, line{text: "they were agreed to for this approval; the secrets screen (F4) puts them back if the grant is not coming", tone: toneDim})
			d.sections = append(d.sections, done)
		}
		m.dialog = d
		return m.loadCredentialRequests()
	}
	m.dialog = nil
	if msg.approved {
		// The lifetime is in the report because it is the half of what was
		// just handed out that nothing else on screen will say again.
		return tea.Batch(m.loadCredentialRequests(),
			m.report(false, "approved %s for %s", credentialName(msg.request), lifetime.Label(msg.ttl)))
	}
	return tea.Batch(m.loadCredentialRequests(), m.report(false, "denied %s", credentialName(msg.request)))
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
	pending := m.pendingFor(m.paneBox.ID)
	if len(pending) == 0 {
		return ""
	}
	st := m.st
	subject := credentialName(pending[0])
	if host := pending[0].Host; host != "" {
		subject += " for " + host
	}
	what := "credential request"
	if len(pending) > 1 {
		what, subject = plural(len(pending), "credential request", "credential requests"), "oldest: "+subject
	}
	body := st.attentionText.Render(what) +
		st.attentionHint.Render("  ·  ") + st.attentionText.Render(subject)
	call := bannerChip(st, "click to answer", colChipLight, bannerPulseHues[m.pulse%len(bannerPulseHues)])
	return bannerRow(st, width, st.attentionMark, "⚠", body, call, m.leader()+" "+credentialsLeaderKey, colAlertBG)
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
