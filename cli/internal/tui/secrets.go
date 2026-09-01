package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
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

// secretsKey opens the screen. F1 is help, F2 the editor, F3 the harnesses —
// the prompt takes every letter, so a screen of its own goes on a function key.
const secretsKey = "f4"

// SecretsKeyName is how that key is spelled to the user.
const SecretsKeyName = "F4"

// secretList is the screen's list: the project's secrets and where the cursor
// is in them.
type secretList struct {
	all    []Secret
	grants []Grant
	cursor int
	offset int

	// loaded distinguishes a project with no secrets from a listing that has
	// not landed, which look the same in all and mean opposite things.
	loaded bool

	width, height int

	now func() time.Time
}

func newSecretList() *secretList { return &secretList{now: time.Now} }

// setAll takes a refreshed listing, keeping the cursor on the secret it was on
// rather than on the row number it was at.
func (l *secretList) setAll(all []Secret) {
	l.loaded = true
	var onID string
	if s := l.current(); s != nil {
		onID = s.ID
	}
	l.all = all
	l.countGrants()
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

// setGrants takes the project's grants, which the rows count and the grants
// dialog reads. They are listed for the project rather than per row: one read
// answers every row, and a count per row would be a call per row.
func (l *secretList) setGrants(grants []Grant) {
	l.grants = grants
	l.countGrants()
}

func (l *secretList) countGrants() {
	counts := map[string]int{}
	for _, g := range l.grants {
		counts[g.SecretID]++
	}
	for i := range l.all {
		l.all[i].Grants = counts[l.all[i].ID]
	}
}

// grantsFor is the grants standing on one secret.
func (l *secretList) grantsFor(secretID string) []Grant {
	var out []Grant
	for _, g := range l.grants {
		if g.SecretID == secretID {
			out = append(out, g)
		}
	}
	return out
}

func (l *secretList) current() *Secret {
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
	if l.offset < 0 {
		l.offset = 0
	}
}

func (l *secretList) view(st *styles, focused bool) string {
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
	for i := l.offset; i < len(l.all) && len(body) < l.height; i++ {
		body = append(body, l.row(st, l.all[i], i, focused))
	}
	for len(body) < l.height {
		body = append(body, blank)
	}
	return lipgloss.JoinVertical(lipgloss.Left, append(out, body...)...)
}

// row draws one secret: what it is called, where it may be sent, and what
// stands on it. Columns drop off the right end as the terminal narrows; the
// name never goes.
//
// Its kind and its grant limit are not here. They are facts about a credential
// rather than ways to tell two of them apart, and a row carrying every field
// is a row nobody reads — Enter opens the secret, and they are there.
func (l *secretList) row(st *styles, s Secret, i int, focused bool) string {
	atCursor := i == l.cursor && focused

	tail := ""
	addCol := func(text string, w int) {
		if l.width-lipgloss.Width(tail)-w < 20 {
			return
		}
		tail += padANSI(text, w)
	}

	// The binding, which is the whole of what limits where this credential may
	// travel. An unbound secret says so rather than showing a blank: blank
	// reads as "not loaded", and this is a fact about the secret.
	host := s.Host
	hostStyle := st.name
	if host == "" {
		host, hostStyle = "any host", st.dimText
	}
	addCol(hostStyle.Render(pad(host, 22)), 23)
	grants := "—"
	grantStyle := st.dimText
	if s.Grants > 0 {
		grants, grantStyle = plural(s.Grants, "grant", "grants"), st.statusOK
	}
	addCol("  "+grantStyle.Render(pad(grants, 9)), 12)
	addCol(st.dimText.Render(pad(secretAge(s, l.now()), 7)), 8)

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
func secretAge(s Secret, now time.Time) string {
	age := since(s.Updated, now)
	if age == "" {
		return ""
	}
	return age + " ago"
}

// shortDuration renders a grant lifetime the way the row has room for: "1h",
// "30m", "—" for one that never expires.
// grantLimit is a secret's ceiling on grant lifetimes. Zero is not absence: it
// is the answer "no limit", so it is spelled out rather than borrowing
// shortDuration's "never", which in this column would read as the opposite of
// what it means.
func grantLimit(d time.Duration) string {
	if d <= 0 {
		return "forever"
	}
	return shortDuration(d)
}

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
	secrets []Secret
	grants  []Grant
	err     error
}

// secretActionMsg carries a completed action back into the update loop, so the
// listing is re-read from one place however the action was started.
type secretActionMsg struct {
	did string
	err error
}

func (m *Model) loadSecrets() tea.Cmd {
	return func() tea.Msg {
		secrets, err := m.ds.Secrets(m.ctx)
		if err != nil {
			return secretsLoadedListMsg{err: err}
		}
		grants, err := m.ds.Grants(m.ctx, "")
		return secretsLoadedListMsg{secrets: secrets, grants: grants, err: err}
	}
}

func (m *Model) openSecrets() tea.Cmd {
	m.expand()
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
	if msg.err != nil {
		return m.report(true, "cannot read secrets: %v", msg.err)
	}
	m.secrets.setAll(msg.secrets)
	m.secrets.setGrants(msg.grants)
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
		m.onRequests = !m.onRequests
		return nil
	case "?":
		m.dialog = textDialog("Keys", m.helpText())
		return nil
	case "r":
		return tea.Batch(m.loadSecrets(), m.loadCredentialRequests(), status("refreshing"))
	case credentialsKey:
		return m.openWaitingRequest()
	}
	if m.onRequests {
		return m.updateRequestTable(msg)
	}
	switch keyName(msg) {
	case "down", "j":
		// Off the bottom of the secrets is the requests table, the way off the
		// bottom of the discobox list is the prompt: the tables are one screen
		// read downwards, not two panes to be switched between.
		if m.secrets.cursor >= len(m.secrets.all)-1 && len(m.requestRows.all) > 0 {
			m.onRequests = true
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
	case "enter", "v":
		return m.showGrants()
	case "e":
		return m.editSecretForm()
	case "d":
		return m.confirmDeleteSecret()
	case grantCreateKey:
		return m.askForGrantScope()
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
			m.onRequests = false
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

// grantCreateKey mints a standing grant on the secret under the cursor: the
// pre-approval an operator makes because they already know the answer, rather
// than waiting for an agent to ask and answering then.
const grantCreateKey = "p"

// askForGrantScope opens the pre-approval as one form.
//
// It was seven dialogs in a row, and a run of modals is the wrong shape for a
// decision whose parts are read against each other: a scope of "one harness"
// and a use of "open a pull request" mean something together, and answering
// them a card at a time hides each behind the next. Everything is on one card,
// and the rows a given answer makes irrelevant are taken away rather than asked
// (ADR 0031 §4: a credential taken through discobox-access is never injected,
// so it alone has a variable and a use).
func (m *Model) askForGrantScope() tea.Cmd {
	secret := m.secrets.current()
	if secret == nil {
		return nil
	}
	scopes := []choice{{key: grantScopeProject, label: "every discobox in this project",
		hint: "the widest of the three: anything the project runs may use the credential"}}
	if len(m.harnesses.all) > 0 {
		scopes = append(scopes, choice{key: "harnessConfig", label: "one harness",
			hint: "every discobox running that harness may use the credential"})
	}
	if len(m.list.all) > 0 {
		scopes = append(scopes, choice{key: "sandbox", label: "one discobox",
			hint: "the narrowest, and what approving a request mints"})
	}

	harnesses := make([]choice, 0, len(m.harnesses.all))
	for _, h := range m.harnesses.all {
		harnesses = append(harnesses, choice{key: h.ID, label: h.Name, hint: h.ID})
	}
	boxes := make([]choice, 0, len(m.list.all))
	for _, box := range m.list.all {
		boxes = append(boxes, choice{key: box.ID, label: box.Name, hint: box.ID})
	}

	const who, how, where = "who may use it", "how it may be used", "where, and for how long"
	rows := []formRow{
		withSection(who, pickRow("scope", "scope", scopes...)),
		unless(withSection(who, pickRow("harness", "harness", harnesses...)),
			func(f *form) bool { return f.chosen("scope") == "harnessConfig" },
			"only when the grant is scoped to one harness"),
		unless(withSection(who, pickRow("sandbox", "discobox", boxes...)),
			func(f *form) bool { return f.chosen("scope") == "sandbox" },
			"only when the grant is scoped to one discobox"),

		// The ordinary kind first, and so the one the form opens on: a
		// pre-approval is usually a credential somebody wants the box simply to
		// have. The narrow kind is what an agent's own request mints, and
		// choosing it here brings out the two rows it needs.
		withSection(how, pickRow("taken", "taken",
			choice{key: "any", label: "anything in the discobox",
				hint: "an environment variable, so everything in the box can read it"},
			choice{key: "access", label: "only through discobox-access",
				hint: "never injected: the CLI takes it one declared use at a time, and the value dies minutes later"})),
	}

	deliveredAs := textRow("envVar", "delivered as", "e.g. GH_TOKEN", "")
	deliveredAs.section = how
	deliveredAs.hint = "the variable `discobox-access run` puts it in, for the one command it wraps"
	deliveredAs.required = "a credential taken by the CLI needs a variable to arrive in"
	deliveredAs.when, deliveredAs.why = takenThroughAccess, injectedNote

	use := textRow("use", "approved use", "e.g. Open a pull request against the current repo", "")
	use.section = how
	use.hint = "`discobox-access run` asks a model whether the command it is about to run is this sentence"
	use.required = "a use is the sentence the credential is granted for"
	use.when, use.why = takenThroughAccess, injectedNote

	host := textRow("host", "may be sent to", "e.g. github.com", secret.Host)
	host.section = where
	host.hint = "the host covers itself and everything beneath it, and may not reach outside the secret's own binding"

	ttl := ttlRows("ttl", "lifetime", where, "never expires", int64(secret.MaxTTL/time.Second))
	hint := "it never expires: a credential nobody has to think about again, and one nothing takes away"
	if secret.MaxTTL > 0 {
		hint = "this credential allows at most " + grantLimit(secret.MaxTTL) + ", so it cannot be granted forever"
	}
	ttl[0].hint, ttl[1].hint = hint, hint

	rows = append(rows, deliveredAs, use, host)
	rows = append(rows, ttl...)

	secretID := secret.ID
	d := formDialog("Grant "+secret.Name, newForm(rows...), func(f *form) tea.Cmd {
		seconds, ok := ttlSeconds(f, "ttl")
		if !ok {
			f.err = "a lifetime is a number of seconds"
			return nil
		}
		grant := NewGrant{
			SecretID:   secretID,
			Scope:      f.chosen("scope"),
			Host:       f.value("host"),
			TTLSeconds: seconds,
		}
		switch grant.Scope {
		case "harnessConfig":
			grant.ScopeKey = f.chosen("harness")
		case "sandbox":
			grant.ScopeKey = f.chosen("sandbox")
		}
		if f.chosen("taken") == "access" {
			grant.EnvVar, grant.Uses = f.value("envVar"), []string{f.value("use")}
		}
		return m.mintGrant(grant)
	})
	d.sections = []section{{lines: []line{
		{text: "a standing grant authorizes this credential before anything asks for it", tone: toneDim},
	}}}
	d.footer = "↑↓ moves · ←→ chooses · enter grants · esc leaves the secret alone"
	m.dialog = d
	return nil
}

// takenThroughAccess is the answer the variable and the use hang off: a
// credential the discobox is simply provisioned with has neither.
func takenThroughAccess(f *form) bool { return f.chosen("taken") == "access" }

// injectedNote is what those two rows say while that is the answer.
const injectedNote = "only for a credential taken through discobox-access"

// withSection and withWhen keep the picker rows above readable as a list.
func withSection(label string, row formRow) formRow {
	row.section = label
	return row
}

// unless is a row that only applies under a condition, with what it says on the
// card when it does not.
func unless(row formRow, when func(f *form) bool, why string) formRow {
	row.when, row.why = when, why
	return row
}

// grantScopeProject is the server's word for the widest scope, spelled here
// because the window speaks the API's vocabulary rather than importing the
// server's.
const grantScopeProject = "project"

func (m *Model) mintGrant(grant NewGrant) tea.Cmd {
	m.dialog = statusDialog("Secrets", "granting…")
	return func() tea.Msg {
		created, err := m.ds.CreateGrant(m.ctx, grant)
		did := "granted"
		if created.Host != "" {
			did = "granted for " + created.Host
		}
		return secretActionMsg{did: did, err: err}
	}
}

// openWaitingRequest answers a request from here, where there is no discobox
// under the cursor to take it from: the oldest one waiting anywhere in the
// project, including the ones no discobox owns.
func (m *Model) openWaitingRequest() tea.Cmd {
	var oldest *CredentialRequest
	for i := range m.allRequests {
		if oldest == nil || m.allRequests[i].Created.Before(oldest.Created) {
			oldest = &m.allRequests[i]
		}
	}
	if oldest == nil {
		return m.report(false, "nothing is waiting")
	}
	return m.openCredentialRequest(*oldest)
}

// showGrants lists what stands on the secret under the cursor, and lets one be
// withdrawn. Revoking is immediate — the credential stops resolving — so the
// row says what each grant covers before it offers to.
func (m *Model) showGrants() tea.Cmd {
	secret := m.secrets.current()
	if secret == nil {
		return nil
	}
	grants := m.secrets.grantsFor(secret.ID)
	// Making one is the first row, so a secret with no grants is not a dead
	// end: this is where somebody looking at a credential decides who may use
	// it, and being told to go and use another command is not deciding.
	items := make([]action, 0, len(grants)+1)
	items = append(items, action{
		key:     grantCreateItem,
		press:   grantCreateKey,
		label:   "Grant this secret…",
		detail:  "authorize it before anything asks",
		enabled: true,
	})
	for _, g := range grants {
		items = append(items, action{
			key:     g.ID,
			label:   grantHost(g),
			detail:  grantDetail(g, m.secrets.now()),
			enabled: true,
		})
	}
	// What the credential is, before what may use it. For an OAuth credential
	// that is most of what there is to know about it — where it renews, what
	// the grant may do, and when the access token goes stale — and none of it
	// is the credential.
	answer := "nothing stands on it yet"
	if len(grants) > 0 {
		answer = plural(len(grants), "grant stands on it", "grants stand on it")
	}
	// Enter reads, x withdraws. The other way round put revoking a credential
	// one keystroke from looking at one, on the key that means "open this"
	// everywhere else in the window.
	d := actionsDialog(secret.Name, "", items, func(result string) tea.Cmd {
		if result == grantCreateItem {
			return m.askForGrantScope()
		}
		return m.reviewGrant(secret.Name, result)
	})
	d.titleRight = secretAge(*secret, m.secrets.now())
	d.sections = describeSecret(*secret, m.secrets.now())
	d.answerLabel = answer
	d.footer = "enter reads · " + grantRevokeKey + " revokes · esc leaves them alone"
	d.altKey = grantRevokeKey
	d.alt = func(grantID string) tea.Cmd {
		if grantID == grantCreateItem {
			return nil
		}
		return m.confirmRevokeGrant(secret.Name, grantID)
	}
	m.dialog = d
	return nil
}

// describeSecret is what a credential is, in the words that can be shown. The
// value is never among them; for an OAuth credential the renewal material is
// not either, and what is left — where it renews, whose grant it is, what it
// may do, when it goes stale — is the part somebody managing it needs.
func describeSecret(secret Secret, now time.Time) []section {
	where := secret.Host
	tone := toneAccent
	if where == "" {
		where, tone = "any host a grant allows", toneDim
	} else {
		where += " and the hosts beneath it"
	}
	limit := "none — grants on it may live forever"
	if secret.MaxTTL > 0 {
		limit = "at most " + shortDuration(secret.MaxTTL)
	}
	credential := section{label: "the credential", fields: []field{
		{label: "kind", value: secret.Type},
		{label: "may be sent to", value: where, tone: tone},
		{label: "grant limit", value: limit},
	}}
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
		return "expired " + since(at, now) + " ago, renewed on next use"
	}
	return "in " + shortDuration(at.Sub(now).Round(time.Minute))
}

// scopeLabel is a grant's scope as it is said to a person. The wire word is
// harnessConfig, because that is the resource; a person calls it the harness,
// and a window that says both is a window teaching two names for one thing.
func scopeLabel(scope string) string {
	if scope == "harnessConfig" {
		return "harness"
	}
	return scope
}

// grantCreateItem is the row that starts a pre-approval from inside the grants
// list, which is where somebody is already looking at what may use a secret.
const grantCreateItem = "grant:new"

// grantRevokeKey withdraws the highlighted grant from the review list.
const grantRevokeKey = "x"

// reviewGrant is the grant read in full: what it covers, how long it lives,
// and — for one minted by approving an agent's request — every use it was
// approved for, with the ID an agent presents to take a value under it.
func (m *Model) reviewGrant(secretName, grantID string) tea.Cmd {
	var grant *Grant
	for i := range m.secrets.grants {
		if m.secrets.grants[i].ID == grantID {
			grant = &m.secrets.grants[i]
		}
	}
	if grant == nil {
		return m.report(true, "that grant is no longer there")
	}
	where := grant.Host
	tone := toneAccent
	if where == "" {
		where, tone = "any host", toneDim
	} else {
		where += " and the hosts beneath it"
	}
	scope := scopeLabel(grant.Scope)
	if grant.ScopeKey != "" {
		scope += " " + grant.ScopeKey
	}
	covers := section{label: "the grant", fields: []field{
		{label: "secret", value: secretName, tone: toneAccent},
		{label: "usable by", value: scope},
		{label: "may go to", value: where, tone: tone},
	}}
	if grant.GrantedBy != "" {
		covers.fields = append(covers.fields, field{label: "granted by", value: grant.GrantedBy})
	}
	covers.fields = append(covers.fields, field{label: "expires", value: grantLifeLeft(*grant, m.secrets.now())})

	uses := section{label: "approved uses"}
	if len(grant.Uses) == 0 {
		uses.lines = []line{{text: "none — a standing grant authorizes the credential without enumerating what it is for", tone: toneDim}}
	}
	for _, use := range grant.Uses {
		uses.fields = append(uses.fields,
			field{label: use.ID, value: use.Description})
	}
	if len(grant.Uses) > 0 {
		uses.lines = append(uses.lines, line{
			text: "an agent takes a value under one of those IDs: discobox-access run --use <id> -- <command>",
			tone: toneDim,
		})
	}

	d := textDialog("Grant", "")
	d.titleRight = grant.ID
	d.sections = []section{covers, uses}
	// Back to the grants you were reading, not to the list behind them. The
	// common next move after reading one is reading the next, or withdrawing
	// this one, and both are in the list this came from.
	d.onCancel = func() tea.Cmd { return m.showGrants() }
	m.dialog = d
	return nil
}

// confirmRevokeGrant asks before withdrawing, and says what stops working.
func (m *Model) confirmRevokeGrant(secretName, grantID string) tea.Cmd {
	var grant Grant
	for _, g := range m.secrets.grants {
		if g.ID == grantID {
			grant = g
		}
	}
	d := confirmDialog("Revoke grant", "", func(string) tea.Cmd { return m.revokeGrant(grantID) })
	d.titleRight = grantID
	d.sections = []section{{
		label: "what goes",
		fields: []field{
			{label: "secret", value: secretName, tone: toneAccent},
			{label: "usable by", value: strings.TrimSpace(scopeLabel(grant.Scope) + " " + grant.ScopeKey)},
			{label: "may go to", value: grantHost(grant)},
		},
		lines: []line{
			{text: "the credential stops resolving at once, wherever it is being used", tone: toneAlert},
			{text: "the request that produced it stays approved, because it is history", tone: toneDim},
		},
	}}
	d.answerLabel = "withdraw this grant?"
	d.defaultNo = true
	// No goes back to the grants, the way leaving a grant you were reading
	// does: a question answered "not that one" should leave you where the
	// question was asked, not on the screen behind it.
	d.onCancel = func() tea.Cmd { return m.showGrants() }
	m.dialog = d
	return nil
}

// grantHost is where a grant may send the credential, which is what its row is
// named by: the host is the fact two grants on one secret differ by most often,
// and the one an operator is looking for when they open the list.
func grantHost(g Grant) string {
	if g.Host == "" {
		return "any host"
	}
	return g.Host
}

// grantDetail is the rest of the row: who may use it, how much it may do, and
// how long it has left.
func grantDetail(g Grant, now time.Time) string {
	who := scopeLabel(g.Scope)
	if g.ScopeKey != "" {
		who += " " + g.ScopeKey
	}
	parts := []string{who}
	if len(g.Uses) > 0 {
		parts = append(parts, plural(len(g.Uses), "approved use", "approved uses"))
	}
	return strings.Join(append(parts, grantExpiry(g, now)), " · ")
}

// grantLifeLeft is how long a grant has, for a field already labeled with what
// it is: "expires  expires in 47m" is the row's phrasing put under the row's
// own heading.
func grantLifeLeft(g Grant, now time.Time) string {
	switch {
	case g.Expires.IsZero():
		return "never"
	case g.Expires.Before(now):
		return "expired"
	default:
		return "in " + shortDuration(g.Expires.Sub(now).Round(time.Minute))
	}
}

func grantExpiry(g Grant, now time.Time) string {
	switch {
	case g.Expires.IsZero():
		return "never expires"
	case g.Expires.Before(now):
		return "expired"
	default:
		return "expires in " + shortDuration(g.Expires.Sub(now).Round(time.Minute))
	}
}

func (m *Model) revokeGrant(grantID string) tea.Cmd {
	m.dialog = statusDialog("Secrets", "revoking…")
	return func() tea.Msg {
		err := m.ds.RevokeGrant(m.ctx, grantID)
		return secretActionMsg{did: "revoked", err: err}
	}
}

// A lifetime is asked as the answers people actually give, with a way out to
// any other: an hour, a day, a week, forever, or a number of seconds behind
// "custom". Seconds alone was a field nobody could answer without arithmetic,
// and 604800 is not a week to anyone reading it back.
//
// The presets are the picker's keys, so reading one back is parsing what was
// chosen rather than remembering which index meant what.
const (
	ttlHour   = "3600"
	ttlDay    = "86400"
	ttlWeek   = "604800"
	ttlNever  = "0"
	ttlCustom = "custom"
)

// ttlDefault is what a new credential limits its grants to when nobody says
// otherwise: nothing. A limit is a decision about how long consent may last,
// and a default one is a decision nobody made — it expires grants mid-task, in
// a place far from this form, and teaches people to grant again rather than to
// pick a limit. The row is right here, on the card, for whoever wants one.
const ttlDefault = ttlNever

// ttlRows is a lifetime as two rows: the presets, and the seconds behind
// custom. The second only applies while the first is on custom, so the number
// is there to be edited when it is wanted and out of the way when it is not.
func ttlRows(key, label, section, forever string, seconds int64) []formRow {
	pick := pickRow(key, label,
		choice{key: ttlHour, label: "1 hour"},
		choice{key: ttlDay, label: "1 day"},
		choice{key: ttlWeek, label: "1 week"},
		choice{key: ttlNever, label: forever},
		choice{key: ttlCustom, label: "custom…"},
	)
	pick.section = section
	pick.at = len(pick.choices) - 1
	for i, c := range pick.choices {
		if c.key == itoa(int(seconds)) {
			pick.at = i
		}
	}
	// The seconds row reads as a continuation of the picker above it rather
	// than as a second field with the same name.
	custom := textRow(key+"Custom", "…in seconds", "e.g. 3600", itoa(int(seconds)))
	custom.section = section
	custom.why = "the presets above cover it"
	custom.when = func(f *form) bool { return f.chosen(key) == ttlCustom }
	return []formRow{pick, custom}
}

// ttlSeconds reads a lifetime back off the two rows, refusing what is not a
// number of seconds rather than quietly meaning something else.
func ttlSeconds(f *form, key string) (int64, bool) {
	chosen := f.chosen(key)
	if chosen != ttlCustom {
		seconds, err := strconv.ParseInt(chosen, 10, 64)
		return seconds, err == nil
	}
	seconds, err := strconv.ParseInt(f.value(key+"Custom"), 10, 64)
	return seconds, err == nil && seconds >= 0
}

// secretForm is a credential on one card: the same rows for storing a new one
// and for editing what an existing one says about itself.
//
// Everything an OAuth credential needs is on the card from the start, dim and
// stepped over until the kind calls for it, so choosing "an OAuth credential"
// reveals nothing that was not already visible as something it would want. The
// difference is not cosmetic: an access token stored as a token expires and
// stays expired, while an oauth secret is refreshed server-side as it ages out
// (ADR 0011), and the four rows that make that possible are the reason.
//
// On an existing secret the rows that cannot change are the same rows again,
// locked: what the credential is, said in the place the form would have asked
// for it. Only the binding and the grant limit are editable, because they are
// the only two the server takes — and the value is not among them, since
// nothing here can read one back.
func secretForm(existing *Secret) *form {
	const what, value = "the credential", "what it is"
	editing := existing != nil
	oauth := func(f *form) bool { return f.chosen("kind") == "oauth" }
	token := func(f *form) bool { return f.chosen("kind") == "token" }

	name := textRow("name", "name", "e.g. github", "")
	name.section = what
	name.hint = "how you pick it out when a request asks for a credential"
	name.required = "a secret needs a name"

	// The kind is the one thing an existing credential cannot be told. A token
	// and an oauth credential are stored and renewed differently, and the
	// server has no answer for a secret that changes from one to the other:
	// that is a new credential, and deleting the old one is somebody's decision
	// rather than a side effect of an edit.

	kind := pickRow("kind", "kind",
		choice{key: "token", label: "a token", hint: "one opaque string, however it is presented"},
		choice{key: "oauth", label: "an OAuth credential", hint: "renews itself: an access token, a refresh token, and where to spend it"})
	kind.section = what

	host := textRow("host", "may be sent to", "any host a grant allows", "")
	host.section = what
	host.hint = "it covers that host and everything beneath it: github.com answers for api.github.com too · empty leaves the grants to decide alone"

	seconds := int64(0)
	if editing {
		seconds = int64(existing.MaxTTL / time.Second)
	} else if v, err := strconv.ParseInt(ttlDefault, 10, 64); err == nil {
		seconds = v
	}
	// "grant limit" rather than "grants last": the value is a ceiling, and a
	// row reading "grants last ‹ 1 day ›" states it as the lifetime every grant
	// gets, which is the opposite of what it does.
	limit := ttlRows("ttl", "grant limit", what, "no limit", seconds)
	limit[0].hint = "the longest a grant on this credential may live: a longer one is refused, and nothing else about it changes"
	limit[1].hint = limit[0].hint

	plain := textRow("token", "token", "the value itself", "")
	plain.section = value
	plain.masked = true
	plain.hint = "stored encrypted, and never shown again"
	plain.required = "a secret is the value: nothing was stored"
	plain.when = token
	plain.why = "only for a token"

	access := textRow("access", "access token", "the value itself", "")
	access.section = value
	access.masked = true
	access.hint = "the one that travels, and the one that expires · stored encrypted, and never shown again"
	access.required = "an oauth credential needs an access token"
	access.when = oauth
	access.why = "only for an OAuth credential"

	refresh := textRow("refresh", "refresh token", "spent to renew the access token", "")
	refresh.section = value
	refresh.masked = true
	refresh.hint = "only the control plane ever spends it: it is never delivered to a discobox, and never leaves in a response"
	refresh.required = "an oauth credential needs a refresh token"
	refresh.when = oauth
	refresh.why = "only for an OAuth credential"

	tokenURL := textRow("tokenURL", "renews at", "e.g. https://console.anthropic.com/v1/oauth/token", "")
	tokenURL.section = value
	tokenURL.hint = "where the refresh token is exchanged for a new access token"
	tokenURL.required = "an oauth credential needs somewhere to renew"
	tokenURL.when = oauth
	tokenURL.why = "only for an OAuth credential"

	client := textRow("client", "client", "client id (optional)", "")
	client.section = value
	client.hint = "which OAuth client the grant belongs to · leave it empty if the token endpoint does not ask"
	client.when = oauth
	client.why = "only for an OAuth credential"

	scopes := textRow("scopes", "may do", "e.g. user:profile user:inference", "")
	scopes.section = value
	scopes.hint = "space separated, as the authorization server returned them — a client may gate features on these, so record them rather than guess"
	scopes.when = oauth
	scopes.why = "only for an OAuth credential"

	if editing {
		// A stored credential opens on what it is. Only the kind is locked; a
		// value cannot be read back, but it can be replaced, and a card that
		// showed the rows for it greyed out was telling people to delete a
		// credential and store it again to rotate a token.
		name.input = valued(name.input, existing.Name)
		kind.locked = true
		if existing.Type == "oauth" {
			kind.at = 1
		}
		host.input = valued(host.input, existing.Host)
		if existing.OAuth != nil {
			tokenURL.input = valued(tokenURL.input, existing.OAuth.TokenURL)
			client.input = valued(client.input, existing.OAuth.ClientID)
			scopes.input = valued(scopes.input, strings.Join(existing.OAuth.Scopes, " "))
		}
		// The value is replaced whole or left alone: the server stores one
		// value per secret, so a new refresh token without the access token
		// beside it would drop the rest. Nothing is required, because leaving
		// every one of these empty is the ordinary answer — the credential
		// stays as it is.
		plain.required, access.required, refresh.required, tokenURL.required = "", "", "", ""
		plain.input.Placeholder = keepStored
		access.input.Placeholder = keepStored
		refresh.input.Placeholder = keepStored
		plain.hint = "typing here replaces the credential · leave it empty and the stored one stays"
		access.hint = "replacing an oauth credential means both tokens and where it renews: the stored ones cannot be read back"
		refresh.hint = access.hint
		tokenURL.hint = access.hint
	}
	rows := append([]formRow{name, kind, host}, limit...)
	return newForm(append(rows, plain, access, refresh, tokenURL, client, scopes)...)
}

// keepStored is what an untouched value row says on an existing credential: a
// value nobody can read back, and one nothing has to be typed to keep.
const keepStored = "unchanged — type to replace it"

// valued is a row's input opened on what it already holds.
func valued(ti textinput.Model, value string) textinput.Model {
	ti.SetValue(value)
	return ti
}

// newSecretForm stores a credential the project does not have yet.
func (m *Model) newSecretForm() tea.Cmd {
	d := formDialog("New secret", secretForm(nil), func(f *form) tea.Cmd {
		seconds, ok := ttlSeconds(f, "ttl")
		if !ok {
			f.err = "a limit is a number of seconds"
			return nil
		}
		return m.storeSecret(NewSecret{
			Name:          f.value("name"),
			Type:          f.chosen("kind"),
			Host:          f.value("host"),
			MaxTTLSeconds: seconds,
			Value:         formSecretValue(f),
		})
	})
	d.footer = "↑↓ moves · ←→ chooses · enter stores it · esc cancels"
	m.dialog = d
	return nil
}

// formSecretValue is the credential's material as the card holds it. An oauth
// credential is the several fields the control plane renews with; a token is
// the one field that travels.
func formSecretValue(f *form) SecretValue {
	if f.chosen("kind") != "oauth" {
		return SecretValue{Token: f.value("token")}
	}
	return SecretValue{
		Token:        f.value("access"),
		RefreshToken: f.value("refresh"),
		TokenURL:     f.value("tokenURL"),
		ClientID:     f.value("client"),
		Scopes:       strings.Fields(f.value("scopes")),
	}
}

func (m *Model) storeSecret(secret NewSecret) tea.Cmd {
	m.dialog = statusDialog("Secrets", "storing "+secret.Name+"…")
	return func() tea.Msg {
		_, err := m.ds.CreateSecret(m.ctx, secret)
		return secretActionMsg{did: "stored " + secret.Name, err: err}
	}
}

// editSecretForm is the same card over a credential that already exists.
//
// Everything but the kind may be told: the name, the binding, the limit, and
// the credential itself. A value cannot be read back — nothing here can — but
// it can be replaced, and a card that would not take one made rotating a token
// a matter of deleting the secret and storing it again, taking every grant that
// stood on it along with it.
//
// Lowering the limit binds what is granted next, not what already stands: a
// live grant is somebody's decision, and it is revoked rather than quietly
// shortened.
func (m *Model) editSecretForm() tea.Cmd {
	secret := m.secrets.current()
	if secret == nil {
		return nil
	}
	was, id := *secret, secret.ID
	d := formDialog("Edit "+secret.Name, secretForm(secret), func(f *form) tea.Cmd {
		seconds, ok := ttlSeconds(f, "ttl")
		if !ok {
			f.err = "a limit is a number of seconds"
			return nil
		}
		update := SecretUpdate{}
		if name := f.value("name"); name != was.Name {
			update.Name = &name
		}
		if host := f.value("host"); host != was.Host {
			update.Host = &host
		}
		if time.Duration(seconds)*time.Second != was.MaxTTL {
			update.MaxTTLSeconds = &seconds
		}
		value, why := replacementValue(f, was)
		if why != "" {
			f.err = why
			return nil
		}
		update.Value = value
		return m.saveSecret(id, was, update)
	})
	d.sections = []section{{lines: []line{
		{text: "the kind is the one thing it cannot be told: a token and an oauth credential renew differently, so that is a new credential rather than an edit", tone: toneDim},
	}}}
	d.footer = "↑↓ moves · enter saves · esc leaves it alone"
	m.dialog = d
	return nil
}

// replacementValue is the credential the card is replacing the stored one with,
// nil when it is replacing nothing, and the reason when it cannot.
//
// The server stores one value per secret and replaces it whole, so half an
// oauth credential is not a smaller change — it is the rest of it dropped.
// Touching any part of one therefore means typing all of it, and the two tokens
// are the part nobody can copy off the card, which is exactly what makes this
// worth refusing rather than sending.
func replacementValue(f *form, was Secret) (*SecretValue, string) {
	value := formSecretValue(f)
	if was.Type != "oauth" {
		if value.Token == "" {
			return nil, ""
		}
		return &value, ""
	}
	stored := SecretOAuth{}
	if was.OAuth != nil {
		stored = *was.OAuth
	}
	renewalChanged := value.TokenURL != stored.TokenURL ||
		value.ClientID != stored.ClientID ||
		strings.Join(value.Scopes, " ") != strings.Join(stored.Scopes, " ")
	if value.Token == "" && value.RefreshToken == "" && !renewalChanged {
		return nil, ""
	}
	switch {
	case value.Token == "":
		return nil, "replacing an oauth credential means its access token too: the stored one cannot be read back"
	case value.RefreshToken == "":
		return nil, "replacing an oauth credential means its refresh token too: the stored one cannot be read back"
	case value.TokenURL == "":
		return nil, "an oauth credential needs somewhere to renew"
	}
	return &value, ""
}

// saveSecret writes the edit back and says what it did, naming each part that
// changed. Nothing changed is not an error and not a write: it closes and says
// so, rather than reporting a save that saved nothing.
func (m *Model) saveSecret(id string, was Secret, update SecretUpdate) tea.Cmd {
	var did []string
	if update.Name != nil {
		did = append(did, "renamed "+was.Name+" to "+*update.Name)
	}
	if update.Host != nil {
		switch *update.Host {
		case "":
			did = append(did, "released "+was.Name+"'s binding")
		default:
			did = append(did, "bound "+was.Name+" to "+*update.Host)
		}
	}
	if update.MaxTTLSeconds != nil {
		limit := grantLimit(time.Duration(*update.MaxTTLSeconds) * time.Second)
		did = append(did, "grants on "+was.Name+" now last "+limit)
	}
	if update.Value != nil {
		did = append(did, "replaced "+was.Name+"'s value")
	}
	if len(did) == 0 {
		m.dialog = nil
		return m.report(false, "%s is unchanged", was.Name)
	}
	m.dialog = statusDialog("Secrets", "saving "+was.Name+"…")
	return func() tea.Msg {
		if err := m.ds.UpdateSecret(m.ctx, id, update); err != nil {
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
		{label: "live grants", value: itoa(secret.Grants) + ", all withdrawn"},
	}}
	if secret.Host != "" {
		goes.fields = append(goes.fields, field{label: "bound to", value: secret.Host})
	}
	goes.lines = []line{{
		text: "a discobox holding a sentinel for it keeps a placeholder that now resolves to nothing",
		tone: toneAlert,
	}}
	d := confirmDialog("Delete secret", "", func(string) tea.Cmd {
		m.dialog = statusDialog("Secrets", "deleting "+name+"…")
		return func() tea.Msg {
			err := m.ds.DeleteSecret(m.ctx, id)
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
	return tea.Batch(m.loadSecrets(), m.loadCredentialRequests(), m.report(false, "%s", msg.did))
}

func (m *Model) viewSecrets() string {
	// The credentials the project holds, and under them what is waiting on a
	// person. They are read together: which secret answers this request is the
	// question, and the answer is in the table above it.
	body := lipgloss.JoinVertical(lipgloss.Left,
		m.secrets.view(m.st, !m.onRequests),
		"",
		"",
		m.requestRows.view(m.st, m.onRequests),
	)
	// The secrets take only the rows they have, so what is left over falls
	// below the lower table rather than between the two: the window keeps its
	// full height, and the tables stay next to each other where they are read
	// together.
	body = fillRows(body, max(m.height-secretsChrome, 0))
	if m.showLogo() {
		body = lipgloss.JoinHorizontal(lipgloss.Top, m.logo.view(lipgloss.Height(body)), body)
	}
	rows := []string{m.viewHeader(m.inner()), ""}
	rows = append(rows, strings.Split(body, "\n")...)
	rows = append(rows, m.viewStatus())
	return m.box("", rows)
}

// secretsChrome is what this screen costs in rows before a single secret is
// drawn: the box's edges, the header and the blank under it, the list title,
// the blank after the rows, and the status line.
const secretsChrome = 7

// requestsChrome is what the lower table costs beside its rows: its title, and
// the two blank rows between the tables.
const requestsChrome = 3

// secretHints is the bottom line here: what the secret under the cursor can
// take, most reached-for first.
func (m *Model) secretHints() string {
	if m.onRequests {
		return strings.Join([]string{"↑↓ move", "enter answer it", "tab secrets", "esc back"}, hintSep)
	}
	hints := []string{"↑↓ move", "n new", "enter grants", grantCreateKey + " grant", "e edit", "d delete"}
	if len(m.requestRows.all) > 0 {
		hints = append(hints, "tab requests")
	}
	return strings.Join(append(hints, "esc back"), hintSep)
}
