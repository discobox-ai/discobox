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
	"github.com/discobox-ai/discobox/hostscope"
	"github.com/discobox-ai/discobox/wellknown"

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
	// approval failed: a credential typed in and stored ahead of the grant that
	// then did not happen. The failure says so rather than leave it behind in
	// silence. A change to an existing secret is never here: it goes with the
	// approval, and the server writes the two together or not at all.
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
	m.requestRows.setAll(rows)
	if len(rows) == 0 {
		m.onRequests = false
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
// the second is how long the grant lives (askLifetime).
func (m *Model) askAboutCredential(req CredentialRequest, secrets []Secret) tea.Cmd {
	if req.Refresh != nil {
		return m.askAboutRefresh(req, secrets)
	}
	if known, ok := wellknown.Lookup(req.WellKnownID); ok && known.Gate {
		return m.askAboutGate(req, secrets, known)
	}
	items := make([]action, 0, len(secrets)+3)
	// Every secret is offered. A secret bound to a neighboring host is the
	// likeliest answer, not the least: a GitHub token is inferred as
	// api.github.com and asked for as github.com, and greying it out leaves the
	// one secret that plainly answers the request unpickable while an unrelated
	// one is offered. Choosing it asks about the binding instead — the remedy
	// the server names when it refuses the grant.
	for _, secret := range secretsForRequest(secrets, req) {
		detail := secretDetail(secret, req.Host)
		if req.WellKnownID != "" && secret.WellKnownID == req.WellKnownID {
			detail = "answers " + req.WellKnownID + " · " + detail
		}
		items = append(items, action{
			key:      "secret:" + secret.ID,
			label:    secret.Name,
			detail:   detail,
			emphasis: refreshcmd.Join(secret.RefreshCommand),
			enabled:  true,
		})
	}
	// A credential that suggests the command printing it can be stored that
	// way: run here, now, and renewed with it when it goes stale
	// (ADR 26-09-25-122 §2).
	//
	// Only while nothing answers the ID: a secret that does is already the
	// answer — the first row — and one stored from the command is renewed
	// with it, so running it again would store a second copy of the same
	// credential.
	answered := slices.ContainsFunc(secrets, func(secret Secret) bool { return secret.WellKnownID == req.WellKnownID })
	if known, ok := wellknown.Lookup(req.WellKnownID); ok && len(known.RefreshCommand) > 0 && !answered {
		command := refreshcmd.Join(known.RefreshCommand)
		items = append(items, action{key: "command", press: "c", label: "From a command…",
			detail: "run " + command + " here, store what it prints, and renew with it", emphasis: command, enabled: true})
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
			return m.startNewCredential(a)
		case result == "command":
			known, _ := wellknown.Lookup(req.WellKnownID)
			a.fresh = true
			a.command = slices.Clone(known.RefreshCommand)
			return m.startNewCredential(a)
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

// askAboutGate is the card for a credential with nothing behind it, such as the
// discobox API (ADR 0140 §1): there is no secret to choose and no value to
// type, only whether to let the discobox in, and for how long.
func (m *Model) askAboutGate(req CredentialRequest, secrets []Secret, known wellknown.Credential) tea.Cmd {
	items := []action{
		{key: "gate", press: "a", label: "Approve", detail: "let it in, then choose for how long", enabled: true},
		{key: "deny", press: "d", label: "Deny", detail: "answer no; the agent is waiting on one", enabled: true},
	}
	d := actionsDialog("Credential request", "", items, func(result string) tea.Cmd {
		a := approval{req: req, secrets: secrets, gate: true}
		switch result {
		case "deny":
			return m.denyCredential(req)
		case "gate":
			return m.askLifetime(a, m.toRequest(a))
		}
		return nil
	})
	// What approving hands over is the whole point of asking, and the request
	// card cannot say it: an ordinary grant is one credential, and this one is
	// the power to give every credential onward (ADR 0140, Consequences).
	d.sections = append(credentialAsk(req, m.secrets.now()), section{
		label: "what approving gives it",
		lines: []line{
			{text: known.Description, tone: toneDim},
			{text: "it may give any secret in this project to the discoboxes it creates", tone: toneAlert},
			{text: "and answer any credential request here, for as long as the grant lives", tone: toneAlert},
		},
	})
	d.answerLabel = "let it in?"
	d.keys = []hint{pressing("enter chooses", "enter"), pressing("esc leaves it waiting", "esc")}
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
// one that gets read. It opens on the lifetime the agent asked for, or on
// lifetime.Default, an hour, when it asked for nothing in particular.
//
// back is the dialog before this one, which Esc returns to: the request card,
// or the binding question when the secret chosen raised one.
func (m *Model) askLifetime(a approval, back func() tea.Cmd) tea.Cmd {
	asked := a.req.GrantTTL
	offered := lifetimeChoices(asked)
	if !slices.Contains(offered, asked) {
		// Not a row, so not what anything below may open on or mark.
		asked = 0
	}
	items := make([]action, 0, len(offered)+1)
	for _, d := range offered {
		items = append(items, action{
			key:     strconv.FormatInt(lifetime.Seconds(d), 10),
			label:   lifetime.Label(d),
			detail:  lifetimeDetail(a.secret, asked, d),
			enabled: true,
		})
	}
	items = append(items, action{key: lifetimeCustom, label: "custom…", detail: "type one: 90m, 3d, 6mo", enabled: true})

	again := func() tea.Cmd { return m.askLifetime(a, back) }
	opensOn := lifetime.Default
	if asked > 0 {
		opensOn = asked
	}
	opens := strconv.FormatInt(lifetime.Seconds(opensOn), 10)
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
	// It opens on the agent's ask, else the default, whatever order the rows
	// are offered in, so Enter is the answer nobody had to choose. The first
	// row carrying the key wins, so a second one that somehow shared it could
	// never take the cursor from it.
	for i, item := range items {
		if item.key == opens {
			d.cursor = i
			break
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

// lifetimeChoices is the presets, with the lifetime the agent asked for in its
// place among them when it is not already one: the row the card opens on has to
// be a row on it. Forever stays last, since nothing an agent asks for is longer.
//
// A row's identity is its count of whole seconds, so an ask that is not one
// cannot be a row: it would collide with the row whose count it shares —
// forever's, for anything under a second — and the card would open on that
// instead of on what was asked for. Such an ask is dropped here and the step
// opens on lifetime.Default, the same as an ask nobody made.
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
// credential allows will be asked about — said before it is chosen rather than
// discovered after.
func lifetimeDetail(secret Secret, asked, d time.Duration) string {
	var notes []string
	if asked > 0 && d == asked {
		notes = append(notes, "what the agent asked for")
	}
	switch limit := secret.MaxTTL; {
	case limit > 0 && (d <= 0 || d > limit):
		notes = append(notes, "longer than "+secret.Name+" allows ("+lifetime.Label(limit)+"), asks first")
	case d <= 0:
		notes = append(notes, "never expires")
	}
	return strings.Join(notes, " · ")
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
	switch {
	case a.fresh:
		answered = "a new credential, stored as " + a.storedAs()
	case a.gate:
		answered = "no secret: its pool lets its uses in"
	}
	fields := []field{
		{label: "credential", value: credentialName(a.req), tone: toneAccent},
		{label: "answered with", value: answered},
	}
	if a.req.Delegate {
		fields = append(fields, field{label: "purpose",
			value: "delegation — may delegate it, never uses it", tone: toneAccent})
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
	if a.fresh && a.command != nil {
		return m.askForRefreshCommand(a, back)
	}
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
	if when := ago(req.Created, now); when != "" {
		asked = when + " by " + asked
	}
	fields := []field{{label: "credential", value: credentialName(req), tone: toneAccent}}
	// An ask to delegate is not the ask a reader assumes, so it says so before
	// anything else about the credential.
	if req.Delegate {
		fields = append(fields, field{label: "purpose",
			value: "delegation — to delegate it, never to use it", tone: toneAccent})
	}
	if req.Type != "" {
		fields = append(fields, field{label: "kind", value: req.Type})
	}
	if req.EnvVar != "" {
		fields = append(fields, field{label: "delivered as", value: req.EnvVar})
	}
	if req.Host != "" {
		fields = append(fields, field{label: "may be sent to", value: req.Host, tone: toneAccent})
	}
	if req.GrantTTL > 0 {
		fields = append(fields, field{label: "wanted for", value: lifetime.Label(req.GrantTTL)})
	}
	fields = append(fields, field{label: "asked", value: asked, tone: toneDim})
	// Where the secrets offered below are from, once there is more than one
	// place they could be: the answer is stored and granted there (ADR 0131
	// §1).
	if req.Server != "" {
		fields = append(fields, field{label: "on server", value: req.Server, tone: toneDim})
	}
	sections := []section{{label: "asked for", fields: fields}}

	if len(req.Uses) == 0 && req.Justification == "" {
		return sections
	}
	what := section{label: "what for"}
	if req.Delegate {
		what.label = "uses it would delegate"
	}
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

// secretType is what a new credential answering the request is stored as. The
// data source defaults an unset type the same way, so the check for a name
// already taken compares what the server will actually be sent.
func secretType(req CredentialRequest) string {
	if t := strings.TrimSpace(req.Type); t != "" {
		return t
	}
	return "token"
}

// secretNameTaken reports whether the project already holds a secret a new one
// stored under this name would collide with. The server's uniqueness is
// (name, type, host, unique_key), and the comparison here is exact because the
// database's is: names are trimmed and hosts normalized on both sides.
//
// The last column is one the API does not carry, so this reads the first
// three. A secret holding a unique key of its own — one the harness configure
// flow made — sits outside the shared slot and would not have collided, and is
// counted here as though it had. That asks a question nobody needed rather
// than letting a create fail, which is the side to be wrong on.
func secretNameTaken(secrets []Secret, name, kind, host string) bool {
	name, host = strings.TrimSpace(name), normalizeHostName(host)
	for _, secret := range secrets {
		if strings.TrimSpace(secret.Name) == name && secret.Type == kind && normalizeHostName(secret.Host) == host {
			return true
		}
	}
	return false
}

// freeSecretName is name, or the first of name-2, name-3… the project does not
// hold. It is a suggestion to type over, not a name anything is stored under
// without being seen: one more collision than there are secrets is impossible,
// which is where the count stops.
func freeSecretName(secrets []Secret, name, kind, host string) string {
	if !secretNameTaken(secrets, name, kind, host) {
		return name
	}
	for n := 2; n <= len(secrets)+2; n++ {
		candidate := fmt.Sprintf("%s-%d", name, n)
		if !secretNameTaken(secrets, candidate, kind, host) {
			return candidate
		}
	}
	return name
}

// secretsForRequest orders the secrets by how likely each is to be the answer:
// the one marked as answering the well-known credential asked for, then the
// host asked for, then a host of the same site, then the unbound ones, then the
// rest. Order is the whole of the opinion here — every one of them can be
// chosen.
func secretsForRequest(secrets []Secret, req CredentialRequest) []Secret {
	out := append([]Secret(nil), secrets...)
	answers := func(secret Secret) bool {
		return req.WellKnownID != "" && secret.WellKnownID == req.WellKnownID
	}
	sort.SliceStable(out, func(i, j int) bool {
		// The secret marked as answering a well-known credential is the answer
		// the last approval already gave.
		if answers(out[i]) != answers(out[j]) {
			return answers(out[i])
		}
		return hostRank(out[i].Host, req.Host) < hostRank(out[j].Host, req.Host)
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
	kind := secretKind(secret)
	detail := kind + " · bound to " + secret.Host + ", asks before using it here"
	switch {
	case bound == "":
		detail = kind + " · any host"
	case hostscope.Covers(bound, host):
		detail = kind + " · " + secret.Host
	}
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

// startNewCredential is the way into storing a credential the project does not
// have: the lifetime, then the value. The name is asked for first only when the
// credential the agent named is one the project already has under that name,
// type, and host — the collision the server refuses the create for, which is
// advice ("pick another name") nothing here could otherwise follow.
func (m *Model) startNewCredential(a approval) tea.Cmd {
	if secretNameTaken(a.secrets, credentialName(a.req), secretType(a.req), a.req.Host) {
		return m.askStoredAs(a, "", m.toRequest(a))
	}
	return m.askLifetime(a, m.toRequest(a))
}

// askStoredAs asks what to store the new credential as, offering the first free
// name so that accepting is one keystroke. note is what is wrong with the name
// that was tried, on the way back round.
func (m *Model) askStoredAs(a approval, note string, back func() tea.Cmd) tea.Cmd {
	taken := credentialName(a.req)
	suggestion := a.name
	if suggestion == "" {
		suggestion = freeSecretName(a.secrets, taken, secretType(a.req), a.req.Host)
	}
	where := "with no host"
	if a.req.Host != "" {
		where = "for " + a.req.Host
	}
	body := note
	if body == "" {
		body = fmt.Sprintf("this project already has a %s secret named %q %s", secretType(a.req), taken, where)
	}
	d := inputDialog("Name the credential", body, "name", suggestion, func(value string) tea.Cmd {
		value = strings.TrimSpace(value)
		if value == "" {
			return m.askStoredAs(a, "a credential is stored under a name; the request is still waiting", back)
		}
		if secretNameTaken(a.secrets, value, secretType(a.req), a.req.Host) {
			return m.askStoredAs(a, fmt.Sprintf("%q is taken %s too; pick another", value, where), back)
		}
		next := a
		next.name = value
		return m.askLifetime(next, func() tea.Cmd { return m.askStoredAs(next, "", back) })
	})
	fields := []field{{label: "answers", value: taken, tone: toneAccent}}
	if a.req.Host != "" {
		fields = append(fields, field{label: "bound to", value: a.req.Host, tone: toneAccent})
	}
	d.sections = []section{{label: "the new project secret", fields: fields}}
	d.answerLabel = "store it as"
	d.footer = "the name is this project's own; the agent never sees it"
	d.keys = []hint{pressing("Enter accepts", "enter"), pressing("Esc goes back", "esc")}
	d.onCancel = back
	m.dialog = d
	return nil
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
	fields := []field{{label: "stored as", value: a.storedAs(), tone: toneAccent}}
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

// askForRefreshCommand shows the command a new credential will be got from,
// editable, before anything runs: it runs on this machine, as the person
// reading it, and is what they will be offered to renew the credential with.
func (m *Model) askForRefreshCommand(a approval, back func() tea.Cmd) tea.Cmd {
	req := a.req
	fields := []field{{label: "stored as", value: a.storedAs(), tone: toneAccent}}
	if req.Host != "" {
		fields = append(fields, field{label: "bound to", value: req.Host, tone: toneAccent})
	}
	fields = append(fields, field{label: "granted for", value: lifetime.Label(a.ttl), tone: toneAccent})
	d := inputDialog("From a command", "", "command", refreshcmd.Join(a.command), func(value string) tea.Cmd {
		command, err := refreshcmd.Split(value)
		if err != nil || len(command) == 0 {
			return m.report(true, "no command to run; the request is still waiting")
		}
		next := a
		next.command = command
		return m.createAndApprove(next, "")
	})
	d.sections = []section{{label: "the new project secret", fields: fields}}
	d.answerLabel = "run this here for the token"
	lasts := defaultValueLifetime
	if ttl := wellKnownValueTTL(req.WellKnownID, a.command); ttl != nil {
		lasts = time.Duration(*ttl) * time.Second
	}
	d.footer = "it runs now, without a shell, and what it prints is stored encrypted · a value lasts " + lifetime.Label(lasts) + ", then it is asked for again"
	d.keys = []hint{pressing("Enter runs it", "enter"), pressing("Esc goes back", "esc")}
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
	status := "storing the credential…"
	if a.command != nil {
		status = "running " + refreshcmd.Join(a.command) + "…"
	}
	m.dialog = statusDialog("Credential request", status)
	return func() tea.Msg {
		secret, err := m.ds.CreateSecret(m.ctx, req.Server, NewSecret{
			Name:           a.storedAs(),
			Type:           req.Type,
			Host:           req.Host,
			Value:          SecretValue{Token: value},
			RefreshCommand: a.command,
			// The lifetime the well-known credential says its value really
			// has, rather than the short one any command gets.
			ValueTTLSeconds: wellKnownValueTTL(req.WellKnownID, a.command),
		})
		if err != nil {
			return credentialAnsweredMsg{request: req, approved: true, ttl: ttl, err: err}
		}
		err = m.ds.ApproveCredentialRequest(m.ctx, req.Server, Approval{
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

// wellKnownValueTTL is how long a value lasts for a credential stored from a
// well-known ID's command, nil when the ID says nothing.
func wellKnownValueTTL(id string, command []string) *int64 {
	known, ok := wellknown.Lookup(id)
	if !ok || command == nil || known.RefreshTTL <= 0 {
		return nil
	}
	seconds := lifetime.Seconds(known.RefreshTTL)
	return &seconds
}

// approval is what answering a request will do, gathered one step at a time:
// the secret chosen, how long its grant lives, and the change to the secret
// those two need, applied with the grant.
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
	// command, on a fresh answer, is the command the value is got from instead
	// of typed: run here when the secret is stored, and kept on it for its
	// renewal.
	command []string
	// gate is an answer that is no secret at all: the credential is a gate
	// (wellknown.Credential.Gate), secret is empty, and the server answers it.
	gate bool
	// name is what the new credential is stored under, when the approver was
	// asked for one because the request's own name is taken. Empty is the
	// request's name, which is the usual case.
	name string
	ttl  time.Duration
	// bindTo and limit are what the approver agreed to change on the secret so
	// the grant fits it: its binding (empty releases it) and its grant limit.
	// Nil leaves each alone. They go with the approval, which the server
	// applies whole or not at all.
	bindTo *string
	limit  *int64
	// changes is what they do, in the words the status dialog says while it
	// happens.
	changes []string
}

// storedAs is the project secret name a new credential takes: the credential
// the agent asked for, unless the approver renamed it because that name was
// already in use.
func (a approval) storedAs() string {
	if a.name != "" {
		return a.name
	}
	return credentialName(a.req)
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
		next.bindTo = &host
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
		next.limit = &seconds
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

// finishApproval mints the grant, with whatever the questions agreed to change
// on the secret carried in the same approval: the server writes them together
// or not at all, so a refused approval leaves the credential as it was.
//
// The lifetime rides on the approval itself rather than being left out for the
// server to default: the default is the secret's own ceiling, which most
// credentials do not have, and a window that stayed quiet about it handed out
// permanent credentials without ever saying the word.
func (m *Model) finishApproval(a approval) tea.Cmd {
	what := "approving for " + lifetime.Label(a.ttl) + "…"
	if len(a.changes) > 0 {
		what = strings.Join(a.changes, " and ") + ", approving…"
	}
	m.dialog = statusDialog("Credential request", what)
	return func() tea.Msg {
		err := m.ds.ApproveCredentialRequest(m.ctx, a.req.Server, Approval{
			RequestID:           a.req.ID,
			SecretID:            a.secret.ID,
			TTLSeconds:          lifetime.Seconds(a.ttl),
			SecretHost:          a.bindTo,
			SecretMaxTTLSeconds: a.limit,
		})
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
			done.lines = append(done.lines, line{}, line{text: "it was done for this approval; the secrets screen (F4) removes it if the grant is not coming", tone: toneDim})
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
	} else if host := pending[0].Host; host != "" {
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
	call := bannerChip(st, "click to answer", colChipLight, bannerPulseHues[m.pulse%len(bannerPulseHues)])
	return bannerRow(st, width, st.attentionMark, "⚠", body, call, m.leader()+" "+credentialsLeaderKey, colAlertBG)
}

// credentialErrorSection says which request is still waiting, under the
// server's own wording of the refusal: that wording is the half that says what
// to do next, so it is left whole as the dialog's body and this stands under it.
func credentialErrorSection(req CredentialRequest) section {
	if req.Trust != nil {
		return section{label: "still waiting", fields: []field{{label: "trust", value: req.Host, tone: toneAccent}}}
	}
	fields := []field{{label: "credential", value: credentialName(req), tone: toneAccent}}
	if req.Host != "" {
		fields = append(fields, field{label: "for", value: req.Host, tone: toneAccent})
	}
	return section{label: "still waiting", fields: fields}
}
