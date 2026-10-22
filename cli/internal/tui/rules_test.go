package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/discobox-ai/discobox/pkg/wellknown"
)

// "Auto approve" makes a standing answer out of one request, and most of its
// text is a model's reading of what the agent wrote. These tests are about the
// things that keep that from being authority nobody read: what can be chosen,
// what is shown before the write, and what is sent.

// errRuleRefused is the server declining to make a rule that would not cover
// the request it is made from.
var errRuleRefused = errors.New("this rule would not cover the request it is made from, so it is not made: the request's policy is not within the rule's (a push to main is not within these); choose a policy that covers it, or approve without a rule")

// writtenStatement is the statement the project's judge writes from
// policyRequest's: the same work, and more of it, on any pull request of the
// repository.
const writtenStatement = "allow create, update on pull request in repository $primary.repo"

// writtenPolicy is what the judge writes from policyRequest's policy.
func writtenPolicy() *WrittenPolicy {
	p := mustPolicy(writtenStatement, "pull request = a pull request on GitHub", "repository = a GitHub repository")
	return &WrittenPolicy{Policy: p, Stored: p, Covers: true, Reason: "every allow the request asks for is within this policy"}
}

// proposalsForTheRequest is what a server with a judge offers for
// policyRequest: the repository of the discobox that asked, and the policy
// the judge wrote, found to cover the request.
func proposalsForTheRequest() RuleProposals {
	return RuleProposals{
		Credential:       "github",
		Hosts:            []string{"api.github.com"},
		Repository:       "github.com/acme/pool",
		RepositoryStored: "github.com/acme/pool",
		Written:          writtenPolicy(),
	}
}

// sourceWithPolicyRequest is sourceWithRequest with the request asking for a
// policy, which a rule may answer.
func sourceWithPolicyRequest(t *testing.T) (*Model, *fakeSource) {
	t.Helper()
	ds := newFakeSource(testSandboxes()...)
	ds.requests = []CredentialRequest{policyRequest()}
	ds.projectSecrets = []Secret{
		{ID: "sec_gh", Name: "GitHub token", Type: "bearer", Host: "api.github.com"},
		{ID: "sec_openai", Name: "OpenAI key", Type: "bearer", Host: "api.openai.com"},
	}
	return newTestModel(t, ds), ds
}

func sourceWithProposals(t *testing.T) (*Model, *fakeSource) {
	t.Helper()
	m, ds := sourceWithPolicyRequest(t)
	ds.proposals = proposalsForTheRequest()
	return m, ds
}

// toRuleScope opens the request, chooses auto approve and the secret that
// answers, and lands on the first of the rule's two choices.
func toRuleDiscoboxes(t *testing.T, m *Model) {
	t.Helper()
	send(t, m, keyPress("tab"), keyPress(credentialsKey), keyPress(autoApproveKey))
	if m.dialog == nil || m.dialog.title != "Auto approve" {
		t.Fatalf("dialog = %s, want the card asking which secret the rule answers with", describe(m.dialog))
	}
	drain(t, m, m.dialog.action("secret:sec_gh"), 0)
	if !onRuleStep(m, "which discoboxes") {
		t.Fatalf("dialog = %s, want the step asking which discoboxes", describe(m.dialog))
	}
}

func onRuleStep(m *Model, step string) bool {
	return m.dialog != nil && m.dialog.title == "Auto approve: "+step
}

// wholeCard is a card with room for all of it: these cards are read for every
// line, and a window-sized draw scrolls the end of a long one out of sight.
func wholeCard(m *Model) string {
	if m.dialog == nil {
		return ""
	}
	return ansi.Strip(m.dialog.view(m.st, &m.zones, 100, 400))
}

// cardWords is wholeCard as running text, for a sentence the card wrapped:
// the box's edges and the line breaks between its rows are taken out.
func cardWords(m *Model) string {
	return strings.Join(strings.Fields(strings.NewReplacer("│", " ", "╭", " ", "╮", " ", "╰", " ", "╯", " ").Replace(wholeCard(m))), " ")
}

// The whole of what is chosen is sent with the approval, in the one write that
// makes the rule: the repository, the policy, and the lifetime — and nothing
// about the credential, which the server copies from the request. The
// approval names the request's policy as read, as every approval of one
// does.
func TestAutoApproveSendsTheChosenRepositoryAndPolicyWithTheApproval(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithProposals(t)

	toRuleDiscoboxes(t, m)
	if len(ds.proposed) != 1 || ds.proposed[0] != "sreq_1" {
		t.Fatalf("proposed = %v, want the proposals asked for once, for this request", ds.proposed)
	}
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	if !onRuleStep(m, "what it approves") {
		t.Fatalf("dialog = %s, want the step asking what it approves", describe(m.dialog))
	}
	drain(t, m, m.dialog.action(""), 0)
	grantFor(t, m, time.Hour)
	if !onRuleStep(m, "the rule") {
		t.Fatalf("dialog = %s, want the rule to read before anything is written", describe(m.dialog))
	}
	if len(ds.approvals) != 0 {
		t.Fatal("it approved before the rule was read")
	}
	send(t, m, keyPress("y"))

	if len(ds.approvals) != 1 {
		t.Fatalf("approvals = %#v, want one", ds.approvals)
	}
	got := ds.approvals[0]
	if got.RequestID != "sreq_1" || got.SecretID != "sec_gh" || got.TTLSeconds != 3600 {
		t.Fatalf("approval = %#v, want the request, the secret and the hour", got)
	}
	if got.Rule == nil || len(got.Rule.Repositories) != 1 || got.Rule.Repositories[0] != "github.com/acme/pool" || got.Rule.EveryDiscobox {
		t.Fatalf("rule = %#v, want it for the repository of the discobox that asked", got.Rule)
	}
	if !samePolicy(got.Rule.Policy, writtenPolicy().Stored) {
		t.Fatalf("rule policy = %q, want the judge's, as written", policyText(got.Rule.Policy))
	}
	if got.Policy == nil || got.Policy.Resolved["$primary.repo"] != "acme/pool" {
		t.Fatalf("approval policy = %+v, want the request's as read, with the values shown", got.Policy)
	}
	if m.dialog != nil {
		t.Fatalf("dialog = %s, want it closed once answered", describe(m.dialog))
	}
}

// Which discoboxes a rule is for has three answers and no sentence: the
// repository of the discobox that asked, first and under the cursor;
// repositories the person chooses or names, the asker's ticked to start
// with; and every discobox in the project.
func TestWhichDiscoboxesIsTheRepositoryChosenOnesOrEveryDiscobox(t *testing.T) {
	t.Parallel()
	m, _ := sourceWithProposals(t)

	toRuleDiscoboxes(t, m)
	items := m.dialog.items
	if len(items) != 3 || items[0].key != ruleForRepository || items[1].key != ruleForTyped || items[2].key != ruleForEvery {
		t.Fatalf("items = %+v, want the repository, those chosen or named, then every discobox", items)
	}
	if m.dialog.cursor != 0 {
		t.Fatalf("cursor = %d, want it on the narrowest answer", m.dialog.cursor)
	}
	card := cardWords(m)
	for _, want := range []string{"Discoboxes of github.com/acme/pool", "whose primary source is this repository",
		"Discoboxes of repositories you choose or name", "now github.com/acme/pool", "Every discobox in this project", everyDiscobox} {
		if !strings.Contains(card, want) {
			t.Fatalf("card = %q, want it to carry %q", card, want)
		}
	}
	for _, gone := range []string{"pinned", "This discobox"} {
		if strings.Contains(card, gone) {
			t.Fatalf("card = %q, want nothing about %q", card, gone)
		}
	}
}

// Repositories chosen or named go to the rule as chosen: the asker's ticked
// to start with, others ticked or typed beside it.
func TestAutoApproveSendsTheRepositoriesChosen(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithProposals(t)
	m.list.all[1].RepositoryIdentity, m.list.all[1].RepositoryShown = "github.com/acme/tools", "github.com/acme/tools"

	toRuleDiscoboxes(t, m)
	drain(t, m, m.dialog.action(ruleForTyped), 0)
	if got := pickLabels(m); len(got) < 2 || got[0] != "[x] github.com/acme/pool" || got[1] != "[ ] github.com/acme/tools" {
		t.Fatalf("picks = %q, want the asker's ticked, then the rest", got)
	}
	send(t, m, keyPress("up"), keyPress(" "))
	send(t, m, keyPress("down"))
	drain(t, m, m.dialog.action("gitlab.example.com/team/other"), 0)
	drain(t, m, m.dialog.action(""), 0)
	if !onRuleStep(m, "what it approves") {
		t.Fatalf("dialog = %s, want the policy next", describe(m.dialog))
	}
	drain(t, m, m.dialog.action(""), 0)
	grantFor(t, m, time.Hour)
	send(t, m, keyPress("y"))
	if len(ds.approvals) != 1 || ds.approvals[0].Rule == nil || ds.approvals[0].Rule.EveryDiscobox ||
		strings.Join(ds.approvals[0].Rule.Repositories, ",") != "github.com/acme/pool,github.com/acme/tools,gitlab.example.com/team/other" {
		t.Fatalf("approvals = %#v, want the repositories chosen and named", ds.approvals)
	}
}

// A discobox started from a local directory with no remote has no repository
// a rule could name for it: the rows are repositories chosen or named, and
// every discobox, and the card says why there is no narrower one.
func TestADiscoboxWithNoRepositoryIsOfferedTheOthers(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithPolicyRequest(t)
	proposals := proposalsForTheRequest()
	proposals.Repository, proposals.RepositoryStored = "", ""
	ds.proposals = proposals

	toRuleDiscoboxes(t, m)
	if items := m.dialog.items; len(items) != 2 || items[0].key != ruleForTyped || items[1].key != ruleForEvery {
		t.Fatalf("items = %+v, want those chosen or named, and every discobox", items)
	}
	if card := cardWords(m); !strings.Contains(card, "no repository a rule could name") || !strings.Contains(card, everyDiscobox) {
		t.Fatalf("card = %q, want it to say why there is no narrower rule, and what this one does", card)
	}
}

// The policy step opens on the policy the judge wrote, whole, in the list a
// person edits a line at a time — each statement its sentence, each
// definition word = what it means — saying it is the model's and that it was
// found to cover this request. Changed, the card says the finding is no
// longer about it, and the rule is sent with the policy as edited.
func TestTheWrittenPolicyIsLoadedToEdit(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithProposals(t)

	toRuleDiscoboxes(t, m)
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	if !onRuleStep(m, "what it approves") || m.dialog.kind != dlgInput {
		t.Fatalf("dialog = %s, want the policy as a list", describe(m.dialog))
	}
	card := cardWords(m)
	for _, want := range []string{writtenStatement, "pull request = a pull request on GitHub", "repository = a GitHub repository",
		"written by the project's judge", "the same work on any target of its kind",
		"it covers this request: every allow the request asks for is within this policy"} {
		if !strings.Contains(card, want) {
			t.Fatalf("card = %q, want it to carry %q", card, want)
		}
	}
	if strings.Contains(card, "not a policy a rule can have yet") {
		t.Fatalf("card = %q, want no problem said of a policy", card)
	}
	// One taken up, edited in place.
	send(t, m, keyPress("up"))
	if m.dialog.typed() != "repository = a GitHub repository" {
		t.Fatalf("field = %q, want the last line to edit", m.dialog.typed())
	}
	send(t, m, keyPress("up"), keyPress("up"))
	if m.dialog.typed() != writtenStatement {
		t.Fatalf("field = %q, want the statement to edit", m.dialog.typed())
	}
	edited := "allow update on pull request in repository $primary.repo"
	drain(t, m, m.dialog.action(edited), 0)
	if card := cardWords(m); !strings.Contains(card, "it was found to cover this request as written, not as it now is") {
		t.Fatalf("card = %q, want it to say the finding no longer applies", card)
	}
	drain(t, m, m.dialog.action(""), 0)
	grantFor(t, m, time.Hour)
	send(t, m, keyPress("y"))
	if len(ds.approvals) != 1 || ds.approvals[0].Rule == nil ||
		!slices.Equal(policyText(ds.approvals[0].Rule.Policy), []string{edited, "pull request = a pull request on GitHub", "repository = a GitHub repository"}) {
		t.Fatalf("approvals = %#v, want the policy as edited", ds.approvals)
	}
}

// Lines that are not a policy a rule can have say what is wrong with them as
// they are written, with the grammar, and are not taken: finishing them is
// refused where they were typed, and nothing is approved.
func TestAPolicyThatDoesNotParseIsRefusedWhereItIsWritten(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithProposals(t)

	toRuleDiscoboxes(t, m)
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	drain(t, m, m.dialog.action("allow read issue in repository $primary.repo"), 0)
	card := cardWords(m)
	for _, want := range []string{"not a policy a rule can have yet", `line 4: at "in" (byte 17)`, "a statement reads: allow|deny", "word = what it means"} {
		if !strings.Contains(card, want) {
			t.Fatalf("card = %q, want it to carry %q", card, want)
		}
	}
	drain(t, m, m.dialog.action(""), 0)
	if m.dialog == nil || m.dialog.kind != dlgInput || !strings.Contains(wholeCard(m), notAPolicyYet) {
		t.Fatalf("dialog = %s, card = %q; want the field still up, saying why", describe(m.dialog), wholeCard(m))
	}
	// Fixed, it is a policy again, and goes on.
	send(t, m, keyPress("up"))
	drain(t, m, m.dialog.action("deny delete on anything in anywhere"), 0)
	if strings.Contains(cardWords(m), "not a policy a rule can have yet") {
		t.Fatalf("card = %q, want no problem left", cardWords(m))
	}
	drain(t, m, m.dialog.action(""), 0)
	if !onLifetimeStep(m) || len(ds.approvals) != 0 {
		t.Fatalf("dialog = %s, approvals = %d; want the lifetime next, nothing approved", describe(m.dialog), len(ds.approvals))
	}
}

// A policy left as asked is not sent with the rule: the server takes the
// request's as asked when the rule names none, which is what was read.
func TestThePolicyAsAskedIsLeftOut(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithPolicyRequest(t)
	ds.proposals = proposalsForTheRequest()
	ds.proposals.Written = nil

	toRuleDiscoboxes(t, m)
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	if card := cardWords(m); !strings.Contains(card, "allow create on pull request in repository $primary.repo") {
		t.Fatalf("card = %q, want the policy as asked to edit", card)
	}
	drain(t, m, m.dialog.action(""), 0)
	grantFor(t, m, time.Hour)
	if card := cardWords(m); !strings.Contains(card, "allow create on pull request") {
		t.Fatalf("card = %q, want the rule's policy read whole", card)
	}
	send(t, m, keyPress("y"))
	if len(ds.approvals) != 1 || ds.approvals[0].Rule == nil || ds.approvals[0].Rule.Policy != nil {
		t.Fatalf("approvals = %#v, want a rule naming no policy: the request's as asked", ds.approvals)
	}
}

// A policy the judge did not find to cover the request is not loaded: the
// list starts from the policy as asked, and the card says why, with the
// model's shown for reference. So does one that could not be a rule's, and a
// proposal no model wrote.
func TestAPolicyThatDoesNotCoverStartsFromTheAsk(t *testing.T) {
	t.Parallel()
	wide := mustPolicy(`allow create, update on pull request in organization "acme"`, "organization = a GitHub organization", "pull request = a pull request on GitHub")
	for _, tc := range []struct {
		name    string
		written *WrittenPolicy
		note    string
		want    string
	}{
		{"not covering", &WrittenPolicy{Policy: wide, Stored: wide, Reason: "the organization is not the repository"},
			"", "the project's judge wrote this, and it does not cover this request — the organization is not the repository"},
		{"refused", &WrittenPolicy{Statements: []string{"allow merging of pull requests"}, Refused: "statement 1 does not parse"},
			"", "what the project's judge wrote could not be a rule's policy — statement 1 does not parse — so it was not held to anything"},
		{"none written", nil, "no model wrote the policy: the project has no judge", "started from the policy as asked: no model wrote the policy: the project has no judge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, ds := sourceWithPolicyRequest(t)
			proposals := proposalsForTheRequest()
			proposals.Written, proposals.Note = tc.written, tc.note
			ds.proposals = proposals
			toRuleDiscoboxes(t, m)
			drain(t, m, m.dialog.action(ruleForRepository), 0)
			card := cardWords(m)
			if !strings.Contains(card, "allow create on pull request in repository $primary.repo") || !strings.Contains(card, tc.want) {
				t.Fatalf("card = %q, want the policy as asked and %q", card, tc.want)
			}
		})
	}
}

// A rule that approves nothing in particular is not one this window makes:
// finishing the policy with every line removed is refused where it was typed.
func TestNoPolicyWrittenIsNotARule(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithProposals(t)

	toRuleDiscoboxes(t, m)
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	for range 3 {
		send(t, m, keyPress("up"))
		drain(t, m, m.dialog.action(""), 0)
	}
	drain(t, m, m.dialog.action(""), 0)

	if m.dialog == nil || m.dialog.kind != dlgInput || !strings.Contains(wholeCard(m), "write at least one statement") {
		t.Fatalf("dialog = %s, card = %q; want the field still up, saying why", describe(m.dialog), wholeCard(m))
	}
	if len(ds.approvals) != 0 {
		t.Fatal("something was approved")
	}
}

// The last card is the rule, whole. That text is what is being approved; a
// generated sentence nobody read is the agent's ask promoted to authority, so
// every part is on it — what the server copies from the request as well as
// what was chosen — and Enter does not make it.
func TestTheLastCardIsTheRuleInFull(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithProposals(t)

	toRuleDiscoboxes(t, m)
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	drain(t, m, m.dialog.action(""), 0)
	grantFor(t, m, 24*time.Hour)

	card := wholeCard(m)
	for _, want := range []string{
		"github", "GitHub token", "api.github.com", "purpose", "use",
		"discoboxes of github.com/acme/pool",
		"allow create, update on pull request (a pull request on GitHub)",
		"in repository (a GitHub repository) $primary.repo",
		"each variable is bound to whichever discobox asks",
		"1 day",
	} {
		if !strings.Contains(card, want) {
			t.Fatalf("card = %q, want the rule's %q on it", card, want)
		}
	}
	if strings.Contains(card, everyDiscobox) {
		t.Fatalf("card = %q, want no warning about every discobox on a rule for a repository", card)
	}
	// The costly answer is yes, so Enter is not it.
	send(t, m, keyPress("enter"))
	if len(ds.approvals) != 0 {
		t.Fatal("Enter made the rule; only y may")
	}
}

// A rule that names no repository approves for every discobox in the project.
// It is never where the cursor starts when there is a narrower answer, and
// both its row and the rule's card say what it does in the words the decision
// to allow it was recorded in. It is its own answer on the rule, set by that
// row and nothing else, which is what the data source passes on.
func TestEveryDiscoboxSaysItApprovesForDiscoboxesThatDoNotExistYet(t *testing.T) {
	t.Parallel()
	const words = "approves for discoboxes that do not exist yet, including ones a discobox creates"
	m, ds := sourceWithProposals(t)

	toRuleDiscoboxes(t, m)
	if row := cardWords(m); !strings.Contains(row, words) {
		t.Fatalf("row = %q, want it to say what a rule for every discobox does", row)
	}
	drain(t, m, m.dialog.action(ruleForEvery), 0)
	drain(t, m, m.dialog.action(""), 0)
	grantFor(t, m, time.Hour)

	if card := cardWords(m); !strings.Contains(card, "every discobox in this project") || !strings.Contains(card, words) {
		t.Fatalf("card = %q, want it to say %q", card, words)
	}
	send(t, m, keyPress("y"))
	if len(ds.approvals) != 1 || ds.approvals[0].Rule == nil {
		t.Fatalf("approvals = %#v, want one carrying a rule", ds.approvals)
	}
	if rule := ds.approvals[0].Rule; len(rule.Repositories) != 0 || !rule.EveryDiscobox {
		t.Fatalf("rule = %#v, want every discobox said, and no repository named", rule)
	}
}

// Esc is "not that one" at every step, and goes back exactly one: a flow this
// long that dropped to the list on a second thought would be answered without
// second thoughts.
func TestEscWalksBackThroughTheRuleOneStepAtATime(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithProposals(t)

	toRuleDiscoboxes(t, m)
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	drain(t, m, m.dialog.action(""), 0)
	grantFor(t, m, time.Hour)

	send(t, m, keyPress("esc"))
	if !onLifetimeStep(m) {
		t.Fatalf("dialog = %s, want the lifetime back from the rule", describe(m.dialog))
	}
	send(t, m, keyPress("esc"))
	if !onRuleStep(m, "what it approves") {
		t.Fatalf("dialog = %s, want the policy back from the lifetime", describe(m.dialog))
	}
	send(t, m, keyPress("esc"))
	if !onRuleStep(m, "which discoboxes") {
		t.Fatalf("dialog = %s, want the discoboxes back from the policy", describe(m.dialog))
	}
	// The proposals took a minute to get; going back does not ask again.
	if len(ds.proposed) != 1 {
		t.Fatalf("proposed = %v, want the one asking", ds.proposed)
	}
	send(t, m, keyPress("esc"))
	if m.dialog == nil || m.dialog.title != "Auto approve" {
		t.Fatalf("dialog = %s, want the secret choice back from the discoboxes", describe(m.dialog))
	}
	send(t, m, keyPress("esc"))
	if !onRequestCard(m) {
		t.Fatalf("dialog = %s, want the request back", describe(m.dialog))
	}
	if len(ds.approvals) != 0 {
		t.Fatal("going back approved something")
	}
}

// Esc never changes a word of the policy: it goes back to which discoboxes,
// and forward again opens on it as it was left, the line half-typed into the
// field included.
func TestEscKeepsThePolicyWritten(t *testing.T) {
	t.Parallel()
	m, _ := sourceWithProposals(t)

	toRuleDiscoboxes(t, m)
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	drain(t, m, m.dialog.action("deny delete on anything in anywhere"), 0)
	setField(m, "allow read on iss")
	send(t, m, keyPress("esc"))
	if !onRuleStep(m, "which discoboxes") {
		t.Fatalf("dialog = %s, want which discoboxes back", describe(m.dialog))
	}
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	card := wholeCard(m)
	if m.dialog == nil || m.dialog.kind != dlgInput || !strings.Contains(card, "deny delete on anything in anywhere") ||
		!strings.Contains(card, writtenStatement) || m.dialog.typed() != "allow read on iss" {
		t.Fatalf("card = %q, field = %q; want every line and the half-typed one kept", card, m.dialog.typed())
	}
}

// The server holds a rule to the request before it makes it and refuses one that would not
// cover the request, with a sentence saying which half fell short. That
// sentence stays on screen, nothing was made, and Esc goes back to the rule
// so another can be chosen — a refusal that dropped to the list would lose
// every choice made on the way.
func TestARefusedRuleIsShownAndCanBeChosenAgain(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithProposals(t)
	ds.mu.Lock()
	ds.approveErr = errRuleRefused
	ds.mu.Unlock()

	toRuleDiscoboxes(t, m)
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	drain(t, m, m.dialog.action(""), 0)
	grantFor(t, m, time.Hour)
	send(t, m, keyPress("y"))

	if m.dialog == nil || !m.dialog.err {
		t.Fatalf("dialog = %s, want the refusal", describe(m.dialog))
	}
	text := cardWords(m)
	for _, want := range []string{"would not cover the request it is made from", "a push to main is not within these", "still waiting", "no rule was made"} {
		if !strings.Contains(text, want) {
			t.Fatalf("refusal = %q, want it to carry %q", text, want)
		}
	}
	if len(m.requests["sbx_one"]) != 1 {
		t.Fatal("the request stopped waiting after a refused rule")
	}

	send(t, m, keyPress("esc"))
	if !onRuleStep(m, "the rule") {
		t.Fatalf("dialog = %s, want the rule back", describe(m.dialog))
	}
	send(t, m, keyPress("esc"), keyPress("esc"))
	if !onRuleStep(m, "what it approves") {
		t.Fatalf("dialog = %s, want the way back to another policy", describe(m.dialog))
	}
}

// A rule's content is a policy a request's is held to. A request the proxy
// opened asks for none, nor does an ask of uses alone; a refresh asks for a
// value, and a trust is answered with a pin: each stays a person's to answer
// every time, so none of them offers a rule.
func TestOnlyAnAgentsOwnAskOffersAutoApprove(t *testing.T) {
	t.Parallel()
	offers := func(t *testing.T, req CredentialRequest) bool {
		t.Helper()
		ds := newFakeSource(testSandboxes()...)
		ds.requests = []CredentialRequest{req}
		ds.projectSecrets = []Secret{{ID: "sec_gh", Name: "GitHub token", Type: "bearer", Host: "api.github.com",
			RefreshCommand: []string{"gh", "auth", "token"}}}
		m := newTestModel(t, ds)
		send(t, m, keyPress("tab"), keyPress(credentialsKey))
		if m.dialog == nil {
			t.Fatal("no dialog")
		}
		for _, item := range m.dialog.items {
			if item.key == "auto" || item.press == autoApproveKey {
				return true
			}
		}
		return false
	}

	if !offers(t, policyRequest()) {
		t.Fatal("an agent's request asking for a policy does not offer auto approve")
	}
	if offers(t, waitingRequest()) {
		t.Fatal("an agent's request of uses alone offers auto approve")
	}
	reactive := policyRequest()
	reactive.Uses, reactive.Justification = nil, ""
	if offers(t, reactive) {
		t.Fatal("a request the proxy opened, naming no uses, offers auto approve")
	}
	refresh := policyRequest()
	refresh.Name, refresh.Refresh = "GitHub token", &RefreshAsk{SecretID: "sec_gh", Cause: "stale"}
	if offers(t, refresh) {
		t.Fatal("a refresh request offers auto approve")
	}
	if offers(t, waitingTrust()) {
		t.Fatal("a trust request offers auto approve")
	}
}

// A rule answers with a credential the project holds, and with none stored
// its first step offers to store one: the request card's own new-credential
// form, filled from the request, stored when it is saved — the form says so —
// and the flow goes on with it chosen. Esc from the form stores nothing.
func TestAutoApproveCanStoreANewCredential(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	req := policyRequest()
	req.WellKnownID = wellknown.GitHubAPI
	ds.requests = []CredentialRequest{req}
	ds.proposals = proposalsForTheRequest()
	m := newTestModel(t, ds)

	send(t, m, keyPress("tab"), keyPress(credentialsKey), keyPress(autoApproveKey))
	if m.dialog == nil || m.dialog.title != "Auto approve" || len(m.dialog.items) != 1 || m.dialog.items[0].key != "new" {
		t.Fatalf("dialog = %s, want the new credential offered alone", describe(m.dialog))
	}
	drain(t, m, m.dialog.action("new"), 0)
	if !onNewCredentialForm(m) || m.dialog.form.chosen("kind") != wellKnownKind+wellknown.GitHubAPI ||
		!strings.Contains(cardWords(m), "when this form is saved") {
		t.Fatalf("dialog = %s, want the filled form, saying when it is stored:\n%s", describe(m.dialog), wholeCard(m))
	}
	send(t, m, keyPress("esc"))
	if m.dialog == nil || m.dialog.title != "Auto approve" || len(ds.createdSecrets) != 0 {
		t.Fatalf("dialog = %s, created %d; want the secret step back and nothing stored", describe(m.dialog), len(ds.createdSecrets))
	}
	drain(t, m, m.dialog.action("new"), 0)
	send(t, m, keyPress("enter"))
	if len(ds.createdSecrets) != 1 || ds.createdSecrets[0].WellKnownID != wellknown.GitHubAPI {
		t.Fatalf("created = %#v, want the well-known credential stored, marked for its ID", ds.createdSecrets)
	}
	if !onRuleStep(m, "which discoboxes") {
		t.Fatalf("dialog = %s, want the flow gone on to the rule", describe(m.dialog))
	}
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	drain(t, m, m.dialog.action(""), 0)
	grantFor(t, m, time.Hour)
	if card := cardWords(m); !strings.Contains(card, "answered with github") && !strings.Contains(card, "answered with GitHub") {
		t.Fatalf("card = %q, want the rule answered with the new secret", card)
	}
	send(t, m, keyPress("y"))
	if len(ds.approvals) != 1 || ds.approvals[0].SecretID != "sec_new" || ds.approvals[0].Rule == nil {
		t.Fatalf("approvals = %#v, want the rule made with the secret just stored", ds.approvals)
	}
}

// Proposing fails for reasons a person can act on — the request was answered
// elsewhere, the judge is down — so the failure is said, and the request is
// one Esc away, still answerable without a rule.
func TestAFailureToProposeIsShownAndGoesBack(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithPolicyRequest(t)
	ds.proposalsErr = errors.New("the project's judge did not answer")

	send(t, m, keyPress("tab"), keyPress(credentialsKey), keyPress(autoApproveKey))
	drain(t, m, m.dialog.action("secret:sec_gh"), 0)
	if m.dialog == nil || !m.dialog.err || !strings.Contains(wholeCard(m), "judge did not answer") {
		t.Fatalf("dialog = %s, want the failure said", describe(m.dialog))
	}
	send(t, m, keyPress("esc"))
	if m.dialog == nil || m.dialog.title != "Auto approve" {
		t.Fatalf("dialog = %s, want the step before it back", describe(m.dialog))
	}
}

// Proposing takes a minute or two, and a wait nobody can leave is a trap. Esc
// leaves it for the step before — and the proposals, when they land after
// that, raise nothing: a dialog that opens on its own is the thing the inbox
// never does.
func TestEscLeavesTheWaitForProposalsAndTheirArrivalRaisesNothing(t *testing.T) {
	t.Parallel()
	m, _ := sourceWithProposals(t)

	send(t, m, keyPress("tab"), keyPress(credentialsKey), keyPress(autoApproveKey))
	// The fetch is started and not run: the answer is still out.
	fetch := m.dialog.action("secret:sec_gh")
	wait := m.dialog
	if wait == nil || wait.kind != dlgStatus {
		t.Fatalf("dialog = %s, want the wait shown while the server is asked", describe(wait))
	}
	send(t, m, keyPress("esc"))
	if m.dialog == nil || m.dialog.title != "Auto approve" || m.dialog.kind != dlgActions {
		t.Fatalf("dialog = %s, want the step before the wait", describe(m.dialog))
	}
	left := m.dialog
	drain(t, m, fetch, 0)
	if m.dialog != left {
		t.Fatalf("dialog = %s, want the late proposals to raise nothing", describe(m.dialog))
	}
}

// A gate — the discobox API — and a delegation were a person's alone to
// approve, and a rule is that approval given in advance. So a rule for one
// says what it hands every discobox it approves, in the words the request's
// own card uses, and it names no secret, as approving a gate does not.
func TestARuleForAGateSaysWhatItHandsOut(t *testing.T) {
	t.Parallel()
	req := policyRequest()
	req.Name, req.EnvVar, req.Host, req.WellKnownID = "discobox", "DISCOBOX_TOKEN", "api.discobox.internal", wellknown.DiscoboxSandbox
	ds := newFakeSource(testSandboxes()...)
	ds.requests = []CredentialRequest{req}
	ds.proposals = RuleProposals{
		Credential: wellknown.DiscoboxSandbox,
		Hosts:      []string{"api.discobox.internal"},
		Repository: "github.com/acme/pool",
		Written:    &WrittenPolicy{Policy: req.Policy, Stored: req.Policy, Covers: true},
	}
	m := newTestModel(t, ds)

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	// Approve keeps its key; the rule is a shift away from it.
	if items := m.dialog.items; len(items) != 3 || items[0].press != "a" || items[1].key != "auto" || items[2].key != "deny" {
		t.Fatalf("items = %+v, want approve, auto approve and deny", items)
	}
	send(t, m, keyPress(autoApproveKey))
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	drain(t, m, m.dialog.action(""), 0)
	grantFor(t, m, time.Hour)

	card := cardWords(m)
	for _, want := range []string{wellknown.DiscoboxSandbox, "no secret", "it may create discoboxes, and hand them credentials", "but only within what you separately delegate to it"} {
		if !strings.Contains(card, want) {
			t.Fatalf("card = %q, want it to say %q", card, want)
		}
	}
	send(t, m, keyPress("y"))
	if len(ds.approvals) != 1 || ds.approvals[0].SecretID != "" || ds.approvals[0].Rule == nil {
		t.Fatalf("approvals = %#v, want one naming no secret and carrying the rule", ds.approvals)
	}
}

// A request reaches a person because no rule answered it. Which rules were
// tried, and what each fell short on, is what says whether to answer it once
// or to widen a rule — so the card lists them, briefly.
func TestTheRequestCardListsTheRulesThatDidNotAnswerIt(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	req := waitingRequest()
	req.RuleTests = []RuleTest{
		{RuleID: "rule_triage", Stage: "facts", Reason: "the rule is for discoboxes of github.com/acme/other, and this discobox is of github.com/acme/pool"},
		{RuleID: "rule_deliver", Stage: "content", Reason: "a push to main is not within its uses"},
		{RuleID: "rule_new", Pending: true},
	}
	ds.requests = []CredentialRequest{req}
	ds.projectSecrets = []Secret{{ID: "sec_gh", Name: "GitHub token", Type: "bearer", Host: "api.github.com"}}
	m := newTestModel(t, ds)

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	card := cardWords(m)
	for _, want := range []string{
		"rules that did not answer it",
		"rule_triage · facts · the rule is for discoboxes of github.com/acme/other, and this discobox is of github.com/acme/pool",
		"rule_deliver · content · a push to main is not within its uses",
		"rule_new · still being judged",
	} {
		if !strings.Contains(card, want) {
			t.Fatalf("card = %q, want it to carry %q", card, want)
		}
	}

	// A request no rule was tested for says nothing of rules.
	plain, _ := sourceWithRequest(t)
	send(t, plain, keyPress("tab"), keyPress(credentialsKey))
	if strings.Contains(wholeCard(plain), "rules that did not answer it") {
		t.Fatal("a request with no rule tests has a section about them")
	}
}

// Every discobox is never what a rule that names no repository is taken to
// mean. A rule nobody has said the discoboxes of is drawn as not chosen, on a
// card and in a row, and only the row that says "every discobox" sets it.
func TestARuleWithNoRepositoryIsNotTakenToBeForEveryDiscobox(t *testing.T) {
	t.Parallel()
	unsaid := ApprovalRule{Policy: askedPolicy()}
	sec := ruleDiscoboxesSection(unsaid)
	if len(sec.lines) != 1 || sec.lines[0].text != ruleForNothingYet {
		t.Fatalf("section = %+v, want a rule that says neither drawn as not chosen", sec.lines)
	}
	if row := ruleRowDiscoboxes(Rule{ApprovalRule: unsaid}); strings.Contains(row, "every discobox") {
		t.Fatalf("row = %q, want no every discobox on a rule that did not say it", row)
	}
	said := ruleDiscoboxesSection(ApprovalRule{EveryDiscobox: true})
	if len(said.lines) != 2 || said.lines[0].text != everyDiscoboxName || said.lines[1].text != everyDiscobox {
		t.Fatalf("section = %+v, want every discobox, and what that means", said.lines)
	}

	// Choosing the repository after having chosen every discobox takes the
	// wider answer back.
	m, ds := sourceWithProposals(t)
	toRuleDiscoboxes(t, m)
	drain(t, m, m.dialog.action(ruleForEvery), 0)
	send(t, m, keyPress("esc"))
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	drain(t, m, m.dialog.action(""), 0)
	grantFor(t, m, time.Hour)
	send(t, m, keyPress("y"))
	if len(ds.approvals) != 1 || ds.approvals[0].Rule == nil || ds.approvals[0].Rule.EveryDiscobox {
		t.Fatalf("approvals = %#v, want a rule for the repository alone", ds.approvals)
	}
}

// A rule made from the secrets screen is in its Rules table once the approval
// comes back: the screen is read again, and only when it is what is behind
// the card.
func TestARuleMadeOverTheSecretsScreenIsReadIntoItsTable(t *testing.T) {
	t.Parallel()
	for _, open := range []bool{true, false} {
		m, ds := sourceWithProposals(t)
		toRuleDiscoboxes(t, m)
		drain(t, m, m.dialog.action(ruleForRepository), 0)
		drain(t, m, m.dialog.action(""), 0)
		grantFor(t, m, time.Hour)
		m.secretsOpen = open
		reads := func() (n int) {
			for _, call := range ds.calls() {
				if strings.HasPrefix(call, "Rules@") {
					n++
				}
			}
			return n
		}
		before := reads()
		send(t, m, keyPress("y"))
		if got := reads() - before; (got > 0) != open {
			t.Fatalf("secrets screen open = %v: the rules were read %d times after the approval", open, got)
		}
	}
}
