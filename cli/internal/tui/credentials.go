package tui

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/cli/internal/refreshcmd"
	"github.com/discobox-ai/discobox/pkg/agentcreds"

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
const credentialsKey = "C"

// credentialsLeaderKey is the same thing behind the leader in the workspace,
// where the banner names it.
//
// It is the one place the workspace does not carry the list's key on the list's
// letter. The leader's c opens another terminal, and a shift away from it sat a
// dialog that binds a credential — two commands one modifier apart, one of
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
// how long the binding the approval made lasts, so the report says how long
// the credential is good for rather than only that it was handed over.
type credentialAnsweredMsg struct {
	request  CredentialRequest
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
	var rows []CredentialRequest
	for _, req := range m.allRequests {
		if m.serverName(req.Server) == m.configServer() {
			rows = append(rows, req)
		}
	}
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
	// A trust request is answered with a pin, not a secret: there is nothing
	// to read first.
	if req.Trust != nil {
		return m.askAboutTrust(req)
	}
	m.dialog = statusDialog("Credential request", "reading the project's secrets…")
	return func() tea.Msg {
		secrets, err := m.ds.Secrets(m.ctx, req.Server)
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
// the second is how long the binding lasts (askLifetime).
func (m *Model) askAboutCredential(req CredentialRequest, secrets []Secret) tea.Cmd {
	if req.Refresh != nil {
		return m.askAboutRefresh(req, secrets)
	}
	items := secretRows(secrets, req)
	// A new credential is the secrets screen's own form, filled from the
	// request: its name and the host it asks for.
	items = append(items, action{key: "new", press: "n", label: "New credential…", detail: "store it as a project secret and approve with it", enabled: true})
	items = append(items, action{key: "deny", press: "d", label: "Deny", detail: "answer no; the agent is waiting on one", enabled: true})
	if req.Raw != "" {
		items = append(items, action{key: "raw", press: "r", label: "Raw request…", detail: "the request as JSON, as the server sent it", enabled: true})
	}

	d := actionsDialog("Credential request", "", items, func(result string) tea.Cmd {
		a := approval{req: req, secrets: secrets}
		switch {
		case result == "raw":
			return m.showRawRequest(req, func() tea.Cmd { return m.askAboutCredential(req, secrets) })
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

// showRawRequest is the request as the server sent it, as JSON highlighted
// the way a code block in a comment is. back is the request card.
func (m *Model) showRawRequest(req CredentialRequest, back func() tea.Cmd) tea.Cmd {
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
	d := actionsDialog("Raw request", "", []action{{key: "back", press: "enter", label: "Back to the request", enabled: true}},
		func(string) tea.Cmd { return back() })
	d.titleRight = req.ID
	d.sections = []section{{label: "as the server sent it", lines: lines}}
	d.keys = []hint{pressing("enter or esc goes back", "esc")}
	d.onCancel = back
	m.dialog = d
	return nil
}

// secretRows is the project's secrets as rows to choose between, likeliest
// first. Every secret is offered: the binding the approval makes is held to
// the secret's routes and the request's both, so any of them can answer.
func secretRows(secrets []Secret, req CredentialRequest) []action {
	items := make([]action, 0, len(secrets)+4)
	for _, secret := range secretsForRequest(secrets, req) {
		detail := secretDetail(secret)
		items = append(items, action{
			key:      "secret:" + secret.ID,
			label:    secret.Name,
			detail:   detail,
			emphasis: refreshcmd.Join(secret.RefreshCommand),
			enabled:  true,
		})
	}
	return items
}

// lifetimeCustom is the lifetime menu's way out to one the presets do not
// have. The presets' own keys are their lifetimes in seconds, so reading one
// back is parsing what was chosen rather than remembering which row meant what.
const lifetimeCustom = "custom"

// askLifetime is the second step of every approval: how long the binding lasts.
//
// It is a step of its own, and a required one, because it is the half of an
// approval nobody thinks to look for: a window that left it out would hand
// out whatever the server defaults to without anyone having read it. A card
// that has to be answered is one that gets read. It opens on the lifetime the agent asked for, or on
// agentcreds.DefaultGrantTTL, an hour, when it asked for nothing in particular.
//
// A lifetime longer than the secret allows is not offered: the server would
// refuse it, and raising a secret's limit is a decision about the credential,
// made on the secrets screen rather than on the way through one approval.
//
// back is the dialog before this one, which Esc returns to: the request card.
func (m *Model) askLifetime(a approval, back func() tea.Cmd) tea.Cmd {
	asked := a.req.AskedTTL
	offered := lifetimeChoices(asked)
	if !slices.Contains(offered, asked) {
		// Not a row, so not what anything below may open on or mark.
		asked = 0
	}
	opensOn := agentcreds.DefaultGrantTTL
	if asked > 0 {
		opensOn = asked
	}
	ask := durationAsk{
		title:        "How long?",
		sections:     []section{bindingSection(a)},
		answerLabel:  "how long may " + credentialName(a.req) + " be used for?",
		footer:       "when it lapses the credential stops working, and the agent asks again",
		customFooter: "1h, 90m, 3d, 2w, 1mo — or forever",
		choices:      offered,
		opensOn:      opensOn,
		detail:       func(d time.Duration) string { return lifetimeDetail(a.secret, asked, d) },
		allowed:      func(d time.Duration) bool { return withinLimit(a.secret, d) },
		refusal:      a.secret.Name + " allows at most " + lifetime.Label(a.secret.MaxTTL),
	}
	return m.askDuration(ask, func(d time.Duration, _ bool, again func() tea.Cmd) tea.Cmd {
		next := a
		next.ttl = d
		return m.lifetimeChosen(next, again)
	}, back)
}

// durationAsk is a lifetime chosen from the presets, or typed: how long a
// binding lasts, or the longest one a secret allows. What a row is called and
// what it says beyond its name are the asker's, since "forever" means
// something different in each.
type durationAsk struct {
	title, answerLabel, footer string
	// customFooter is what the field for one the presets lack says it takes.
	customFooter string
	sections     []section
	// choices are the rows, and opensOn the one the cursor starts on.
	choices []time.Duration
	opensOn time.Duration
	// name is a row's label, lifetime.Label when nil; detail what it means
	// beyond that.
	name, detail func(time.Duration) string
	// keep, when set, is a first row that leaves the value as it is, labeled
	// with what it is; the cursor starts there.
	keep string
	// allowed, when set, says whether a lifetime may be chosen at all, and
	// refusal what is said of one typed that may not.
	allowed func(time.Duration) bool
	refusal string
}

// withinLimit reports whether a binding of secret may last d: the secret
// caps nothing, or d is no longer than its cap. Forever is longer than any.
func withinLimit(secret Secret, d time.Duration) bool {
	return secret.MaxTTL <= 0 || (d > 0 && d <= secret.MaxTTL)
}

// askDuration asks for the lifetime and hands chose what was chosen — kept
// when it is the row that leaves it as it is — with the way back to this
// card, for whatever follows to return to. back is the step before.
func (m *Model) askDuration(ask durationAsk, chose func(d time.Duration, kept bool, again func() tea.Cmd) tea.Cmd, back func() tea.Cmd) tea.Cmd {
	name := ask.name
	if name == nil {
		name = lifetime.Label
	}
	items := make([]action, 0, len(ask.choices)+2)
	if ask.keep != "" {
		items = append(items, action{key: lifetimeKeep, label: "as it is", detail: ask.keep, enabled: true})
	}
	for _, d := range ask.choices {
		detail := ""
		if ask.detail != nil {
			detail = ask.detail(d)
		}
		enabled := ask.allowed == nil || ask.allowed(d)
		items = append(items, action{key: strconv.FormatInt(lifetime.Seconds(d), 10), label: name(d), detail: detail, enabled: enabled})
	}
	items = append(items, action{key: lifetimeCustom, label: "custom…", detail: "type one: 90m, 3d, 6mo", enabled: true})

	again := func() tea.Cmd { return m.askDuration(ask, chose, back) }
	d := actionsDialog(ask.title, "", items, func(result string) tea.Cmd {
		switch result {
		case lifetimeCustom:
			return m.askCustomDuration(ask, chose, again, "")
		case lifetimeKeep:
			return chose(0, true, again)
		}
		seconds, err := strconv.ParseInt(result, 10, 64)
		if err != nil {
			return nil
		}
		return chose(time.Duration(seconds)*time.Second, false, again)
	})
	// It opens on the row asked for whatever order the rows are in, so Enter
	// is the answer nobody had to choose. The first row carrying the key
	// wins, so a second one that somehow shared it could never take the
	// cursor from it.
	opens := strconv.FormatInt(lifetime.Seconds(ask.opensOn), 10)
	if ask.keep != "" {
		opens = lifetimeKeep
	}
	for i, item := range items {
		if item.key == opens {
			d.cursor = i
			break
		}
	}
	d.sections = ask.sections
	d.answerLabel = ask.answerLabel
	d.footer = ask.footer
	d.keys = []hint{pressing("enter chooses the highlighted lifetime", "enter"), pressing("esc goes back", "esc")}
	d.onCancel = back
	m.dialog = d
	return nil
}

// lifetimeKeep is the row that leaves a lifetime as it is.
const lifetimeKeep = "keep"

// lifetimeChoices is the presets, with the lifetime the agent asked for in its
// place among them when it is not already one: the row the card opens on has to
// be a row on it. Forever stays last, since nothing an agent asks for is longer.
//
// A row's identity is its count of whole seconds, so an ask that is not one
// cannot be a row: it would collide with the row whose count it shares —
// forever's, for anything under a second — and the card would open on that
// instead of on what was asked for. Such an ask is dropped here and the step
// opens on agentcreds.DefaultGrantTTL, the same as an ask nobody made.
func lifetimeChoices(asked time.Duration) []time.Duration {
	if asked <= 0 || asked%time.Second != 0 || slices.Contains(lifetime.Presets, asked) {
		return lifetime.Presets
	}
	at := len(lifetime.Presets)
	for i, d := range lifetime.Presets {
		if d <= 0 || d > asked {
			at = i
			break
		}
	}
	return slices.Insert(slices.Clone(lifetime.Presets), at, asked)
}

// lifetimeDetail says what a lifetime means beyond its name: that it is what
// the agent asked for, that forever never lapses, and that one longer than the
// credential allows cannot be chosen — said beside the row rather than
// discovered after.
func lifetimeDetail(secret Secret, asked, d time.Duration) string {
	var notes []string
	if asked > 0 && d == asked {
		notes = append(notes, "what the agent asked for")
	}
	switch limit := secret.MaxTTL; {
	case limit > 0 && (d <= 0 || d > limit):
		notes = append(notes, "longer than "+secret.Name+" allows ("+lifetime.Label(limit)+")")
	case d <= 0:
		notes = append(notes, "never expires")
	}
	return strings.Join(notes, " · ")
}

// askCustomDuration takes a lifetime the presets do not offer. refused is
// the last answer's refusal, which the card keeps rather than closing onto
// the screen behind it: what is wrong with "3 weeks-ish" has to be said where
// it was typed. back is the lifetime menu.
func (m *Model) askCustomDuration(ask durationAsk, chose func(d time.Duration, kept bool, again func() tea.Cmd) tea.Cmd, back func() tea.Cmd, refused string) tea.Cmd {
	d := inputDialog(ask.title, "", "e.g. 90m, 3d, 6mo", "", func(value string) tea.Cmd {
		chosen, err := lifetime.Parse(value)
		if err != nil {
			return m.askCustomDuration(ask, chose, back, fmt.Sprintf("%v", err))
		}
		if ask.allowed != nil && !ask.allowed(chosen) {
			return m.askCustomDuration(ask, chose, back, ask.refusal)
		}
		return chose(chosen, false, func() tea.Cmd { return m.askCustomDuration(ask, chose, back, "") })
	})
	d.sections = ask.sections
	d.answerLabel = ask.answerLabel
	d.footer = ask.customFooter
	d.emphasis = refused
	d.onCancel = back
	m.dialog = d
	return nil
}

// bindingSection is what the lifetime is being chosen for, so the card asking
// "how long" says how long *what*: the credential, what answers it, and where
// it may be sent.
func bindingSection(a approval) section {
	answered := a.secret.Name
	if a.fresh {
		answered = "a new credential, filled in on the next card"
	}
	fields := []field{
		{label: "credential", value: credentialName(a.req), tone: toneAccent},
		{label: "answered with", value: answered},
	}
	if a.req.Delegate {
		fields = append(fields, field{label: "purpose", value: delegationPurpose, tone: toneAccent})
	}
	if where := a.req.where(); where != "" {
		fields = append(fields, field{label: "may be sent to", value: where, tone: toneAccent})
	}
	return section{label: "the binding", fields: fields}
}

// delegationPurpose is what a binding to delegate is for, said the same way on
// the request and on the binding.
const delegationPurpose = "delegation — may give it to the discoboxes it creates, never uses it"

// lifetimeChosen is where the two kinds of answer part: a new credential still
// needs its value, and an existing one is approved with.
func (m *Model) lifetimeChosen(a approval, _ func() tea.Cmd) tea.Cmd {
	if a.fresh {
		return m.askNewCredentialForm(a)
	}
	return m.finishApproval(a)
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
	if when := ago(req.Created, now); when != "" {
		asked = when + " by " + asked
	}
	fields := []field{{label: "credential", value: credentialName(req), tone: toneAccent}}
	// An ask to delegate is not the ask a reader assumes, so it says so before
	// anything else about the credential.
	if req.Delegate {
		fields = append(fields, field{label: "purpose", value: delegationPurpose, tone: toneAccent})
	}
	if req.EnvVar != "" {
		fields = append(fields, field{label: "delivered as", value: req.EnvVar})
	}
	if where := req.where(); where != "" {
		fields = append(fields, field{label: "may be sent to", value: where, tone: toneAccent})
	}
	if req.AskedTTL > 0 {
		fields = append(fields, field{label: "wanted for", value: lifetime.Label(req.AskedTTL)})
	}
	fields = append(fields, field{label: "asked", value: asked, tone: toneDim})
	// Where the secrets offered below are from, once there is more than one
	// place they could be: the answer is stored and bound there (ADR 0131
	// §1).
	if req.Server != "" {
		fields = append(fields, field{label: "on server", value: req.Server, tone: toneDim})
	}
	sections := []section{{label: "asked for", fields: fields}}

	if req.Justification != "" {
		sections = append(sections, section{label: "why", lines: []line{{text: req.Justification, tone: toneDim}}})
	}
	return sections
}

func credentialName(req CredentialRequest) string {
	if req.Trust != nil {
		return "trust of " + req.Host
	}
	if req.Name != "" {
		return req.Name
	}
	if req.EnvVar != "" {
		return req.EnvVar
	}
	return "a credential"
}

// secretNameTaken reports whether the project already holds a secret by this
// name: a secret's name is unique in its project.
func secretNameTaken(secrets []Secret, name string) bool {
	name = strings.TrimSpace(name)
	for _, secret := range secrets {
		if strings.TrimSpace(secret.Name) == name {
			return true
		}
	}
	return false
}

// freeSecretName is name, or the first of name-2, name-3… the project does not
// hold. It is a suggestion to type over, not a name anything is stored under
// without being seen: one more collision than there are secrets is impossible,
// which is where the count stops.
func freeSecretName(secrets []Secret, name string) string {
	if !secretNameTaken(secrets, name) {
		return name
	}
	for n := 2; n <= len(secrets)+2; n++ {
		candidate := fmt.Sprintf("%s-%d", name, n)
		if !secretNameTaken(secrets, candidate) {
			return candidate
		}
	}
	return name
}

// secretsForRequest orders the secrets by how likely each is to be the answer:
// the one named what the request asked for first, then the rest as listed.
// Order is the whole of the opinion here — every one of them can be chosen.
func secretsForRequest(secrets []Secret, req CredentialRequest) []Secret {
	out := append([]Secret(nil), secrets...)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Name == req.Name && out[j].Name != req.Name
	})
	return out
}

// secretDetail says what a secret is and where it may be sent. A credential
// that caps how long its bindings may last says so here, beside the row.
func secretDetail(secret Secret) string {
	where := "sent nowhere"
	if len(secret.Routes) > 0 {
		where = routesText(secret.Routes)
	}
	detail := secretKind(secret) + " · " + where
	if secret.MaxTTL > 0 {
		detail += " · at most " + lifetime.Label(secret.MaxTTL)
	}
	return detail
}

// secretKind is what a secret is, said the way that matters when choosing it:
// a token got from a command is named by the command, which is how its value
// is kept current (ADR 26-09-25-122).
func secretKind(secret Secret) string {
	if len(secret.RefreshCommand) > 0 {
		return "from " + refreshcmd.Join(secret.RefreshCommand)
	}
	return secret.Type
}

// askNewCredentialForm stores a credential the project does not have yet and
// answers the request with it: the secrets screen's form (secretForm), filled
// from the request (fillFromRequest), every field still the person's to
// change. It comes after the lifetime, so the token typed into it is never
// held by a card that can be gone back to: Esc here is the request, still
// waiting.
func (m *Model) askNewCredentialForm(a approval) tea.Cmd {
	return m.newCredentialForm(a, section{label: "approves the request", fields: []field{
		{label: "credential", value: credentialName(a.req), tone: toneAccent},
		{label: "bound for", value: lifetime.Label(a.ttl), tone: toneAccent},
	}}, "enter stores and approves", func(f *form, secret NewSecret) tea.Cmd {
		// A limit shorter than the binding this approval is making is one the
		// server would refuse the binding for, after the secret was stored.
		if limit := time.Duration(secret.MaxTTLSeconds) * time.Second; limit > 0 && (a.ttl <= 0 || a.ttl > limit) {
			f.err = "the binding limit is shorter than the " + lifetime.Label(a.ttl) + " this request is being approved for"
			return nil
		}
		return m.createAndApprove(a, secret)
	}, m.toRequest(a))
}

// newCredentialForm is the new-secret form filled from a request, with what
// storing it then does. about says so above the form, and back is where Esc
// goes, nothing stored.
func (m *Model) newCredentialForm(a approval, about section, store string, then func(*form, NewSecret) tea.Cmd, back func() tea.Cmd) tea.Cmd {
	f := secretForm(nil, a.req.Routes)
	fillFromRequest(f, a.req, a.secrets)
	d := formDialog("New credential", f, func(f *form) tea.Cmd {
		secret, ok := secretFromForm(f)
		if !ok {
			return nil
		}
		return then(f, secret)
	})
	d.sections = []section{about}
	// Esc second: a key line is cut from its tail, and the way back — the
	// request still waiting — is the one thing it must always say.
	d.keys = []hint{pressing(store, "enter"), pressing("esc back, nothing stored", "esc"), says("↑↓ moves"), says("←→ chooses")}
	d.onCancel = back
	m.dialog = d
	return nil
}

// fillFromRequest fills the new-secret form from what a request asks for:
// named as the request asks; the routes it asks for are the form's own rows
// (newCredentialForm). A name the project already holds is the first free one
// instead, which the server would otherwise refuse the create for.
func fillFromRequest(f *form, req CredentialRequest, secrets []Secret) {
	f.set("name", freeSecretName(secrets, credentialName(req)))
}

// createAndApprove stores the new credential and answers with it, for the
// lifetime chosen. A secret stored from a command is run for its first value
// by the data source. A refusal after the secret was stored says so: the
// secret stays, and the request still waits.
func (m *Model) createAndApprove(a approval, secret NewSecret) tea.Cmd {
	req, ttl := a.req, a.ttl
	status := "storing the credential…"
	if secret.RefreshCommand != nil {
		status = "running " + refreshcmd.Join(secret.RefreshCommand) + "…"
	}
	m.dialog = statusDialog("Credential request", status)
	return func() tea.Msg {
		stored, err := m.ds.CreateSecret(m.ctx, req.Server, secret)
		if err != nil {
			return credentialAnsweredMsg{request: req, approved: true, ttl: ttl, err: err}
		}
		err = m.ds.ApproveCredentialRequest(m.ctx, req.Server, Approval{
			RequestID:  req.ID,
			SecretID:   stored.ID,
			TTLSeconds: lifetime.Seconds(ttl),
		})
		msg := credentialAnsweredMsg{request: req, approved: true, ttl: ttl, err: err}
		if err != nil {
			msg.applied = []string{"the token was stored as the project secret " + stored.Name}
		}
		return msg
	}
}

// approval is what answering a request will do, gathered one step at a time:
// the secret chosen, and how long the binding it makes lasts.
//
// It is carried whole, and copied rather than changed at each step, because
// every step can be gone back from: the dialog before one is rebuilt from the
// approval as it stood when that dialog was asked, and has to find it that way.
type approval struct {
	req     CredentialRequest
	secrets []Secret
	secret  Secret
	// fresh is a new credential rather than a secret the project holds:
	// secret is empty, and the new-secret form is filled in after the
	// lifetime.
	fresh bool
	ttl   time.Duration
}

// toRequest is the way back to the request card: where Esc goes from the first
// step after it.
func (m *Model) toRequest(a approval) func() tea.Cmd {
	return func() tea.Cmd { return m.askAboutCredential(a.req, a.secrets) }
}

// chooseSecret is the first step's answer: the secret, then how long.
func (m *Model) chooseSecret(a approval, secretID string) tea.Cmd {
	for _, secret := range a.secrets {
		if secret.ID == secretID {
			a.secret = secret
		}
	}
	return m.askLifetime(a, m.toRequest(a))
}

// finishApproval makes the binding.
//
// The lifetime rides on the approval itself rather than being left out for the
// server to default: the default is the secret's own ceiling, which most
// credentials do not have, and a window that stayed quiet about it handed out
// permanent credentials without ever saying the word.
func (m *Model) finishApproval(a approval) tea.Cmd {
	what := "approving for " + lifetime.Label(a.ttl) + "…"
	sent := Approval{
		RequestID:  a.req.ID,
		SecretID:   a.secret.ID,
		TTLSeconds: lifetime.Seconds(a.ttl),
	}
	m.dialog = statusDialog("Credential request", what)
	return func() tea.Msg {
		err := m.ds.ApproveCredentialRequest(m.ctx, a.req.Server, sent)
		return credentialAnsweredMsg{request: a.req, approved: true, ttl: a.ttl, err: err}
	}
}

func (m *Model) denyCredential(req CredentialRequest) tea.Cmd {
	if req.Trust != nil {
		m.dialog = statusDialog("Trust request", "denying…")
		return func() tea.Msg {
			err := m.ds.DenyTrustRequest(m.ctx, req.Server, req.ID)
			return credentialAnsweredMsg{request: req, approved: false, err: err}
		}
	}
	if req.Refresh != nil {
		m.dialog = statusDialog("Refresh request", "dismissing…")
		return func() tea.Msg {
			err := m.ds.DismissRefresh(m.ctx, req.Server, req.ID)
			return credentialAnsweredMsg{request: req, approved: false, err: err}
		}
	}
	m.dialog = statusDialog("Credential request", "denying…")
	return func() tea.Msg {
		err := m.ds.DenyCredentialRequest(m.ctx, req.Server, req.ID)
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
			done.lines = append(done.lines, line{}, line{text: "it was done for this approval; the secrets screen (F4) removes it if the binding is not coming", tone: toneDim})
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
	if msg.request.Refresh != nil {
		return tea.Batch(m.loadCredentialRequests(), m.report(false, "dismissed the refresh of %s", credentialName(msg.request)))
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
	what := "credential request"
	if pending[0].Trust != nil {
		// The name already says the host, so it is not said twice.
		what = "trust request"
	} else if pending[0].Refresh != nil {
		// A token this discobox already has, wanting a new value.
		what = "refresh request"
	} else if host := pending[0].where(); host != "" {
		subject += " for " + host
	}
	if len(pending) > 1 {
		one, many := "credential request", "credential requests"
		trusts := 0
		for _, req := range pending {
			if req.Trust != nil {
				trusts++
			}
		}
		switch trusts {
		case 0:
		case len(pending):
			one, many = "trust request", "trust requests"
		default:
			one, many = "request", "requests"
		}
		what, subject = plural(len(pending), one, many), "oldest: "+subject
	}
	body := st.attentionText.Render(what) +
		st.attentionHint.Render("  ·  ") + st.attentionText.Render(subject)
	call := bannerChip(st, "click to answer", bannerPulseHues[m.pulse%len(bannerPulseHues)])
	return bannerRow(st, width, body, call, m.leader()+" "+credentialsLeaderKey)
}

// credentialErrorSection says which request is still waiting, under the
// server's own wording of the refusal: that wording is the half that says what
// to do next, so it is left whole as the dialog's body and this stands under it.
func credentialErrorSection(req CredentialRequest) section {
	if req.Trust != nil {
		return section{label: "still waiting", fields: []field{{label: "trust", value: req.Host, tone: toneAccent}}}
	}
	fields := []field{{label: "credential", value: credentialName(req), tone: toneAccent}}
	if where := req.where(); where != "" {
		fields = append(fields, field{label: "for", value: where, tone: toneAccent})
	}
	return section{label: "still waiting", fields: fields}
}
