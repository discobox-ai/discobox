package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/discobox-ai/discobox/wellknown"
)

// "Auto approve" makes a standing answer out of one request, and most of its
// text is a model's reading of what the agent wrote. These tests are about the
// things that keep that from being authority nobody read: what can be chosen,
// what is shown before the write, and what is sent.

// errRuleRefused is the server declining to make a rule that would not cover
// the request it is made from.
var errRuleRefused = errors.New("this rule would not cover the request it is made from, so it is not made: its uses do not hold what the request asked for (a push to main is not within these); choose a proposal the judge found to cover it, or approve without a rule")

// proposalsForTheRequest is what a server with a judge offers for
// waitingRequest: the repository of the discobox that asked, and three
// contents the judge found to cover the request.
func proposalsForTheRequest() RuleProposals {
	return RuleProposals{
		Credential: "github",
		Hosts:      []string{"api.github.com"},
		Repository: "github.com/acme/pool",
		Contents: []RuleContentProposal{
			{Level: 1, Uses: []string{"Open a pull request against the current repo"}, Covers: true},
			{Level: 2, Uses: []string{"Open a pull request against acme/pool from any discobox/<slug> branch"}, Covers: true},
			{Level: 3, Uses: []string{"Deliver pull requests to acme/pool from any branch but main"}, Covers: true},
		},
	}
}

func sourceWithProposals(t *testing.T) (*Model, *fakeSource) {
	t.Helper()
	m, ds := sourceWithRequest(t)
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
// makes the rule: the repository, the uses, and the lifetime — and nothing
// about the credential, which the server copies from the request.
func TestAutoApproveSendsTheChosenRepositoryAndUsesWithTheApproval(t *testing.T) {
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
	drain(t, m, m.dialog.action("content:2"), 0)
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
	if want := []string{"Open a pull request against acme/pool from any discobox/<slug> branch"}; strings.Join(got.Rule.Uses, "\n") != strings.Join(want, "\n") {
		t.Fatalf("uses = %q, want %q", got.Rule.Uses, want)
	}
	if m.dialog != nil {
		t.Fatalf("dialog = %s, want it closed once answered", describe(m.dialog))
	}
}

// Which discoboxes a rule is for has two answers and no sentence: the
// repository of the discobox that asked, first and under the cursor, and
// every discobox in the project. Nothing here is a model's writing, and
// nothing is written by hand.
func TestWhichDiscoboxesIsTheRepositoryOrEveryDiscobox(t *testing.T) {
	t.Parallel()
	m, _ := sourceWithProposals(t)

	toRuleDiscoboxes(t, m)
	items := m.dialog.items
	if len(items) != 2 || items[0].key != ruleForRepository || items[1].key != ruleForEvery || !items[0].enabled || !items[1].enabled {
		t.Fatalf("items = %+v, want the repository, then every discobox", items)
	}
	if m.dialog.cursor != 0 {
		t.Fatalf("cursor = %d, want it on the narrower answer", m.dialog.cursor)
	}
	card := cardWords(m)
	for _, want := range []string{"Discoboxes of github.com/acme/pool", "whose primary source is this repository", "Every discobox in this project", everyDiscobox} {
		if !strings.Contains(card, want) {
			t.Fatalf("card = %q, want it to carry %q", card, want)
		}
	}
	for _, gone := range []string{"by hand", "pinned", "This discobox"} {
		if strings.Contains(card, gone) {
			t.Fatalf("card = %q, want nothing about %q", card, gone)
		}
	}
	// No key writes one: there is nothing to write.
	step := m.dialog
	send(t, m, keyPress("w"))
	if m.dialog != step {
		t.Fatalf("dialog = %s, want w to do nothing on this step", describe(m.dialog))
	}
}

// A discobox started from a local directory with no remote has no repository
// a rule could name. Only a rule for every discobox answers it, so that is
// the one row, and the card says why there is no narrower one.
func TestADiscoboxWithNoRepositoryIsOfferedOnlyEveryDiscobox(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	proposals := proposalsForTheRequest()
	proposals.Repository = ""
	ds.proposals = proposals

	toRuleDiscoboxes(t, m)
	if items := m.dialog.items; len(items) != 1 || items[0].key != ruleForEvery {
		t.Fatalf("items = %+v, want every discobox alone", items)
	}
	if card := cardWords(m); !strings.Contains(card, "no repository a rule could name") || !strings.Contains(card, everyDiscobox) {
		t.Fatalf("card = %q, want it to say why there is no narrower rule, and what this one does", card)
	}
}

// A proposal the judge did not find to cover this very request would be a rule
// that does not approve the ask it was made from. It is shown, with the
// judge's reason — that is what says how to write a better one — and it cannot
// be chosen by its row, by the pointer, or by its key.
func TestFailedUsesAreShownWithTheirReasonAndCannotBeChosen(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	proposals := proposalsForTheRequest()
	proposals.Contents[2].Covers, proposals.Contents[2].Reason = false, "opening a pull request is not delivering one"
	ds.proposals = proposals

	toRuleDiscoboxes(t, m)
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	card := wholeCard(m)
	for _, want := range []string{"As asked", "The same work on any target of its kind", "The same kind of work across the resource",
		"Deliver pull requests to acme/pool from any branch but main", "failed, and cannot be chosen", "opening a pull request is not delivering one"} {
		if !strings.Contains(card, want) {
			t.Fatalf("card = %q, want it to carry %q", card, want)
		}
	}
	var failed action
	for _, item := range m.dialog.items {
		if item.key == "content:3" {
			failed = item
		}
	}
	if failed.key == "" || failed.enabled {
		t.Fatalf("row = %+v, want the failed uses listed and not choosable", failed)
	}
	content := m.dialog
	drain(t, m, content.action("content:3"), 0)
	if m.dialog != content {
		t.Fatalf("dialog = %s, want the failed uses to answer nothing", describe(m.dialog))
	}
	// The cursor walks past it.
	send(t, m, keyPress("down"), keyPress("down"))
	if at := m.dialog.items[m.dialog.cursor]; at.key == "content:3" {
		t.Fatal("the cursor rests on uses that cannot be chosen")
	}
	if len(ds.approvals) != 0 {
		t.Fatal("something was approved")
	}
}

// Uses nobody proposed are the person's own words, and they are the rule:
// they go to the server as written, where the judge reads them before the
// rule is made.
func TestHandWrittenTextIsSentAsWritten(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithProposals(t)

	toRuleDiscoboxes(t, m)
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	send(t, m, keyPress("w"))
	if m.dialog == nil || m.dialog.kind != dlgInput {
		t.Fatalf("dialog = %s, want a field to write the uses in", describe(m.dialog))
	}
	// One use a line; an empty line is the end of them.
	drain(t, m, m.dialog.action("Open pull requests against acme/pool"), 0)
	drain(t, m, m.dialog.action("Never push to main"), 0)
	if card := wholeCard(m); !strings.Contains(card, "Open pull requests against acme/pool") || !strings.Contains(card, "Never push to main") {
		t.Fatalf("card = %q, want the uses written so far", card)
	}
	drain(t, m, m.dialog.action(""), 0)
	grantFor(t, m, time.Hour)
	send(t, m, keyPress("y"))

	if len(ds.approvals) != 1 || ds.approvals[0].Rule == nil {
		t.Fatalf("approvals = %#v, want one carrying a rule", ds.approvals)
	}
	rule := ds.approvals[0].Rule
	if len(rule.Repositories) != 1 || rule.Repositories[0] != "github.com/acme/pool" {
		t.Fatalf("rule = %#v, want the repository chosen", rule)
	}
	if strings.Join(rule.Uses, "\n") != "Open pull requests against acme/pool\nNever push to main" {
		t.Fatalf("uses = %q, want the two lines as written, in order", rule.Uses)
	}
}

// A rule that approves nothing in particular is not one this window makes: an
// empty line with no use written is refused where it was typed.
func TestNoUsesWrittenIsNotARule(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithProposals(t)

	toRuleDiscoboxes(t, m)
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	send(t, m, keyPress("w"))
	drain(t, m, m.dialog.action(""), 0)

	if m.dialog == nil || m.dialog.kind != dlgInput || !strings.Contains(wholeCard(m), "write at least one use") {
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
	drain(t, m, m.dialog.action("content:3"), 0)
	grantFor(t, m, 24*time.Hour)

	card := wholeCard(m)
	for _, want := range []string{
		"github", "GitHub token", "api.github.com", "purpose", "use",
		"discoboxes of github.com/acme/pool",
		"Deliver pull requests to acme/pool from any branch but main",
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
	drain(t, m, m.dialog.action("content:1"), 0)
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
	drain(t, m, m.dialog.action("content:2"), 0)
	grantFor(t, m, time.Hour)

	send(t, m, keyPress("esc"))
	if !onLifetimeStep(m) {
		t.Fatalf("dialog = %s, want the lifetime back from the rule", describe(m.dialog))
	}
	send(t, m, keyPress("esc"))
	if !onRuleStep(m, "what it approves") {
		t.Fatalf("dialog = %s, want the uses back from the lifetime", describe(m.dialog))
	}
	send(t, m, keyPress("esc"))
	if !onRuleStep(m, "which discoboxes") {
		t.Fatalf("dialog = %s, want the discoboxes back from the uses", describe(m.dialog))
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

// Esc never changes a word of the uses written by hand: it goes back to the
// menu, and writing them by hand again opens on them as they were left, the
// line half-typed into the field included.
func TestEscKeepsTheUsesWritten(t *testing.T) {
	t.Parallel()
	m, _ := sourceWithProposals(t)

	toRuleDiscoboxes(t, m)
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	send(t, m, keyPress("w"))
	drain(t, m, m.dialog.action("Open pull requests against acme/pool"), 0)
	drain(t, m, m.dialog.action("Merge them"), 0)
	m.dialog.input.SetValue("Comment on th")
	send(t, m, keyPress("esc"))
	if !onRuleStep(m, "what it approves") || m.dialog.kind != dlgActions {
		t.Fatalf("dialog = %s, want the menu of uses back", describe(m.dialog))
	}
	send(t, m, keyPress("w"))
	card := wholeCard(m)
	if m.dialog == nil || m.dialog.kind != dlgInput || !strings.Contains(card, "Merge them") ||
		!strings.Contains(card, "Open pull requests against acme/pool") || m.dialog.input.Value() != "Comment on th" {
		t.Fatalf("card = %q, field = %q; want both lines and the half-typed one kept", card, m.dialog.input.Value())
	}
}

// The server judges a rule before it makes it and refuses one that would not
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
	drain(t, m, m.dialog.action("content:1"), 0)
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
		t.Fatalf("dialog = %s, want the way back to other uses", describe(m.dialog))
	}
}

// A rule's content is uses read against uses. A request the proxy opened names
// none, a refresh asks for a value, and a trust is answered with a pin: each
// stays a person's to answer every time, so none of them offers a rule.
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

	if !offers(t, waitingRequest()) {
		t.Fatal("an agent's request naming its uses does not offer auto approve")
	}
	reactive := waitingRequest()
	reactive.Uses, reactive.Justification = nil, ""
	if offers(t, reactive) {
		t.Fatal("a request the proxy opened, naming no uses, offers auto approve")
	}
	refresh := waitingRequest()
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
	req := waitingRequest()
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
	drain(t, m, m.dialog.action("content:1"), 0)
	grantFor(t, m, time.Hour)
	if card := cardWords(m); !strings.Contains(card, "answered with github") && !strings.Contains(card, "answered with GitHub") {
		t.Fatalf("card = %q, want the rule answered with the new secret", card)
	}
	send(t, m, keyPress("y"))
	if len(ds.approvals) != 1 || ds.approvals[0].SecretID != "sec_new" || ds.approvals[0].Rule == nil {
		t.Fatalf("approvals = %#v, want the rule made with the secret just stored", ds.approvals)
	}
}

// Where no model could write wider uses the server offers the uses as asked
// alone and says why. Without that sentence a card with one row reads as the
// window having lost the others. It is about the uses, so it is said there.
func TestTheNoteSaysWhyOnlyTheNarrowestIsOffered(t *testing.T) {
	t.Parallel()
	const note = "only the uses as asked are offered, because no model wrote wider ones"
	m, ds := sourceWithRequest(t)
	proposals := proposalsForTheRequest()
	proposals.Contents, proposals.Note = proposals.Contents[:1], note
	ds.proposals = proposals

	toRuleDiscoboxes(t, m)
	if card := cardWords(m); strings.Contains(card, note) {
		t.Fatalf("discoboxes = %q, want no note about uses where no use is chosen", card)
	}
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	if card := cardWords(m); !strings.Contains(card, note) {
		t.Fatalf("uses = %q, want the note", card)
	}
}

// Proposing fails for reasons a person can act on — the request was answered
// elsewhere, the judge is down — so the failure is said, and the request is
// one Esc away, still answerable without a rule.
func TestAFailureToProposeIsShownAndGoesBack(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
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
	req := waitingRequest()
	req.Name, req.EnvVar, req.Host, req.WellKnownID = "discobox", "DISCOBOX_TOKEN", "api.discobox.internal", wellknown.DiscoboxSandbox
	ds := newFakeSource(testSandboxes()...)
	ds.requests = []CredentialRequest{req}
	ds.proposals = RuleProposals{
		Credential: wellknown.DiscoboxSandbox,
		Hosts:      []string{"api.discobox.internal"},
		Repository: "github.com/acme/pool",
		Contents:   []RuleContentProposal{{Level: 1, Uses: req.Uses, Covers: true}},
	}
	m := newTestModel(t, ds)

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	// Approve keeps its key; the rule is a shift away from it.
	if items := m.dialog.items; len(items) != 3 || items[0].press != "a" || items[1].key != "auto" || items[2].key != "deny" {
		t.Fatalf("items = %+v, want approve, auto approve and deny", items)
	}
	send(t, m, keyPress(autoApproveKey))
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	drain(t, m, m.dialog.action("content:1"), 0)
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

// A rule to delegate mints grants that hand a credential on rather than use
// it, which is not what a reader assumes of a rule; its card says so the way
// a request to delegate does.
func TestARuleToDelegateSaysWhatItIs(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithRequest(t)
	ds.mu.Lock()
	ds.requests[0].Delegate = true
	ds.mu.Unlock()
	proposals := proposalsForTheRequest()
	proposals.Delegate = true
	ds.proposals = proposals
	send(t, m, tickMsg{})

	toRuleDiscoboxes(t, m)
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	drain(t, m, m.dialog.action("content:1"), 0)
	grantFor(t, m, time.Hour)

	if card := wholeCard(m); !strings.Contains(card, "may delegate it, never uses it") || !strings.Contains(card, "uses it would delegate") {
		t.Fatalf("card = %q, want it to say the rule delegates", card)
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
	unsaid := ApprovalRule{Uses: []string{"Read any issue in org/repo"}}
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
	drain(t, m, m.dialog.action("content:1"), 0)
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
		drain(t, m, m.dialog.action("content:2"), 0)
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
