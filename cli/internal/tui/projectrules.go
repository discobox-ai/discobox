package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/wellknown"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// The project's approval rules: the third table of the secrets screen, under
// the requests they answer, and one rule's card (ADR 26-10-12-330 §§1, 4).
//
// A rule answers requests nobody is shown, so what a project holds has to be
// somewhere a person can read it: a row a rule, then one rule whole — in the
// sections the card that made it was read in (rules.go) — with the grants it
// minted that still stand. It is made (n) and edited (e) here, a step at a
// time (ruleedit.go); disabled, enabled or deleted from its row or its card;
// and one of its grants revoked from the card. Auto approve on a request and
// the CLI make the same rule.

// ruleToggleKey disables or enables the rule under the cursor, and the rule a
// card is of; ruleDeleteKey deletes it, behind a question.
const (
	ruleToggleKey = "s"
	ruleDeleteKey = "d"
)

// Rows of a rule's card that are not a grant.
const (
	ruleToggleItem = "rule:toggle"
	ruleDeleteItem = "rule:delete"
)

// ruleList is the screen's third table and where the cursor is in it.
type ruleList struct {
	all    []Rule
	cursor int
	offset int

	// loaded is a listing that has landed, as on the secrets: until it has,
	// the table does not say the project has none.
	loaded bool
	// failed is why the last listing could not be read, on one line. The
	// table then says so where its rows would be, and holds none.
	failed string

	width, height int

	now func() time.Time

	// drawn is which rows the last render put on screen; see zones.go.
	drawn drawn
}

func newRuleList() *ruleList { return &ruleList{now: time.Now} }

// clear forgets the listing, for a server whose rules have not been read yet.
func (l *ruleList) clear() { l.all, l.loaded, l.failed, l.cursor, l.offset = nil, false, "", 0, 0 }

// fail takes a listing that could not be read. The rows go with it: rules
// drawn from an earlier read would be acted on as though they were current.
// The next read tries again.
func (l *ruleList) fail(err error) {
	l.clear()
	l.failed = strings.Join(strings.Fields(err.Error()), " ")
}

// setAll takes a refreshed listing, keeping the cursor on the rule it was on.
// One that is gone leaves the cursor where it was, on the row that took its
// place.
func (l *ruleList) setAll(all []Rule) {
	l.loaded, l.failed = true, ""
	var onID string
	if r := l.current(); r != nil {
		onID = r.ID
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

func (l *ruleList) current() *Rule {
	if l.cursor < 0 || l.cursor >= len(l.all) {
		return nil
	}
	return &l.all[l.cursor]
}

func (l *ruleList) move(delta int) { l.cursor += delta; l.clamp() }

func (l *ruleList) moveTo(i int) { l.cursor = i; l.clamp() }

func (l *ruleList) clamp() {
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

// noRules is the table's empty line: that there are none, and how one is
// made. The empty table still takes the cursor, so n reaches it.
const noRules = "no rules — n makes one; auto approve on a request makes one too"

// view draws the table. credential names a rule's credential, which for a
// project secret is the secret's name and so the screen's to say.
func (l *ruleList) view(st *styles, z *zones, focused bool, credential func(Rule) string) string {
	titleStyle := st.titleList
	if !focused {
		titleStyle = st.titleDim
	}
	right := ""
	if len(l.all) > 0 {
		right = plural(len(l.all), "rule", "rules")
	}
	blank := strings.Repeat(" ", max(l.width, 0))
	out := []string{renderTitle(titleStyle, "Rules", right, l.width)}

	body := make([]string, 0, l.height)
	switch {
	case l.failed != "":
		body = append(body, st.dimText.Render(pad(truncate("  cannot read rules: "+l.failed, l.width), l.width)))
	case len(l.all) == 0 && l.loaded:
		marker := "  "
		if focused {
			marker = st.key.Render("❯") + " "
		}
		body = append(body, padANSI(marker+st.dimText.Render(truncate(noRules, max(l.width-2, 0))), l.width))
	}
	// The credential and repositories columns are as wide as the widest of
	// them, within bounds, so the uses line up and get the rest.
	credW, forW := 0, 0
	for _, rule := range l.all {
		credW = max(credW, lipgloss.Width(credential(rule))+2)
		forW = max(forW, lipgloss.Width(ruleRowDiscoboxes(rule))+2)
	}
	credW = min(max(credW, ruleCredentialMin), ruleCredentialWidth)
	forW = min(max(forW, ruleDiscoboxesMin), ruleDiscoboxesMax)
	l.drawn = drawn{top: len(out), first: l.offset}
	for i := l.offset; i < len(l.all) && len(body) < l.height; i++ {
		body = append(body, l.row(st, l.all[i], credential(l.all[i]), credW, forW, i, focused))
		l.drawn.count++
	}
	for len(body) < l.height {
		body = append(body, blank)
	}
	block := append(out, body...)
	z.markList(hitRuleRow, l.drawn, l.width, len(block))
	return lipgloss.JoinVertical(lipgloss.Left, block...)
}

// row draws one rule: its credential, whether it is answering, which
// discoboxes for, how much it approves and until when. Columns drop off the
// right end as the terminal narrows, as on the tables above; the credential
// never goes.
//
// The uses are the column that takes what the others leave and is cut to
// it: they are what tells two rules apart, and Enter reads them whole.
func (l *ruleList) row(st *styles, rule Rule, credential string, credW, forW, i int, focused bool) string {
	atCursor := i == l.cursor && focused
	now := l.now()

	tail := ""
	addCol := func(text string, w int) {
		if l.width-lipgloss.Width(tail)-w < ruleLeadWidth {
			return
		}
		tail += padANSI(text, w)
	}
	addCol("  "+st.dimText.Render(pad(ruleRowExpiry(rule, now), 14)), 17)

	marker := "  "
	if atCursor {
		marker = st.key.Render("❯") + " "
	}
	nameStyle := st.name
	if atCursor {
		nameStyle = st.cursorName
	}
	room := max(l.width-lipgloss.Width(marker)-lipgloss.Width(tail), 4)
	// The state and then the repositories come out of the credential's room, each
	// only while the credential keeps a column of its own; narrower than
	// that the row is the credential.
	lead := padANSI(nameStyle.Render(truncate(credential, room)), room)
	if rest := room - credW - ruleStateWidth; rest >= 0 {
		state := ruleState(rule, now)
		lead = padANSI(nameStyle.Render(truncate(credential, credW-2)), credW) +
			ruleStateTone(state).style(st).Render(pad(state, ruleStateWidth))
		// What it approves takes what the repositories leave, which is most
		// of a wide row: the uses are what tells one rule from the next.
		// Narrower, the repositories give up room to the uses down to two
		// fifths of what the two share once the uses would have less than
		// ruleUsesWanted, and only then do the uses go.
		if rest-forW < ruleUsesWanted {
			forW = min(forW, max(ruleDiscoboxesMin, rest*2/5))
		}
		usesW := 0
		if rest-forW >= ruleUsesMin {
			usesW = rest - forW
		} else {
			forW = rest
		}
		discoboxes := ""
		if forW >= ruleDiscoboxesMin {
			discoboxes = truncate(ruleRowDiscoboxes(rule), forW-2)
		}
		lead += st.name.Render(pad(discoboxes, forW))
		if usesW > 0 {
			lead += st.dimText.Render(pad(truncate(ruleRowUses(rule), usesW), usesW))
		}
	}
	line := padANSI(marker+lead+tail, l.width)
	if atCursor {
		return highlight(st, line, colHighlightBG)
	}
	return line
}

// A rule row's columns. ruleLeadWidth is what the tail columns leave the
// credential, state and repositories before one of them is dropped instead.
const (
	ruleCredentialWidth = 24
	ruleCredentialMin   = 12
	ruleStateWidth      = 10
	ruleDiscoboxesMin   = 16
	ruleDiscoboxesMax   = 36
	ruleUsesMin         = 12
	ruleUsesWanted      = 24
	ruleLeadWidth       = ruleCredentialWidth + ruleStateWidth + ruleDiscoboxesMin + 2
)

// ruleRowDiscoboxes is which discoboxes a rule is for at the width of a column:
// its repositories, or — with none — that it is for every discobox, which the
// card says the meaning of. A rule to delegate says so first, as a grant's row
// does: one to use the same credential is a different rule.
func ruleRowDiscoboxes(rule Rule) string {
	discoboxes := strings.Join(rule.Repositories, ", ")
	if rule.EveryDiscobox {
		discoboxes = "every discobox"
	}
	if rule.Delegate {
		return "delegates · " + discoboxes
	}
	return discoboxes
}

// ruleRowUses is what a rule approves on one line: its uses in order, each
// flattened, cut by the column rather than counted, so a row says as much of
// them as it has room for.
func ruleRowUses(rule Rule) string {
	uses := make([]string, 0, len(rule.Uses))
	for _, use := range rule.Uses {
		uses = append(uses, strings.Join(strings.Fields(use), " "))
	}
	return strings.Join(uses, " · ")
}

// ruleRowExpiry is the last column: when the rule itself stops answering.
func ruleRowExpiry(rule Rule, now time.Time) string {
	switch {
	case rule.Expires.IsZero():
		return "no expiry"
	case !rule.Expires.After(now):
		return "lapsed " + ago(rule.Expires, now)
	}
	return "expires " + ruleExpiry(rule, now)
}

// ruleRowCredential is a rule's credential as its row names it.
func (m *Model) ruleRowCredential(rule Rule) string {
	credential, _ := m.ruleCredential(rule)
	return credential
}

// updateRuleTable handles the third table: moving in it, and reading or
// changing the rule under the cursor.
func (m *Model) updateRuleTable(msg tea.KeyPressMsg) tea.Cmd {
	switch keyName(msg) {
	case "up", "k":
		// Off the top is what the rules answer, and above that the secrets.
		if m.rules.cursor <= 0 {
			m.table = m.tableAbove(tableRules)
			return nil
		}
		m.rules.move(-1)
	case "down", "j":
		m.rules.move(1)
	case "pgup":
		m.rules.move(-m.rules.height)
	case "pgdown":
		m.rules.move(m.rules.height)
	case "home", "g":
		m.rules.moveTo(0)
	case "end", "G":
		m.rules.moveTo(len(m.rules.all) - 1)
	case "enter", "v":
		if rule := m.rules.current(); rule != nil {
			return m.openRule(m.configServer(), rule.ID)
		}
	case ruleNewKey:
		return m.newRule()
	case ruleEditKey:
		if rule := m.rules.current(); rule != nil {
			return m.editRule(m.configServer(), *rule, false)
		}
	case ruleToggleKey:
		if rule := m.rules.current(); rule != nil {
			return m.setRuleEnabled(m.configServer(), *rule, !rule.Enabled, false)
		}
	case ruleDeleteKey:
		if rule := m.rules.current(); rule != nil {
			return m.confirmDeleteRule(m.configServer(), *rule, len(m.standingGrants(m.secrets.grants, rule.ID)), nil)
		}
	}
	return nil
}

// ruleHints is the bottom line on a rule row: what that rule can take, with
// the word for what the toggle would do to it. On the empty table it is only
// making one.
func (m *Model) ruleHints(tab hint) []hint {
	rule := m.rules.current()
	if rule == nil {
		return []hint{keyed(ruleNewKey, ruleNewKey, "new rule"), tab, keyed("esc", "esc", "back")}
	}
	toggle := "disable"
	if !rule.Enabled {
		toggle = "enable"
	}
	return []hint{
		says("↑↓ move"),
		keyed(ruleNewKey, ruleNewKey, "new"),
		keyed("enter", "enter", "read it"),
		keyed(ruleEditKey, ruleEditKey, "edit"),
		keyed(ruleToggleKey, ruleToggleKey, toggle),
		keyed(ruleDeleteKey, ruleDeleteKey, "delete"),
		tab,
		keyed("esc", "esc", "back"),
	}
}

// standingGrants is the grants one rule minted that still authorize
// something. One that has lapsed has nothing to withdraw.
func (m *Model) standingGrants(grants []Grant, ruleID string) []Grant {
	now := m.secrets.now()
	var out []Grant
	for _, g := range grants {
		if g.RuleID == ruleID && (g.Expires.IsZero() || g.Expires.After(now)) {
			out = append(out, g)
		}
	}
	return out
}

// ruleCardMsg carries what a rule's card is drawn from back. wait is the
// dialog that was up while it was read, as for the proposals
// (ruleProposalsMsg).
type ruleCardMsg struct {
	wait   *dialog
	server string
	rule   string
	rules  []Rule
	grants []Grant
	err    error
}

// ruleActionMsg carries a finished change to one rule back, so the rules are
// re-read from one place however it was made.
type ruleActionMsg struct {
	server string
	rule   string
	// card is a change made from the rule's card, which comes back to it;
	// one made from its row stays on the screen. gone is a rule deleted,
	// which has no card to come back to.
	card, gone bool
	did        string
	err        error
}

// openRule reads one rule and the grants it minted — by rule, since a grant
// stands on its own and nothing else says which rule it came from. The card
// starts from a read rather than from the row: a rule can lapse, or be
// changed from the CLI, behind the table.
func (m *Model) openRule(server, ruleID string) tea.Cmd {
	wait := statusDialog("Approval rule", "reading the rule and the grants it minted…")
	m.dialog = wait
	return func() tea.Msg {
		msg := ruleCardMsg{wait: wait, server: server, rule: ruleID}
		msg.rules, msg.err = m.ds.Rules(m.ctx, server)
		if msg.err == nil {
			msg.grants, msg.err = m.ds.Grants(m.ctx, server, GrantFilter{RuleID: ruleID})
		}
		return msg
	}
}

// ruleCardLoaded puts the card up, unless the wait was left, and gives the
// table the listing it was read with, so the row behind the card agrees with
// it. A rule that is gone by the time it is opened has no card.
func (m *Model) ruleCardLoaded(msg ruleCardMsg) tea.Cmd {
	if m.dialog != msg.wait {
		return nil
	}
	if msg.err != nil {
		m.dialog = errorDialog("Could not read the approval rule", fmt.Sprintf("%v", msg.err))
		return nil
	}
	if m.serverName(msg.server) == m.configServer() {
		m.rules.setAll(msg.rules)
		m.leaveEmptyRules()
		m.layout()
	}
	for _, rule := range msg.rules {
		if rule.ID == msg.rule {
			return m.showRule(msg.server, rule, m.standingGrants(msg.grants, rule.ID))
		}
	}
	m.dialog = nil
	return m.report(true, "that rule is no longer there")
}

// showRule is one rule read whole, in the sections the card that made it had,
// and under them what can be done about it: turn it off or on, delete it, or
// withdraw a grant it minted. The grants are rows the way a secret's are —
// Enter reads one, x withdraws it — because a rule that should not have
// approved something is undone a grant at a time (ADR 26-10-12-330 §4).
// server is where the rule was read, which every action from the card is sent
// to; grants are the ones it minted that still stand. Leaving the card is
// back on the screen, on the rule's row.
func (m *Model) showRule(server string, rule Rule, grants []Grant) tea.Cmd {
	now := m.secrets.now()
	credential, answered := m.ruleCredential(rule)
	again := func() tea.Cmd { return m.showRule(server, rule, grants) }
	reread := func() tea.Cmd { return m.openRule(server, rule.ID) }

	toggle := action{key: ruleToggleItem, press: ruleToggleKey, label: "Disable this rule",
		detail: "it mints nothing more, and revokes nothing", enabled: true}
	if !rule.Enabled {
		toggle.label, toggle.detail = "Enable this rule", "it answers the requests it covers again, without asking"
	}
	items := []action{
		{key: ruleEditItem, press: ruleEditKey, label: "Edit this rule…",
			detail: "which discoboxes, what it approves, its longest grant and when it lapses, read whole before it is saved", enabled: true},
		toggle,
		{key: ruleDeleteItem, press: ruleDeleteKey, label: "Delete this rule…",
			detail: "the grants it minted stay, and are revoked one by one", enabled: true},
	}
	standing := map[string]Grant{}
	for _, g := range grants {
		standing[g.ID] = g
		items = append(items, action{key: g.ID, label: m.grantHolder(g), detail: grantHost(g) + " · " + grantExpiry(g, now), enabled: true})
	}
	// The name a grant's cards put on its credential: the secret it stands
	// on, or — for a gate, which no secret answers — the rule's credential.
	named := func(g Grant) string {
		if g.SecretID == "" {
			return credential
		}
		return m.secretName(g.SecretID)
	}

	d := actionsDialog("Approval rule", "", items, func(result string) tea.Cmd {
		switch result {
		case ruleEditItem:
			return m.editRule(server, rule, true)
		case ruleToggleItem:
			return m.setRuleEnabled(server, rule, !rule.Enabled, true)
		case ruleDeleteItem:
			return m.confirmDeleteRule(server, rule, len(standing), again)
		}
		return m.reviewGrant(server, named(standing[result]), standing[result], again)
	})
	d.titleRight = rule.ID
	// What turning it off and deleting it do not do is the reason to read
	// those rows, so their sentences are drawn whole.
	d.wrapDetails = true

	facts := ruleFactsSection(rule, credential, answered, ruleCap(rule))
	state := ruleState(rule, now)
	facts.fields = append([]field{{label: "state", value: state, tone: ruleStateTone(state)}}, facts.fields...)
	facts.fields = append(facts.fields, field{label: "expires", value: ruleExpiry(rule, now)})
	if rule.CreatedBy != "" {
		facts.fields = append(facts.fields, field{label: "made by", value: rule.CreatedBy})
	}
	if made := ago(rule.Created, now); made != "" {
		facts.fields = append(facts.fields, field{label: "made", value: made})
	}
	d.sections = ruleText(rule, facts)

	d.answerLabel = "no grant it minted still stands"
	if len(standing) > 0 {
		d.answerLabel = plural(len(standing), "grant it minted still stands", "grants it minted still stand")
	}
	d.footer = "an edit is read whole before it is saved, and the rule is then tested afresh against what is pending"
	d.keys = []hint{pressing("enter runs it, or reads a grant", "enter"), keyed(grantRevokeKey, grantRevokeKey, "revokes a grant"), pressing("esc leaves it alone", "esc")}
	d.altKey = grantRevokeKey
	d.alt = func(key string) tea.Cmd {
		grant, ok := standing[key]
		if !ok {
			// Not a grant: the card stays, rather than closing on a key that
			// had nothing to withdraw.
			return again()
		}
		return m.confirmRevokeGrant(named(grant), grant, again, reread)
	}
	m.dialog = d
	return nil
}

// setRuleEnabled turns a rule off or on. It asks nothing first: either is
// undone by the other on the same key, and neither touches a grant. card is
// whether it was asked from the rule's card, which it then comes back to.
func (m *Model) setRuleEnabled(server string, rule Rule, enabled, card bool) tea.Cmd {
	doing, did := "disabling", "disabled"
	if enabled {
		doing, did = "enabling", "enabled"
	}
	m.dialog = statusDialog("Approval rules", doing+" "+rule.ID+"…")
	return func() tea.Msg {
		err := m.ds.SetRuleEnabled(m.ctx, server, rule.ID, enabled)
		return ruleActionMsg{server: server, rule: rule.ID, card: card, did: did + " " + rule.ID, err: err}
	}
}

// confirmDeleteRule asks before removing a rule, and says what does not go
// with it: the grants it minted are independent once made, so deleting the
// rule is not how what it approved is taken back. back is the card it was
// asked from, which No returns to; nil is the rule's row.
func (m *Model) confirmDeleteRule(server string, rule Rule, standing int, back func() tea.Cmd) tea.Cmd {
	credential, _ := m.ruleCredential(rule)
	d := confirmDialog("Delete approval rule", "", func(string) tea.Cmd {
		m.dialog = statusDialog("Approval rules", "deleting "+rule.ID+"…")
		return func() tea.Msg {
			err := m.ds.DeleteRule(m.ctx, server, rule.ID)
			return ruleActionMsg{server: server, rule: rule.ID, card: back != nil, gone: true, did: "deleted " + rule.ID, err: err}
		}
	})
	d.titleRight = rule.ID
	stays := line{text: "it has no grants standing", tone: toneDim}
	if standing > 0 {
		stays = line{text: plural(standing, "grant it minted stays", "grants it minted stay") +
			": each is revoked on its own, from the rule's card before it goes or from its secret after", tone: toneAlert}
	}
	goes := section{label: "what goes", fields: []field{
		{label: "credential", value: credential, tone: toneAccent},
		{label: "for", value: ruleDiscoboxesText(rule)},
		{label: "approves", value: plural(len(rule.Uses), "use", "uses")},
	}, lines: []line{
		{text: "requests it would have answered wait for a person again"},
		stays,
	}}
	d.sections = []section{goes}
	d.answerLabel = "delete this rule?"
	d.footer = "to stop it answering and keep what it says, disable it instead"
	// The answer that cannot be undone is yes: the rule's sentences were
	// judged when it was made, and making it again means asking again.
	d.defaultNo = true
	d.onCancel = back
	m.dialog = d
	return nil
}

// ruleActed re-reads the screen after a change, which keeps the cursor on the
// rule — or, for one deleted, on the row that took its place — and comes back
// to the rule's card when that is where it was made. A refusal is shown, and
// leaving it goes back to the card as the server now has the rule.
func (m *Model) ruleActed(msg ruleActionMsg) tea.Cmd {
	if msg.err != nil {
		d := errorDialog("Approval rules", fmt.Sprintf("%v", msg.err))
		if msg.card {
			d.onCancel = func() tea.Cmd { return m.openRule(msg.server, msg.rule) }
		}
		m.dialog = d
		return m.loadSecrets()
	}
	m.dialog = nil
	cmds := []tea.Cmd{m.loadSecrets(), m.report(false, "%s", msg.did)}
	if msg.card && !msg.gone {
		cmds = append(cmds, m.openRule(msg.server, msg.rule))
	}
	return tea.Batch(cmds...)
}

// ruleCredential is what a rule calls its credential, and what answers it
// when that is worth a row of its own: a well-known ID is answered by a
// secret, or — a gate — by none. A rule for a project secret is named by the
// secret, which is also its answer.
func (m *Model) ruleCredential(rule Rule) (credential, answered string) {
	if rule.WellKnownID == "" {
		return m.secretName(rule.SecretID), ""
	}
	switch known, ok := wellknown.Lookup(rule.WellKnownID); {
	case ok && known.Gate:
		answered = gateAnswers
	case rule.SecretID != "":
		answered = m.secretName(rule.SecretID)
	}
	return rule.WellKnownID, answered
}

// secretName is a secret as the screen lists it, or its ID for one the screen
// does not hold — deleted since, or not read yet.
func (m *Model) secretName(secretID string) string {
	for _, secret := range m.secrets.all {
		if secret.ID == secretID {
			return secret.Name
		}
	}
	return "secret " + secretID
}

// grantHolder is who a grant is for, by the name the list knows a discobox
// under where it knows one: a grant a rule mints is one discobox's, and an ID
// alone does not say which.
func (m *Model) grantHolder(g Grant) string {
	who := strings.TrimSpace(scopeLabel(g.Scope) + " " + g.ScopeKey)
	for _, box := range m.list.all {
		if g.ScopeKey != "" && box.ID == g.ScopeKey {
			return box.Name + " · " + who
		}
	}
	return who
}

// ruleDiscoboxesText is which discoboxes a rule is for on one line, for the
// question before it is deleted: ruleDiscoboxesSection's lines, joined.
func ruleDiscoboxesText(rule Rule) string {
	lines := ruleDiscoboxesSection(rule.ApprovalRule).lines
	text := make([]string, 0, len(lines))
	for _, l := range lines {
		text = append(text, l.text)
	}
	return strings.Join(text, "; ")
}

// The three things a rule can be, in the CLI's words.
const (
	ruleEnabled  = "enabled"
	ruleDisabled = "disabled"
	ruleLapsed   = "lapsed"
)

// ruleState is whether a rule is answering. One switched off says that before
// that it has also lapsed, since off is the half a person can change here.
func ruleState(rule Rule, now time.Time) string {
	switch {
	case !rule.Enabled:
		return ruleDisabled
	case !rule.Expires.IsZero() && !rule.Expires.After(now):
		return ruleLapsed
	}
	return ruleEnabled
}

func ruleStateTone(state string) tone {
	switch state {
	case ruleEnabled:
		return toneOK
	case ruleLapsed:
		return toneAlert
	}
	return toneDim
}

// ruleExpiry is when the rule itself stops answering, for a field already
// labeled with what it is.
func ruleExpiry(rule Rule, now time.Time) string {
	switch {
	case rule.Expires.IsZero():
		return "never"
	case !rule.Expires.After(now):
		return "lapsed " + ago(rule.Expires, now)
	}
	return "in " + shortDuration(rule.Expires.Sub(now).Round(time.Minute))
}

// ruleCap is the longest a grant the rule mints may live. Zero is not
// forever: it leaves the limit to the secret the grant stands on.
func ruleCap(rule Rule) string {
	if rule.MaxGrantTTL <= 0 {
		return "the secret's own limit"
	}
	return "at most " + lifetime.Label(rule.MaxGrantTTL)
}
