package tui

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/sourceref"
	"github.com/discobox-ai/discobox/wellknown"

	tea "charm.land/bubbletea/v2"
)

// Making and editing an approval rule from the secrets screen (ADR
// 26-10-12-330 §1): the same rule a person makes with `discobox secret rule
// create`, asked a card at a time on the steps auto approve has (rules.go) —
// which discoboxes, the uses a line at a time, a lifetime — and read whole
// before anything is written, with No the default.
//
// The steps are a sequence a draft walks: Esc on one is the one before it,
// opened on what the draft holds now, so going back and forth loses nothing
// chosen. A refusal from the server comes back on a card whose Esc is the
// step the refusal is about.

// The keys that make a rule, and edit the one under the cursor.
const (
	ruleNewKey  = "n"
	ruleEditKey = "e"
)

// ruleEditItem is the rule card's row that edits it.
const ruleEditItem = "rule:edit"

// ruleStep is one card of making or editing a rule, in the order they come.
type ruleStep int

const (
	stepCredential ruleStep = iota
	stepDiscoboxes
	stepUses
	stepGrantCap
	stepExpiry
	stepRead
)

// ruleDraft is a rule being made or edited, as far as it has been chosen.
type ruleDraft struct {
	server string
	// was is the rule as the server holds it, for an edit; nil makes a new
	// one. The credential is what a rule is, so an edit never asks it.
	was *Rule
	// rule is what it will say, as it is drawn. A new rule's hosts are sent
	// as they are drawn, so the rule stored is the one read.
	rule Rule
	// repositories and uses are those two lists as they are being written:
	// what each line sends beside what it draws, and where the field was
	// left, half-typed text included, so going back loses nothing.
	repositories, uses lineList
	// fromCard is an edit begun on the rule's card, which leaving the first
	// step and saving both go back to.
	fromCard bool
	// from is the grant a new rule is being made of, nil for any other.
	from *ruleFromGrant
}

// ruleFromGrant is a new rule made of a grant (makeRuleFromGrant): what the
// server proposed from it, and the grant's card, which leaving the first step
// goes back to. The rule's credential, hosts and purpose are the grant's, so
// its credential is not asked.
type ruleFromGrant struct {
	grantID, secretID string
	proposed          RuleProposals
	card              func() tea.Cmd
}

// fromGrantSection says what a rule made of a grant is made of, and — when
// the request asked for a well-known ID and the grant stands on a secret
// other than the one marked for it — that the rule answers with the marked
// secret, not the grant's: a rule for an ID is answered by its mark.
func (m *Model) fromGrantSection(d ruleDraft) []section {
	if d.from == nil {
		return nil
	}
	sec := section{label: "made of a grant", fields: []field{{label: "grant", value: d.from.grantID + ", which stays as it is"}}}
	if d.rule.WellKnownID != "" && d.from.secretID != d.rule.SecretID {
		sec.lines = append(sec.lines, line{
			text: "the request it answered asked for " + d.rule.WellKnownID + ": the rule is for " + d.rule.WellKnownID +
				", and answers with the marked secret " + m.secretName(d.rule.SecretID) + ", not " + m.secretName(d.from.secretID),
			tone: toneAlert,
		})
	}
	return []section{sec}
}

func (d ruleDraft) first() ruleStep {
	if d.was != nil || d.from != nil {
		return stepDiscoboxes
	}
	return stepCredential
}

func (d ruleDraft) title(step string) string {
	if d.was != nil {
		return "Edit rule: " + step
	}
	return "New rule: " + step
}

// newRule starts a rule on the screen's server.
func (m *Model) newRule() tea.Cmd {
	return m.ruleStep(ruleDraft{server: m.configServer(), rule: Rule{MaxGrantTTL: time.Hour}}, stepCredential)
}

// grantRuleProposedMsg carries what the server proposed from a grant back.
// wait is the dialog that was up while it was asked, which is how an answer
// nobody is waiting for any more is told from one somebody is.
type grantRuleProposedMsg struct {
	wait      *dialog
	server    string
	grant     Grant
	card      func() tea.Cmd
	proposals RuleProposals
	err       error
}

// proposeGrantRule asks the server for the rule auto approve would have
// proposed from the request a grant answered, written from the uses the
// grant approved, and waits where it can be seen: a model writes the uses
// and the judge reads them. Esc stops the asking and goes back to the grant.
func (m *Model) proposeGrantRule(server string, grant Grant, card func() tea.Cmd) tea.Cmd {
	ctx, stop := context.WithCancel(m.ctx)
	wait := statusDialog("New rule", "asking the judge to write a rule from this grant…")
	wait.footer = "a model writes its uses from the grant's, and the judge reads them against the request it answered; it can take a minute or two"
	wait.onCancel = func() tea.Cmd {
		stop()
		return card()
	}
	m.dialog = wait
	return func() tea.Msg {
		defer stop()
		proposals, err := m.ds.GrantRuleProposals(ctx, server, grant.ID)
		return grantRuleProposedMsg{wait: wait, server: server, grant: grant, card: card, proposals: proposals, err: err}
	}
}

// grantRuleProposed starts the new rule from what was proposed, unless the
// wait was left: the rule's credential, hosts and purpose are the grant's;
// its repository the asking discobox's, ticked; its uses the judge's when it
// found they cover the request the grant answered, and the grant's own when
// not; and each grant it mints lives as long as this one was given. Every
// step is the new rule's, and the last reads the rule whole.
func (m *Model) grantRuleProposed(msg grantRuleProposedMsg) tea.Cmd {
	if m.dialog != msg.wait {
		return nil
	}
	if msg.err != nil {
		e := errorDialog("Could not propose a rule", fmt.Sprintf("%v", msg.err))
		e.sections = []section{{lines: []line{{text: "no rule was made · esc goes back to the grant", tone: toneDim}}}}
		e.onCancel = msg.card
		m.dialog = e
		return nil
	}
	grant, proposed := msg.grant, msg.proposals
	// The rule is for the credential the request asked for, because that is
	// the rule the server judged: for its well-known ID when it named one,
	// answered with the secret marked for it — which need not be the secret
	// this grant stands on — and otherwise for this grant's secret.
	rule := Rule{SecretID: grant.SecretID, Hosts: proposed.Hosts, Delegate: proposed.Delegate || grant.Delegate}
	if id := proposed.WellKnownID; id != "" {
		marked := markedFor(m.secrets.all, id)
		if marked == nil {
			e := errorDialog("Could not make a rule from this grant",
				"the request it answered asked for "+id+", and a rule for "+id+" answers with the secret marked for it: this project has none marked")
			e.sections = []section{{lines: []line{{text: "a secret is marked for " + id + " by storing it as that credential (n on the secrets screen, its kind " + id + "), or by approving a request for " + id + " by hand while none is marked · esc goes back to the grant", tone: toneDim}}}}
			e.onCancel = msg.card
			m.dialog = e
			return nil
		}
		rule.WellKnownID, rule.SecretID = id, marked.ID
	}
	if !grant.Granted.IsZero() && grant.Expires.After(grant.Granted) {
		rule.MaxGrantTTL = grant.Expires.Sub(grant.Granted).Round(time.Minute)
	}
	if proposed.Repository != "" {
		rule.Repositories = []string{proposed.Repository}
	}
	asked := make([]string, 0, len(grant.Uses))
	for _, use := range grant.Uses {
		asked = append(asked, use.Description)
	}
	uses := newLineList(typedLines(asked))
	if written := proposed.Written; written != nil && written.Covers {
		uses = newLineList(storedLines(written.Uses, written.Stored))
	}
	d := ruleDraft{server: msg.server, rule: rule, uses: uses,
		from: &ruleFromGrant{grantID: grant.ID, secretID: grant.SecretID, proposed: proposed, card: msg.card}}
	return m.ruleStep(d, stepDiscoboxes)
}

// editRule starts an edit of one rule, from its row or its card.
func (m *Model) editRule(server string, rule Rule, fromCard bool) tea.Cmd {
	was := rule
	d := ruleDraft{server: server, was: &was, rule: rule, fromCard: fromCard,
		uses: newLineList(storedLines(rule.Uses, rule.Stored.Uses))}
	return m.ruleStep(d, stepDiscoboxes)
}

// ruleStep opens one step on what the draft holds now.
func (m *Model) ruleStep(d ruleDraft, step ruleStep) tea.Cmd {
	var back func() tea.Cmd
	switch {
	case step > d.first():
		back = func() tea.Cmd { return m.ruleStep(d, step-1) }
	case d.fromCard:
		back = func() tea.Cmd { return m.openRule(d.server, d.was.ID) }
	case d.from != nil:
		back = d.from.card
	}
	// A list step hands back the list as it was left, which the draft keeps
	// on the way back.
	backWith := func(keep func(*ruleDraft)) func(lineList) tea.Cmd {
		return func(lineList) tea.Cmd {
			kept := d
			keep(&kept)
			switch {
			case step > d.first():
				return m.ruleStep(kept, step-1)
			case d.fromCard:
				return m.openRule(d.server, d.was.ID)
			case d.from != nil:
				return d.from.card()
			}
			return nil
		}
	}
	switch step {
	case stepCredential:
		return m.askNewRuleCredential(d, back)
	case stepDiscoboxes:
		return m.askDiscoboxes(discoboxesAsk{
			title:    d.title("which discoboxes"),
			sections: m.draftSections(d),
			typed:    true,
			named:    m.repositoryList(d),
			was:      d.rule.ApprovalRule,
		}, func(chosen ApprovalRule, named lineList) tea.Cmd {
			next := d
			next.rule.Repositories, next.rule.EveryDiscobox = chosen.Repositories, chosen.EveryDiscobox
			next.repositories = named
			return m.ruleStep(next, stepUses)
		}, func(named lineList) tea.Cmd {
			return backWith(func(kept *ruleDraft) { kept.repositories = named })(named)
		})
	case stepUses:
		var judged func(lineList) []section
		if d.from != nil {
			judged = func(now lineList) []section { return usesSections(d.from.proposed, grantOrigin, now) }
		}
		return m.askLines(linesAsk{
			title:       d.title("what it approves"),
			sections:    m.draftSections(d),
			answerLabel: "a use, one sentence",
			placeholder: "e.g. Comment on any issue in org/repo",
			written:     "what it approves",
			footer:      "a request is approved only when every use it asks for is within these",
			numbered:    true,
			required:    "a rule says what it approves: write at least one use",
			sectionsFor: judged,
		}, d.uses, "", func(uses lineList) tea.Cmd {
			next := d
			next.uses, next.rule.Uses = uses, uses.shown()
			return m.ruleStep(next, stepGrantCap)
		}, func(uses lineList) tea.Cmd {
			return backWith(func(kept *ruleDraft) { kept.uses = uses })(uses)
		})
	case stepGrantCap:
		current := d.rule.MaxGrantTTL
		return m.askDuration(durationAsk{
			title:        d.title("longest grant"),
			sections:     m.draftSections(d),
			answerLabel:  "how long may each grant it mints live, at most?",
			footer:       "what a request asks for is fitted under it, and the secret's own limit holds either way",
			customFooter: "1h, 90m, 3d, 2w, 1mo — or forever, for the secret's own limit",
			choices:      lifetimeChoices(current),
			opensOn:      current,
			name:         ruleCapName,
		}, func(chosen time.Duration, _ bool, _ func() tea.Cmd) tea.Cmd {
			next := d
			next.rule.MaxGrantTTL = max(chosen, 0)
			return m.ruleStep(next, stepExpiry)
		}, back)
	case stepExpiry:
		now := m.secrets.now()
		ask := durationAsk{
			title:        d.title("when it lapses"),
			sections:     m.draftSections(d),
			answerLabel:  "how long does the rule itself approve for?",
			footer:       "when it lapses it approves nothing more; the grants it minted are independent of it",
			customFooter: "1h, 90m, 3d, 2w, 1mo — or forever, for a rule that does not lapse",
			choices:      lifetimeChoices(0),
			name:         ruleLapseName,
		}
		if d.was != nil && !d.was.Expires.IsZero() {
			ask.keep = "lapses " + ruleExpiry(*d.was, now)
		}
		return m.askDuration(ask, func(chosen time.Duration, kept bool, _ func() tea.Cmd) tea.Cmd {
			next := d
			switch {
			case kept:
				next.rule.Expires = d.was.Expires
			case chosen <= 0:
				next.rule.Expires = time.Time{}
			default:
				next.rule.Expires = m.secrets.now().Add(chosen)
			}
			return m.ruleStep(next, stepRead)
		}, back)
	}
	return m.readRuleDraft(d, back)
}

// repositoryList is the repositories a rule may name, as the step opens on
// them: as they were left, or picks of every repository the console knows
// for the project — the rule's own first, chosen, then those of the
// project's discoboxes and of its other rules, each said once — with a line
// written for each of the rule's that is none of those.
func (m *Model) repositoryList(d ruleDraft) lineList {
	if d.repositories.held {
		return d.repositories
	}
	var own []listLine
	switch {
	case d.was != nil && !d.was.EveryDiscobox:
		own = storedLines(d.was.Repositories, d.was.Stored.Repositories)
	case d.from != nil && d.from.proposed.Repository != "":
		// The repository of the discobox that asked for the grant.
		own = []listLine{{shown: d.from.proposed.Repository, value: d.from.proposed.RepositoryStored}}
	}
	return m.knownRepositories(d.server, own)
}

// knownRepositories is every repository the console knows for a server's
// project as picks — own first, chosen, then those of the project's
// discoboxes and its rules, each once, sorted — above an empty list of
// repositories to name.
func (m *Model) knownRepositories(server string, own []listLine) lineList {
	seen := map[string]bool{}
	picks := slices.Clone(own)
	for _, pick := range own {
		if id := repositoryIdentity(pick.value); id != "" {
			seen[id] = true
		}
		seen[pick.value] = true
	}
	var others []listLine
	offer := func(line listLine) {
		// A value that does not reduce — one with a character a URL cannot
		// hold — is its own identity: it is offered as the discobox has it.
		id := repositoryIdentity(line.value)
		if id == "" {
			id = line.value
		}
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		others = append(others, line)
	}
	for _, box := range m.list.all {
		if m.serverName(box.Server) == m.serverName(server) && box.RepositoryIdentity != "" {
			offer(listLine{shown: box.RepositoryShown, value: box.RepositoryIdentity})
		}
	}
	if m.serverName(server) == m.configServer() {
		for _, rule := range m.rules.all {
			for _, line := range storedLines(rule.Repositories, rule.Stored.Repositories) {
				offer(line)
			}
		}
	}
	slices.SortFunc(others, func(a, b listLine) int { return strings.Compare(a.shown, b.shown) })
	chosen := make([]string, 0, len(own))
	for _, pick := range own {
		chosen = append(chosen, pick.value)
	}
	return newPickList(append(picks, others...), chosen, nil, repositoryIdentity)
}

// repositoryIdentity is a repository as the server compares it, host/path,
// from a URL, git's scp-like form, or host/path itself, so an entry typed is
// known to be one of the picks however it was spelled. It is the server's own
// reduction (sourceref.NormalizeRemote); a typed entry it refuses matches
// nothing here and is the server's to refuse.
func repositoryIdentity(entry string) string {
	if id := sourceref.NormalizeRemote(entry); id != "" {
		return id
	}
	return sourceref.NormalizeRemote("https://" + strings.TrimSpace(entry))
}

// ruleCapName is a row of the longest-grant step: forever is not forever
// there, but the secret's own limit.
func ruleCapName(d time.Duration) string {
	if d <= 0 {
		return "the secret's own limit"
	}
	return lifetime.Label(d)
}

// ruleLapseName is a row of the expiry step, where forever is never lapsing.
func ruleLapseName(d time.Duration) string {
	if d <= 0 {
		return "never lapses"
	}
	return lifetime.Label(d)
}

// askNewRuleCredential is the first step of a new rule: the one credential
// it answers with. A well-known ID is offered where a project secret answers
// it, and every project secret on its own; a rule for a secret recognizes an
// ask naming no well-known credential by the host the secret is bound to.
func (m *Model) askNewRuleCredential(d ruleDraft, back func() tea.Cmd) tea.Cmd {
	var items []action
	for _, secret := range m.secrets.all {
		if secret.WellKnownID != "" {
			items = append(items, action{key: "wk:" + secret.ID, label: secret.WellKnownID,
				detail: "asked for by its ID, and answered with " + secret.Name, enabled: true})
		}
	}
	for _, secret := range m.secrets.all {
		detail := "an ask naming no well-known credential, for a host " + secret.Host + " covers"
		if secret.Host == "" {
			detail = "bound to no host, so it recognizes no ask: bind it first (e on the secret)"
		}
		items = append(items, action{key: "secret:" + secret.ID, label: secret.Name, detail: detail, enabled: secret.Host != "",
			why: "bound to no host, so it recognizes no ask: bind it first (e on the secret)"})
	}
	if len(items) == 0 {
		e := errorDialog(d.title("which credential"), "a rule answers with a credential the project holds, and it holds none yet: n on the secrets stores one")
		e.onCancel = back
		m.dialog = e
		return nil
	}
	dlg := actionsDialog(d.title("which credential"), "", items, func(result string) tea.Cmd {
		kind, id, _ := strings.Cut(result, ":")
		next := d
		for _, secret := range m.secrets.all {
			if secret.ID != id {
				continue
			}
			next.rule.SecretID = secret.ID
			next.rule.WellKnownID, next.rule.Hosts = "", nil
			if secret.Host != "" {
				next.rule.Hosts = []string{secret.Host}
			}
			if kind == "wk" {
				next.rule.WellKnownID = secret.WellKnownID
				if known, ok := wellknown.Lookup(secret.WellKnownID); ok {
					next.rule.Hosts = known.Hosts
				}
			}
			return m.ruleStep(next, stepDiscoboxes)
		}
		return nil
	})
	chosen := "secret:" + d.rule.SecretID
	if d.rule.WellKnownID != "" {
		chosen = "wk:" + d.rule.SecretID
	}
	for i, item := range items {
		if item.key == chosen && item.enabled {
			dlg.cursor = i
		}
	}
	dlg.wrapDetails = true
	dlg.answerLabel = "which credential does the rule answer with?"
	dlg.footer = "a grant it mints names this credential, and may be sent to its hosts"
	dlg.keys = []hint{pressing("enter chooses the highlighted one", "enter"), pressing("esc goes back", "esc")}
	dlg.onCancel = back
	m.dialog = dlg
	return nil
}

// ruleFor is which discoboxes a rule is for, in a field: its repositories, or
// every discobox. What every discobox means is said on the rule's own section.
func ruleFor(rule Rule) string {
	if rule.EveryDiscobox {
		return everyDiscoboxName
	}
	return strings.Join(rule.Repositories, ", ")
}

// draftSections is what has been chosen so far, above each step's question,
// so every card says what it is a step of.
func (m *Model) draftSections(d ruleDraft) []section {
	sec := section{label: "the rule so far"}
	if d.rule.WellKnownID != "" || d.rule.SecretID != "" {
		credential, answered := m.ruleCredential(d.rule)
		sec.fields = append(sec.fields, field{label: "credential", value: credential, tone: toneAccent})
		if answered != "" {
			sec.fields = append(sec.fields, field{label: "answered with", value: answered})
		}
	}
	if d.rule.EveryDiscobox || len(d.rule.Repositories) > 0 {
		sec.fields = append(sec.fields, field{label: "for", value: ruleFor(d.rule)})
	}
	if len(d.rule.Uses) > 0 {
		sec.fields = append(sec.fields, field{label: "approves", value: plural(len(d.rule.Uses), "use", "uses")})
	}
	out := m.fromGrantSection(d)
	if len(sec.fields) > 0 {
		out = append(out, sec)
	}
	return out
}

// readRuleDraft is the last card before the write: the rule's whole text, as
// a stored rule's card draws it, and for an edit what changes, before and
// after. It asks rather than offers, and No is the default: the costly
// answer is yes, which approves requests nobody will be shown.
func (m *Model) readRuleDraft(d ruleDraft, back func() tea.Cmd) tea.Cmd {
	now := m.secrets.now()
	credential, answered := m.ruleCredential(d.rule)
	facts := ruleFactsSection(d.rule, credential, answered, ruleCap(d.rule))
	if len(d.rule.Hosts) == 0 {
		facts.fields = slices.DeleteFunc(facts.fields, func(f field) bool { return f.label == "may be sent to" })
	}
	facts.fields = append(facts.fields, field{label: "expires", value: ruleExpiry(d.rule, now)})

	var sections []section
	change := d.change()
	if d.was != nil {
		sections = append(sections, m.ruleChangeSections(*d.was, d.rule, change, now)...)
	}
	sections = append(sections, ruleText(d.rule, facts)...)
	sections = append(sections, m.fromGrantSection(d)...)

	dlg := confirmDialog(d.title("the rule"), "", func(string) tea.Cmd {
		if d.was != nil && change.Empty() {
			m.dialog = nil
			return m.report(false, "nothing changed on %s", d.was.ID)
		}
		return m.writeRuleDraft(d)
	})
	if d.was != nil {
		dlg.titleRight = d.was.ID
	}
	dlg.sections = sections
	dlg.answerLabel = "make this rule?"
	dlg.footer = "a request it covers is approved without asking; one it does not still waits for you · no goes back"
	if d.was != nil {
		dlg.answerLabel = "change this rule?"
		dlg.footer = "it is tested afresh against what is pending; the grants it already minted stay as they are · no goes back"
	}
	dlg.defaultNo = true
	dlg.onCancel = back
	m.dialog = dlg
	return nil
}

// change is what an edit sends: each part of the rule that differs from the
// rule as it was read, and nothing else.
func (d ruleDraft) change() RuleChange {
	if d.was == nil {
		return RuleChange{}
	}
	was, now := *d.was, d.rule
	// Compared, and sent, in the server's own text: a line nobody changed
	// is the server's line, whatever drawing it took.
	var c RuleChange
	repositories := d.repositories.values()
	if was.EveryDiscobox != now.EveryDiscobox || (!now.EveryDiscobox && !slices.Equal(newLineList(storedLines(was.Repositories, was.Stored.Repositories)).values(), repositories)) {
		if now.EveryDiscobox {
			c.EveryDiscobox = true
		} else {
			c.Repositories = repositories
		}
	}
	if uses := d.uses.values(); !slices.Equal(newLineList(storedLines(was.Uses, was.Stored.Uses)).values(), uses) {
		c.Uses = uses
	}
	if was.MaxGrantTTL != now.MaxGrantTTL {
		capped := now.MaxGrantTTL
		c.MaxGrantTTL = &capped
	}
	if !was.Expires.Equal(now.Expires) {
		if now.Expires.IsZero() {
			c.NeverExpires = true
		} else {
			lapses := now.Expires
			c.Expires = &lapses
		}
	}
	return c
}

// ruleChangeSections say what an edit changes, before → after, or that it
// changes nothing. The uses it approved before are listed whole: the rule's
// text below is what it will say, and what it stops saying is as much the
// edit as what it starts to.
func (m *Model) ruleChangeSections(was, now Rule, c RuleChange, at time.Time) []section {
	sec := section{label: "what changes"}
	if c.Empty() {
		sec.lines = []line{{text: "nothing: every part is as it was", tone: toneDim}}
		return []section{sec}
	}
	if c.EveryDiscobox || c.Repositories != nil {
		sec.fields = append(sec.fields, field{label: "for", value: ruleFor(was) + " → " + ruleFor(now), tone: toneAccent})
	}
	if c.Uses != nil {
		sec.fields = append(sec.fields, field{label: "approves", value: plural(len(was.Uses), "use", "uses") + " → " + plural(len(now.Uses), "use", "uses") + ", read whole below", tone: toneAccent})
	}
	if c.MaxGrantTTL != nil {
		sec.fields = append(sec.fields, field{label: "each grant lasts", value: ruleCap(was) + " → " + ruleCap(now), tone: toneAccent})
	}
	if c.Expires != nil || c.NeverExpires {
		sec.fields = append(sec.fields, field{label: "expires", value: ruleExpiry(was, at) + " → " + ruleExpiry(now, at), tone: toneAccent})
	}
	out := []section{sec}
	if c.Uses != nil {
		before := section{label: "what it approved before"}
		for _, use := range was.Uses {
			before.lines = append(before.lines, useLines(use, toneDim)...)
		}
		out = append(out, before)
	}
	return out
}

// ruleWrittenMsg carries a rule made or changed back.
type ruleWrittenMsg struct {
	draft ruleDraft
	made  string
	err   error
}

// writeRuleDraft sends the rule: a new one whole, an edit only what changed.
func (m *Model) writeRuleDraft(d ruleDraft) tea.Cmd {
	doing := "making the rule…"
	if d.was != nil {
		doing = "changing " + d.was.ID + "…"
	}
	m.dialog = statusDialog("Approval rules", doing)
	return func() tea.Msg {
		if d.was != nil {
			err := m.ds.UpdateRule(m.ctx, d.server, d.was.ID, d.change())
			return ruleWrittenMsg{draft: d, made: d.was.ID, err: err}
		}
		// A rule for a well-known ID names only the ID: the server answers it
		// with the secret marked for it, and the secret here was for reading.
		secretID := d.rule.SecretID
		if d.rule.WellKnownID != "" {
			secretID = ""
		}
		made := NewRule{
			WellKnownID: d.rule.WellKnownID, SecretID: secretID, Hosts: d.rule.Hosts, Delegate: d.rule.Delegate,
			ApprovalRule: ApprovalRule{EveryDiscobox: d.rule.EveryDiscobox, Uses: d.uses.values()},
			MaxGrantTTL:  d.rule.MaxGrantTTL, Expires: d.rule.Expires,
		}
		if !d.rule.EveryDiscobox {
			made.Repositories = d.repositories.values()
		}
		rule, err := m.ds.CreateRule(m.ctx, d.server, made)
		return ruleWrittenMsg{draft: d, made: rule.ID, err: err}
	}
}

// ruleWritten re-reads the screen after a rule was made or changed, and for
// an edit begun on the rule's card goes back to it. A refusal is shown with
// what the server said, and Esc there is the step it is about, opened on what
// was chosen, so it can be changed rather than chosen again from the start.
func (m *Model) ruleWritten(msg ruleWrittenMsg) tea.Cmd {
	d := msg.draft
	if msg.err != nil {
		title, nothing := "Could not make the rule", "no rule was made"
		if d.was != nil {
			title, nothing = "Could not change the rule", d.was.ID+" was not changed"
		}
		step := refusedStep(msg.err, d.first())
		e := errorDialog(title, fmt.Sprintf("%v", msg.err))
		e.sections = []section{{lines: []line{
			{text: nothing + " · esc goes back to " + ruleStepName(step) + ", to change it", tone: toneDim},
		}}}
		e.onCancel = func() tea.Cmd { return m.ruleStep(d, step) }
		m.dialog = e
		return nil
	}
	m.dialog = nil
	did := "made " + msg.made
	if d.was != nil {
		did = "changed " + msg.made
	}
	cmds := []tea.Cmd{m.loadSecrets(), m.report(false, "%s", did)}
	if d.fromCard {
		cmds = append(cmds, m.openRule(d.server, msg.made))
	}
	return tea.Batch(cmds...)
}

// refusalUses finds a server's refusal about a rule's uses.
var refusalUses = regexp.MustCompile(`\buses?\b`)

// refusedStep is the step a server's refusal is about, read from the words
// the server refuses a rule in: which discoboxes, the uses, the cap, the
// expiry, the credential. Anything else is about the rule as a whole, and
// goes back to the card that read it.
func refusedStep(err error, first ruleStep) ruleStep {
	said := strings.ToLower(err.Error())
	step := stepRead
	switch {
	case strings.Contains(said, "repositor") || strings.Contains(said, "names no host") || strings.Contains(said, "every discobox"):
		step = stepDiscoboxes
	case refusalUses.MatchString(said):
		step = stepUses
	case strings.Contains(said, "lifetime cap"):
		step = stepGrantCap
	case strings.Contains(said, "expir"):
		step = stepExpiry
	case strings.Contains(said, "credential") || strings.Contains(said, "secret") || strings.Contains(said, "host"):
		step = stepCredential
	}
	return max(step, first)
}

// ruleStepName is a step as the way back to it is said.
func ruleStepName(step ruleStep) string {
	switch step {
	case stepCredential:
		return "the credential"
	case stepDiscoboxes:
		return "which discoboxes"
	case stepUses:
		return "what it approves"
	case stepGrantCap:
		return "the longest grant"
	case stepExpiry:
		return "when it lapses"
	}
	return "the rule"
}
