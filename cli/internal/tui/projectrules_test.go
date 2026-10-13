package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/wellknown"
)

// A rule answers requests nobody is shown, so the rules a project holds have
// to be readable where its credentials are: listed, read whole with the grants
// each minted, and turned off, deleted, or undone a grant at a time.

// longUse is a use too long for one row of any card, which is the kind a
// model writes.
const longUse = "Open, update, comment on and close pull requests against org/repo from any branch named discobox/<slug>, and never push to main, delete a branch or change the repository's settings"

// rulesFixture is the secrets screen over a project with three rules: one
// answering, one switched off, and one for every discobox whose own expiry has
// passed. The first has minted two grants, one of which has lapsed.
func rulesFixture(t *testing.T) (*Model, *fakeSource) {
	t.Helper()
	m, ds := secretsFixture(t)
	now := time.Now()
	ds.rules = []Rule{
		{
			ID:           "rule_1",
			ApprovalRule: ApprovalRule{Scope: "discoboxes started to deliver an issue in org/repo", Uses: []string{longUse, "Read any issue in org/repo"}},
			WellKnownID:  "com.github.api", SecretID: "sec_gh", Hosts: []string{"github.com", "api.github.com"},
			MaxGrantTTL: 8 * time.Hour, CreatedBy: "user:darren", Created: now.Add(-3 * time.Hour), Enabled: true,
		},
		{
			ID:           "rule_2",
			ApprovalRule: ApprovalRule{SandboxID: "sbx_two", Uses: []string{"Call the OpenAI API"}},
			SecretID:     "sec_loose", Hosts: []string{"api.openai.com"}, Created: now.Add(-time.Hour),
		},
		{
			ID:           "rule_3",
			ApprovalRule: ApprovalRule{Uses: []string{"Read any issue in org/repo"}},
			WellKnownID:  "com.github.api", Hosts: []string{"github.com"},
			Expires: now.Add(-time.Hour), Created: now.Add(-48 * time.Hour), Enabled: true,
		},
	}
	ds.projectGrants = append(ds.projectGrants,
		Grant{ID: "grant_r1", SecretID: "sec_gh", Scope: "sandbox", ScopeKey: "sbx_one", Host: "api.github.com",
			RuleID: "rule_1", Expires: now.Add(2 * time.Hour)},
		Grant{ID: "grant_old", SecretID: "sec_gh", Scope: "sandbox", ScopeKey: "sbx_two", Host: "api.github.com",
			RuleID: "rule_1", Expires: now.Add(-time.Minute)},
	)
	return m, ds
}

// openRules presses the screen's key and lands on the list.
func openRules(t *testing.T, m *Model) {
	t.Helper()
	send(t, m, keyPress(rulesKey))
	if m.dialog == nil || m.dialog.title != "Approval rules" || m.dialog.kind != dlgActions {
		t.Fatalf("dialog = %s, want the project's rules", describe(m.dialog))
	}
}

// openRule goes on from the list to one rule's card.
func openRule(t *testing.T, m *Model, ruleID string) {
	t.Helper()
	openRules(t, m)
	drain(t, m, m.dialog.action(ruleID), 0)
	if m.dialog == nil || m.dialog.title != "Approval rule" || m.dialog.titleRight != ruleID {
		t.Fatalf("dialog = %s, want the card of %s", describe(m.dialog), ruleID)
	}
}

func itemKeys(d *dialog) []string {
	keys := make([]string, 0, len(d.items))
	for _, it := range d.items {
		keys = append(keys, it.key)
	}
	return keys
}

// The rules are reached from the screen that owns credentials, and a way in
// nobody is told about is one nobody finds: it is on the key line over either
// table, where it is a button, and in the help.
func TestTheRulesAreOfferedOnTheSecretsScreen(t *testing.T) {
	t.Parallel()
	m, ds := rulesFixture(t)
	ds.requests = []CredentialRequest{{ID: "req_1", SandboxID: "sbx_one", Name: "gh", Host: "github.com"}}

	for _, onRequests := range []bool{false, true} {
		m.onRequests = onRequests
		offered := false
		for _, h := range m.secretHints() {
			if h.text == rulesKey+" rules" && slices.Equal(h.keys, []string{rulesKey}) {
				offered = true
			}
		}
		if !offered {
			t.Fatalf("key line (requests table: %t) = %q, want it to offer the rules on their key", onRequests, hintLine(m.secretHints()))
		}
		openRules(t, m)
		send(t, m, keyPress("esc"))
		if m.dialog != nil || !m.secretsOpen {
			t.Fatalf("esc left %s over the screen, want the screen back", describe(m.dialog))
		}
	}
	if !strings.Contains(m.helpText(), rulesKey+"              the approval rules") {
		t.Fatal("the help does not list the key that opens the rules")
	}
}

// A row is how one rule is told from the next without opening it: whether it
// is answering, for which credential, and for which discoboxes — the sentence
// whole, since two rules for one credential differ by nothing else.
func TestTheListShowsEachRulesStateCredentialAndScope(t *testing.T) {
	t.Parallel()
	m, _ := rulesFixture(t)
	openRules(t, m)

	if got := itemKeys(m.dialog); !slices.Equal(got, []string{"rule_1", "rule_2", "rule_3"}) {
		t.Fatalf("rows = %v, want one a rule", got)
	}
	card := cardWords(m)
	for _, want := range []string{
		"3 rules",
		"com.github.api", "enabled · discoboxes started to deliver an issue in org/repo · 2 uses",
		// A rule for a project secret is named by the secret, not its ID.
		"OpenAI key", "disabled · pinned to sbx_two by its ID; nothing is read · 1 use",
		"lapsed · every discobox in this project",
	} {
		if !strings.Contains(card, want) {
			t.Fatalf("list does not carry %q:\n%s", want, wholeCard(m))
		}
	}
}

// An empty menu looks like a listing that did not arrive. A project with no
// rules says so, and says the two ways one is made, since neither is on this
// screen.
func TestAProjectWithNoRulesSaysSoAndHowOneIsMade(t *testing.T) {
	t.Parallel()
	m, ds := rulesFixture(t)
	ds.rules = nil

	send(t, m, keyPress(rulesKey))
	if m.dialog == nil || m.dialog.title != "Approval rules" {
		t.Fatalf("dialog = %s, want the rules card", describe(m.dialog))
	}
	card := cardWords(m)
	for _, want := range []string{"no approval rules", "auto approve", "discobox secret rule create"} {
		if !strings.Contains(card, want) {
			t.Fatalf("card does not carry %q:\n%s", want, wholeCard(m))
		}
	}
}

// A rule's text is the authorization, so its card is the whole of it: every
// use to its last word, what the server holds around them, and the grants it
// has minted that still authorize something — each by the discobox it is for.
func TestARulesCardIsTheRuleWholeWithTheGrantsItMinted(t *testing.T) {
	t.Parallel()
	m, _ := rulesFixture(t)
	openRule(t, m, "rule_1")

	card := cardWords(m)
	for _, want := range []string{
		"state enabled", "credential com.github.api", "answered with gh",
		"github.com, api.github.com", "purpose use", "at most 8 hours",
		"expires never", "made by user:darren", "3h ago",
		"discoboxes started to deliver an issue in org/repo",
		longUse, "Read any issue in org/repo",
		"1 grant it minted still stands",
		"fix flaky pool reaper tests · sandbox sbx_one api.github.com · expires in",
		// What this card does not do, and where it is done.
		"discobox secret rule update rule_1",
	} {
		if !strings.Contains(card, want) {
			t.Fatalf("card does not carry %q:\n%s", want, wholeCard(m))
		}
	}
	// A grant that has lapsed authorizes nothing, and one another rule or a
	// person made is not this rule's.
	if got := itemKeys(m.dialog); !slices.Equal(got, []string{ruleToggleItem, ruleDeleteItem, "grant_r1"}) {
		t.Fatalf("rows = %v, want the two actions and the one grant still standing", got)
	}

	// Enter on a grant reads it, and leaving it comes back to the rule.
	drain(t, m, m.dialog.action("grant_r1"), 0)
	if m.dialog == nil || m.dialog.kind != dlgText || !strings.Contains(dialogText(m), "approval rule rule_1") {
		t.Fatalf("dialog = %s, want the grant read, naming the rule that minted it:\n%s", describe(m.dialog), dialogText(m))
	}
	send(t, m, keyPress("esc"))
	if m.dialog == nil || m.dialog.titleRight != "rule_1" {
		t.Fatalf("dialog = %s, want the rule back", describe(m.dialog))
	}
	// And leaving the rule comes back to the list, on the rule that was read.
	send(t, m, keyPress("esc"))
	if m.dialog == nil || m.dialog.title != "Approval rules" || m.dialog.items[m.dialog.cursor].key != "rule_1" {
		t.Fatalf("dialog = %s, want the list back on the rule it was opened from", describe(m.dialog))
	}
}

// Disabling is an edit of one field: a write that sent the rule back would
// have its sentences judged again, or replaced by what this window happened to
// hold. The card says what turning it off does not do, and comes back showing
// the rule as the server now has it.
func TestDisablingAndEnablingSendOnlyThatAndComeBackToTheRule(t *testing.T) {
	t.Parallel()
	m, ds := rulesFixture(t)
	openRule(t, m, "rule_1")

	if !strings.Contains(cardWords(m), "Disable this rule it mints nothing more, and revokes nothing") {
		t.Fatalf("the row does not say what disabling does:\n%s", wholeCard(m))
	}
	send(t, m, keyPress("s"))
	if !slices.Equal(ds.ruleEdits, []string{"rule_1 enabled=false"}) {
		t.Fatalf("edits = %v, want the one rule switched off", ds.ruleEdits)
	}
	if len(ds.revoked) != 0 || len(ds.deletedRules) != 0 {
		t.Fatalf("disabling revoked %v and deleted %v, want neither", ds.revoked, ds.deletedRules)
	}
	if m.dialog == nil || m.dialog.titleRight != "rule_1" || !strings.Contains(cardWords(m), "state disabled") {
		t.Fatalf("dialog = %s, want the rule's card back, saying it is off:\n%s", describe(m.dialog), wholeCard(m))
	}
	// Its grant still stands, and the same key turns it back on.
	if !slices.Contains(itemKeys(m.dialog), "grant_r1") || m.dialog.items[0].label != "Enable this rule" {
		t.Fatalf("rows = %v (first %q), want the grant kept and the way back on", itemKeys(m.dialog), m.dialog.items[0].label)
	}
	send(t, m, keyPress("s"))
	if !slices.Equal(ds.ruleEdits, []string{"rule_1 enabled=false", "rule_1 enabled=true"}) || !strings.Contains(cardWords(m), "state enabled") {
		t.Fatalf("edits = %v, want it switched back on and the card to say so:\n%s", ds.ruleEdits, wholeCard(m))
	}
}

// Deleting a rule looks like taking back what it approved, and is not: the
// grants it minted stand on their own. The question says so before anything
// goes, and its default is the answer that can be taken back.
func TestDeletingARuleAsksFirstAndSaysItsGrantsStay(t *testing.T) {
	t.Parallel()
	m, ds := rulesFixture(t)
	openRule(t, m, "rule_1")

	send(t, m, keyPress("d"))
	if m.dialog == nil || m.dialog.kind != dlgConfirm || !m.dialog.defaultNo {
		t.Fatalf("dialog = %s, want a question whose default is no", describe(m.dialog))
	}
	if card := cardWords(m); !strings.Contains(card, "1 grant it minted stays") || !strings.Contains(card, "revoked on its own") {
		t.Fatalf("the question does not say the grants stay:\n%s", wholeCard(m))
	}
	// Enter is No, and No is back on the rule.
	send(t, m, keyPress("enter"))
	if len(ds.deletedRules) != 0 || m.dialog == nil || m.dialog.titleRight != "rule_1" || m.dialog.kind != dlgActions {
		t.Fatalf("deleted = %v, dialog = %s; want nothing deleted and the rule back", ds.deletedRules, describe(m.dialog))
	}

	send(t, m, keyPress("d"), keyPress("y"))
	if !slices.Equal(ds.deletedRules, []string{"rule_1"}) || len(ds.revoked) != 0 {
		t.Fatalf("deleted = %v, revoked = %v; want the rule gone and no grant with it", ds.deletedRules, ds.revoked)
	}
	// It has no card to come back to, so the list is where this lands.
	if m.dialog == nil || m.dialog.title != "Approval rules" || !slices.Equal(itemKeys(m.dialog), []string{"rule_2", "rule_3"}) {
		t.Fatalf("dialog = %s rows %v, want the list without it", describe(m.dialog), itemKeys(m.dialog))
	}
}

// What a rule should not have approved is undone a grant at a time, so the
// grant is withdrawn from the rule's own card — through the question every
// revoke asks — and the card comes back without it.
func TestAGrantIsRevokedFromTheRuleThatMintedIt(t *testing.T) {
	t.Parallel()
	m, ds := rulesFixture(t)
	openRule(t, m, "rule_1")

	// The key does nothing on a row that is no grant, and the card stays.
	send(t, m, keyPress(grantRevokeKey))
	if m.dialog == nil || m.dialog.kind != dlgActions || m.dialog.titleRight != "rule_1" {
		t.Fatalf("dialog = %s, want the rule's card still up", describe(m.dialog))
	}

	send(t, m, keyPress("down"), keyPress("down"), keyPress(grantRevokeKey))
	if m.dialog == nil || m.dialog.kind != dlgConfirm || m.dialog.titleRight != "grant_r1" || !m.dialog.defaultNo {
		t.Fatalf("dialog = %s, want the revoke question for the grant, defaulting to no", describe(m.dialog))
	}
	if !strings.Contains(cardWords(m), "the credential stops resolving at once") {
		t.Fatalf("the question does not say what stops:\n%s", wholeCard(m))
	}
	send(t, m, keyPress("y"))
	if !slices.Equal(ds.revoked, []string{"grant_r1"}) {
		t.Fatalf("revoked = %v, want the grant chosen", ds.revoked)
	}
	if m.dialog == nil || m.dialog.titleRight != "rule_1" || slices.Contains(itemKeys(m.dialog), "grant_r1") {
		t.Fatalf("dialog = %s rows %v, want the rule back without the grant", describe(m.dialog), itemKeys(m.dialog))
	}
	if !strings.Contains(cardWords(m), "no grant it minted still stands") {
		t.Fatalf("the card does not say nothing stands:\n%s", wholeCard(m))
	}
	// The screen behind it re-read too: its count is the server's.
	if _, standing := m.secrets.grant("grant_r1"); standing {
		t.Fatal("the secrets screen still lists the revoked grant")
	}
}

// A rule with no scope and no pin approves for discoboxes that do not exist
// yet. That is the widest thing on this screen, so it is said in the words the
// decision to allow it was recorded in, on its row and on its card.
func TestARuleForEveryDiscoboxSaysWhatThatMeans(t *testing.T) {
	t.Parallel()
	m, _ := rulesFixture(t)
	openRules(t, m)
	if !strings.Contains(cardWords(m), "every discobox in this project; "+everyDiscobox) {
		t.Fatalf("its row does not say what every discobox means:\n%s", wholeCard(m))
	}
	drain(t, m, m.dialog.action("rule_3"), 0)
	card := cardWords(m)
	for _, want := range []string{"every discobox in this project", everyDiscobox, "state lapsed", "expires lapsed 1h ago", "the secret's own limit"} {
		if !strings.Contains(card, want) {
			t.Fatalf("card does not carry %q:\n%s", want, wholeCard(m))
		}
	}
}

// A rule for a gate or a delegation approves, in advance, what only a person
// could: its card says what it hands out in the words the card that made it
// did.
func TestAStoredRuleSaysWhatItHandsOut(t *testing.T) {
	t.Parallel()
	var gate wellknown.Credential
	for _, known := range wellknown.All() {
		if known.Gate {
			gate = known
		}
	}
	if gate.ID == "" {
		t.Skip("no gate credential is registered")
	}
	m, ds := rulesFixture(t)
	ds.rules = []Rule{{ID: "rule_gate", WellKnownID: gate.ID, Delegate: true, Enabled: true,
		ApprovalRule: ApprovalRule{SandboxID: "sbx_one", Uses: []string{"Create discoboxes to deliver issues"}}}}
	openRule(t, m, "rule_gate")

	card := cardWords(m)
	for _, want := range []string{gateAnswers, "it may create discoboxes, and hand them credentials", delegationPurpose, "uses it would delegate"} {
		if !strings.Contains(card, want) {
			t.Fatalf("card does not carry %q:\n%s", want, wholeCard(m))
		}
	}
}

// A refusal is the answer, and one that flashed past on the status line under
// a card would read as nothing having happened: a failed read and a failed
// change are each shown, and nothing is drawn as though it had worked.
func TestAFailedReadOrChangeOfTheRulesIsShown(t *testing.T) {
	t.Parallel()
	m, ds := rulesFixture(t)

	ds.rulesErr = errors.New("only a person may read approval rules")
	send(t, m, keyPress(rulesKey))
	if m.dialog == nil || !m.dialog.err || !strings.Contains(dialogText(m), "only a person may read approval rules") {
		t.Fatalf("dialog = %s, want the failure shown", describe(m.dialog))
	}
	send(t, m, keyPress("esc"))
	ds.rulesErr = nil

	// The grants a rule minted are half of its card: without them it is not
	// put up as though it had none.
	openRules(t, m)
	ds.grantsErr = errors.New("grants unavailable")
	drain(t, m, m.dialog.action("rule_1"), 0)
	if m.dialog == nil || !m.dialog.err || !strings.Contains(dialogText(m), "grants unavailable") {
		t.Fatalf("dialog = %s, want the failure shown", describe(m.dialog))
	}
	send(t, m, keyPress("esc"))
	ds.grantsErr = nil

	openRule(t, m, "rule_1")
	ds.ruleEditErr = errors.New("the server refused the edit")
	send(t, m, keyPress("s"))
	if m.dialog == nil || !m.dialog.err || !strings.Contains(dialogText(m), "the server refused the edit") {
		t.Fatalf("dialog = %s, want the refusal shown", describe(m.dialog))
	}
	// Leaving it goes back to the rule, still as it was.
	send(t, m, keyPress("esc"))
	if m.dialog == nil || m.dialog.titleRight != "rule_1" || !strings.Contains(cardWords(m), "state enabled") {
		t.Fatalf("dialog = %s, want the rule back unchanged", describe(m.dialog))
	}

	ds.deleteRuleErr = errors.New("the server refused the delete")
	send(t, m, keyPress("d"), keyPress("y"))
	if m.dialog == nil || !m.dialog.err || !strings.Contains(dialogText(m), "the server refused the delete") {
		t.Fatalf("dialog = %s, want the refusal shown", describe(m.dialog))
	}
	if len(ds.deletedRules) != 0 {
		t.Fatalf("deleted = %v, want nothing", ds.deletedRules)
	}
}

// The rules are a server's, like the secrets under them: a window showing
// several asks the one in its header, and changes a rule where it read it.
func TestTheRulesAreReadAndChangedOnTheScreensServer(t *testing.T) {
	t.Parallel()
	m, ds := rulesFixture(t)
	openRule(t, m, "rule_1")
	send(t, m, keyPress("s"))

	server := m.configServer()
	calls := ds.calls()
	for _, want := range []string{"Rules@" + server, "SetRuleEnabled@" + server} {
		if !slices.Contains(calls, want) {
			t.Fatalf("calls = %v, want %s", calls, want)
		}
	}
}
