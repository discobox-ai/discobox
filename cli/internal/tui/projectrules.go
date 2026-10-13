package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/wellknown"

	tea "charm.land/bubbletea/v2"
)

// The project's approval rules, read and managed from the secrets screen
// (ADR 26-10-12-330 §§1, 4).
//
// A rule answers requests nobody is shown, so what a project holds has to be
// somewhere a person can read it: the list, then one rule whole — in the
// sections the card that made it was read in (rules.go) — with the grants it
// minted that still stand. From that card it is disabled, enabled or deleted,
// and one of its grants revoked. Its scope and uses are not edited here: the
// server judges a rule's sentences, and `discobox secret rule update` is where
// that is asked for. Making one stays with "auto approve" and the CLI.
//
// Every card is a dialog over the secrets screen, as a secret's grants are:
// rules are about credentials, and that screen already owns what stands on
// them.

// rulesKey opens the rules from the secrets screen, over either of its tables:
// a rule belongs to the project, not to the secret or the request under the
// cursor. Lower case beside the A that makes one from a request's card.
const rulesKey = "a"

// Rows of a rule's card that are not a grant.
const (
	ruleToggleItem = "rule:toggle"
	ruleDeleteItem = "rule:delete"
)

// ruleBook is the rules as last read, and the server they were read from:
// what a card is drawn from and goes back to, so every action lands on the
// server the rule was read on whatever the header has moved to since.
type ruleBook struct {
	server string
	rules  []Rule
}

// rulesLoadedMsg carries a reading of the rules back. wait is the dialog that
// was up while it was made, as for the proposals (ruleProposalsMsg); open is
// the rule whose card goes up, with the grants it minted, or empty for the
// list.
type rulesLoadedMsg struct {
	wait   *dialog
	book   ruleBook
	open   string
	grants []Grant
	err    error
}

// ruleActionMsg carries a finished change to one rule back, so the rules are
// re-read from one place however it was made.
type ruleActionMsg struct {
	server string
	rule   string
	// gone is a rule deleted, which has no card to come back to.
	gone bool
	did  string
	err  error
}

// showRules opens the list for the server the screen shows.
func (m *Model) showRules() tea.Cmd { return m.loadRules(m.configServer(), "") }

// loadRules reads the rules and, for a rule that is being opened, the grants
// it minted — by rule, since a grant stands on its own and nothing else says
// which rule it came from. Every card here starts from a read rather than
// from what the last one held: a rule can be made, or lapse, behind it.
func (m *Model) loadRules(server, open string) tea.Cmd {
	wait := statusDialog("Approval rules", "reading the project's approval rules…")
	m.dialog = wait
	return func() tea.Msg {
		msg := rulesLoadedMsg{wait: wait, book: ruleBook{server: server}, open: open}
		msg.book.rules, msg.err = m.ds.Rules(m.ctx, server)
		if msg.err == nil && open != "" {
			msg.grants, msg.err = m.ds.Grants(m.ctx, server, GrantFilter{RuleID: open})
		}
		return msg
	}
}

// rulesLoaded puts up what was asked for, unless the wait was left. A rule
// that is gone by the time it is opened lands on the list it is gone from.
func (m *Model) rulesLoaded(msg rulesLoadedMsg) tea.Cmd {
	if m.dialog != msg.wait {
		return nil
	}
	if msg.err != nil {
		m.dialog = errorDialog("Could not read the approval rules", fmt.Sprintf("%v", msg.err))
		return nil
	}
	for _, rule := range msg.book.rules {
		if msg.open != "" && rule.ID == msg.open {
			return m.showRule(msg.book, rule, msg.grants)
		}
	}
	return m.listRules(msg.book, msg.open)
}

// listRules is the project's rules, one a row: its credential, and under it
// whether it is answering, which discoboxes for, how much, and until when.
// The scope is the row's sentence and is drawn whole (dialog.wrapDetails),
// since it is what tells two rules for one credential apart. at is the rule
// the cursor returns to.
func (m *Model) listRules(book ruleBook, at string) tea.Cmd {
	const made = "one is made by answering a request with auto approve (" + autoApproveKey + " on its card), or with `discobox secret rule create`"
	if len(book.rules) == 0 {
		// Said, with the way to one: an empty menu reads as a listing that
		// did not land.
		d := textDialog("Approval rules", "")
		d.titleRight = "none"
		d.sections = []section{{lines: []line{
			{text: "this project has no approval rules: every credential request waits for a person"},
			{text: made, tone: toneDim},
		}}}
		m.dialog = d
		return nil
	}
	now := m.secrets.now()
	items := make([]action, 0, len(book.rules))
	for _, rule := range book.rules {
		credential, _ := m.ruleCredential(rule)
		items = append(items, action{key: rule.ID, label: credential, detail: ruleDetail(rule, now), enabled: true})
	}
	d := actionsDialog("Approval rules", "", items, func(ruleID string) tea.Cmd {
		return m.loadRules(book.server, ruleID)
	})
	for i, rule := range book.rules {
		if rule.ID == at {
			d.cursor = i
		}
	}
	d.titleRight = plural(len(book.rules), "rule", "rules")
	d.wrapDetails = true
	d.answerLabel = "what answers a request without asking"
	d.footer = made
	d.keys = []hint{pressing("enter reads the highlighted rule", "enter"), pressing("esc leaves them alone", "esc")}
	m.dialog = d
	return nil
}

// showRule is one rule read whole, in the sections the card that made it had,
// and under them what can be done about it: turn it off or on, delete it, or
// withdraw a grant it minted. The grants are rows the way a secret's are —
// Enter reads one, x withdraws it — because a rule that should not have
// approved something is undone a grant at a time (ADR 26-10-12-330 §4).
func (m *Model) showRule(book ruleBook, rule Rule, grants []Grant) tea.Cmd {
	now := m.secrets.now()
	credential, answered := m.ruleCredential(rule)
	again := func() tea.Cmd { return m.showRule(book, rule, grants) }
	reread := func() tea.Cmd { return m.loadRules(book.server, rule.ID) }

	toggle := action{key: ruleToggleItem, press: "s", label: "Disable this rule",
		detail: "it mints nothing more, and revokes nothing", enabled: true}
	if !rule.Enabled {
		toggle.label, toggle.detail = "Enable this rule", "it answers the requests it covers again, without asking"
	}
	items := []action{toggle, {key: ruleDeleteItem, press: "d", label: "Delete this rule…",
		detail: "the grants it minted stay, and are revoked one by one", enabled: true}}
	standing := map[string]Grant{}
	for _, g := range grants {
		// One that has lapsed authorizes nothing and has nothing to withdraw.
		if !g.Expires.IsZero() && !g.Expires.After(now) {
			continue
		}
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
		case ruleToggleItem:
			return m.setRuleEnabled(book.server, rule, !rule.Enabled)
		case ruleDeleteItem:
			return m.confirmDeleteRule(book.server, rule, len(standing), again)
		}
		return m.reviewGrant(named(standing[result]), standing[result], again)
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
	d.sections = append([]section{facts}, ruleSections(rule)...)
	if gives, ok := ruleHandsOut(rule); ok {
		d.sections = append(d.sections, gives)
	}

	d.answerLabel = "no grant it minted still stands"
	if len(standing) > 0 {
		d.answerLabel = plural(len(standing), "grant it minted still stands", "grants it minted still stand")
	}
	d.footer = "its scope and uses are changed with `discobox secret rule update " + rule.ID + "`, where the server judges them"
	d.keys = []hint{pressing("enter runs it, or reads a grant", "enter"), keyed(grantRevokeKey, grantRevokeKey, "revokes a grant"), pressing("esc goes back to the rules", "esc")}
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
	d.onCancel = func() tea.Cmd { return m.listRules(book, rule.ID) }
	m.dialog = d
	return nil
}

// setRuleEnabled turns a rule off or on. It asks nothing first: either is
// undone by the other from the same card, and neither touches a grant.
func (m *Model) setRuleEnabled(server string, rule Rule, enabled bool) tea.Cmd {
	doing, did := "disabling", "disabled"
	if enabled {
		doing, did = "enabling", "enabled"
	}
	m.dialog = statusDialog("Approval rules", doing+" "+rule.ID+"…")
	return func() tea.Msg {
		err := m.ds.SetRuleEnabled(m.ctx, server, rule.ID, enabled)
		return ruleActionMsg{server: server, rule: rule.ID, did: did + " " + rule.ID, err: err}
	}
}

// confirmDeleteRule asks before removing a rule, and says what does not go
// with it: the grants it minted are independent once made, so deleting the
// rule is not how what it approved is taken back.
func (m *Model) confirmDeleteRule(server string, rule Rule, standing int, back func() tea.Cmd) tea.Cmd {
	credential, _ := m.ruleCredential(rule)
	d := confirmDialog("Delete approval rule", "", func(string) tea.Cmd {
		m.dialog = statusDialog("Approval rules", "deleting "+rule.ID+"…")
		return func() tea.Msg {
			err := m.ds.DeleteRule(m.ctx, server, rule.ID)
			return ruleActionMsg{server: server, rule: rule.ID, gone: true, did: "deleted " + rule.ID, err: err}
		}
	})
	d.titleRight = rule.ID
	stays := line{text: "it has no grants standing", tone: toneDim}
	if standing > 0 {
		stays = line{text: plural(standing, "grant it minted stays", "grants it minted stay") +
			": each is revoked on its own, from this rule's card before it goes or from its secret after", tone: toneAlert}
	}
	goes := section{label: "what goes", fields: []field{
		{label: "credential", value: credential, tone: toneAccent},
		{label: "for", value: ruleScopeText(rule)},
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

// ruleActed re-reads after a change and comes back to where it was made: the
// rule's card, or the list for one that is gone. A refusal is shown, and
// leaving it goes back to the rule as the server now has it.
func (m *Model) ruleActed(msg ruleActionMsg) tea.Cmd {
	if msg.err != nil {
		d := errorDialog("Approval rules", fmt.Sprintf("%v", msg.err))
		d.onCancel = func() tea.Cmd { return m.loadRules(msg.server, msg.rule) }
		m.dialog = d
		return nil
	}
	open := msg.rule
	if msg.gone {
		open = ""
	}
	return tea.Batch(m.loadRules(msg.server, open), m.report(false, "%s", msg.did))
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

// ruleDetail is the rest of a rule's row: whether it is answering, which
// discoboxes for, how much it approves, and until when.
func ruleDetail(rule Rule, now time.Time) string {
	parts := []string{ruleState(rule, now)}
	if rule.Delegate {
		parts = append(parts, "delegates")
	}
	parts = append(parts, ruleScopeText(rule), plural(len(rule.Uses), "use", "uses"))
	if !rule.Expires.IsZero() && rule.Expires.After(now) {
		parts = append(parts, "expires "+ruleExpiry(rule, now))
	}
	return strings.Join(parts, " · ")
}

// ruleScopeText is a rule's scope on one line, for a row and for the question
// before it is deleted: ruleScopeSection's lines, joined.
func ruleScopeText(rule Rule) string {
	lines := ruleScopeSection(rule.ApprovalRule).lines
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
