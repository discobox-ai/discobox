package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/wellknown"

	tea "charm.land/bubbletea/v2"
)

// "Auto approve": answering a request by making the approval rule that
// answers it, and requests like it from now on, without asking
// (ADR 26-10-12-330 §§7–9).
//
// A rule replaces the person who reads a card, so its text is the
// authorization — and most of that text is written by a model from the agent's
// own ask. What keeps a generated sentence from becoming authority nobody read
// is the shape of this flow: one scope and one set of uses are each chosen
// from proposals shown whole, a proposal the judge did not find to cover this
// very request cannot be chosen, and the last card before the write is the
// rule in full.
//
// The steps run: request → what answers it, and its binding question, as for
// any approval → the proposals are fetched → which discoboxes → which uses →
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
	d := actionsDialog("Auto approve", "", secretRows(a.secrets, a.req), func(result string) tea.Cmd {
		return m.chooseSecret(a, strings.TrimPrefix(result, "secret:"))
	})
	d.sections = credentialAsk(a.req, m.secrets.now())
	d.answerLabel = "which secret answers this, and requests like it?"
	d.footer = "next the server proposes rules, narrowest first; nothing is approved until you have read one"
	d.keys = []hint{pressing("enter chooses the highlighted secret", "enter"), pressing("esc goes back", "esc")}
	d.onCancel = m.toRequest(a)
	m.dialog = d
	return nil
}

// proposeRules asks the server what rules could answer the request, and waits
// where it can be seen: a model writes the wider ones and the judge reads each
// against this request, which takes a minute or two. Esc stops the asking as
// well as the watching, and goes back.
func (m *Model) proposeRules(a approval, back func() tea.Cmd) tea.Cmd {
	ctx, stop := context.WithCancel(m.ctx)
	wait := statusDialog("Auto approve", "asking the judge which rules would cover this request…")
	wait.footer = "a model writes the wider ones and the judge reads each against this request; it can take a minute or two"
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
	return m.askRuleScope(next, msg.back)
}

// Scope levels, as the server numbers them, and what each is called.
var ruleScopeLabels = map[int]string{
	1: "This discobox",
	2: "Discoboxes doing this work on this repository",
	3: "Any discobox on this repository",
}

// Content levels, the same way.
var ruleContentLabels = map[int]string{
	1: "As asked",
	2: "The same work on any target of its kind",
	3: "The same kind of work across the resource",
}

// ruleByHand is the row, on either choice, that leaves the proposals for a
// sentence typed in.
const ruleByHand = "hand"

// everyDiscobox is what a rule with no scope and no pin does, in the words
// the decision to allow one was recorded in. They are said wherever such a
// rule can be made or is read.
const everyDiscobox = "approves for discoboxes that do not exist yet, including ones a discobox creates"

// askRuleScope is the rule's first half: which discoboxes it is tested for.
// The proposals are listed narrowest first, each row carrying its sentence
// whole and wrapped under it: the sentence is what is being chosen, and a row
// that cut it off would have a person choose a rule by its label.
func (m *Model) askRuleScope(a approval, back func() tea.Cmd) tea.Cmd {
	items := make([]action, 0, len(a.proposals.Scopes)+1)
	for _, p := range a.proposals.Scopes {
		text := p.Scope
		if p.SandboxID != "" {
			text = pinnedScope(p.SandboxID)
		}
		items = append(items, action{
			key:     fmt.Sprintf("scope:%d", p.Level),
			label:   ruleLevelLabel(ruleScopeLabels, p.Level),
			detail:  text,
			enabled: p.Covers,
			why:     failedProposal(text, "this discobox", p.Reason),
		})
	}
	items = append(items, action{key: ruleByHand, press: "w", label: "Write the scope by hand…",
		detail: "a sentence the judge reads; the server judges it before the rule is made", enabled: true})

	again := func() tea.Cmd { return m.askRuleScope(a, back) }
	d := actionsDialog("Auto approve: which discoboxes", "", items, func(result string) tea.Cmd {
		if result == ruleByHand {
			return m.askRuleScopeByHand(a, again)
		}
		for _, p := range a.proposals.Scopes {
			if result == fmt.Sprintf("scope:%d", p.Level) && p.Covers {
				next := a
				next.rule.Scope, next.rule.SandboxID = p.Scope, p.SandboxID
				return m.askRuleContent(next, again)
			}
		}
		return nil
	})
	d.sections = append([]section{grantSection(a)}, ruleNote(a)...)
	d.wrapDetails = true
	d.answerLabel = "which discoboxes is the rule for?"
	d.footer = "a discobox is read by what was recorded when it was created, never by what it says of itself"
	d.keys = []hint{pressing("enter chooses the highlighted scope", "enter"), pressing("esc goes back", "esc")}
	d.onCancel = back
	m.dialog = d
	return nil
}

// askRuleScopeByHand takes a scope nobody proposed. It is sent as written and
// the server puts it to the judge before it makes the rule. Left empty it is
// no scope at all, which is every discobox in the project — the one width no
// proposal offers, reachable only by asking for it here, under the sentence
// saying what it does.
func (m *Model) askRuleScopeByHand(a approval, back func() tea.Cmd) tea.Cmd {
	d := inputDialog("Auto approve: which discoboxes", "", "e.g. discoboxes started to triage an issue", a.rule.Scope, func(value string) tea.Cmd {
		next := a
		next.rule.Scope, next.rule.SandboxID = value, ""
		return m.askRuleContent(next, func() tea.Cmd { return m.askRuleScopeByHand(next, back) })
	})
	d.sections = []section{grantSection(a)}
	d.answerLabel = "the rule is for"
	d.footer = "left empty it is for every discobox in this project: it " + everyDiscobox
	d.keys = []hint{pressing("Enter accepts", "enter"), pressing("Esc goes back", "esc")}
	d.onCancel = back
	m.dialog = d
	return nil
}

// askRuleContent is the rule's second half: the uses it approves, chosen the
// way the scope was, each row carrying its uses whole.
func (m *Model) askRuleContent(a approval, back func() tea.Cmd) tea.Cmd {
	items := make([]action, 0, len(a.proposals.Contents)+1)
	for _, p := range a.proposals.Contents {
		// One use a line, each wrapped under itself, so where one sentence
		// ends and the next begins is not a matter of reading for it.
		uses := "· " + strings.Join(p.Uses, "\n· ")
		items = append(items, action{
			key:     fmt.Sprintf("content:%d", p.Level),
			label:   ruleLevelLabel(ruleContentLabels, p.Level),
			detail:  uses,
			enabled: p.Covers,
			why:     failedProposal(uses, "what was asked", p.Reason),
		})
	}
	items = append(items, action{key: ruleByHand, press: "w", label: "Write the uses by hand…",
		detail: "one sentence a line; the server judges them before the rule is made", enabled: true})

	again := func() tea.Cmd { return m.askRuleContent(a, back) }
	d := actionsDialog("Auto approve: what it approves", "", items, func(result string) tea.Cmd {
		if result == ruleByHand {
			return m.askRuleUsesByHand(a, again, nil, "")
		}
		for _, p := range a.proposals.Contents {
			if result == fmt.Sprintf("content:%d", p.Level) && p.Covers {
				next := a
				next.rule.Uses = p.Uses
				return m.askLifetime(next, again)
			}
		}
		return nil
	})
	d.sections = append([]section{grantSection(a), ruleScopeSection(a.rule)}, ruleNote(a)...)
	d.wrapDetails = true
	d.answerLabel = "what may the rule approve?"
	d.footer = "a later request is approved only when every use it asks for is within these"
	d.keys = []hint{pressing("enter chooses the highlighted uses", "enter"), pressing("esc goes back", "esc")}
	d.onCancel = back
	m.dialog = d
	return nil
}

// askRuleUsesByHand takes the uses a line at a time: Enter adds the sentence
// typed, and Enter on an empty line is the end of them. The card's field is
// one line, and a use is one sentence, so the list is built above the field
// rather than typed into it. Esc takes back the last line, as every step here
// goes back one. refused is what was wrong with the last answer.
func (m *Model) askRuleUsesByHand(a approval, back func() tea.Cmd, uses []string, refused string) tea.Cmd {
	again := func() tea.Cmd { return m.askRuleUsesByHand(a, back, uses, "") }
	d := inputDialog("Auto approve: what it approves", "", "e.g. Comment on any issue in org/repo", "", func(value string) tea.Cmd {
		if value != "" {
			// Clipped, so a line added after going back does not write over
			// the one that was taken back.
			return m.askRuleUsesByHand(a, again, append(uses[:len(uses):len(uses)], value), "")
		}
		if len(uses) == 0 {
			return m.askRuleUsesByHand(a, back, uses, "a rule says what it approves: write at least one use")
		}
		next := a
		next.rule.Uses = uses
		return m.askLifetime(next, again)
	})
	written := section{label: "uses written so far"}
	for _, use := range uses {
		written.lines = append(written.lines, line{text: use, bullet: true})
	}
	if len(uses) == 0 {
		written.lines = []line{{text: "none yet", tone: toneDim}}
	}
	d.sections = []section{grantSection(a), ruleScopeSection(a.rule), written}
	d.answerLabel = "another use, one sentence"
	d.footer = "Enter adds the line; Enter on an empty line finishes · keep every \"never …\" the request stated"
	d.emphasis = refused
	d.keys = []hint{pressing("Enter adds it, or finishes", "enter"), pressing("Esc goes back", "esc")}
	d.onCancel = back
	m.dialog = d
	return nil
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
	d.sections = append([]section{ruleFactsSection(rule, a.ruleCredential(), answered, lifetime.Label(a.ttl))}, ruleSections(rule)...)
	if gives, ok := ruleHandsOut(rule); ok {
		d.sections = append(d.sections, gives)
	}
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
// the scope and uses picked, around what the server copies from the request.
// It is the value the review card and a stored rule's card are both drawn
// from, so a rule reads the same before it is made and after.
func (a approval) asRule() Rule {
	hosts := a.proposals.Hosts
	if len(hosts) == 0 {
		hosts = a.req.hosts()
	}
	return Rule{
		ApprovalRule: a.rule,
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

// ruleSections is the half that was chosen, as far as it has been: which
// discoboxes, and every use.
func ruleSections(rule Rule) []section {
	uses := section{label: "what it approves"}
	if rule.Delegate {
		uses.label = "uses it would delegate"
	}
	for _, use := range rule.Uses {
		uses.lines = append(uses.lines, line{text: use, bullet: true})
	}
	return []section{ruleScopeSection(rule.ApprovalRule), uses}
}

// ruleScopeSection says which discoboxes a rule is for: the one it is pinned
// to, the sentence the judge will read, or — with neither — all of them, in
// the words that say what that means.
func ruleScopeSection(rule ApprovalRule) section {
	sec := section{label: "the discoboxes it is for"}
	switch {
	case rule.SandboxID != "" && rule.Scope != "":
		// Both, which only the CLI makes: the one discobox, and it still has
		// to read as the sentence says.
		sec.lines = []line{{text: rule.Scope}, {text: "and only " + rule.SandboxID + ", by its ID"}}
	case rule.SandboxID != "":
		sec.lines = []line{{text: pinnedScope(rule.SandboxID)}}
	case rule.Scope != "":
		sec.lines = []line{{text: rule.Scope}}
	default:
		sec.lines = []line{
			{text: everyDiscoboxScope},
			{text: everyDiscobox, tone: toneAlert},
		}
	}
	return sec
}

// everyDiscoboxScope names the scope that is neither a sentence nor a pin.
const everyDiscoboxScope = "every discobox in this project"

// pinnedScope is the scope that is no sentence.
func pinnedScope(sandboxID string) string {
	return "pinned to " + sandboxID + " by its ID; nothing is read"
}

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

// failedProposal is what the row of a proposal the judge did not find to
// cover says: the proposal still whole, and under it that it failed and why —
// covering names what it was read against — since why it failed is what says
// which way to write one by hand.
func failedProposal(text, covering, reason string) string {
	return text + "\nfailed, and cannot be chosen: " + reasonOr(reason, "the judge did not find it covers "+covering)
}

func reasonOr(reason, otherwise string) string {
	if reason = strings.TrimSpace(reason); reason != "" {
		return reason
	}
	return otherwise
}

// ruleLevelLabel names a proposal's level, and still names one the server
// numbered past the ones known here.
func ruleLevelLabel(labels map[int]string, level int) string {
	if label, ok := labels[level]; ok {
		return label
	}
	return fmt.Sprintf("Level %d", level)
}

// ruleNote is the server saying why only the narrowest of each is offered,
// on both choices: it is the answer to "where are the others".
func ruleNote(a approval) []section {
	if a.proposals.Note == "" {
		return nil
	}
	return []section{{label: "why there are no wider ones", lines: []line{{text: a.proposals.Note, tone: toneDim}}}}
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
			// The judge's yes is not the approval, which can still be refused.
			said = "the judge found it covers this, and it has not approved it yet"
		case test.Stage != "":
			said = test.Stage + " · " + said
		}
		sec.lines = append(sec.lines, line{text: test.RuleID + " · " + said, bullet: true, tone: toneDim})
	}
	return sec
}
