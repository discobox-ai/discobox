package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/wellknown"

	tea "charm.land/bubbletea/v2"
)

// "Auto approve": answering a request by making the approval rule that
// answers it, and requests like it from now on, without asking
// (ADR 26-10-12-330 §§7–9).
//
// A rule replaces the person who reads a card, so its policy is the
// authorization — and most of it is written by a model from the agent's own
// ask. What keeps a generated statement from becoming authority nobody read
// is the shape of this flow: the policy is shown whole and edited a line at a
// time, a proposal not found to cover this very request is not what the list
// opens on, and the last card before the write is the rule in full. Which
// discoboxes the rule is for is no sentence at all: the repository of the
// discobox that asked, which the server compares, or every discobox.
//
// The steps run: request → what answers it, and its binding question, as for
// any approval → the proposals are fetched → which discoboxes → its policy →
// how long → the limit question, if the lifetime raises one → the rule, read
// whole → approve. Each goes back to the one before it, like every other step
// of an approval, and the answers travel in the same approval value.

// autoApproveKey starts it, on the request card and on a gate's. Capitalized
// like the key that opens a request, and for its reason: what it leads to
// answers requests nobody will be shown, and on a gate's card it is one shift
// away from the Approve that answers only this one.
const autoApproveKey = "A"

// ruleProposalsMsg carries the proposals back. wait is the dialog that was up
// while they were fetched, which is how an answer nobody is waiting for any
// more is told from one somebody is.
type ruleProposalsMsg struct {
	wait      *dialog
	a         approval
	back      func() tea.Cmd
	proposals RuleProposals
	err       error
}

// askRuleSecret asks which project secret the rule answers with: the request
// card's own question, asked again without the answers a rule cannot take. A
// credential typed in on the spot is stored after the lifetime is chosen, and
// a rule is read whole before anything is written.
func (m *Model) askRuleSecret(a approval) tea.Cmd {
	newRow := action{key: "new", press: "n", label: "New credential…",
		detail: "store it as a project secret now, then make the rule with it", enabled: true}
	if marked := markedFor(a.secrets, a.req.WellKnownID); marked != nil {
		// A rule for the ID answers with the secret marked for it, and the
		// server marks no second one: a new credential could never be the
		// rule's.
		newRow.enabled = false
		newRow.why = "a rule for " + a.req.WellKnownID + " answers with " + marked.Name +
			", the secret marked for it: to change what it answers with, edit " + marked.Name + " on the secrets screen (F4, e)"
	}
	items := append(secretRows(a.secrets, a.req), newRow)
	d := actionsDialog("Auto approve", "", items, func(result string) tea.Cmd {
		if result == "new" {
			return m.askRuleNewCredential(a)
		}
		return m.chooseSecret(a, strings.TrimPrefix(result, "secret:"))
	})
	d.sections = credentialAsk(a.req, m.secrets.now())
	d.answerLabel = "which secret answers this, and requests like it?"
	d.footer = "next the server proposes rules, narrowest first; nothing is approved until you have read one"
	d.keys = []hint{pressing("enter chooses the highlighted secret", "enter"), pressing("esc goes back", "esc")}
	d.onCancel = m.toRequest(a)
	m.dialog = d
	if a.stored != "" {
		// Back here, the rule that secret was stored for was not made.
		return m.report(false, "%s", storedNote(a.stored, a.req.WellKnownID))
	}
	return nil
}

// storedNote says a secret stored on the way to a rule stays, what it now
// answers, and how to remove it: the flow was left without the rule.
func storedNote(name, wellKnownID string) string {
	what := "it stays as a project secret"
	if wellKnownID != "" {
		what = "it stays, answering every request for " + wellKnownID
	}
	return "stored " + name + " with no rule made: " + what + " · d on the secrets table (F4) removes it"
}

// askRuleNewCredential stores a new credential to make the rule with: the
// request card's own new-credential form, filled from the request. It is
// stored when the form is saved, not when the rule is: the steps that follow
// — the binding question, the grant limit, and the server holding the rule to
// the secret marked for a well-known ID, which storing one from its kind
// marks — are then about the secret that will answer, rather than one that
// does not exist yet. The form says so. Esc is the step before, with nothing
// stored.
func (m *Model) askRuleNewCredential(a approval) tea.Cmd {
	answers := "the rule, and stays a project secret even if you decline the rule"
	if a.req.WellKnownID != "" {
		answers = "every request for " + a.req.WellKnownID + " from then on, by a rule or by you — even if you decline the rule"
	}
	return m.newCredentialForm(a, section{label: "the rule will answer with it", fields: []field{
		{label: "credential", value: credentialName(a.req), tone: toneAccent},
		{label: "stored", value: "when this form is saved — before the rule, which you read before it is made", tone: toneAlert},
		{label: "it answers", value: answers, tone: toneAlert},
	}}, "enter stores and goes on", func(_ *form, secret NewSecret) tea.Cmd {
		m.dialog = statusDialog("Auto approve", "storing "+secret.Name+"…")
		return func() tea.Msg {
			stored, err := m.ds.CreateSecret(m.ctx, a.req.Server, secret)
			return ruleSecretStoredMsg{a: a, secret: stored, asked: secret, err: err}
		}
	}, func() tea.Cmd { return m.askRuleSecret(a) })
}

// ruleSecretStoredMsg carries a secret stored to make a rule with back.
type ruleSecretStoredMsg struct {
	a      approval
	secret Secret
	asked  NewSecret
	err    error
}

// ruleSecretStored goes on with the secret just stored, chosen as though it
// had been on the list, or says why it was not stored and goes back to the
// list.
func (m *Model) ruleSecretStored(msg ruleSecretStoredMsg) tea.Cmd {
	if msg.err != nil {
		e := errorDialog("Could not store the credential", fmt.Sprintf("%v", msg.err))
		e.sections = []section{{lines: []line{{text: "nothing was stored · esc goes back to the secrets, where the request still waits", tone: toneDim}}}}
		e.onCancel = func() tea.Cmd { return m.askRuleSecret(msg.a) }
		m.dialog = e
		return nil
	}
	stored := msg.secret
	if stored.WellKnownID == "" {
		stored.WellKnownID = msg.asked.WellKnownID
	}
	if stored.MaxTTL == 0 {
		stored.MaxTTL = time.Duration(msg.asked.MaxTTLSeconds) * time.Second
	}
	next := msg.a
	next.secrets = append(slices.Clone(next.secrets), stored)
	next.stored = stored.Name
	return tea.Batch(m.loadSecrets(), m.chooseSecret(next, stored.ID))
}

// proposeRules asks the server what rule could answer the request, and waits
// where it can be seen: a model writes its policy, which is then held to
// covering this request, and that takes a minute or two. Esc stops the asking
// as well as the watching, and goes back.
func (m *Model) proposeRules(a approval, back func() tea.Cmd) tea.Cmd {
	ctx, stop := context.WithCancel(m.ctx)
	wait := statusDialog("Auto approve", "asking the judge to write a rule that covers this request…")
	wait.footer = "a model writes its policy from the request's, and it is held to covering this request; it can take a minute or two"
	wait.onCancel = func() tea.Cmd {
		stop()
		return back()
	}
	m.dialog = wait
	return func() tea.Msg {
		defer stop()
		proposals, err := m.ds.RuleProposals(ctx, a.req.Server, a.req.ID)
		return ruleProposalsMsg{wait: wait, a: a, back: back, proposals: proposals, err: err}
	}
}

// ruleProposalsLoaded opens the first of the rule's two choices, unless the
// wait was left: proposals arriving over whatever the person went on to would
// be a dialog raised on its own.
func (m *Model) ruleProposalsLoaded(msg ruleProposalsMsg) tea.Cmd {
	if m.dialog != msg.wait {
		return nil
	}
	if msg.err != nil {
		d := errorDialog("Could not propose a rule", fmt.Sprintf("%v", msg.err))
		d.sections = []section{credentialErrorSection(msg.a.req), {lines: []line{
			{text: "esc goes back, where it can still be approved without a rule", tone: toneDim},
		}}}
		d.onCancel = msg.back
		m.dialog = d
		return nil
	}
	next := msg.a
	next.proposals = msg.proposals
	return m.askAutoDiscoboxes(next, func(approval) tea.Cmd { return msg.back() })
}

// everyDiscobox is what a rule that names no repository does, in the words
// the decision to allow one was recorded in. They are said wherever such a
// rule can be made or is read.
const everyDiscobox = "approves for discoboxes that do not exist yet, including ones a discobox creates"

// The answers to which discoboxes a rule is for: the repository offered by
// name, repositories typed in, or every discobox.
const (
	ruleForRepository = "repository"
	ruleForTyped      = "typed"
	ruleForEvery      = "every"
)

// discoboxesAsk is the "which discoboxes" step, as each flow that makes or
// edits a rule asks it. There is no sentence among its answers: a rule is for
// repositories the server compares with a discobox's primary source, or for
// every discobox in the project, which is its own row and its own value.
type discoboxesAsk struct {
	title    string
	sections []section
	// repository is one offered by name — the asking discobox's, for auto
	// approve — and noRepository what the card says when there is none to
	// offer. Empty offers no such row.
	repository, noRepository string
	// typed offers naming repositories, a line at a time; named is that list
	// as it stands — the rule's, or what was typed before going back.
	typed bool
	named lineList
	// was is the answer already given — by the rule being edited, or before
	// going back — which the cursor opens on.
	was ApprovalRule
}

// askDiscoboxes asks which discoboxes a rule is for, and hands chose the
// answer as a rule naming only that — its repositories, or every discobox —
// with the typed list as it stands, which the caller keeps. The narrower
// answers come first and the cursor starts on the first of them, unless an
// answer was already given. back is the step before, handed the typed list
// so going back loses none of it.
func (m *Model) askDiscoboxes(ask discoboxesAsk, chose func(ApprovalRule, lineList) tea.Cmd, back func(lineList) tea.Cmd) tea.Cmd {
	var items []action
	if ask.repository != "" {
		items = append(items, action{key: ruleForRepository, label: "Discoboxes of " + ask.repository,
			detail: "any discobox whose primary source is this repository, now or later; its other sources do not count", enabled: true})
	}
	typedDetail := "chosen from the repositories this project's discoboxes and rules have, or named: a URL or host/path, such as github.com/org/repo"
	if named := ask.named.shown(); len(named) > 0 {
		typedDetail = "now " + strings.Join(named, ", ")
	}
	if ask.typed {
		items = append(items, action{key: ruleForTyped, label: "Discoboxes of repositories you choose or name…", detail: typedDetail, enabled: true})
	}
	items = append(items, action{key: ruleForEvery, label: "Every discobox in this project",
		detail: "whatever its repository: it " + everyDiscobox, enabled: true})

	d := actionsDialog(ask.title, "", items, func(result string) tea.Cmd {
		switch result {
		case ruleForRepository:
			return chose(ApprovalRule{Repositories: []string{ask.repository}}, ask.named)
		case ruleForTyped:
			return m.askLines(linesAsk{
				title:       ask.title,
				sections:    ask.sections,
				answerLabel: "a repository, or several with commas",
				placeholder: "e.g. github.com/org/repo",
				written:     "repositories",
				footer:      "the server reads each, and refuses a bare org/repo",
				split:       splitRepositories,
				required:    "choose or name at least one repository, or go back and choose every discobox",
			}, ask.named, "", func(named lineList) tea.Cmd {
				return chose(ApprovalRule{Repositories: named.shown()}, named)
			}, func(named lineList) tea.Cmd {
				again := ask
				again.named = named
				return m.askDiscoboxes(again, chose, back)
			})
		case ruleForEvery:
			// The one place the wider rule is chosen.
			return chose(ApprovalRule{EveryDiscobox: true}, ask.named)
		}
		return nil
	})
	switch {
	case ask.was.EveryDiscobox:
		d.cursor = len(items) - 1
	case len(ask.was.Repositories) > 0 && ask.typed && (len(ask.was.Repositories) != 1 || ask.was.Repositories[0] != ask.repository):
		d.cursor = len(items) - 2
	}
	d.sections = ask.sections
	if ask.repository == "" && ask.noRepository != "" {
		d.sections = append(d.sections, section{label: "why there is no narrower one", lines: []line{{text: ask.noRepository, tone: toneDim}}})
	}
	d.wrapDetails = true
	d.answerLabel = "which discoboxes is the rule for?"
	d.footer = "a repository is compared with a discobox's primary source by the server; no model reads it"
	d.keys = []hint{pressing("enter chooses the highlighted one", "enter"), pressing("esc goes back", "esc")}
	if back != nil {
		d.onCancel = func() tea.Cmd { return back(ask.named) }
	}
	m.dialog = d
	return nil
}

// splitRepositories is one typed line as the repositories it names.
func splitRepositories(typed string) []string {
	var out []string
	for _, one := range strings.Split(typed, ",") {
		if one = strings.TrimSpace(one); one != "" {
			out = append(out, one)
		}
	}
	return out
}

// askAutoDiscoboxes is auto approve's first half of the rule: the repository
// of the discobox that asked, repositories the person chooses or names — the
// asker's ticked to start with — or every discobox. A discobox with no
// repository has the other two to offer, and the card says why.
//
// Each step after the proposals hands the step before it the approval as it
// now stands, and that step reopens from it: what was written on a later step
// — the policy, half-typed included — rides every step back and forward, as a
// rule's draft does (ruleStep).
func (m *Model) askAutoDiscoboxes(a approval, back func(approval) tea.Cmd) tea.Cmd {
	again := func(now approval) tea.Cmd { return m.askAutoDiscoboxes(now, back) }
	named := a.heldRepositories
	if !named.held {
		var own []listLine
		if a.proposals.Repository != "" {
			own = []listLine{{shown: a.proposals.Repository, value: a.proposals.RepositoryStored}}
		}
		named = m.knownRepositories(a.req.Server, own)
	}
	return m.askDiscoboxes(discoboxesAsk{
		title:        "Auto approve: which discoboxes",
		sections:     []section{grantSection(a)},
		repository:   a.proposals.Repository,
		noRepository: "the discobox that asked has no repository a rule could name: its source is a local one with no remote",
		typed:        true,
		named:        named,
		was:          a.rule,
	}, func(chosen ApprovalRule, named lineList) tea.Cmd {
		next := a
		next.heldRepositories = named
		next.rule.EveryDiscobox = chosen.EveryDiscobox
		next.rule.Repositories, next.repositoriesShown = nil, nil
		switch {
		case chosen.EveryDiscobox:
		case len(chosen.Repositories) == 1 && chosen.Repositories[0] == a.proposals.Repository:
			// The asker's alone: sent in the server's text.
			next.rule.Repositories, next.repositoriesShown = []string{a.proposals.RepositoryStored}, chosen.Repositories
		default:
			next.rule.Repositories, next.repositoriesShown = named.values(), named.shown()
		}
		return m.askRuleContent(next, again)
	}, func(named lineList) tea.Cmd {
		kept := a
		kept.heldRepositories = named
		return back(kept)
	})
}

// askRuleContent is the rule's second half: the policy it approves, as a
// list written a line at a time (askLines), a statement or a definition on
// each. It opens on the policy the project's judge wrote — the same work on
// any target of its kind — when it was found to cover this request, and
// otherwise on the policy as asked, so there is always something to edit; the
// card says which, and why. Each line is the person's to change, and a policy
// changed is no longer what was held to the request: the server holds it
// again when the rule is made. What keeps the lines from being a policy is
// said under them as they are written, and a list that is not one is not
// taken. A policy left as asked is not sent, which is the server's own
// default.
func (m *Model) askRuleContent(a approval, back func(approval) tea.Cmd) tea.Cmd {
	written := a.proposals.Written
	list := a.heldPolicy
	if !list.held {
		switch {
		case written != nil && written.Covers && written.Policy != nil:
			list = newLineList(policyEditLines(written.Policy, written.Stored))
		default:
			list = newLineList(policyEditLines(a.req.PolicyShown, a.req.Policy))
		}
	}
	ask := linesAsk{
		title:       "Auto approve: what it approves",
		sections:    []section{grantSection(a), ruleDiscoboxesSection(a.asRule().ApprovalRule)},
		answerLabel: "another line: a statement, or word = what it means",
		placeholder: "e.g. allow read on issue in repository $primary.repo",
		written:     "its policy",
		footer:      "keep every deny the request stated",
		numbered:    true,
		required:    "a rule says what it approves: write at least one statement",
		sectionsFor: func(now lineList) []section {
			return append(policySections(a.proposals, requestOrigin, now), policyProblemsSection(now)...)
		},
	}
	keep := func(held lineList) tea.Cmd {
		kept := a
		kept.heldPolicy = held
		return back(kept)
	}
	var done func(lineList) tea.Cmd
	done = func(list lineList) tea.Cmd {
		sent, shown, problems := listPolicy(list)
		if len(problems) > 0 {
			return m.askLines(ask, list, notAPolicyYet, done, keep)
		}
		next := a
		next.heldPolicy, next.policyShown, next.rule.Policy = list, shown, sent
		if samePolicy(sent, a.req.Policy) {
			next.rule.Policy = nil
		}
		return m.askLifetime(next, func() tea.Cmd { return m.askRuleContent(next, back) })
	}
	return m.askLines(ask, list, "", done, keep)
}

// policyOrigin is what a proposal's policy was written from and held to, as
// policySections says it.
type policyOrigin struct {
	// from is what the judge wrote it from, and asked what the list starts
	// from when it is not used.
	from, asked string
	// against is what it was held to covering.
	against string
	// held is when a policy nobody has held to anything yet will be.
	held string
}

// requestOrigin is auto approve's proposal: written from a pending request's
// policy, and held to covering it again when the rule approves it.
var requestOrigin = policyOrigin{
	from: "the request's policy", asked: "the policy as asked", against: "this request",
	held: "the server holds it to covering this request when the rule is made",
}

// grantOrigin is a proposal made from a grant: written from the policy it
// approved, held to covering that policy, and once made held against
// requests to come.
var grantOrigin = policyOrigin{
	from: "the policy this grant approved", asked: "the policy this grant approved", against: "the policy this grant approved",
	held: "each request the rule is tested on is held to it",
}

// policySections say where the policy on the card came from and what holding
// it to the request found: the model's, found to cover what it was held to —
// until a line is changed, when the finding is no longer about the text — or
// the policy it was written from, because the model's did not cover it,
// could not be a rule's, or no model wrote one.
func policySections(proposals RuleProposals, origin policyOrigin, now lineList) []section {
	written := proposals.Written
	sec := section{label: "where this comes from"}
	switch {
	case written != nil && written.Covers && written.Policy != nil && slices.Equal(now.values(), policyText(written.Stored)):
		sec.lines = []line{
			{text: "written by the project's judge from " + origin.from + ": the same work on any target of its kind"},
			{text: "it covers " + origin.against + ": " + reasonOr(written.Reason, "no reason given"), tone: toneOK},
		}
	case written != nil && written.Covers && written.Policy != nil:
		sec.lines = []line{
			{text: "started from the policy the project's judge wrote, and changed"},
			{text: "it was found to cover " + origin.against + " as written, not as it now is: " + origin.held, tone: toneAlert},
		}
	case written != nil && written.Refused != "":
		sec.lines = []line{
			{text: "started from " + origin.asked + ": what the project's judge wrote could not be a rule's policy — " + written.Refused + " — so it was not held to anything", tone: toneAlert},
		}
		for _, statement := range written.Statements {
			sec.lines = append(sec.lines, line{text: statement, bullet: true, tone: toneDim})
		}
	case written != nil:
		sec.lines = []line{
			{text: "started from " + origin.asked + ": the project's judge wrote this, and it does not cover " + origin.against + " — " + reasonOr(written.Reason, "no reason given"), tone: toneAlert},
		}
		sec.lines = append(sec.lines, rulePolicyLines(written.Policy, toneDim)...)
		sec.lines = append(sec.lines, line{text: origin.held, tone: toneDim})
	default:
		sec.lines = []line{
			{text: "started from " + origin.asked + ": " + reasonOr(proposals.Note, "no model wrote one"), tone: toneAlert},
			{text: origin.held, tone: toneDim},
		}
	}
	return []section{sec}
}

// reviewRule is the last card before the write: the rule's full text. That
// text is what is being approved, so every part of it is here — what the
// server copies from the request as well as what was chosen — and nothing is
// summarized. It asks rather than offers, and No is the default: the costly
// answer is yes, which approves requests nobody will be shown.
func (m *Model) reviewRule(a approval, back func() tea.Cmd) tea.Cmd {
	again := func() tea.Cmd { return m.reviewRule(a, back) }
	d := confirmDialog("Auto approve: the rule", "", func(string) tea.Cmd {
		return m.finishApproval(a, again)
	})
	rule := a.asRule()
	answered := a.secret.Name
	if a.gate {
		answered = gateAnswers
	}
	d.sections = ruleText(rule, ruleFactsSection(rule, a.ruleCredential(), answered, lifetime.Label(a.ttl)))
	if len(a.changes) > 0 {
		with := section{label: "changed with it"}
		for _, change := range a.changes {
			with.lines = append(with.lines, line{text: change, bullet: true})
		}
		d.sections = append(d.sections, with)
	}
	d.answerLabel = "make this rule, and approve this request under it?"
	d.footer = "a request it covers is approved without asking; one it does not still waits for you · no goes back"
	d.defaultNo = true
	d.onCancel = back
	m.dialog = d
	return nil
}

// asRule is the rule this approval would make, as far as it has been chosen:
// the repository and policy picked, around what the server copies from the
// request.
// It is the value the review card and a stored rule's card are both drawn
// from, so a rule reads the same before it is made and after.
func (a approval) asRule() Rule {
	hosts := a.proposals.Hosts
	if len(hosts) == 0 {
		hosts = a.req.hosts()
	}
	rule := a.rule
	if a.repositoriesShown != nil {
		// Drawn as made safe; the rule sent names them in the server's text.
		rule.Repositories = a.repositoriesShown
	}
	if a.policyShown != nil {
		// The same for the policy, which is sent only when it is not the
		// request's as asked.
		rule.Policy = a.policyShown
	}
	return Rule{
		ApprovalRule: rule,
		WellKnownID:  a.req.WellKnownID,
		Hosts:        hosts,
		Delegate:     a.proposals.Delegate || a.req.Delegate,
		MaxGrantTTL:  a.ttl,
	}
}

// ruleCredential is what the rule being made will call its credential: the
// server's word for it, or the request's own.
func (a approval) ruleCredential() string {
	if a.proposals.Credential != "" {
		return a.proposals.Credential
	}
	return credentialName(a.req)
}

// gateAnswers is what answers a rule for a gate, which no secret does.
const gateAnswers = "no secret: its pool lets its uses in"

// ruleFactsSection is the half of a rule nobody chose: its credential, where
// that may be sent and what for, as the server copies them from the request,
// with what answers it and how long each grant it mints may live. credential
// and lasts are said by the caller, which knows the names and whether the
// lifetime is one being chosen or a cap; answered is left out when empty.
func ruleFactsSection(rule Rule, credential, answered, lasts string) section {
	sec := section{label: "the rule", fields: []field{{label: "credential", value: credential, tone: toneAccent}}}
	if answered != "" {
		sec.fields = append(sec.fields, field{label: "answered with", value: answered})
	}
	purpose := "use"
	if rule.Delegate {
		purpose = delegationPurpose
	}
	sec.fields = append(sec.fields,
		field{label: "may be sent to", value: strings.Join(rule.Hosts, ", "), tone: toneAccent},
		field{label: "purpose", value: purpose, tone: toneAccent},
		field{label: "each grant lasts", value: lasts, tone: toneAccent},
	)
	return sec
}

// ruleText is a rule's whole text, as every card that shows one draws it — the
// one before it is made, the one before it is changed, and a stored rule's:
// facts, then which discoboxes and its whole policy, then what it hands out
// when that is more than a credential to use.
func ruleText(rule Rule, facts section) []section {
	out := append([]section{facts}, ruleSections(rule)...)
	if gives, ok := ruleHandsOut(rule); ok {
		out = append(out, gives)
	}
	return out
}

// ruleSections is the half that was chosen, as far as it has been: which
// discoboxes, and its policy, every statement and definition of it.
func ruleSections(rule Rule) []section {
	approves := section{label: "what it approves"}
	if rule.Delegate {
		approves.label = "what it would delegate"
	}
	approves.lines = rulePolicyLines(rule.Policy, tonePlain)
	if rule.Policy != nil && len(rule.Policy.Variables()) > 0 {
		approves.lines = append(approves.lines, line{text: "each variable is bound to whichever discobox asks", tone: toneDim})
	}
	return []section{ruleDiscoboxesSection(rule.ApprovalRule), approves}
}

// ruleDiscoboxesSection says which discoboxes a rule is for: those of the
// repositories it names, one a line, or every one of them, in the words that
// say what that means. A rule that says neither has not been given an answer,
// and is not drawn as the wider one.
func ruleDiscoboxesSection(rule ApprovalRule) section {
	sec := section{label: "the discoboxes it is for"}
	switch {
	case rule.EveryDiscobox:
		sec.lines = []line{
			{text: everyDiscoboxName},
			{text: everyDiscobox, tone: toneAlert},
		}
	case len(rule.Repositories) == 0:
		sec.lines = []line{{text: ruleForNothingYet, tone: toneDim}}
	}
	for _, repository := range rule.Repositories {
		sec.lines = append(sec.lines, line{text: "discoboxes of " + repository, bullet: len(rule.Repositories) > 1})
	}
	return sec
}

// ruleForNothingYet is a rule nobody has said the discoboxes of.
const ruleForNothingYet = "not chosen"

// everyDiscoboxName names what a rule with no repository is for.
const everyDiscoboxName = "every discobox in this project"

// ruleHandsOut says what a rule gives away when what it approves is more than
// one credential to use: a gate, or a delegation. Either was a person's alone
// to approve, and a rule is that approval given in advance, so the card says
// it in the words the request's own card does.
func ruleHandsOut(rule Rule) (section, bool) {
	sec := section{label: "what the rule gives each discobox it approves"}
	if known, ok := wellknown.Lookup(rule.WellKnownID); ok && known.Gate {
		sec.lines = gateGives(known)
	}
	if rule.Delegate {
		sec.lines = append(sec.lines, line{text: delegationPurpose, tone: toneAlert})
	}
	return sec, len(sec.lines) > 0
}

func reasonOr(reason, otherwise string) string {
	if reason = strings.TrimSpace(reason); reason != "" {
		return reason
	}
	return otherwise
}

// ruleTestsSection lists the approval rules already tested against a request
// and why each did not answer it: a request is on a person's card because no
// rule covered it, and which one nearly did is what says whether to widen it.
func ruleTestsSection(req CredentialRequest) section {
	sec := section{label: "rules that did not answer it"}
	for _, test := range req.RuleTests {
		said := reasonOr(test.Reason, "did not cover it")
		switch {
		case test.Pending:
			said = "still being judged"
		case test.Covered:
			// Covering is not the approval, which can still be refused.
			said = "it covers this, and has not approved it yet"
		case test.Stage != "":
			said = test.Stage + " · " + said
		}
		sec.lines = append(sec.lines, line{text: test.RuleID + " · " + said, bullet: true, tone: toneDim})
	}
	return sec
}
