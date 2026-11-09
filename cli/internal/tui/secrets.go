package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/cli/internal/refreshcmd"
	"github.com/discobox-ai/discobox/pkg/secretroute"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// The secrets screen: the project's credentials, what stands on them, and what
// is waiting to. It is the operator's side of the credential inbox — the list's
// mark and the workspace's banner answer one discobox's question, and this
// answers "what does this project hold, and who may use it".
//
// It is the harnesses screen's shape on purpose: a list on a function key, one
// letter per action, and every action a call to the same API the `discobox
// secret` commands make. A window that could approve something the CLI could
// not would be a second policy.

// secretsKey opens the screen, after F1 help, F2 the editor and F3 the
// harnesses — the prompt takes every letter, so a screen of its own goes on a
// function key.
var secretsKey = bareKey("credentials")

// SecretsKeyName is how that key is spelled to the user.
var SecretsKeyName = keyLabel(secretsKey)

// secretList is the screen's list: the project's secrets and where the cursor
// is in them.
type secretList struct {
	all      []console.Secret
	bindings []console.SecretBinding
	cursor   int
	offset   int

	// loaded distinguishes a project with no secrets from a listing that has
	// not landed, which look the same in all and mean opposite things.
	loaded bool

	// refused is which secrets an upstream has rejected, by ID. A credential
	// that does not work is the first thing to know about it on this screen —
	// ahead of what stands on it — because it is the answer to the question
	// that brought somebody here (ADR 0132).
	refused map[string]console.SecretRejection

	width, height int

	now func() time.Time

	// drawn is which rows the last render put on screen; see zones.go.
	drawn drawn
}

func newSecretList() *secretList { return &secretList{now: time.Now} }

// setAll takes a refreshed listing, keeping the cursor on the secret it was on
// rather than on the row number it was at.
// clear forgets the listing, for a server whose secrets have not been read yet.
func (l *secretList) clear() {
	l.all, l.bindings, l.loaded, l.cursor, l.offset = nil, nil, false, 0, 0
}

func (l *secretList) setAll(all []console.Secret) {
	l.loaded = true
	var onID string
	if s := l.current(); s != nil {
		onID = s.ID
	}
	l.all = all
	if onID != "" {
		for i := range l.all {
			if l.all[i].ID == onID {
				l.cursor = i
				break
			}
		}
	}
	l.clamp()
}

// setRefused takes the credentials an upstream has refused, so the rows can say
// which of them does not work.
func (l *secretList) setRefused(rejections []console.SecretRejection) {
	refused := make(map[string]console.SecretRejection, len(rejections))
	for _, rejection := range rejections {
		if _, seen := refused[rejection.SecretID]; !seen {
			refused[rejection.SecretID] = rejection
		}
	}
	l.refused = refused
}

// setBindings takes the project's bindings, which the rows count and the
// bindings dialog reads. They are listed for the project rather than per row:
// one read answers every row, and a count per row would be a call per row.
func (l *secretList) setBindings(bindings []console.SecretBinding) {
	l.bindings = bindings
}

// binding is one of the project's bindings, by its ID.
func (l *secretList) binding(id string) (console.SecretBinding, bool) {
	for _, b := range l.bindings {
		if b.ID == id {
			return b, true
		}
	}
	return console.SecretBinding{}, false
}

// bindingsFor is the live bindings of one secret. One that has lapsed is gone
// at the server's next sweep, and until then it carries nothing, so it is not
// listed: a list of what may use a credential is what may use it now.
func (l *secretList) bindingsFor(secretID string) []console.SecretBinding {
	now := l.now()
	var live []console.SecretBinding
	for _, b := range l.bindings {
		if b.SecretID == secretID && bindingLive(b, now) {
			live = append(live, b)
		}
	}
	return live
}

// liveBindings counts the bindings still standing on a secret. It is counted
// when drawn rather than when the listing lands: a binding lapses between
// reads, and a count taken at the read would go on saying a credential is in
// use after nothing may use it.
func (l *secretList) liveBindings(secretID string) int {
	return len(l.bindingsFor(secretID))
}

// bindingLive is whether a binding still carries anything: it never expires,
// or has not yet.
func bindingLive(b console.SecretBinding, now time.Time) bool {
	return b.Expires.IsZero() || b.Expires.After(now)
}

func (l *secretList) current() *console.Secret {
	if l.cursor < 0 || l.cursor >= len(l.all) {
		return nil
	}
	return &l.all[l.cursor]
}

func (l *secretList) move(delta int) {
	l.cursor += delta
	l.clamp()
}

func (l *secretList) moveTo(i int) {
	l.cursor = i
	l.clamp()
}

func (l *secretList) clamp() {
	if l.cursor >= len(l.all) {
		l.cursor = len(l.all) - 1
	}
	if l.cursor < 0 {
		l.cursor = 0
	}
	if l.height <= 0 {
		l.offset = 0
		return
	}
	if l.cursor < l.offset {
		l.offset = l.cursor
	}
	if l.cursor >= l.offset+l.height {
		l.offset = l.cursor - l.height + 1
	}
	// A window that grew shows the rows it now has room for, rather than
	// staying scrolled past them.
	if l.offset > len(l.all)-l.height {
		l.offset = len(l.all) - l.height
	}
	if l.offset < 0 {
		l.offset = 0
	}
}

func (l *secretList) view(st *styles, z *zones, focused bool) string {
	titleStyle := st.titleList
	if !focused {
		titleStyle = st.titleDim
	}
	blank := strings.Repeat(" ", max(l.width, 0))
	out := []string{renderTitle(titleStyle, "Secrets", plural(len(l.all), "secret", "secrets"), l.width)}

	body := make([]string, 0, l.height)
	if len(l.all) == 0 {
		body = append(body, st.dimText.Render(pad("  no secrets in this project yet — n stores one", l.width)))
	}
	l.drawn = drawn{top: len(out), first: l.offset}
	for i := l.offset; i < len(l.all) && len(body) < l.height; i++ {
		body = append(body, l.row(st, l.all[i], i, focused))
		l.drawn.count++
	}
	for len(body) < l.height {
		body = append(body, blank)
	}
	block := append(out, body...)
	z.markList(hitSecretRow, l.drawn, l.width, len(block))
	return lipgloss.JoinVertical(lipgloss.Left, block...)
}

// row draws one secret: what it is called, where it may be sent, and what
// stands on it. Columns drop off the right end as the terminal narrows; the
// name never goes.
//
// Its kind and its binding limit are not here. They are facts about a credential
// rather than ways to tell two of them apart, and a row carrying every field
// is a row nobody reads — Enter opens the secret, and they are there.
func (l *secretList) row(st *styles, s console.Secret, i int, focused bool) string {
	atCursor := i == l.cursor && focused

	tail := ""
	addCol := func(text string, w int) {
		if l.width-lipgloss.Width(tail)-w < 20 {
			return
		}
		tail += padANSI(text, w)
	}

	// Its routes, which are the whole of what limits where this credential
	// may travel. One with none says so rather than showing a blank: blank
	// reads as "not loaded", and this is a fact about the secret.
	where := console.RoutesText(s.Routes)
	whereStyle := st.name
	if len(s.Routes) == 0 {
		where, whereStyle = "sent nowhere", st.dimText
	}
	addCol(whereStyle.Render(pad(where, 22)), 23)
	bindings := "—"
	bindingStyle := st.dimText
	if n := l.liveBindings(s.ID); n > 0 {
		bindings, bindingStyle = plural(n, "binding", "bindings"), st.statusOK
	}
	if _, refused := l.refused[s.ID]; refused {
		// What stands on a credential matters less than whether it works. A
		// row saying "2 bindings" about a credential the upstream refuses is
		// answering a question nobody is asking.
		bindings, bindingStyle = "refused", st.statusER
	}
	addCol("  "+bindingStyle.Render(pad(bindings, 10)), 13)
	addCol(st.dimText.Render(pad(secretAge(s, l.now()), 8)), 9)

	marker := "  "
	if atCursor {
		marker = st.key.Render("❯") + " "
	}
	nameW := max(l.width-lipgloss.Width(marker)-lipgloss.Width(tail), 4)
	nameStyle := st.name
	if atCursor {
		nameStyle = st.cursorName
	}
	line := padANSI(marker+padANSI(nameStyle.Render(truncate(s.Name, nameW)), nameW)+tail, l.width)
	if atCursor {
		return highlight(st, line, colHighlightBG)
	}
	return line
}

// secretAge is the age column, the same shape the other screens use.
func secretAge(s console.Secret, now time.Time) string {
	return console.Ago(s.Updated, now)
}

// bindingLimit is a secret's ceiling on binding lifetimes, said in the words it was
// chosen in — a limit set as "1 month" reads back as one, not as 720h — since
// the same limit is read on the credential dialog through the same package.
// Zero is not absence: it is the answer "no limit", so it is spelled "forever"
// rather than borrowing shortDuration's "never", which would read as the
// opposite of what it means.
func bindingLimit(d time.Duration) string {
	return lifetime.Label(d)
}

// shortDuration renders the time left on a binding the way a column has room
// for: "1h", "30m", "never" for one that does not expire. It is for countdowns,
// not for a lifetime somebody chose — those are bindingLimit's.
func shortDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "never"
	case d >= time.Hour:
		return itoa(int(d/time.Hour)) + "h"
	case d >= time.Minute:
		return itoa(int(d/time.Minute)) + "m"
	default:
		return itoa(int(d/time.Second)) + "s"
	}
}

// ---------------------------------------------------------------------------
// the screen

type secretsLoadedListMsg struct {
	// server is the server the listing was read from; see harnessesLoadedMsg.
	server   string
	secrets  []console.Secret
	bindings []console.SecretBinding
	err      error
}

// secretsTable is which of the screen's tables has the keys: they are one
// screen read downwards, in this order.
type secretsTable int

const (
	tableSecrets secretsTable = iota
	tableRequests
)

// secretActionMsg carries a completed action back into the update loop, so the
// listing is re-read from one place however the action was started.
type secretActionMsg struct {
	did string
	err error
	// after is where a finished action goes on to, for one started from a
	// card that should come back. Nil leaves the screen.
	after func() tea.Cmd
}

func (m *Model) loadSecrets() tea.Cmd {
	server := m.configServer()
	return func() tea.Msg {
		secrets, err := m.ds.Secrets(m.ctx, server)
		if err != nil {
			return secretsLoadedListMsg{server: server, err: err}
		}
		bindings, err := m.ds.Bindings(m.ctx, server, console.BindingFilter{})
		if err != nil {
			return secretsLoadedListMsg{server: server, err: err}
		}
		return secretsLoadedListMsg{server: server, secrets: secrets, bindings: bindings}
	}
}

func (m *Model) openSecrets() tea.Cmd {
	m.commands.Use("credentials")
	m.secretsOpen = true
	m.optionsOpen = false
	m.harnessesOpen = false
	m.layout()
	return m.loadSecrets()
}

func (m *Model) closeSecrets() {
	m.secretsOpen = false
	m.layout()
}

func (m *Model) secretsLoaded(msg secretsLoadedListMsg) tea.Cmd {
	if m.session.ServerName(msg.server) != m.configServer() {
		// Read for a server the header has since moved off; see
		// harnessesLoaded.
		return nil
	}
	if msg.err != nil {
		return m.report(true, "cannot read secrets: %v", msg.err)
	}
	m.secrets.setAll(msg.secrets)
	m.secrets.setBindings(msg.bindings)
	m.layout()
	return nil
}

// updateSecrets handles the screen. A letter is a command here, the way it is
// on the discobox list.
func (m *Model) updateSecrets(msg tea.KeyPressMsg) tea.Cmd {
	// What both tables answer to.
	switch keyName(msg) {
	case "esc", "q":
		m.closeSecrets()
		return nil
	case "tab":
		m.table = m.nextTable()
		return nil
	case "?":
		m.dialog = m.helpDialog()
		return nil
	case "r":
		return tea.Batch(m.loadSecrets(), m.loadCredentialRequests(), status("refreshing"))
	case "left", "right":
		// The header's server, as on the harnesses screen.
		return m.cycleServer(serverStep(msg))
	case credentialsKey:
		return m.openWaitingRequest()
	}
	switch m.table {
	case tableRequests:
		return m.updateRequestTable(msg)
	}
	switch keyName(msg) {
	case "down", "j":
		// Off the bottom of the secrets is the requests table, the way off the
		// bottom of the discobox list is the prompt: the tables are one screen
		// read downwards, not two panes to be switched between.
		if below := m.tableBelow(tableSecrets); m.secrets.cursor >= len(m.secrets.all)-1 && below != tableSecrets {
			m.table = below
			return nil
		}
		m.secrets.move(1)
	case "up", "k":
		m.secrets.move(-1)
	case "pgdown":
		m.secrets.move(m.secrets.height)
	case "pgup":
		m.secrets.move(-m.secrets.height)
	case "home", "g":
		m.secrets.moveTo(0)
	case "end", "G":
		m.secrets.moveTo(len(m.secrets.all) - 1)
	case "n":
		return m.newSecretForm()
	case "h":
		return m.loadBindingHistory()
	case "enter", "v":
		return m.showBindings()
	case "e":
		if secret := m.secrets.current(); secret != nil {
			return m.editSecretForm(m.configServer(), *secret)
		}
		return nil
	case "d":
		return m.confirmDeleteSecret()
	case bindingCreateKey:
		return m.askForBinding()
	}
	return nil
}

// updateRequestTable handles the lower table: moving in it, and answering the
// request under the cursor.
func (m *Model) updateRequestTable(msg tea.KeyPressMsg) tea.Cmd {
	switch keyName(msg) {
	case "up", "k":
		// Off the top is the secrets, which is where the answer comes from.
		if m.requestRows.cursor <= 0 {
			m.table = tableSecrets
			return nil
		}
		m.requestRows.move(-1)
	case "down", "j":
		m.requestRows.move(1)
	case "pgup":
		m.requestRows.move(-m.requestRows.height)
	case "pgdown":
		m.requestRows.move(m.requestRows.height)
	case "home", "g":
		m.requestRows.moveTo(0)
	case "end", "G":
		m.requestRows.moveTo(len(m.requestRows.all) - 1)
	case "enter", "v":
		if req := m.requestRows.current(); req != nil {
			return m.openCredentialRequest(*req)
		}
	}
	return nil
}

// tableBelow is the table ↓ leaves from's last row for: the requests when
// there is one to land on, or from itself when there is none. An empty table
// says so in a line and has nothing to put a cursor on.
func (m *Model) tableBelow(from secretsTable) secretsTable {
	if from == tableSecrets && len(m.requestRows.all) > 0 {
		return tableRequests
	}
	return from
}

// nextTable is where Tab goes: down to the requests when there are any, and
// round to the secrets.
func (m *Model) nextTable() secretsTable {
	if below := m.tableBelow(m.table); below != m.table {
		return below
	}
	return tableSecrets
}

// tableName is a table as the key line names it.
func tableName(t secretsTable) string {
	if t == tableRequests {
		return "requests"
	}
	return "secrets"
}

// bindingCreateKey binds the secret under the cursor by hand: the binding an
// operator makes because they already know the answer, rather than waiting for
// an agent to ask and answering then.
const bindingCreateKey = "b"

// askForBinding opens the binding as one form: what it is on, what for, as
// which variable, where it may be sent, and for how long, read against each
// other on one card. The rows a given answer makes irrelevant are taken away
// rather than asked.
func (m *Model) askForBinding() tea.Cmd {
	secret := m.secrets.current()
	if secret == nil {
		return nil
	}
	// A binding names a discobox on the secret's own server: one on another
	// server is not one this secret can ever reach.
	var boxes []console.Sandbox
	for _, box := range m.list.All() {
		if m.session.ServerName(box.Server) == m.configServer() {
			boxes = append(boxes, box)
		}
	}
	secretName := secret.Name
	d := formDialog("Bind "+secret.Name, console.BindingForm(*secret, m.harnesses.all, boxes), func(binding console.NewBinding) tea.Cmd {
		return m.makeBinding(secretName, binding)
	})
	d.sections = []section{{lines: []line{
		{text: "a binding hands this credential over before anything asks for it", tone: toneDim},
	}}}
	d.keys = []hint{says("↑↓ moves"), says("←→ chooses"), pressing("enter binds", "enter"), pressing("esc leaves the secret alone", "esc")}
	m.dialog = d
	return nil
}

func (m *Model) makeBinding(secretName string, binding console.NewBinding) tea.Cmd {
	m.dialog = statusDialog("Secrets", "binding…")
	server := m.configServer()
	return func() tea.Msg {
		_, err := m.ds.CreateBinding(m.ctx, server, binding)
		did := "bound " + secretName
		if binding.Use {
			did += " as " + binding.EnvVar
		}
		return secretActionMsg{did: did, err: err}
	}
}

// openWaitingRequest answers a request from here, where there is no discobox
// under the cursor to take it from: the oldest one waiting on the server this
// screen shows, including the ones no discobox owns. Only that server's,
// because its secrets are the only ones that can answer it.
func (m *Model) openWaitingRequest() tea.Cmd {
	var oldest *console.CredentialRequest
	for i := range m.requestRows.all {
		if oldest == nil || m.requestRows.all[i].Created.Before(oldest.Created) {
			oldest = &m.requestRows.all[i]
		}
	}
	if oldest == nil {
		return m.report(false, "nothing is waiting")
	}
	return m.openCredentialRequest(*oldest)
}

// showBindings lists what stands on the secret under the cursor, and lets one
// be taken back. Unbinding is immediate — the credential stops resolving — so
// the row says what each binding covers before it offers to.
func (m *Model) showBindings() tea.Cmd {
	secret := m.secrets.current()
	if secret == nil {
		return nil
	}
	bindings := m.secrets.bindingsFor(secret.ID)
	// Making one is the first row, so a secret with no bindings is not a dead
	// end: this is where somebody looking at a credential decides who gets
	// it, and being told to go and use another command is not deciding.
	items := make([]action, 0, len(bindings)+1)
	items = append(items, action{
		key:     bindingCreateItem,
		press:   bindingCreateKey,
		label:   "Bind this secret…",
		detail:  "hand it over before anything asks",
		enabled: true,
	})
	for _, b := range bindings {
		items = append(items, action{
			key:     b.ID,
			label:   bindingLabel(b),
			detail:  m.bindingDetail(b, m.secrets.now()),
			enabled: true,
		})
	}
	// What the credential is, before what may use it. For an OAuth credential
	// that is most of what there is to know about it — where it renews, what
	// the grant may do, and when the access token goes stale — and none of it
	// is the credential.
	answer := "nothing is bound to it"
	if len(bindings) > 0 {
		answer = plural(len(bindings), "binding stands on it", "bindings stand on it")
	}
	// Enter reads, x takes back. The other way round put unbinding a credential
	// one keystroke from looking at one, on the key that means "open this"
	// everywhere else in the window.
	d := actionsDialog(secret.Name, "", items, func(result string) tea.Cmd {
		if result == bindingCreateItem {
			return m.askForBinding()
		}
		binding, ok := m.secrets.binding(result)
		if !ok {
			return m.report(true, "that binding is no longer there")
		}
		return m.reviewBinding(secret.Name, binding, m.showBindings)
	})
	d.titleRight = secretAge(*secret, m.secrets.now())
	d.sections = describeSecret(*secret, m.secrets.now())
	d.answerLabel = answer
	d.keys = []hint{pressing("enter reads", "enter"), keyed(unbindKey, unbindKey, "unbinds"), pressing("esc leaves them alone", "esc")}
	d.altKey = unbindKey
	d.alt = func(bindingID string) tea.Cmd {
		if bindingID == bindingCreateItem {
			return nil
		}
		binding, _ := m.secrets.binding(bindingID)
		return m.confirmUnbind(secret.Name, binding, m.showBindings, nil)
	}
	m.dialog = d
	return nil
}

// describeSecret is what a credential is, in the words that can be shown. The
// value is never among them; for an OAuth credential the renewal material is
// not either, and what is left — where it renews, whose OAuth grant it is, what it
// may do, when it goes stale — is the part somebody managing it needs.
func describeSecret(secret console.Secret, now time.Time) []section {
	limit := "none — bindings of it may last forever"
	if secret.MaxTTL > 0 {
		limit = "at most " + bindingLimit(secret.MaxTTL)
	}
	credential := section{label: "the credential", fields: []field{
		{label: "kind", value: secret.Type},
		{label: "binding limit", value: limit},
	}}
	// Where it may be sent, whoever holds it (ADR 26-10-27-880 §4). None is
	// nowhere, and says so: a credential no request may carry.
	if len(secret.Routes) == 0 {
		credential.fields = append(credential.fields, field{label: "may be sent to", value: "nowhere — it has no routes", tone: toneAlert})
	}
	for i, route := range secret.Routes {
		label := "route"
		if i > 0 {
			label = "or route"
		}
		open := route
		open.Blocked = false
		where := secretroute.Format(open)
		value, tone := where+" · allowed", toneDim
		if route.Blocked {
			value, tone = where+" · blocked", toneAccent
		}
		credential.fields = append(credential.fields, field{label: label, value: value, tone: tone})
	}
	// How the value is renewed, for a token that has a lifetime
	// (ADR 26-09-25-122).
	if len(secret.RefreshCommand) > 0 {
		credential.fields = append(credential.fields, field{label: "renewed by", value: refreshcmd.Join(secret.RefreshCommand), tone: toneAccent})
	}
	if secret.ValueTTL > 0 {
		credential.fields = append(credential.fields, field{label: "a value lasts", value: lifetime.Label(secret.ValueTTL)})
	}
	// What stands in for it inside a discobox. A chosen shape is said as one,
	// since it is the one that outlives a new value.
	if secret.Format != "" {
		f := field{label: "sentinels look like", value: secret.Format + " · read from the value", tone: toneDim}
		if secret.FormatSet {
			f = field{label: "sentinels look like", value: secret.Format + " · set", tone: toneAccent}
		}
		credential.fields = append(credential.fields, f)
	}
	if !secret.StaleAt.IsZero() {
		stale := "stale " + console.Ago(secret.StaleAt, now) + "; renewed when next needed"
		if secret.StaleAt.After(now) {
			stale = "stale at " + secret.StaleAt.Local().Format("15:04")
		}
		credential.fields = append(credential.fields, field{label: "value", value: stale, tone: toneDim})
	}
	if secret.Exchange != nil {
		exchange := section{label: "exchanged"}
		exchange.fields = append(exchange.fields, field{label: "at", value: secret.Exchange.URL})
		exchange.fields = append(exchange.fields, field{label: "holds", value: strings.Join(secret.Exchange.Fields, " ")})
		if !secret.Exchange.TokenExpiresAt.IsZero() {
			exchange.fields = append(exchange.fields, field{label: "token stale", value: accessTokenExpiry(secret.Exchange.TokenExpiresAt, now)})
		}
		return []section{credential, exchange}
	}
	if secret.OAuth == nil {
		return []section{credential}
	}

	oauth := section{label: "oauth"}
	add := func(label, value string) {
		if value != "" {
			oauth.fields = append(oauth.fields, field{label: label, value: value})
		}
	}
	add("renews at", secret.OAuth.TokenURL)
	add("client", secret.OAuth.ClientID)
	add("may do", strings.Join(secret.OAuth.Scopes, " "))
	add("plan", secret.OAuth.SubscriptionType)
	if !secret.OAuth.AccessTokenExpiresAt.IsZero() {
		oauth.fields = append(oauth.fields, field{label: "token stale", value: accessTokenExpiry(secret.OAuth.AccessTokenExpiresAt, now)})
	}
	if !secret.OAuth.Refreshable {
		// The one thing worth saying loudly about an oauth credential: it
		// cannot renew, so it will expire and stay expired.
		oauth.lines = append(oauth.lines, line{text: "⚠ cannot renew itself: no refresh token or nowhere to spend it", tone: toneAlert})
	}
	return []section{credential, oauth}
}

// accessTokenExpiry says when the access token goes stale, and whether it
// already has — the control plane refreshes it on the way past, so a stale one
// is ordinary rather than broken.
func accessTokenExpiry(at, now time.Time) string {
	if at.Before(now) {
		return "expired " + console.Ago(at, now) + ", renewed on next use"
	}
	return "in " + shortDuration(at.Sub(now).Round(time.Minute))
}

// bindingCreateItem is the row that starts a binding from inside the bindings
// list, which is where somebody is already looking at what may use a secret.
const bindingCreateItem = "binding:new"

// unbindKey takes the highlighted binding back from the review list.
const unbindKey = "x"

// bindingTarget is what a binding is on, as it is said to a person: the
// project, a harness by name, or a discobox by name. The wire word for a
// harness is harnessConfig, because that is the resource; a person calls it
// the harness, and a window that says both teaches two names for one thing.
func (m *Model) bindingTarget(b console.SecretBinding) string {
	switch b.TargetKind {
	case console.BindingProject:
		return "every discobox in the project"
	case console.BindingHarnessConfig:
		for _, h := range m.harnesses.all {
			if h.ID == b.TargetID {
				return "harness " + h.Name
			}
		}
		return "harness " + b.TargetID
	case console.BindingSandbox:
		for _, box := range m.list.All() {
			if box.ID == b.TargetID {
				return "discobox " + box.Name
			}
		}
		return "discobox " + b.TargetID
	}
	return b.TargetKind + " " + b.TargetID
}

// bindingLabel is what a binding's row is named by: the variable it is in,
// which is what two bindings of one secret differ by most often, or that it
// is only given on.
func bindingLabel(b console.SecretBinding) string {
	switch {
	case b.Use && b.Delegate:
		return b.EnvVar + ", and given on"
	case b.Use && b.RawDelivery != "":
		return b.EnvVar + ", the value itself"
	case b.Use:
		return b.EnvVar
	default:
		return "given on"
	}
}

// bindingDetail is the rest of the row: what it is on, where it may be sent,
// and how long it has left.
func (m *Model) bindingDetail(b console.SecretBinding, now time.Time) string {
	parts := []string{m.bindingTarget(b)}
	if len(b.Routes) > 0 {
		parts = append(parts, "to "+console.RoutesText(b.Routes))
	}
	if b.ParentID != "" {
		parts = append(parts, "handed on")
	}
	return strings.Join(append(parts, bindingExpiry(b, now)), " · ")
}

// reviewBinding is the binding read in full: what it is on, where it may go,
// who made it, and how long it lasts. back is the list it was opened from.
func (m *Model) reviewBinding(secretName string, b console.SecretBinding, back func() tea.Cmd) tea.Cmd {
	where, tone := "where the secret's routes send it", toneDim
	if len(b.Routes) > 0 {
		where, tone = console.RouteRows(b.Routes)+"\n(within the secret's routes)", toneAccent
	}
	covers := section{label: "the binding", fields: []field{
		{label: "secret", value: secretName, tone: toneAccent},
		{label: "bound to", value: m.bindingTarget(b)},
	}}
	if b.Use {
		covers.fields = append(covers.fields, field{label: "as variable", value: b.EnvVar, tone: toneAccent})
	}
	if b.RawDelivery != "" {
		covers.fields = append(covers.fields, field{label: "hands over", value: rawDeliveryText(b.RawDelivery), tone: toneAccent})
	}
	if b.Delegate {
		covers.fields = append(covers.fields, field{label: "purpose", value: delegationPurpose, tone: toneAccent})
	}
	covers.fields = append(covers.fields, field{label: "may go to", value: where, tone: tone})
	if b.ParentID != "" {
		covers.fields = append(covers.fields, field{label: "handed on under", value: b.ParentID, tone: toneDim})
	}
	if b.CreatedBy != "" {
		covers.fields = append(covers.fields, field{label: "bound by", value: b.CreatedBy})
	}
	covers.fields = append(covers.fields, field{label: "expires", value: bindingLifeLeft(b, m.secrets.now())})

	d := actionsDialog("Binding", "", nil, func(string) tea.Cmd { return back() })
	d.titleRight = b.ID
	d.sections = []section{covers}
	d.wrapDetails = true
	d.keys = []hint{pressing("esc goes back", "esc")}
	// Back to the bindings you were reading, not to the list behind them. The
	// common next move after reading one is reading the next, or taking this
	// one back, and both are in the list this came from.
	d.onCancel = back
	m.dialog = d
	return nil
}

// confirmUnbind asks before taking a binding back, and says what stops
// working. back is the list the question was asked from, which No returns to,
// and after where a finished unbind goes on to (secretActionMsg.after).
func (m *Model) confirmUnbind(secretName string, b console.SecretBinding, back, after func() tea.Cmd) tea.Cmd {
	d := confirmDialog("Unbind", "", func(string) tea.Cmd { return m.unbind(b.ID, after) })
	d.titleRight = b.ID
	stops := line{text: "the credential stops resolving at once, wherever it is being used", tone: toneAlert}
	if !b.Use {
		stops = line{text: "it can no longer be given on, and what was given on under it goes too", tone: toneAlert}
	}
	fields := []field{
		{label: "secret", value: secretName, tone: toneAccent},
		{label: "bound to", value: m.bindingTarget(b)},
	}
	if b.Use {
		fields = append(fields, field{label: "as variable", value: b.EnvVar})
	}
	d.sections = []section{{
		label:  "what goes",
		fields: fields,
		lines: []line{
			stops,
			{text: "the request that produced it stays approved, because it is history", tone: toneDim},
		},
	}}
	d.answerLabel = "take this binding back?"
	d.defaultNo = true
	// No goes back to the bindings, the way leaving a binding you were
	// reading does: a question answered "not that one" should leave you where
	// the question was asked, not on the screen behind it.
	d.onCancel = back
	m.dialog = d
	return nil
}

// bindingLifeLeft is how long a binding has, for a field already labeled with
// what it is: "expires  expires in 47m" is the row's phrasing put under the
// row's own heading.
func bindingLifeLeft(b console.SecretBinding, now time.Time) string {
	switch {
	case b.Expires.IsZero():
		return "never"
	case b.Expires.Before(now):
		return "expired"
	default:
		return "in " + shortDuration(bindingLeft(b, now))
	}
}

// bindingLeft is the time a live binding has left, to the minute while there
// are minutes of it. Rounded to the minute under one, half a minute rounds to
// zero, which shortDuration says as "never": a binding about to lapse read as
// one that never would.
func bindingLeft(b console.SecretBinding, now time.Time) time.Duration {
	left := b.Expires.Sub(now)
	if left < time.Minute {
		return max(left.Round(time.Second), time.Second)
	}
	return left.Round(time.Minute)
}

func bindingExpiry(b console.SecretBinding, now time.Time) string {
	switch {
	case b.Expires.IsZero():
		return "never expires"
	case b.Expires.Before(now):
		return "expired"
	default:
		return "expires in " + shortDuration(bindingLeft(b, now))
	}
}

func (m *Model) unbind(bindingID string, after func() tea.Cmd) tea.Cmd {
	m.dialog = statusDialog("Secrets", "unbinding…")
	server := m.configServer()
	return func() tea.Msg {
		handedOn, err := m.ds.DeleteBinding(m.ctx, server, bindingID)
		did := "unbound"
		if handedOn > 0 {
			did += ", and " + plural(handedOn, "binding", "bindings") + " handed on under it"
		}
		return secretActionMsg{did: did, err: err, after: after}
	}
}

// newSecretForm stores a credential the project does not have yet.
func (m *Model) newSecretForm() tea.Cmd {
	d := formDialog("New secret", console.NewSecretForm(console.NewSecretAsk{}), m.storeSecret)
	d.keys = []hint{says("↑↓ moves"), says("←→ chooses"), pressing("enter stores it", "enter"), pressing("esc cancels", "esc")}
	m.dialog = d
	return nil
}

func (m *Model) storeSecret(secret console.NewSecret) tea.Cmd {
	m.dialog = statusDialog("Secrets", "storing "+secret.Name+"…")
	server := m.configServer()
	return func() tea.Msg {
		_, err := m.ds.CreateSecret(m.ctx, server, secret)
		return secretActionMsg{did: "stored " + secret.Name, err: err}
	}
}

// editSecretForm is the same card over a credential that already exists.
//
// Everything but the kind may be told: the name, the binding, the limit, and
// the credential itself. A value cannot be read back — nothing here can — but
// it can be replaced, and a card that would not take one made rotating a token
// a matter of deleting the secret and storing it again, taking every binding that
// stood on it along with it.
//
// Lowering the limit holds what is bound next, not what already stands: a
// live binding is somebody's decision, and it is taken back rather than quietly
// shortened.
// editSecretForm opens the card that replaces a credential, saving to the server
// named. It takes the secret and the server rather than reading the cursor and
// the header, because the same card answers a refused credential raised in a
// workspace, where there is no cursor to read and the header may name a
// different server from the one the credential lives on (rejections.go,
// ADR 0131 §2).
func (m *Model) editSecretForm(server string, secret console.Secret) tea.Cmd {
	was, id := secret, secret.ID
	d := formDialog("Edit "+secret.Name, console.EditSecretForm(secret), func(update console.SecretUpdate) tea.Cmd {
		return m.saveSecret(server, id, was, update)
	})
	lines := []line{
		{text: "the kind is the one thing it cannot be told: a token and an oauth credential renew differently, so that is a new credential rather than an edit", tone: toneDim},
	}
	if rejection, refused := m.secrets.refused[id]; refused {
		// Why the card was opened, for the card opened from the band: the
		// upstream's refusal is the reason a value is being replaced at all.
		lines = append([]line{{text: rejectionSummary(rejection), tone: toneAccent}}, lines...)
	}
	d.sections = []section{{lines: lines}}
	d.keys = []hint{says("↑↓ moves"), pressing("enter saves", "enter"), pressing("esc leaves it alone", "esc")}
	m.dialog = d
	return nil
}

// saveSecret writes the edit back and says what it did, naming each part that
// changed. Nothing changed is not an error and not a write: it closes and says
// so, rather than reporting a save that saved nothing.
func (m *Model) saveSecret(server, id string, was console.Secret, update console.SecretUpdate) tea.Cmd {
	var did []string
	if update.Name != nil {
		did = append(did, "renamed "+was.Name+" to "+*update.Name)
	}
	if update.MaxTTLSeconds != nil {
		limit := bindingLimit(time.Duration(*update.MaxTTLSeconds) * time.Second)
		did = append(did, "bindings of "+was.Name+" now last at most "+limit)
	}
	if update.Routes != nil {
		switch len(*update.Routes) {
		case 0:
			did = append(did, was.Name+" is sent nowhere")
		default:
			did = append(did, was.Name+" is sent to "+console.RoutesText(*update.Routes))
		}
	}
	if update.Value != nil {
		did = append(did, "replaced "+was.Name+"'s value")
	}
	if update.Format != nil {
		switch *update.Format {
		case "":
			did = append(did, was.Name+"'s sentinels are read from its value again")
		default:
			did = append(did, was.Name+"'s sentinels now look like "+*update.Format)
		}
	}
	if update.ValueTTLSeconds != nil {
		switch *update.ValueTTLSeconds {
		case 0:
			did = append(did, was.Name+"'s value never goes stale")
		default:
			did = append(did, "a value of "+was.Name+" now lasts "+lifetime.Label(time.Duration(*update.ValueTTLSeconds)*time.Second))
		}
	}
	if update.RefreshCommand != nil {
		switch len(*update.RefreshCommand) {
		case 0:
			did = append(did, "removed "+was.Name+"'s refresh command")
		default:
			did = append(did, was.Name+" now renews with "+refreshcmd.Join(*update.RefreshCommand))
		}
	}
	if len(did) == 0 {
		m.dialog = nil
		return m.report(false, "%s is unchanged", was.Name)
	}
	m.dialog = statusDialog("Secrets", "saving "+was.Name+"…")
	return func() tea.Msg {
		if err := m.ds.UpdateSecret(m.ctx, server, id, update); err != nil {
			return secretActionMsg{err: err}
		}
		return secretActionMsg{did: strings.Join(did, ", ")}
	}
}

func (m *Model) confirmDeleteSecret() tea.Cmd {
	secret := m.secrets.current()
	if secret == nil {
		return nil
	}
	id, name := secret.ID, secret.Name
	goes := section{label: "what goes", fields: []field{
		{label: "secret", value: name, tone: toneAccent},
		{label: "live bindings", value: itoa(m.secrets.liveBindings(id)) + ", all taken back"},
	}}
	goes.lines = []line{{
		text: "a discobox holding a sentinel for it keeps a placeholder that now resolves to nothing",
		tone: toneAlert,
	}}
	server := m.configServer()
	d := confirmDialog("Delete secret", "", func(string) tea.Cmd {
		m.dialog = statusDialog("Secrets", "deleting "+name+"…")
		return func() tea.Msg {
			err := m.ds.DeleteSecret(m.ctx, server, id)
			return secretActionMsg{did: "deleted " + name, err: err}
		}
	})
	d.sections = []section{goes}
	d.answerLabel = "delete " + name + "?"
	// The costly answer is yes.
	d.defaultNo = true
	m.dialog = d
	return nil
}

// secretActed closes the dialog, says what happened, and re-reads: every action
// here changes what the list shows, and guessing at the new state is how a
// screen and a server drift.
func (m *Model) secretActed(msg secretActionMsg) tea.Cmd {
	if msg.err != nil {
		// Shown, not reported: these refusals carry the remedy, and the status
		// line clears itself before it has been read.
		m.dialog = errorDialog("Secrets", fmt.Sprintf("%v", msg.err))
		return m.loadSecrets()
	}
	m.dialog = nil
	cmds := []tea.Cmd{m.loadSecrets(), m.loadCredentialRequests(), m.report(false, "%s", msg.did)}
	if msg.after != nil {
		cmds = append(cmds, msg.after())
	}
	return tea.Batch(cmds...)
}

func (m *Model) viewSecrets() string {
	// The credentials the project holds, and under them what is waiting on a
	// person. They are read together: which secret answers this request is the
	// question, and the answer is in the table above it.
	m.zones.push(bodyLeft+m.logoColumn(), m.pageBodyTop())
	body := m.secrets.view(m.st, &m.zones, m.table == tableSecrets)
	// Each lower table is pushed past everything drawn above it and the two
	// blank rows between, so its rows are marked where they are actually
	// drawn.
	under := func(view func() string) {
		m.zones.push(0, lipgloss.Height(body)+2)
		table := view()
		m.zones.pop()
		body = lipgloss.JoinVertical(lipgloss.Left, body, "", "", table)
	}
	under(func() string { return m.requestRows.view(m.st, &m.zones, m.table == tableRequests) })
	m.zones.pop()
	// The secrets take only the rows they have, so what is left over falls
	// below the lower table rather than between the two: the window keeps its
	// full height, and the tables stay next to each other where they are read
	// together.
	//
	// Filled to what the frame leaves the body — the box's two edges, the
	// header and the blank under it, and the message and keys rows — rather
	// than to secretsChrome, which also counts the tables' own title rows
	// and so left the window two rows short of the terminal.
	body = fillRows(body, max(m.pageHeight()-6, 0))
	if m.showLogo() {
		body = lipgloss.JoinHorizontal(lipgloss.Top, m.logo.view(lipgloss.Height(body)), body)
	}
	if m.treeShown() {
		return m.pageFrame(strings.Split(body, "\n"))
	}
	m.zones.push(bodyLeft, headerTop)
	rows := []string{m.viewHeader(m.inner()), ""}
	m.zones.pop()
	rows = append(rows, strings.Split(body, "\n")...)

	m.zones.push(bodyLeft, headerTop+len(rows))
	rows = append(rows, strings.Split(m.viewStatus(), "\n")...)
	m.zones.pop()
	return m.box(rows)
}

// secretsChrome is what this screen costs in rows before a single secret is
// drawn: the box's edges, the header and the blank under it, the list title,
// the blank after the rows, and the message and keys rows.
const secretsChrome = 8

// requestsChrome is what the lower table costs beside its rows: its title, and
// the two blank rows between the tables.
const requestsChrome = 3

// secretHints is the bottom line here: what the secret under the cursor can
// take, most reached-for first.
func (m *Model) secretHints() []hint {
	tab := keyed("tab", "tab", tableName(m.nextTable()))
	switch m.table {
	case tableRequests:
		return []hint{
			says("↑↓ move"),
			keyed("enter", "enter", "answer it"),
			tab,
			keyed("esc", "esc", "back"),
		}
	}
	hints := []hint{
		says("↑↓ move"),
		keyed("n", "n", "new"),
		keyed("enter", "enter", "bindings"),
		keyed(bindingCreateKey, bindingCreateKey, "bind"),
		keyed("e", "e", "edit"),
		keyed("d", "d", "delete"),
	}
	if m.nextTable() != tableSecrets {
		hints = append(hints, tab)
	}
	if m.manyServers() {
		hints = append(hints, says("←→ server"))
	}
	return append(hints, keyed("esc", "esc", "back"), keyed("h", "h", "history"))
}

type bindingHistoryMsg struct {
	wait           *dialog
	server, secret string
	entries        []console.BindingDecision
	err            error
}

func (m *Model) loadBindingHistory() tea.Cmd {
	secret := m.secrets.current()
	if secret == nil {
		return nil
	}
	server, id := m.configServer(), secret.ID
	wait := statusDialog("Binding history", "reading decisions…")
	m.dialog = wait
	return func() tea.Msg {
		entries, err := m.ds.BindingHistory(m.ctx, server, id)
		return bindingHistoryMsg{wait: wait, server: server, secret: id, entries: entries, err: err}
	}
}

func (m *Model) bindingHistoryLoaded(msg bindingHistoryMsg) tea.Cmd {
	if m.dialog != msg.wait || msg.server != m.configServer() || !m.secretsOpen {
		return nil
	}
	m.dialog = nil
	if msg.err != nil {
		return m.report(true, "cannot read binding history: %v", msg.err)
	}
	return m.showBindingHistory(msg)
}
func (m *Model) showBindingHistory(msg bindingHistoryMsg) tea.Cmd {
	if len(msg.entries) == 0 {
		return m.report(false, "no binding history")
	}
	var items []action
	for _, entry := range msg.entries {
		items = append(items, action{key: entry.ID, enabled: true, label: entry.At.Format(time.RFC3339) + " · " + entry.Event + " · " + entry.Actor})
	}
	d := actionsDialog("Binding history and answered requests", "Latest 100 changes", items, func(id string) tea.Cmd {
		for _, entry := range msg.entries {
			if entry.ID == id {
				return m.showBindingDecision(entry, func() tea.Cmd { return m.showBindingHistory(msg) })
			}
		}
		return nil
	})
	d.keys = []hint{pressing("enter reads", "enter"), pressing("esc goes back", "esc")}
	m.dialog = d
	return nil
}

func (m *Model) showBindingDecision(e console.BindingDecision, back func() tea.Cmd) tea.Cmd {
	label := "Bound by"
	if e.RequestID != "" {
		label = "Approved by"
	}
	expiry := "forever"
	if !e.Expires.IsZero() {
		expiry = e.Expires.Format(time.RFC3339)
	}
	grantor, grantedAt := e.GrantedBy, e.GrantedAt
	if grantor == "" {
		grantor, grantedAt = e.Actor, e.At
	}
	fields := []field{{label: "event", value: e.Event}, {label: "event actor", value: e.Actor}, {label: "event at", value: e.At.Format(time.RFC3339)}, {label: label, value: grantor}, {label: "granted at", value: grantedAt.Format(time.RFC3339)}, {label: "target ID", value: e.TargetID}, {label: "variable", value: e.EnvVar}, {label: "binding ID", value: e.BindingID}, {label: "delegation ID", value: e.ParentID}, {label: "expires", value: expiry}}
	if e.RequestID != "" {
		fields = append(fields, field{label: "answered request", value: e.RequestID})
	}
	if len(e.RouteLists) == 0 {
		fields = append(fields, field{label: "effective limits", value: "not recorded for this historical change"})
	}
	for _, list := range e.RouteLists {
		fields = append(fields, field{label: "within", value: console.RouteRows(list), tone: toneAccent})
	}
	d := actionsDialog("Credential decision", "", nil, func(string) tea.Cmd { return back() })
	d.sections = []section{{label: "the decision", fields: fields}}
	d.wrapDetails = true
	d.onCancel = back
	d.keys = []hint{pressing("esc goes back", "esc")}
	if parent, ok := m.secrets.binding(e.ParentID); ok {
		d.altKey = "d"
		d.alt = func(string) tea.Cmd {
			return m.reviewBinding(parent.SecretName, parent, func() tea.Cmd { return m.showBindingDecision(e, back) })
		}
		d.keys = append(d.keys, keyed("d", "d", "reads delegation"))
	}
	m.dialog = d
	return nil
}
