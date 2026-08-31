package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

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
		return m.askForSecretName()
	case "enter", "v":
		return m.showGrants()
	case "e":
		return m.askWhatToEdit()
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

// askForGrantScope starts a pre-approval. Scope first, because it decides
// whether there is a key to ask for next: a project grant needs none, and the
// other two are a discobox or a harness to pick out of what the window
// already holds.
func (m *Model) askForGrantScope() tea.Cmd {
	secret := m.secrets.current()
	if secret == nil {
		return nil
	}
	grant := NewGrant{SecretID: secret.ID, Host: secret.Host, TTLSeconds: int64(secret.MaxTTL / time.Second)}
	items := []action{
		{key: "project", label: "Every discobox in this project", detail: "the widest of the three", enabled: true},
		{key: "harnessConfig", label: "One harness", detail: "every discobox running it", enabled: len(m.harnesses.all) > 0,
			why: "this project has no harnesses"},
		{key: "sandbox", label: "One discobox", detail: "the narrowest, and what approving a request mints", enabled: len(m.list.all) > 0,
			why: "this project has no discoboxes"},
	}
	d := actionsDialog("Grant "+secret.Name, "A standing grant authorizes this credential before anything asks for it.\n\nWho may use it?", items,
		func(scope string) tea.Cmd {
			grant.Scope = scope
			if scope == grantScopeProject {
				return m.askHowItMayBeUsed(grant, "every discobox in this project")
			}
			return m.askForGrantScopeKey(grant)
		})
	d.footer = "the narrower the scope, the less a leaked sentinel is worth"
	m.dialog = d
	return nil
}

// grantScopeProject is the server's word for the widest scope, spelled here
// because the window speaks the API's vocabulary rather than importing the
// server's.
const grantScopeProject = "project"

// askForGrantScopeKey picks what the scope resolves against, out of what the
// window is already holding: no typing an ID that has to be right.
func (m *Model) askForGrantScopeKey(grant NewGrant) tea.Cmd {
	var items []action
	var title, body string
	if grant.Scope == "sandbox" {
		title, body = "Which discobox?", "Only this discobox may use the credential."
		for _, box := range m.list.all {
			items = append(items, action{key: box.ID, label: box.Name, detail: box.ID, enabled: true})
		}
	} else {
		title, body = "Which harness?", "Every discobox running it may use the credential."
		for _, h := range m.harnesses.all {
			items = append(items, action{key: h.ID, label: h.Name, detail: h.ID, enabled: true})
		}
	}
	m.dialog = actionsDialog(title, body, items, func(key string) tea.Cmd {
		grant.ScopeKey = key
		label := key
		for _, item := range items {
			if item.key == key {
				label = item.label
			}
		}
		return m.askHowItMayBeUsed(grant, label)
	})
	return nil
}

// askHowItMayBeUsed is the question that decides what kind of credential this
// is. Anything in the discobox can read an injected one — it is an environment
// variable, and everything in the box shares the environment. A credential
// with declared uses is never injected: the in-sandbox CLI takes it one use at
// a time, for one command, and the value dies minutes later (ADR 0031 §4).
//
// It is asked at every scope. The binding a credential resolves through is per
// discobox, but it is minted when that discobox's agent first asks, so a grant
// on a harness or a project can carry uses without knowing which boxes
// it will cover.
func (m *Model) askHowItMayBeUsed(grant NewGrant, who string) tea.Cmd {
	items := []action{
		{key: "access", label: "Only through discobox-access", enabled: true,
			detail: "never injected; taken one declared use at a time"},
		{key: "any", label: "Anything in the discobox", enabled: true,
			detail: "the credential the discobox is already provisioned with"},
	}
	d := actionsDialog("How may it be used?",
		"A credential in the discobox's environment is readable by everything in it. One with declared uses is not there at all.",
		items, func(kind string) tea.Cmd {
			if kind != "access" {
				return m.askForGrantHost(grant, who)
			}
			return m.askForGrantEnvVar(grant, who)
		})
	d.footer = "the narrower kind is the one an agent asks for"
	m.dialog = d
	return nil
}

// askForGrantEnvVar names the variable the wrapped command receives it in.
func (m *Model) askForGrantEnvVar(grant NewGrant, who string) tea.Cmd {
	body := "Which environment variable does the agent receive it in?\n\n" +
		"Only the command `discobox-access run` wraps ever sees it, and only for that one command."
	m.dialog = inputDialog("Delivered as", body, "GH_TOKEN", "", func(envVar string) tea.Cmd {
		grant.EnvVar = strings.TrimSpace(envVar)
		if grant.EnvVar == "" {
			return m.report(true, "a credential taken by the CLI needs a variable to arrive in; nothing was granted")
		}
		return m.askForGrantUse(grant, who)
	})
	return nil
}

// askForGrantUse is the sentence the credential is granted for. The agent
// presents its ID to take a value, and the in-sandbox judge holds the command
// it is about to run up against these words (ADR 0078).
func (m *Model) askForGrantUse(grant NewGrant, who string) tea.Cmd {
	body := "What may it be used for?\n\n" +
		"The agent names this use to take a value, and `discobox-access run` asks a model whether the command it is about to run is this sentence. Write it the way you would tell a person."
	m.dialog = inputDialog("Approved use", body, "Open a pull request against the current repo", "", func(use string) tea.Cmd {
		use = strings.TrimSpace(use)
		if use == "" {
			return m.report(true, "a use is the sentence the credential is granted for; nothing was granted")
		}
		grant.Uses = []string{use}
		return m.askForGrantHost(grant, who)
	})
	return nil
}

// askForGrantHost asks where the credential may travel under this grant. It
// opens on the secret's own binding, which is both the common answer and the
// widest one the server will accept.
func (m *Model) askForGrantHost(grant NewGrant, who string) tea.Cmd {
	body := fmt.Sprintf("Where may it be sent, for %s?\n\n"+
		"The host covers itself and everything beneath it. A grant may not reach outside the secret's own binding.", who)
	m.dialog = inputDialog("Grant host", body, "github.com", grant.Host, func(host string) tea.Cmd {
		grant.Host = strings.TrimSpace(host)
		return m.askForGrantTTL(grant)
	})
	return nil
}

// askForGrantTTL asks how long it lives. Seconds, prefilled from the secret's
// limit, which is also the lifetime nobody has to choose.
//
// The limit is said in the question rather than left for the server to refuse:
// a capped secret cannot be granted forever, and finding that out after typing
// 0 teaches the rule one rejection at a time.
func (m *Model) askForGrantTTL(grant NewGrant) tea.Cmd {
	limit := m.secretLimit(grant.SecretID)
	body := "How long does it live, in seconds?\n\n0 never expires, which is a credential nobody has to think about again — and one nothing takes away."
	if limit > 0 {
		body = fmt.Sprintf("How long does it live, in seconds?\n\nThis credential allows at most %s, so it cannot be granted forever.",
			grantLimit(limit))
	}
	m.dialog = inputDialog("Grant lifetime", body, "3600", itoa(int(grant.TTLSeconds)), func(value string) tea.Cmd {
		seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || seconds < 0 {
			return m.report(true, "a lifetime is a number of seconds; nothing was granted")
		}
		grant.TTLSeconds = seconds
		return m.mintGrant(grant)
	})
	return nil
}

// secretLimit is the ceiling of the secret a grant stands on, or zero when it
// has none — read from the rows the screen already holds rather than asked for
// again.
func (m *Model) secretLimit(secretID string) time.Duration {
	for _, s := range m.secrets.all {
		if s.ID == secretID {
			return s.MaxTTL
		}
	}
	return 0
}

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
		label:   "Grant this secret…",
		detail:  "authorize it before anything asks",
		enabled: true,
	})
	for _, g := range grants {
		items = append(items, action{
			key:     g.ID,
			label:   grantLabel(g),
			detail:  grantDetail(g, m.secrets.now()),
			enabled: true,
		})
	}
	// Enter reads, x withdraws. The other way round put revoking a credential
	// one keystroke from looking at one, on the key that means "open this"
	// everywhere else in the window.
	// What the credential is, before what may use it. For an OAuth credential
	// that is most of what there is to know about it — where it renews, what
	// the grant may do, and when the access token goes stale — and none of it
	// is the credential.
	body := describeSecret(*secret, m.secrets.now())
	if len(grants) == 0 {
		body += "\n\nNothing stands on this secret yet, so nothing may use it."
	} else {
		body += "\n\nEnter reads a grant and what it was approved for. " + grantRevokeKey +
			" withdraws it: the credential stops resolving at once, and the request that produced it stays approved, because it is history."
	}
	d := actionsDialog(secret.Name, body, items, func(result string) tea.Cmd {
		if result == grantCreateItem {
			return m.askForGrantScope()
		}
		return m.reviewGrant(secret.Name, result)
	})
	d.footer = "enter reads · " + grantRevokeKey + " revokes · esc leaves them alone"
	d.altKey = grantRevokeKey
	d.alt = func(grantID string) tea.Cmd { return m.confirmRevokeGrant(secret.Name, grantID) }
	m.dialog = d
	return nil
}

// describeSecret is what a credential is, in the words that can be shown. The
// value is never among them; for an OAuth credential the renewal material is
// not either, and what is left — where it renews, whose grant it is, what it
// may do, when it goes stale — is the part somebody managing it needs.
func describeSecret(secret Secret, now time.Time) string {
	var b strings.Builder
	where := secret.Host
	if where == "" {
		where = "any host a grant allows"
	} else {
		where += " and the hosts beneath it"
	}
	fmt.Fprintf(&b, "  %s · may be sent to %s\n", secret.Type, where)
	if secret.MaxTTL > 0 {
		fmt.Fprintf(&b, "  grants on it last %s, and no longer", shortDuration(secret.MaxTTL))
	} else {
		b.WriteString("  grants on it may live forever: nothing limits them")
	}
	if secret.OAuth == nil {
		return b.String()
	}
	b.WriteString("\n")
	if secret.OAuth.TokenURL != "" {
		fmt.Fprintf(&b, "\n  renews at    %s", secret.OAuth.TokenURL)
	}
	if secret.OAuth.ClientID != "" {
		fmt.Fprintf(&b, "\n  client       %s", secret.OAuth.ClientID)
	}
	if len(secret.OAuth.Scopes) > 0 {
		fmt.Fprintf(&b, "\n  may do       %s", strings.Join(secret.OAuth.Scopes, " "))
	}
	if secret.OAuth.SubscriptionType != "" {
		fmt.Fprintf(&b, "\n  plan         %s", secret.OAuth.SubscriptionType)
	}
	if !secret.OAuth.AccessTokenExpiresAt.IsZero() {
		fmt.Fprintf(&b, "\n  token stale  %s", accessTokenExpiry(secret.OAuth.AccessTokenExpiresAt, now))
	}
	if !secret.OAuth.Refreshable {
		// The one thing worth saying loudly about an oauth credential: it
		// cannot renew, so it will expire and stay expired.
		b.WriteString("\n  cannot renew itself: no refresh token or nowhere to spend it")
	}
	return b.String()
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
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", grant.ID)
	fmt.Fprintf(&b, "  secret     %s\n", secretName)
	fmt.Fprintf(&b, "  scope      %s", scopeLabel(grant.Scope))
	if grant.ScopeKey != "" {
		fmt.Fprintf(&b, "  %s", grant.ScopeKey)
	}
	where := grant.Host
	if where == "" {
		where = "any host"
	}
	fmt.Fprintf(&b, "\n  may go to  %s", where)
	if grant.Host != "" {
		b.WriteString(" and the hosts beneath it")
	}
	if grant.GrantedBy != "" {
		fmt.Fprintf(&b, "\n  granted by %s", grant.GrantedBy)
	}
	fmt.Fprintf(&b, "\n  expires    %s\n", grantExpiry(*grant, m.secrets.now()))
	switch {
	case len(grant.Uses) == 0:
		b.WriteString("\nNo declared uses: a standing grant authorizes the credential without enumerating what it is for.")
	default:
		b.WriteString("\nApproved for:\n")
		for _, use := range grant.Uses {
			fmt.Fprintf(&b, "\n  %s\n    %s\n", use.Description, use.ID)
		}
		b.WriteString("\nAn agent takes a value under one of those IDs: `discobox-access run --use <id> -- <command>`.")
	}
	d := textDialog("Grant", b.String())
	// Back to the grants you were reading, not to the list behind them. The
	// common next move after reading one is reading the next, or withdrawing
	// this one, and both are in the list this came from.
	d.onCancel = func() tea.Cmd { return m.showGrants() }
	m.dialog = d
	return nil
}

// confirmRevokeGrant asks before withdrawing, and says what stops working.
func (m *Model) confirmRevokeGrant(secretName, grantID string) tea.Cmd {
	body := fmt.Sprintf("Withdraw this grant on %s?\n\nThe credential stops resolving at once, wherever it is being used. The request that produced it stays approved, because it is history.", secretName)
	d := confirmDialog("Revoke grant", body, func(string) tea.Cmd { return m.revokeGrant(grantID) })
	d.defaultNo = true
	// No goes back to the grants, the way leaving a grant you were reading
	// does: a question answered "not that one" should leave you where the
	// question was asked, not on the screen behind it.
	d.onCancel = func() tea.Cmd { return m.showGrants() }
	m.dialog = d
	return nil
}

func grantLabel(g Grant) string {
	where := g.Host
	if where == "" {
		where = "any host"
	}
	return where + "  ·  " + scopeLabel(g.Scope)
}

func grantDetail(g Grant, now time.Time) string {
	parts := []string{}
	if g.ScopeKey != "" {
		parts = append(parts, g.ScopeKey)
	}
	if len(g.Uses) > 0 {
		parts = append(parts, plural(len(g.Uses), "approved use", "approved uses"))
	}
	return strings.Join(append(parts, grantExpiry(g, now)), " · ")
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

// askForSecretName starts a new secret: a name, then the host it belongs to,
// then the value. Three questions rather than one form, because that is the
// window's one modal shape — and because the value is the one that must be
// masked, so it comes last and alone.
func (m *Model) askForSecretName() tea.Cmd {
	m.dialog = inputDialog("New secret", "What is it called?", "github", "", func(name string) tea.Cmd {
		name = strings.TrimSpace(name)
		if name == "" {
			return m.report(true, "a secret needs a name")
		}
		return m.askForSecretKind(NewSecret{Name: name})
	})
	return nil
}

// askForSecretKind splits the two credentials the system has. A token is one
// opaque string. An OAuth credential is that plus what the control plane spends
// to renew it — and the difference is not cosmetic: an access token stored as a
// token expires and stays expired, while an oauth secret is refreshed
// server-side as it ages out (ADR 0011).
func (m *Model) askForSecretKind(secret NewSecret) tea.Cmd {
	items := []action{
		{key: "token", label: "A token", detail: "one opaque string, however it is presented", enabled: true},
		{key: "oauth", label: "An OAuth credential", detail: "renews itself: an access token, a refresh token, and where to spend it", enabled: true},
	}
	m.dialog = actionsDialog("New secret", "What kind of credential is it?", items, func(kind string) tea.Cmd {
		secret.Type = kind
		return m.askForSecretBinding(secret)
	})
	return nil
}

func (m *Model) askForSecretBinding(secret NewSecret) tea.Cmd {
	body := "Which host may it be sent to?\n\n" +
		"It covers that host and everything beneath it: github.com answers for\n" +
		"api.github.com too. Leave it empty and the grants decide alone."
	m.dialog = inputDialog("New secret", body, "github.com (optional)", "", func(host string) tea.Cmd {
		secret.Host = strings.TrimSpace(host)
		return m.askForSecretValue(secret)
	})
	return nil
}

func (m *Model) askForSecretValue(secret NewSecret) tea.Cmd {
	what := "Paste the token. It is stored encrypted and never shown again."
	if secret.Type == "oauth" {
		what = "Paste the access token — the one that travels, and the one that expires.\n\nIt is stored encrypted and never shown again."
	}
	d := inputDialog("New secret", what, "token", "", func(value string) tea.Cmd {
		value = strings.TrimSpace(value)
		if value == "" {
			return m.report(true, "no token entered; nothing was stored")
		}
		secret.Value = value
		if secret.Type == "oauth" {
			return m.askForRefreshToken(secret)
		}
		return m.storeSecret(secret)
	})
	d.input.EchoMode = 1 // textinput.EchoPassword: a credential is not drawn back.
	m.dialog = d
	return nil
}

// askForRefreshToken and the two questions after it are what make an OAuth
// credential one: the token the control plane spends, where to spend it, and
// who it is spent as. Without them there is nothing to renew with, and the
// server refuses the secret rather than storing a promise it cannot keep.
func (m *Model) askForRefreshToken(secret NewSecret) tea.Cmd {
	d := inputDialog("New secret",
		"Paste the refresh token.\n\nOnly the control plane ever spends it: it is never delivered to a discobox, and never leaves in a response.",
		"refresh token", "", func(value string) tea.Cmd {
			secret.RefreshToken = strings.TrimSpace(value)
			if secret.RefreshToken == "" {
				return m.report(true, "an oauth credential needs a refresh token; nothing was stored")
			}
			return m.askForTokenURL(secret)
		})
	d.input.EchoMode = 1
	m.dialog = d
	return nil
}

func (m *Model) askForTokenURL(secret NewSecret) tea.Cmd {
	m.dialog = inputDialog("New secret", "Where is the refresh token exchanged for a new access token?",
		"https://console.anthropic.com/v1/oauth/token", "", func(value string) tea.Cmd {
			secret.TokenURL = strings.TrimSpace(value)
			if secret.TokenURL == "" {
				return m.report(true, "an oauth credential needs somewhere to renew; nothing was stored")
			}
			return m.askForClientID(secret)
		})
	return nil
}

// askForClientID and the scopes after it are optional: a token endpoint may not
// ask who is calling, and scopes are what a client gates its own features on
// rather than anything this system enforces.
func (m *Model) askForClientID(secret NewSecret) tea.Cmd {
	m.dialog = inputDialog("New secret", "Which OAuth client does the grant belong to?\n\nEnter alone if the token endpoint does not ask.",
		"client id (optional)", "", func(value string) tea.Cmd {
			secret.ClientID = strings.TrimSpace(value)
			return m.askForScopes(secret)
		})
	return nil
}

func (m *Model) askForScopes(secret NewSecret) tea.Cmd {
	body := "What may the grant do?\n\n" +
		"Space separated, as the authorization server returned them. A client may gate features on these — Claude Code refuses Remote Control without user:profile — so record them rather than guess. Enter alone to record none."
	m.dialog = inputDialog("New secret", body, "user:profile user:inference", "", func(value string) tea.Cmd {
		secret.Scopes = strings.Fields(value)
		return m.storeSecret(secret)
	})
	return nil
}

func (m *Model) storeSecret(secret NewSecret) tea.Cmd {
	m.dialog = statusDialog("Secrets", "storing "+secret.Name+"…")
	return func() tea.Msg {
		_, err := m.ds.CreateSecret(m.ctx, secret)
		return secretActionMsg{did: "stored " + secret.Name, err: err}
	}
}

// askWhatToEdit offers the two things a secret says about itself. They are the
// same shape of statement — the binding limits where the credential may go, the
// limit limits how long consent to it may last — and both are answered without
// touching the value, which nothing here can read.
func (m *Model) askWhatToEdit() tea.Cmd {
	secret := m.secrets.current()
	if secret == nil {
		return nil
	}
	where := secret.Host
	if where == "" {
		where = "any host a grant allows"
	}
	items := []action{
		{key: "host", label: "Which hosts it may reach", detail: where, enabled: true},
		{key: "ttl", label: "How long a grant on it may live", detail: grantLimit(secret.MaxTTL), enabled: true},
	}
	m.dialog = actionsDialog("Edit "+secret.Name, "What about it changes?", items, func(what string) tea.Cmd {
		if what == "ttl" {
			return m.askForSecretMaxTTL()
		}
		return m.askForSecretHost()
	})
	return nil
}

// askForSecretMaxTTL edits the ceiling on grant lifetimes. Lowering it binds
// what is granted next, not what already stands: a live grant is somebody's
// decision, and it is revoked rather than quietly shortened.
func (m *Model) askForSecretMaxTTL() tea.Cmd {
	secret := m.secrets.current()
	if secret == nil {
		return nil
	}
	body := fmt.Sprintf("How long may a grant on %s live, in seconds?\n\n"+
		"It is a ceiling, not a suggestion: a longer grant is refused. 0 lifts it,\n"+
		"and grants on it may then never expire.", secret.Name)
	id, name := secret.ID, secret.Name
	m.dialog = inputDialog("Grant limit", body, "3600", itoa(int(secret.MaxTTL/time.Second)), func(value string) tea.Cmd {
		seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || seconds < 0 {
			return m.report(true, "a limit is a number of seconds; nothing changed")
		}
		m.dialog = statusDialog("Secrets", "limiting "+name+"…")
		return func() tea.Msg {
			err := m.ds.SetSecretMaxGrantTTL(m.ctx, id, seconds)
			did := "grants on " + name + " now last at most " + grantLimit(time.Duration(seconds)*time.Second)
			if seconds <= 0 {
				did = "grants on " + name + " may now live forever"
			}
			return secretActionMsg{did: did, err: err}
		}
	})
	return nil
}

// askForSecretHost edits the binding of the secret under the cursor. Widening
// it is the act worth being deliberate about, so the question says what the
// field means rather than only naming it.
func (m *Model) askForSecretHost() tea.Cmd {
	secret := m.secrets.current()
	if secret == nil {
		return nil
	}
	body := fmt.Sprintf("Which host may %s be sent to?\n\n"+
		"It covers that host and everything beneath it. Empty releases the\n"+
		"binding, and the grants decide alone.", secret.Name)
	id, name := secret.ID, secret.Name
	m.dialog = inputDialog("Binding", body, "github.com", secret.Host, func(host string) tea.Cmd {
		host = strings.TrimSpace(host)
		m.dialog = statusDialog("Secrets", "binding "+name+"…")
		return func() tea.Msg {
			err := m.ds.SetSecretHost(m.ctx, id, host)
			did := "bound " + name + " to " + host
			if host == "" {
				did = "released " + name + "'s binding"
			}
			return secretActionMsg{did: did, err: err}
		}
	})
	return nil
}

func (m *Model) confirmDeleteSecret() tea.Cmd {
	secret := m.secrets.current()
	if secret == nil {
		return nil
	}
	body := fmt.Sprintf("Delete %s?\n\nEverything standing on it goes too", secret.Name)
	if secret.Grants > 0 {
		body += fmt.Sprintf(" — %s", plural(secret.Grants, "live grant", "live grants"))
	}
	body += ". A discobox holding a sentinel for it keeps a placeholder that now resolves to nothing."
	id, name := secret.ID, secret.Name
	d := confirmDialog("Delete secret", body, func(string) tea.Cmd {
		m.dialog = statusDialog("Secrets", "deleting "+name+"…")
		return func() tea.Msg {
			err := m.ds.DeleteSecret(m.ctx, id)
			return secretActionMsg{did: "deleted " + name, err: err}
		}
	})
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
