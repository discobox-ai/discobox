package tui

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/cli/internal/refreshcmd"
	"github.com/discobox-ai/discobox/cli/internal/requestrole"
	"github.com/discobox-ai/discobox/pkg/bindingplan"
	"github.com/discobox-ai/discobox/pkg/secretroute"

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

// standingLoadedMsg carries the bindings of the chosen secret the asking
// discobox already holds itself: what an approved part would replace (ADR
// 26-11-02-022 §5), read once the secret is chosen.
type standingLoadedMsg struct {
	wait     *dialog
	parents  []console.SecretBinding
	approval approval
	standing []console.SecretBinding
	held     []console.HeldGrant
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
	var rows []console.CredentialRequest
	for _, req := range m.allRequests {
		if m.session.ServerName(req.Server) == m.configServer() {
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
func (m *Model) pendingFor(box console.Sandbox) []console.CredentialRequest {
	out := []console.CredentialRequest{}
	for _, req := range m.requests[box.ID] {
		if m.session.ServerName(req.Server) == m.session.ServerName(box.Server) {
			out = append(out, req)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// openCredentialDialog asks about the oldest request on the discobox the
// workspace is showing.
func (m *Model) openCredentialDialog(box console.Sandbox) tea.Cmd {
	pending := m.pendingFor(box)
	if len(pending) == 0 {
		return m.report(false, "nothing waiting on this discobox")
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
	m.dialog = statusDialog("Credential request", "reading the project's secrets…")
	return func() tea.Msg {
		secrets, err := m.ds.Secrets(m.ctx, req.Server)
		var held []console.HeldGrant
		if err == nil && req.Use != nil && req.EnvVar != "" && req.SandboxID != "" {
			held, err = m.ds.HeldGrants(m.ctx, req.Server, req.SandboxID, req.EnvVar)
		}
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
	return m.approvalPart(newApproval(req, secrets, held), req.Use == nil && req.Delegate != nil)
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
	d := actionsDialog("Raw request", "", []action{{key: "back", press: "enter", label: "Back to the request", enabled: true}},
		func(string) tea.Cmd { return back() })
	d.titleRight = req.ID
	d.sections = []section{{label: "as the server sent it", lines: lines}}
	d.keys = []hint{pressing("enter or esc goes back", "esc")}
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
	if a.req.Delegate != nil {
		fields = append(fields, field{label: "purpose", value: partsPurpose(a.req), tone: toneAccent})
	}
	if where := a.req.WhereRows(); where != "" {
		fields = append(fields, field{label: "may be sent to", value: where, tone: toneAccent})
	}
	for _, kind := range []struct {
		name     string
		answer   string
		delegate bool
	}{{"use", a.use, false}, {"delegate", a.delegate, true}} {
		if kind.answer == "" {
			continue
		}
		if kind.answer == console.PartDeny {
			fields = append(fields, field{label: kind.name + " part", value: "denied — what the discobox holds of it stays as it is", tone: toneDim})
			continue
		}
		asked := a.grantedRoutes(kind.delegate)
		scope := a.useScope
		if kind.delegate {
			scope = a.delegateScope
		}
		if scope == "" {
			scope = "routes"
		}
		summary := "Requested routes"
		switch scope {
		case "hosts":
			summary = "Requested hosts · Any method and path on these hosts; any protocol\n" + console.RouteRows(asked)
		case "anywhere":
			summary = "Anywhere · No grant restriction"
		}
		fields = append(fields, field{label: kind.name + " scope", value: summary + "\nStill limited by credential scope", tone: toneAccent})
		fields = append(fields, replacedFields(a.standing, kind.delegate, a.req.EnvVar, a.usePlanned(), asked, a.secret.Routes)...)
	}
	return section{label: "the binding", fields: fields}
}

// usePlanned is how the use part is planned on the cards: as it will be
// granted, empty when it is denied or not asked.
func (a approval) usePlanned() bindingplan.As {
	if a.use != console.PartApprove {
		return ""
	}
	return a.useAs
}

// partsPurpose says what a request with a delegate part asks, said the same
// way on the request and on the binding.
func partsPurpose(req console.CredentialRequest) string {
	if req.AsksBoth() {
		return "use it, and " + delegationPurpose
	}
	return delegationPurpose
}

// replacedFields is what an approved part would do to the asking discobox's
// own bindings of the chosen secret (ADR 26-11-02-022 §5): which is kept and
// takes the approval, which go, which lose their use or are split, and
// whether a new one is made. bindingplan decides it, by the rule the server
// applies, so access lost to a replacement is said before the approval, not
// found after it. envVar is the variable the use part asks for; useAs is how
// the use part is granted, empty when it is not approved, since a delegate
// part is applied to what the use part leaves (bindingplan.Answer).
//
// Each binding is a heading row and, under it, a diff of its routes against
// asked, the routes the part approves: the routes it loses, and — on the one
// kept, which takes asked in place (bindingplan.Updated, and the delegation
// of a bindingplan.Split) — the routes it gains, with the ones both name
// counted rather than listed, since the card lists them already under "may be
// sent to". secretRoutes stand in for either side that names none. A diff
// is what a person approving is checking for; two long lists side by side
// leave them to find the difference themselves.
func replacedFields(standing []console.SecretBinding, delegate bool, envVar string, useAs bindingplan.As, asked, secretRoutes []secretroute.Route) []field {
	held := map[string]console.SecretBinding{}
	var mine []bindingplan.Binding
	for _, b := range standing {
		held[b.ID] = b
		mine = append(mine, bindingplan.Binding{ID: b.ID, Use: b.Use, Delegate: b.Delegate, Adds: b.Adds, EnvName: b.EnvVar, Created: b.Created})
	}
	if !delegate && useAs == "" {
		useAs = bindingplan.Replace
	}
	use, delegatePlan := bindingplan.Answer(mine, envVar, useAs, delegate)
	plan := use
	if delegate {
		plan = delegatePlan
	}
	if plan.Adds {
		return []field{{label: "replaces", value: "nothing: an addition beside what it holds", tone: toneDim}}
	}
	var fields []field
	add := func(value string, t tone) {
		label := ""
		if len(fields) == 0 {
			label = "replaces"
		}
		fields = append(fields, field{label: label, value: value, tone: t})
	}
	for _, step := range plan.Steps {
		// Said as the binding stands when this part is applied: a delegate
		// part after a use part reads what the use part left.
		b := held[step.Binding.ID]
		what := "the delegation"
		if step.Binding.Use {
			what = "the one in " + step.Binding.EnvName
		}
		var fate string
		switch step.Fate {
		case bindingplan.Updated:
			fate = "kept, takes this approval"
		case bindingplan.Deleted:
			fate = "goes"
		case bindingplan.LosesUse:
			fate = "loses its use; its delegation stays"
		case bindingplan.Split:
			fate = "stays the delegation; its use moves to a binding of its own in " + b.EnvVar
		}
		// A binding or a part with no routes of its own is sent to the
		// secret's, so the secret's are what it is set against.
		was, now := b.Routes, asked
		if len(was) == 0 {
			was = secretRoutes
		}
		if len(now) == 0 {
			now = secretRoutes
		}
		if len(was) == 0 || len(now) == 0 {
			add(what+", sent to the secret's routes — "+fate, toneAlert)
			continue
		}
		add(what+" — "+fate, toneAlert)
		for _, row := range routeDiff(was, now, step.Binding.ID == plan.Keep) {
			add(row.text, row.tone)
		}
	}
	if len(fields) > 0 && plan.Keep == "" {
		add("and a new binding is made", toneAlert)
	}
	return fields
}

// routeDiff is was set against now as diff rows: "- " for each route of was
// that now does not name, then, for the binding kept, "+ " for each of now
// that was does not, and before them one row counting the routes both name.
// Only the binding kept takes now, so only it gains, and only it can say its
// routes are the same: one that goes, or loses its use, says how many of its
// routes this approval carries on. Routes are compared as secretroute.Format
// writes them, the way a person reads them.
func routeDiff(was, now []secretroute.Route, kept bool) []line {
	wasText, nowText := console.FormatRoutes(was), console.FormatRoutes(now)
	var removed, added []line
	same := 0
	for _, r := range wasText {
		if slices.Contains(nowText, r) {
			same++
			continue
		}
		removed = append(removed, line{text: "- " + r, tone: toneAlert})
	}
	if kept {
		for _, r := range nowText {
			if !slices.Contains(wasText, r) {
				added = append(added, line{text: "+ " + r, tone: toneOK})
			}
		}
	}
	var count string
	switch {
	case same == 0:
	case kept && len(removed) == 0 && len(added) == 0:
		count = "the same routes"
	case kept:
		count = plural(same, "route", "routes") + " unchanged"
	case len(removed) == 0:
		count = "every route also in this approval"
	default:
		count = plural(same, "route", "routes") + " also in this approval"
	}
	var rows []line
	if count != "" {
		rows = append(rows, line{text: "  " + count, tone: toneDim})
	}
	return append(append(rows, removed...), added...)
}

// delegationPurpose is what a binding to delegate is for, said the same way on
// the request and on the binding.
const delegationPurpose = "delegation — may give it to the discoboxes it leads, never uses it"

// rawDeliveryText is what a raw binding hands over (ADR 26-10-27-840),
// said so a person approving it reads that the value itself leaves the pool.
func rawDeliveryText(delivery string) string {
	where := "in its variable"
	if delivery == "file" {
		where = "in a file in memory, its path in the variable"
	}
	return "the value itself, " + where + " — refused off its routes, but the discobox can send it anywhere"
}

func (a approval) grantedRoutes(delegate bool) []secretroute.Route {
	part, scope := a.req.Use, a.useScope
	if delegate {
		part, scope = a.req.Delegate, a.delegateScope
	}
	var routes []secretroute.Route
	if part != nil {
		routes = part.Routes
	} else {
		routes = a.req.Routes()
	}
	if scope == "anywhere" {
		return nil
	}
	if scope == "hosts" {
		hosts := secretroute.OpenRouteHosts(routes)
		return []secretroute.Route{{Hosts: hosts}}
	}
	return routes
}

// heldSection is what applies in the use part's variable now, grant by
// grant with its lifetime, then the routes asked that none of them names,
// compared as written (ADR 26-11-03-329 §5), and what replacing would do to
// each.
func heldSection(a approval) section {
	variable := console.VariableOf(a.req)
	s := section{label: "holds in " + variable + " now"}
	if len(a.held) == 0 {
		s.fields = append(s.fields, field{label: "nothing", value: "an addition is the first grant there", tone: toneDim})
	}
	// What a replacement does to each grant is bindingplan's rule, the one
	// the server applies; a lifetime of forever cuts nothing short.
	var until time.Time
	if a.ttl > 0 {
		until = time.Now().Add(a.ttl)
	}
	planned := make([]bindingplan.Held, 0, len(a.held))
	for _, grant := range a.held {
		planned = append(planned, bindingplan.Held{ID: grant.BindingID, Adds: grant.Adds, Own: grant.From == console.BindingSandbox, Expires: grant.Expires})
	}
	fates := map[string]bindingplan.HeldFate{}
	for _, step := range bindingplan.Replacing(planned, until) {
		fates[step.Held.ID] = step.Fate
	}
	var named [][]string
	for _, grant := range a.held {
		what := "binding"
		if grant.Adds {
			what = "addition"
		}
		value := strings.Join(console.FormatRoutes(grant.Routes), "; ") + " · no expiry"
		if !grant.Expires.IsZero() {
			value = strings.Join(console.FormatRoutes(grant.Routes), "; ") + " · until " + grant.Expires.Local().Format("Jan 2 15:04")
		}
		switch grant.From {
		case console.BindingHarnessConfig:
			value += " · from its harness config"
		case console.BindingProject:
			value += " · from its project"
		}
		s.fields = append(s.fields, field{label: what, value: value})
		named = append(named, console.FormatRoutes(grant.Routes))
		var fate string
		switch fates[grant.BindingID] {
		case bindingplan.Goes:
			fate = "goes"
		case bindingplan.Shadowed:
			fate = "is shadowed for " + lifetime.Label(a.ttl)
		case bindingplan.Shortened:
			fate = "lasts only " + lifetime.Label(a.ttl)
		}
		if fate != "" {
			s.fields = append(s.fields, field{label: "", value: "if replaced, it " + fate, tone: toneAlert})
		}
	}
	var asked []secretroute.Route
	if a.req.Use != nil {
		asked = a.req.Use.Routes
	}
	if fresh := bindingplan.NewRoutes(console.FormatRoutes(asked), named); len(fresh) == 0 {
		s.fields = append(s.fields, field{label: "new routes", value: "none, as written", tone: toneDim})
	} else {
		s.fields = append(s.fields, field{label: "new routes", value: strings.Join(fresh, "; ") + " (as written)", tone: toneAccent})
	}
	return s
}

// roleLines is one role: its name and arguments, its description marked as
// the discobox's own, what its routes grant, and anything a reader must not
// miss about it.
func roleLines(role requestrole.Role) []line {
	head := role.Name
	if args := role.ArgsText(); args != "" {
		head += ": " + args
	}
	lines := []line{{text: head, bullet: true, tone: toneAccent}}
	if role.Description != "" {
		lines = append(lines, line{text: "“" + role.Description + "” — the discobox's words", tone: toneDim})
	}
	lines = append(lines, line{text: "grants " + requestrole.Summary(role.Routes)})
	for _, note := range role.Notes() {
		lines = append(lines, line{text: note, tone: toneAlert})
	}
	return lines
}

func credentialName(req console.CredentialRequest) string {
	if req.APIAccess {
		return "discobox API access"
	}
	if req.Adoption != nil {
		if req.Adoption.ChildName != "" {
			return "adoption of " + req.Adoption.ChildName
		}
		return "adoption of " + req.Adoption.ChildID
	}
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

// askNewCredentialForm stores a credential the project does not have yet and
// answers the request with it: the secrets screen's form
// (console.NewSecretForm), filled from the request — named as it asks, or the
// first free name when the project already holds that one, and with the routes
// it asks for — every field still the person's to change. It comes after the
// lifetime, so the token typed into it is never held by a card that can be
// gone back to: Esc here is the request, still waiting.
func (m *Model) askNewCredentialForm(a approval) tea.Cmd {
	q := console.NewSecretForm(console.NewSecretAsk{
		Name:      credentialName(a.req),
		Secrets:   a.secrets,
		Routes:    a.req.Routes(),
		Approving: true,
		BindFor:   a.ttl,
	})
	d := formDialog("New credential", q, func(secret console.NewSecret) tea.Cmd { return m.createAndApprove(a, secret) })
	d.sections = []section{{label: "approves the request", fields: []field{
		{label: "credential", value: credentialName(a.req), tone: toneAccent},
		{label: "bound for", value: lifetime.Label(a.ttl), tone: toneAccent},
	}}}
	// Esc second: a key line is cut from its tail, and the way back — the
	// request still waiting — is the one thing it must always say.
	d.keys = []hint{pressing("enter stores and approves", "enter"), pressing("esc back, nothing stored", "esc"), says("↑↓ moves"), says("←→ chooses")}
	d.onCancel = func() tea.Cmd { return m.askAboutCredential(a.req, a.secrets, a.held) }
	m.dialog = d
	return nil
}

// createAndApprove stores the new credential and answers with it, for the
// lifetime chosen. A secret stored from a command is run for its first value
// by the data source. A refusal after the secret was stored says so: the
// secret stays, and the request still waits.
func (m *Model) createAndApprove(a approval, secret console.NewSecret) tea.Cmd {
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
		err = m.ds.ApproveCredentialRequest(m.ctx, req.Server, console.Approval{
			RequestID:     req.ID,
			SecretID:      stored.ID,
			TTLSeconds:    lifetime.Seconds(ttl),
			Use:           a.use,
			Delegate:      a.delegate,
			UseScope:      a.useScope,
			DelegateScope: a.delegateScope,
			UseAs:         string(a.usePlanned()),
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
	req     console.CredentialRequest
	secrets []console.Secret
	secret  console.Secret
	// fresh is a new credential rather than a secret the project holds:
	// secret is empty, and the new-secret form is filled in after the
	// lifetime.
	fresh bool
	ttl   time.Duration
	// raw is the delivery the person chose for the value itself, empty for a
	// sentinel (ADR 26-10-27-840 §2).
	raw string
	// use and delegate are each part's answer, PartApprove or PartDeny, empty
	// for a part the request does not have. A request with one part is
	// approved by choosing a secret; one with both is answered part by part
	// on separate cards; partsChosen marks the staged first decision.
	use, delegate           string
	useScope, delegateScope string
	partsChosen             bool
	// standing is the asking discobox's own bindings of the chosen secret:
	// what an approved part replaces.
	standing []console.SecretBinding
	parents  []console.SecretBinding
	// held is what applies in the use part's variable now, from whichever
	// level: what an addition stacks on (ADR 26-11-03-329 §5).
	held []console.HeldGrant
	// useAs is how an approved use part is granted, as the request asked
	// until the person chooses a mode on the use card.
	useAs bindingplan.As
}

// newApproval is an answer to req, its parts approved until asked otherwise.
// held is what applies in the use part's variable now.
func newApproval(req console.CredentialRequest, secrets []console.Secret, held []console.HeldGrant) approval {
	a := approval{req: req, secrets: secrets, held: held, useAs: bindingplan.Replace}
	if req.Use != nil || req.Delegate == nil {
		a.use = console.PartApprove
	}
	if req.Use != nil && req.Use.Add {
		a.useAs = bindingplan.Add
	}
	if req.Delegate != nil {
		a.delegate = console.PartApprove
	}
	return a
}

// chooseSecret reads live standing grants before the final confirmation.
func (m *Model) chooseSecret(a approval, secretID string) tea.Cmd {
	if holder, _ := console.HeldBy(a.held); holder.SecretID == secretID {
		a.secret = console.HeldSecret(a.secrets, holder)
	}
	for _, secret := range a.secrets {
		if secret.ID == secretID {
			a.secret = secret
		}
	}
	if a.req.SandboxID == "" {
		return m.confirmCredentialApproval(a)
	}
	// What the approval would replace is read before it is decided, so the
	// cards that follow can say it.
	wait := statusDialog("Credential request", "reading what the discobox holds of "+a.secret.Name+"…")
	m.dialog = wait
	return func() tea.Msg {
		bindings, err := m.ds.Bindings(m.ctx, a.req.Server, console.BindingFilter{SecretID: a.secret.ID})
		var standing []console.SecretBinding
		for _, b := range bindings {
			if b.TargetKind == console.BindingSandbox && b.TargetID == a.req.SandboxID {
				standing = append(standing, b)
			}
		}
		var held []console.HeldGrant
		if err == nil && a.req.Use != nil && a.req.EnvVar != "" {
			held, err = m.ds.HeldGrants(m.ctx, a.req.Server, a.req.SandboxID, a.req.EnvVar)
		}
		var parents []console.SecretBinding
		byID := map[string]console.SecretBinding{}
		for _, b := range bindings {
			byID[b.ID] = b
		}
		seen := map[string]bool{}
		for _, b := range standing {
			for id := b.ParentID; id != "" && !seen[id]; {
				seen[id] = true
				parent, ok := byID[id]
				if !ok {
					break
				}
				parents = append(parents, parent)
				id = parent.ParentID
			}
		}
		return standingLoadedMsg{wait: wait, parents: parents, approval: a, standing: standing, held: held, err: err}
	}
}

// standingLoaded opens the final confirmation with what the approval replaces.
func (m *Model) standingLoaded(msg standingLoadedMsg) tea.Cmd {
	if m.dialog != msg.wait {
		return nil
	}
	if msg.err != nil {
		m.dialog = errorDialog("Credential request", fmt.Sprintf("Cannot read what the discobox holds of %s: %v\n\nThe request is still waiting.", msg.approval.secret.Name, msg.err))
		return nil
	}
	a := msg.approval
	a.standing, a.held, a.parents = msg.standing, msg.held, msg.parents
	return m.confirmCredentialApproval(a)
}

// finishApproval makes the binding.
//
// The lifetime rides on the approval itself rather than being left out for the
// server to default: the default is the secret's own ceiling, which most
// credentials do not have, and a window that stayed quiet about it handed out
// permanent credentials without ever saying the word.
func (m *Model) finishApproval(a approval) tea.Cmd {
	what := "approving for " + lifetime.Label(a.ttl) + "…"
	sent := console.Approval{
		RequestID:     a.req.ID,
		SecretID:      a.secret.ID,
		TTLSeconds:    lifetime.Seconds(a.ttl),
		Raw:           a.raw,
		Use:           a.use,
		Delegate:      a.delegate,
		UseScope:      a.useScope,
		DelegateScope: a.delegateScope,
	}
	if a.use == console.PartApprove && a.req.Use != nil {
		// Always said, so the binding made is the one the card showed.
		sent.UseAs = string(a.useAs)
		if a.useAs == bindingplan.Add {
			what = "approving an addition for " + lifetime.Label(a.ttl) + "…"
		}
	}
	if a.raw != "" {
		what = "approving the value itself for " + lifetime.Label(a.ttl) + "…"
	}
	m.dialog = statusDialog("Credential request", what)
	return func() tea.Msg {
		err := m.ds.ApproveCredentialRequest(m.ctx, a.req.Server, sent)
		return credentialAnsweredMsg{request: a.req, approved: true, ttl: a.ttl, err: err}
	}
}

func (m *Model) denyCredential(req console.CredentialRequest) tea.Cmd {
	if req.APIAccess {
		return m.answerAPIAccess(req, false)
	}
	if req.Adoption != nil {
		return m.answerAdoption(req, false)
	}
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
	if msg.request.Adoption != nil || msg.request.APIAccess {
		verb := "denied"
		if msg.approved {
			verb = "approved"
		}
		return tea.Batch(m.loadCredentialRequests(), m.report(false, "%s %s", verb, credentialName(msg.request)))
	}
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
	pending := m.pendingFor(m.Box)
	if len(pending) == 0 {
		return ""
	}
	st := m.st
	subject := credentialName(pending[0])
	what := "credential request"
	if pending[0].APIAccess {
		what = "API access request"
	} else if pending[0].Adoption != nil {
		what = "adoption request"
	} else if pending[0].Trust != nil {
		// The name already says the host, so it is not said twice.
		what = "trust request"
	} else if pending[0].Refresh != nil {
		// A token this discobox already has, wanting a new value.
		what = "refresh request"
	} else if host := pending[0].Where(); host != "" {
		subject += " for " + host
	}
	if len(pending) > 1 {
		one, many := "credential request", "credential requests"
		trusts, adoptions, refreshes := 0, 0, 0
		for _, req := range pending {
			switch {
			case req.Adoption != nil:
				adoptions++
			case req.Trust != nil:
				trusts++
			case req.Refresh != nil:
				refreshes++
			}
		}
		switch {
		case trusts+adoptions+refreshes == 0:
		case trusts == len(pending):
			one, many = "trust request", "trust requests"
		case adoptions == len(pending):
			one, many = "adoption request", "adoption requests"
		case refreshes == len(pending):
			one, many = "refresh request", "refresh requests"
		default:
			one, many = "request", "requests"
		}
		what, subject = plural(len(pending), one, many), "oldest: "+subject
	}
	body := st.attentionText.Render(what) +
		st.attentionHint.Render("  ·  ") + st.attentionText.Render(subject)
	call := bannerChip(st, "click to answer", bannerPulseHues[m.pulse%len(bannerPulseHues)])
	return bannerRow(st, alertPalette, width, body, call, m.leader()+" "+credentialsLeaderKey)
}

// credentialErrorSection says which request is still waiting, under the
// server's own wording of the refusal: that wording is the half that says what
// to do next, so it is left whole as the dialog's body and this stands under it.
func credentialErrorSection(req console.CredentialRequest) section {
	if req.Adoption != nil {
		return section{label: "still waiting", fields: []field{{label: "request", value: credentialName(req), tone: toneAccent}}}
	}
	if req.Trust != nil {
		return section{label: "still waiting", fields: []field{{label: "trust", value: req.Host, tone: toneAccent}}}
	}
	fields := []field{{label: "credential", value: credentialName(req), tone: toneAccent}}
	if where := req.WhereRows(); where != "" {
		fields = append(fields, field{label: "for", value: where, tone: toneAccent})
	}
	return section{label: "still waiting", fields: fields}
}
