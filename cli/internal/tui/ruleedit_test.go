package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// newRuleFixture is the secrets screen with the GitHub secret marked as what
// answers com.github.api, and the keys on the rules table.
func newRuleFixture(t *testing.T) (*Model, *fakeSource) {
	t.Helper()
	m, ds := rulesFixture(t)
	ds.mu.Lock()
	ds.projectSecrets[0].WellKnownID = "com.github.api"
	ds.mu.Unlock()
	send(t, m, keyPress("r"))
	onRule(t, m, "rule_1")
	return m, ds
}

// onRuleCard is whether the card up is the given step of making or editing.
func onRuleCard(m *Model, title string) bool { return m.dialog != nil && m.dialog.title == title }

func mustBeOn(t *testing.T, m *Model, title string) {
	t.Helper()
	if !onRuleCard(m, title) {
		t.Fatalf("dialog = %s, want %q", describe(m.dialog), title)
	}
}

// typeLines answers a list step a line at a time, and finishes it.
func typeLines(t *testing.T, m *Model, lines ...string) {
	t.Helper()
	for _, l := range append(lines, "") {
		drain(t, m, m.dialog.action(l), 0)
	}
}

// A new rule is made a step at a time — the credential, which discoboxes,
// what it approves, its longest grant, when it lapses — and read whole before
// y makes it. What is sent is what was chosen: the ID alone for a well-known
// credential, the repositories as typed, a line or a comma at a time.
func TestANewRuleIsMadeAStepAtATime(t *testing.T) {
	t.Parallel()
	m, ds := newRuleFixture(t)

	send(t, m, keyPress(ruleNewKey))
	mustBeOn(t, m, "New rule: which credential")
	if card := cardWords(m); !strings.Contains(card, "com.github.api asked for by its ID, and answered with gh") ||
		!strings.Contains(card, "bound to no host") {
		t.Fatalf("card = %q, want the well-known ID its secret answers and each secret, the unbound one said why", card)
	}
	drain(t, m, m.dialog.action("wk:sec_gh"), 0)

	mustBeOn(t, m, "New rule: which discoboxes")
	if got := itemKeys(m.dialog); !slices.Equal(got, []string{ruleForTyped, ruleForEvery}) || m.dialog.cursor != 0 {
		t.Fatalf("rows = %v at %d, want repositories named first and under the cursor, then every discobox", got, m.dialog.cursor)
	}
	drain(t, m, m.dialog.action(ruleForTyped), 0)
	typeLines(t, m, "github.com/org/repo, https://github.com/org/tools", "git@github.com:org/docs.git")

	mustBeOn(t, m, "New rule: what it approves")
	if card := cardWords(m); !strings.Contains(card, "for github.com/org/repo, github.com/org/tools, git@github.com:org/docs.git") {
		t.Fatalf("card = %q, want the rule so far above the question", card)
	}
	typeLines(t, m, "Read any issue in org/repo", "Comment on any issue in org/repo; never close one")

	mustBeOn(t, m, "New rule: longest grant")
	if at := m.dialog.items[m.dialog.cursor]; at.key != "3600" {
		t.Fatalf("cursor on %q, want an hour, which the server would take", at.key)
	}
	drain(t, m, m.dialog.action("7200"), 0)

	mustBeOn(t, m, "New rule: when it lapses")
	if at := m.dialog.items[m.dialog.cursor]; at.label != "never lapses" {
		t.Fatalf("cursor on %q, want a rule that does not lapse by default", at.label)
	}
	drain(t, m, m.dialog.action("0"), 0)

	mustBeOn(t, m, "New rule: the rule")
	card := cardWords(m)
	for _, want := range []string{"credential com.github.api", "answered with gh", "github.com", "at most 2 hours", "expires never",
		"discoboxes of github.com/org/repo", "discoboxes of github.com/org/tools", "Comment on any issue in org/repo; never close one",
		"make this rule?"} {
		if !strings.Contains(card, want) {
			t.Fatalf("card = %q, want it to carry %q", card, want)
		}
	}
	if !m.dialog.defaultNo {
		t.Fatal("the rule's card makes it on Enter; only y may")
	}
	send(t, m, keyPress("enter"))
	if len(ds.madeRules) != 0 {
		t.Fatal("Enter made the rule")
	}
	mustBeOn(t, m, "New rule: when it lapses")
	drain(t, m, m.dialog.action("0"), 0)
	send(t, m, keyPress("y"))

	if len(ds.madeRules) != 1 {
		t.Fatalf("made = %+v, want one rule", ds.madeRules)
	}
	got := ds.madeRules[0]
	if got.WellKnownID != "com.github.api" || got.SecretID != "" || got.EveryDiscobox ||
		!slices.Equal(got.Repositories, []string{"github.com/org/repo", "github.com/org/tools", "git@github.com:org/docs.git"}) ||
		len(got.Uses) != 2 || got.MaxGrantTTL != 2*time.Hour || !got.Expires.IsZero() {
		t.Fatalf("made = %+v, want what was chosen and nothing else", got)
	}
	if m.dialog != nil || len(m.rules.all) != 4 {
		t.Fatalf("dialog = %s, rules = %d; want the table back, with the new rule in it", describe(m.dialog), len(m.rules.all))
	}
}

// Every discobox is its own answer, sent only when it was chosen: the typed
// list cannot be finished empty, and going back from every discobox to name
// repositories takes the wider answer back.
func TestANewRuleIsForEveryDiscoboxOnlyByChoosingIt(t *testing.T) {
	t.Parallel()
	m, ds := newRuleFixture(t)
	send(t, m, keyPress(ruleNewKey))
	drain(t, m, m.dialog.action("secret:sec_gh"), 0)

	drain(t, m, m.dialog.action(ruleForTyped), 0)
	drain(t, m, m.dialog.action(""), 0)
	if m.dialog == nil || m.dialog.kind != dlgInput || !strings.Contains(m.dialog.emphasis, "name at least one repository") {
		t.Fatalf("dialog = %s, want an empty list refused where it was typed", describe(m.dialog))
	}
	send(t, m, keyPress("esc"))
	mustBeOn(t, m, "New rule: which discoboxes")
	drain(t, m, m.dialog.action(ruleForEvery), 0)
	typeLines(t, m, "Read any issue in org/repo")
	drain(t, m, m.dialog.action("3600"), 0)
	drain(t, m, m.dialog.action("0"), 0)
	if card := cardWords(m); !strings.Contains(card, "every discobox in this project") || !strings.Contains(card, everyDiscobox) {
		t.Fatalf("card = %q, want every discobox said, and what it means", card)
	}
	send(t, m, keyPress("y"))
	if len(ds.madeRules) != 1 || !ds.madeRules[0].EveryDiscobox || ds.madeRules[0].Repositories != nil ||
		ds.madeRules[0].SecretID != "sec_gh" || ds.madeRules[0].WellKnownID != "" {
		t.Fatalf("made = %+v, want every discobox, naming the secret", ds.madeRules)
	}
}

// An edit opens each step on the rule as it is, and sends only what changed.
// The last card says what changes, before and after, with the uses it
// approved before listed whole.
func TestAnEditSendsOnlyWhatChanged(t *testing.T) {
	t.Parallel()
	m, ds := newRuleFixture(t)
	send(t, m, keyPress(ruleEditKey))

	mustBeOn(t, m, "Edit rule: which discoboxes")
	if at := m.dialog.items[m.dialog.cursor]; at.key != ruleForTyped || !strings.Contains(at.detail, "github.com/org/repo, github.com/org/tools") {
		t.Fatalf("cursor on %+v, want the rule's repositories", at)
	}
	drain(t, m, m.dialog.action(ruleForTyped), 0)
	// The list opens on the rule's repositories; Enter keeps them.
	drain(t, m, m.dialog.action(""), 0)

	mustBeOn(t, m, "Edit rule: what it approves")
	// ↑ puts the last use in the field, and Enter puts it back as edited, in
	// its place; Enter on the new line, empty, finishes.
	send(t, m, keyPress("up"))
	if m.dialog.typed() != "Read any issue in org/repo" {
		t.Fatalf("field = %q, want the last use to edit", m.dialog.typed())
	}
	typeLines(t, m, "Read and label any issue in org/repo")
	// The longest grant and the expiry open on the rule's own, and are kept.
	if at := m.dialog.items[m.dialog.cursor]; at.key != "28800" {
		t.Fatalf("cursor on %q, want the rule's eight hours", at.key)
	}
	drain(t, m, m.dialog.action("28800"), 0)
	drain(t, m, m.dialog.action("0"), 0)

	mustBeOn(t, m, "Edit rule: the rule")
	card := cardWords(m)
	for _, want := range []string{"what changes", "approves 2 uses → 2 uses", "what it approved before", "Read any issue in org/repo",
		"Read and label any issue in org/repo", "change this rule?"} {
		if !strings.Contains(card, want) {
			t.Fatalf("card = %q, want it to carry %q", card, want)
		}
	}
	if strings.Contains(card, "each grant lasts at most 8 hours →") || strings.Contains(card, "for discoboxes of github.com/org/repo; discoboxes of github.com/org/tools →") {
		t.Fatalf("card = %q, want only what changed under what changes", card)
	}
	send(t, m, keyPress("y"))
	if len(ds.changedRules) != 1 {
		t.Fatalf("changes = %+v, want one", ds.changedRules)
	}
	c := ds.changedRules[0]
	if c.Repositories != nil || c.EveryDiscobox || c.MaxGrantTTL != nil || c.Expires != nil || c.NeverExpires ||
		!slices.Equal(c.Uses, []string{longUse, "Read and label any issue in org/repo"}) {
		t.Fatalf("change = %+v, want the uses alone", c)
	}
}

// An edit that changes nothing sends nothing, and says so; one that widens a
// rule to every discobox sends that and only that.
func TestAnEditThatChangesNothingSendsNothing(t *testing.T) {
	t.Parallel()
	m, ds := newRuleFixture(t)
	walk := func(discoboxes string) {
		t.Helper()
		send(t, m, keyPress(ruleEditKey))
		drain(t, m, m.dialog.action(discoboxes), 0)
		if discoboxes == ruleForTyped {
			drain(t, m, m.dialog.action(""), 0)
		}
		drain(t, m, m.dialog.action(""), 0)
		drain(t, m, m.dialog.action("28800"), 0)
		drain(t, m, m.dialog.action("0"), 0)
	}
	walk(ruleForTyped)
	if card := cardWords(m); !strings.Contains(card, "nothing: every part is as it was") {
		t.Fatalf("card = %q, want it to say nothing changes", card)
	}
	send(t, m, keyPress("y"))
	if len(ds.changedRules) != 0 || m.dialog != nil {
		t.Fatalf("changes = %+v, dialog = %s; want nothing sent and the table back", ds.changedRules, describe(m.dialog))
	}

	walk(ruleForEvery)
	send(t, m, keyPress("y"))
	if len(ds.changedRules) != 1 || !ds.changedRules[0].EveryDiscobox || ds.changedRules[0].Repositories != nil || ds.changedRules[0].Uses != nil {
		t.Fatalf("changes = %+v, want every discobox alone", ds.changedRules)
	}
}

// A refusal from the server is shown whole, and Esc there is the step it is
// about, opened on what was chosen, so it can be changed rather than chosen
// again from the start.
func TestAServerRefusalLeadsBackToItsStep(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		refusal string
		back    string
	}{
		{`"org/repo" names no host: say which, as in github.com/org/repo`, "New rule: which discoboxes"},
		{"each of a rule's uses is one sentence of at most 2048 bytes, on one line", "New rule: what it approves"},
		{"a rule's expiry is in the future; leave it out for a rule that does not lapse", "New rule: when it lapses"},
		{"the server is unavailable", "New rule: the rule"},
	} {
		t.Run(tc.back, func(t *testing.T) {
			t.Parallel()
			m, ds := newRuleFixture(t)
			ds.mu.Lock()
			ds.writeRuleErr = errors.New(tc.refusal)
			ds.mu.Unlock()
			send(t, m, keyPress(ruleNewKey))
			drain(t, m, m.dialog.action("wk:sec_gh"), 0)
			drain(t, m, m.dialog.action(ruleForTyped), 0)
			typeLines(t, m, "org/repo")
			typeLines(t, m, "Read any issue in org/repo")
			drain(t, m, m.dialog.action("3600"), 0)
			drain(t, m, m.dialog.action("0"), 0)
			send(t, m, keyPress("y"))

			if m.dialog == nil || !m.dialog.err || !strings.Contains(cardWords(m), tc.refusal) || !strings.Contains(cardWords(m), "no rule was made") {
				t.Fatalf("dialog = %s, want the refusal said:\n%s", describe(m.dialog), wholeCard(m))
			}
			send(t, m, keyPress("esc"))
			mustBeOn(t, m, tc.back)
			// What was chosen is still there.
			if tc.back == "New rule: which discoboxes" && !strings.Contains(m.dialog.items[m.dialog.cursor].detail, "now org/repo") {
				t.Fatalf("cursor on %+v, want the repositories typed", m.dialog.items[m.dialog.cursor])
			}
			if tc.back == "New rule: what it approves" && !strings.Contains(wholeCard(m), "Read any issue in org/repo") {
				t.Fatalf("card = %q, want the uses written", wholeCard(m))
			}
		})
	}
}

// refusedStep reads the server's own words for which step a refusal is
// about, and never reaches before the first step an edit has.
func TestARefusalIsReadForItsStep(t *testing.T) {
	t.Parallel()
	for said, want := range map[string]ruleStep{
		"a rule is for the repositories it names or for every discobox in the project, not both": stepDiscoboxes,
		"a rule says what may be approved: at least one use":                                     stepUses,
		"a rule's lifetime cap is a number of seconds; 0 leaves it to the secret's own limit":    stepGrantCap,
		"secret not found": stepCredential,
		"because it broke": stepRead,
	} {
		if got := refusedStep(errors.New(said), stepCredential); got != want {
			t.Errorf("refusedStep(%q) = %d, want %d", said, got, want)
		}
	}
	if got := refusedStep(errors.New("secret not found"), stepDiscoboxes); got != stepDiscoboxes {
		t.Errorf("an edit's refusal about its credential = %d, want its first step", got)
	}
}

// The table's key line offers making and editing, and the rule's card has a
// row for an edit on the same key, which leads back to the card.
func TestTheTableAndTheCardOfferNewAndEdit(t *testing.T) {
	t.Parallel()
	m, _ := newRuleFixture(t)
	frame := plainFrame(m)
	for _, want := range []string{"n new", "e edit", "enter read it", "s disable", "d delete"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("key line does not offer %q:\n%s", want, frame)
		}
	}
	openRule(t, m, "rule_1")
	if m.dialog.items[0].key != ruleEditItem || m.dialog.items[0].press != ruleEditKey {
		t.Fatalf("rows = %v, want an edit first, on %q", itemKeys(m.dialog), ruleEditKey)
	}
	send(t, m, keyPress(ruleEditKey))
	mustBeOn(t, m, "Edit rule: which discoboxes")
	// Leaving the first step goes back to the card it was asked from.
	send(t, m, keyPress("esc"))
	if m.dialog == nil || m.dialog.title != "Approval rule" || m.dialog.titleRight != "rule_1" {
		t.Fatalf("dialog = %s, want the rule's card back", describe(m.dialog))
	}
}

// ↑ and ↓ walk the field through the lines written. A line taken up keeps
// what was half-typed on the new line; Enter on a line edits it in place, and
// on a line emptied removes it; moving off a line never removes it; Esc
// keeps every word, an unconfirmed edit included.
func TestTheLinesAreEditedInPlace(t *testing.T) {
	t.Parallel()
	m, _ := newRuleFixture(t)
	send(t, m, keyPress(ruleNewKey))
	drain(t, m, m.dialog.action("wk:sec_gh"), 0)
	drain(t, m, m.dialog.action(ruleForEvery), 0)
	mustBeOn(t, m, "New rule: what it approves")
	drain(t, m, m.dialog.action("one"), 0)
	drain(t, m, m.dialog.action("two"), 0)
	drain(t, m, m.dialog.action("three"), 0)
	setField(m, "four, half")

	frame := plainFrame(m)
	if !strings.Contains(frame, "↑ edit a line") || !strings.Contains(frame, "Enter adds; empty, finishes") {
		t.Fatalf("key line on the new line:\n%s", frame)
	}
	send(t, m, keyPress("up"), keyPress("up"))
	if m.dialog.typed() != "two" || m.dialog.editAt != 1 || !strings.Contains(cardWords(m), "1. one ❯ 2. two") ||
		strings.Count(cardWords(m), "two") != 1 || m.dialog.answerLabel != "what it approves" {
		t.Fatalf("field = %q, card = %q; want line 2 up for editing", m.dialog.typed(), wholeCard(m))
	}
	if frame := plainFrame(m); !strings.Contains(frame, "Enter keeps; emptied, removes") || !strings.Contains(frame, "Esc back, kept") {
		t.Fatalf("key line on a line:\n%s", frame)
	}
	// Edited, and left unconfirmed by Esc: kept, field and all.
	setField(m, "TWO")
	send(t, m, keyPress("esc"))
	mustBeOn(t, m, "New rule: which discoboxes")
	drain(t, m, m.dialog.action(ruleForEvery), 0)
	if m.dialog.typed() != "TWO" || m.dialog.editAt != 1 {
		t.Fatalf("field = %q at %q, want the unconfirmed edit kept", m.dialog.typed(), m.dialog.answerLabel)
	}
	drain(t, m, m.dialog.action("TWO"), 0)
	// Back on the new line, with what was half-typed there.
	if m.dialog.typed() != "four, half" {
		t.Fatalf("field = %q, want the new line as it was left", m.dialog.typed())
	}
	// A line emptied and moved off is left as it was; emptied and Entered,
	// it goes.
	send(t, m, keyPress("up"))
	setField(m, "")
	send(t, m, keyPress("down"))
	if !strings.Contains(wholeCard(m), "three") {
		t.Fatalf("card = %q, want a line moved off kept", wholeCard(m))
	}
	send(t, m, keyPress("up"), keyPress("up"), keyPress("up"))
	drain(t, m, m.dialog.action(""), 0)
	setField(m, "")
	drain(t, m, m.dialog.action(""), 0)
	mustBeOn(t, m, "New rule: longest grant")
	drain(t, m, m.dialog.action("3600"), 0)
	drain(t, m, m.dialog.action("0"), 0)
	send(t, m, keyPress("y"))
	if made := m.ds.(*fakeSource).madeRules; len(made) != 1 || !slices.Equal(made[0].Uses, []string{"TWO", "three"}) {
		t.Fatalf("made = %+v, want one removed, one edited in place", made)
	}
}

// A use the server holds in text that had to be made safe to draw is sent
// back in the server's own text when an edit leaves it alone: only a line the
// person changed is new text.
func TestAnEditSendsTheServersTextForLinesLeftAlone(t *testing.T) {
	t.Parallel()
	m, ds := newRuleFixture(t)
	const stored, shown = "Read issues\u202e in org/repo", `Read issues\u202e in org/repo`
	ds.mu.Lock()
	ds.rules[0].Uses = []string{shown, "Read any issue in org/repo"}
	ds.rules[0].Stored = ApprovalRule{Repositories: []string{"github.com/org/repo", "github.com/org/tools"}, Uses: []string{stored, "Read any issue in org/repo"}}
	ds.mu.Unlock()
	send(t, m, keyPress("r"))
	onRule(t, m, "rule_1")
	send(t, m, keyPress(ruleEditKey))
	drain(t, m, m.dialog.action(ruleForTyped), 0)
	drain(t, m, m.dialog.action(""), 0)
	send(t, m, keyPress("up"))
	drain(t, m, m.dialog.action("Read and label any issue in org/repo"), 0)
	drain(t, m, m.dialog.action(""), 0)
	drain(t, m, m.dialog.action("28800"), 0)
	drain(t, m, m.dialog.action("0"), 0)
	send(t, m, keyPress("y"))
	if len(ds.changedRules) != 1 || ds.changedRules[0].Repositories != nil ||
		!slices.Equal(ds.changedRules[0].Uses, []string{stored, "Read and label any issue in org/repo"}) {
		t.Fatalf("changes = %+v, want the untouched use in the server's text and the repositories unsent", ds.changedRules)
	}
}

// The hosts the last card reads are the hosts sent, so the rule stored is
// the rule read.
func TestANewRuleSendsTheHostsItsCardReads(t *testing.T) {
	t.Parallel()
	m, ds := newRuleFixture(t)
	send(t, m, keyPress(ruleNewKey))
	drain(t, m, m.dialog.action("wk:sec_gh"), 0)
	drain(t, m, m.dialog.action(ruleForEvery), 0)
	typeLines(t, m, "Read any issue in org/repo")
	drain(t, m, m.dialog.action("3600"), 0)
	drain(t, m, m.dialog.action("0"), 0)
	var read string
	for _, sec := range m.dialog.sections {
		for _, f := range sec.fields {
			if f.label == "may be sent to" {
				read = f.value
			}
		}
	}
	send(t, m, keyPress("y"))
	if len(ds.madeRules) != 1 || read == "" || strings.Join(ds.madeRules[0].Hosts, ", ") != read {
		t.Fatalf("made = %+v, card read %q; want the hosts sent as read", ds.madeRules, read)
	}
}

// A window too short to draw the rules still reaches an empty rules table:
// moving into it swaps it in, and n makes the first rule there.
func TestAnEmptyRulesTableIsReachableInAShortWindow(t *testing.T) {
	t.Parallel()
	m, ds := rulesFixture(t)
	ds.mu.Lock()
	ds.rules = nil
	ds.mu.Unlock()
	send(t, m, keyPress("r"), sizeMsg(120, 16))
	if !m.rulesHidden {
		t.Fatalf("want the rules left out of a short window")
	}
	send(t, m, keyPress("tab"))
	for m.table != tableRules {
		if m.table == tableSecrets {
			t.Fatalf("tab went round without reaching the empty rules")
		}
		send(t, m, keyPress("tab"))
	}
	if m.rulesHidden || !strings.Contains(plainFrame(m), "n new rule") {
		t.Fatalf("hidden = %v, want the rules swapped in with n offered:\n%s", m.rulesHidden, plainFrame(m))
	}
	send(t, m, keyPress(ruleNewKey))
	mustBeOn(t, m, "New rule: which credential")
}

// knownRepositories gives the project's discoboxes repositories: one of a
// repository no rule names, one of a repository a rule already names, and
// one with none.
func knownRepositories(m *Model) {
	for i := range m.list.all {
		switch i {
		case 0:
			m.list.all[i].RepositoryIdentity, m.list.all[i].RepositoryShown = "github.com/acme/pool", "github.com/acme/pool"
		case 1:
			m.list.all[i].RepositoryIdentity, m.list.all[i].RepositoryShown = "github.com/org/repo", "github.com/org/repo"
		}
	}
}

// pickLabels is the picks a repositories step offers, as its rows say them.
func pickLabels(m *Model) []string {
	var out []string
	for _, item := range m.dialog.items {
		if strings.HasPrefix(item.key, "pick:") {
			out = append(out, item.label)
		}
	}
	return out
}

// The repositories step offers the repositories the console knows for the
// project — its discoboxes' and its rules', each once, sorted — as picks
// chosen with Space, above the lines written for others. An entry typed that
// is one of them, however spelled, is that pick chosen, not a second line.
func TestTheRepositoriesStepOffersWhatItKnows(t *testing.T) {
	t.Parallel()
	m, ds := newRuleFixture(t)
	knownRepositories(m)
	send(t, m, keyPress(ruleNewKey))
	drain(t, m, m.dialog.action("wk:sec_gh"), 0)
	drain(t, m, m.dialog.action(ruleForTyped), 0)

	want := []string{"[ ] github.com/acme/pool", "[ ] github.com/org/repo", "[ ] github.com/org/tools", "[ ] gitlab.example.com/team/models"}
	if got := pickLabels(m); !slices.Equal(got, want) {
		t.Fatalf("picks = %q, want %q", got, want)
	}
	if frame := plainFrame(m); !strings.Contains(frame, "↑ tick a known one") {
		t.Fatalf("key line on the new line:\n%s", frame)
	}
	// Up into the picks: the last, and Space chooses it.
	send(t, m, keyPress("up"))
	if frame := plainFrame(m); !strings.Contains(frame, "Space ticks") || !strings.Contains(frame, "↑↓ move") {
		t.Fatalf("key line on a pick:\n%s", frame)
	}
	send(t, m, keyPress(" "), keyPress("up"), keyPress("up"), keyPress("up"), keyPress("enter"))
	if got := pickLabels(m); got[0] != "[x] github.com/acme/pool" || got[3] != "[x] gitlab.example.com/team/models" || got[1] != "[ ] github.com/org/repo" {
		t.Fatalf("picks = %q, want the first and the last chosen", got)
	}
	// A letter on a pick goes nowhere: the field is not up.
	send(t, m, keyPress("x"))
	// Down off the picks is the field's new line.
	send(t, m, keyPress("down"), keyPress("down"), keyPress("down"), keyPress("down"))
	// Typed as a URL, the known one is chosen rather than written twice.
	drain(t, m, m.dialog.action("https://github.com/org/tools.git"), 0)
	drain(t, m, m.dialog.action("github.com/new/thing"), 0)
	if got := pickLabels(m); got[2] != "[x] github.com/org/tools" {
		t.Fatalf("picks = %q, want the typed one chosen as its pick", got)
	}
	if card := wholeCard(m); strings.Count(card, "github.com/org/tools") != 1 || !strings.Contains(card, "github.com/new/thing") {
		t.Fatalf("card = %q, want the known one once and the new one written", card)
	}
	drain(t, m, m.dialog.action(""), 0)
	typeLines(t, m, "Read any issue")
	drain(t, m, m.dialog.action("3600"), 0)
	drain(t, m, m.dialog.action("0"), 0)
	send(t, m, keyPress("y"))
	if len(ds.madeRules) != 1 || !slices.Equal(ds.madeRules[0].Repositories,
		[]string{"github.com/acme/pool", "github.com/org/tools", "gitlab.example.com/team/models", "github.com/new/thing"}) {
		t.Fatalf("made = %+v, want the picks chosen in order, then the one written", ds.madeRules)
	}
}

// Finishing the repositories with none chosen or written is refused where it
// was asked, and Esc there goes back with every choice as it was.
func TestTheRepositoriesStepNeedsOne(t *testing.T) {
	t.Parallel()
	m, _ := newRuleFixture(t)
	knownRepositories(m)
	send(t, m, keyPress(ruleNewKey))
	drain(t, m, m.dialog.action("wk:sec_gh"), 0)
	drain(t, m, m.dialog.action(ruleForTyped), 0)
	drain(t, m, m.dialog.action(""), 0)
	if !strings.Contains(m.dialog.emphasis, "choose or name at least one repository") {
		t.Fatalf("emphasis = %q, want the step to say one is needed", m.dialog.emphasis)
	}
	send(t, m, keyPress("up"), keyPress(" "), keyPress("esc"))
	mustBeOn(t, m, "New rule: which discoboxes")
	if at := m.dialog.items[0]; !strings.Contains(at.detail, "now gitlab.example.com/team/models") {
		t.Fatalf("row = %+v, want the choice kept going back", at)
	}
}

// An edit opens on the rule's own repositories first, chosen, and the rest
// the console knows after them, not chosen; one of the rule's that is also a
// discobox's is offered once.
func TestAnEditOpensOnTheRulesRepositoriesChosen(t *testing.T) {
	t.Parallel()
	m, ds := newRuleFixture(t)
	knownRepositories(m)
	send(t, m, keyPress(ruleEditKey))
	drain(t, m, m.dialog.action(ruleForTyped), 0)
	want := []string{"[x] github.com/org/repo", "[x] github.com/org/tools", "[ ] github.com/acme/pool", "[ ] gitlab.example.com/team/models"}
	if got := pickLabels(m); !slices.Equal(got, want) {
		t.Fatalf("picks = %q, want %q", got, want)
	}
	// Clearing one is the change, and only it is sent.
	send(t, m, keyPress("up"), keyPress("up"), keyPress("up"), keyPress(" "))
	drain(t, m, m.dialog.action(""), 0)
	drain(t, m, m.dialog.action(""), 0)
	drain(t, m, m.dialog.action("28800"), 0)
	drain(t, m, m.dialog.action("0"), 0)
	send(t, m, keyPress("y"))
	if len(ds.changedRules) != 1 || !slices.Equal(ds.changedRules[0].Repositories, []string{"github.com/org/repo"}) || ds.changedRules[0].Uses != nil {
		t.Fatalf("changes = %+v, want the one repository left, and nothing else", ds.changedRules)
	}
}

// Auto approve's uses ride every step back and forward: Esc to which
// discoboxes, Esc again to the secrets, forward again, and the uses open as
// they were left, half-typed line included — and the same back from the
// lifetime.
func TestAutoApprovesUsesSurviveStepsBack(t *testing.T) {
	t.Parallel()
	m, _ := sourceWithProposals(t)
	toRuleDiscoboxes(t, m)
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	drain(t, m, m.dialog.action("Never push to main"), 0)
	setField(m, "Comment on th")
	send(t, m, keyPress("esc"))
	if !onRuleStep(m, "which discoboxes") {
		t.Fatalf("dialog = %s, want which discoboxes", describe(m.dialog))
	}
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	if card := cardWords(m); !strings.Contains(card, "from any discobox/<slug> branch") || !strings.Contains(card, "Never push to main") ||
		m.dialog.typed() != "Comment on th" {
		t.Fatalf("card = %q, field = %q; want every line and the half-typed one", card, m.dialog.typed())
	}
	drain(t, m, m.dialog.action("Comment on the issue"), 0)
	drain(t, m, m.dialog.action(""), 0)
	send(t, m, keyPress("esc"), keyPress("esc"))
	if !onRuleStep(m, "which discoboxes") {
		t.Fatalf("dialog = %s, want which discoboxes", describe(m.dialog))
	}
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	if card := cardWords(m); !strings.Contains(card, "Comment on the issue") {
		t.Fatalf("card = %q, want the uses finished before kept", card)
	}
}

// At 90 columns the line list's key line is whole in every position: on a
// pick, on a line being edited, and on the new line.
func TestTheLineListsKeyLineFitsNinetyColumns(t *testing.T) {
	t.Parallel()
	m, _ := newRuleFixture(t)
	knownRepositories(m)
	send(t, m, sizeMsg(90, 50))
	send(t, m, keyPress(ruleNewKey))
	drain(t, m, m.dialog.action("wk:sec_gh"), 0)
	drain(t, m, m.dialog.action(ruleForTyped), 0)
	drain(t, m, m.dialog.action("github.com/new/thing"), 0)
	for _, tc := range []struct {
		name string
		keys []string
		want []string
	}{
		{"the new line", nil, []string{"Enter adds; empty, finishes", "Esc back, kept", "↑ edit a line"}},
		{"a line", []string{"up"}, []string{"Enter keeps; emptied, removes", "Esc back, kept", "↑↓ line"}},
		{"a pick", []string{"up"}, []string{"Space ticks", "Esc back, kept", "↑↓ move"}},
	} {
		for _, k := range tc.keys {
			send(t, m, keyPress(k))
		}
		frame := plainFrame(m)
		for _, want := range tc.want {
			if !strings.Contains(frame, want) {
				t.Fatalf("on %s the key line lacks %q at 90 columns:\n%s", tc.name, want, frame)
			}
		}
	}
}

// The picks and the lines are the card's rows, and the pointer answers them
// as the keys do: a press on a pick ticks or clears it, on a line puts it in
// the field. Every row marked is the row drawn there, in a card short enough
// to window its rows too.
func TestTheListsRowsAnswerThePointer(t *testing.T) {
	t.Parallel()
	for _, height := range []int{50, 24} {
		m, _ := newRuleFixture(t)
		knownRepositories(m)
		send(t, m, sizeMsg(100, height))
		send(t, m, keyPress(ruleNewKey))
		drain(t, m, m.dialog.action("wk:sec_gh"), 0)
		drain(t, m, m.dialog.action(ruleForTyped), 0)
		for i := range 6 {
			drain(t, m, m.dialog.action("github.com/new/thing"+itoa(i)), 0)
		}
		// Every marked row draws the row it answers for.
		lines := strings.Split(plainFrame(m), "\n")
		marked := 0
		for _, mark := range m.zones.marks {
			if mark.what.kind != hitDialogItem || mark.what.idx < 0 {
				continue
			}
			marked++
			item := m.dialog.items[mark.what.idx]
			if label := strings.TrimPrefix(strings.TrimPrefix(item.label, "· "), "[ ] "); !strings.Contains(lines[mark.y], label) {
				t.Errorf("height %d: row %d is marked as %q and draws %q", height, mark.y, item.label, lines[mark.y])
			}
		}
		if marked == 0 {
			t.Fatalf("height %d: no row is marked:\n%s", height, strings.Join(lines, "\n"))
		}
		// A press on a pick drawn ticks it: in a short card the list is
		// windowed on the cursor, so it is walked up to the picks first.
		for range 12 {
			if strings.Contains(plainFrame(m), "[ ] github.com/acme/pool") {
				break
			}
			send(t, m, keyPress("up"))
		}
		x, y := at(t, m, "[ ] github.com/acme/pool")
		tap(t, m, x, y)
		if got := pickLabels(m); got[0] != "[x] github.com/acme/pool" {
			t.Fatalf("height %d: picks = %q, want the pressed one ticked", height, got)
		}
		// A press on a line drawn puts it in the field, keeping what the
		// new line held. The press on the pick put the cursor there, so the
		// field is reached first.
		for m.dialog.editAt != len(m.dialog.items)-1 {
			send(t, m, keyPress("down"))
		}
		setField(m, "half")
		for range 12 {
			if strings.Contains(plainFrame(m), "github.com/new/thing0") {
				break
			}
			send(t, m, keyPress("up"))
		}
		x, y = at(t, m, "github.com/new/thing0")
		tap(t, m, x, y)
		if v := m.dialog.typed(); !strings.HasPrefix(v, "github.com/new/thing") || m.dialog.editAt == len(m.dialog.items)-1 {
			t.Fatalf("height %d: field = %q at %q, want the pressed line to edit", height, v, m.dialog.answerLabel)
		}
		send(t, m, keyPress("down"), keyPress("down"), keyPress("down"), keyPress("down"), keyPress("down"), keyPress("down"), keyPress("down"))
		if m.dialog.typed() != "half" {
			t.Fatalf("height %d: field = %q, want the new line as it was left", height, m.dialog.typed())
		}
	}
}

// A repository from a discobox is drawn made safe and sent as it is, and a
// paste on a pick is said to go nowhere rather than dropped silently.
func TestADiscoboxsRepositoryIsDrawnSafe(t *testing.T) {
	t.Parallel()
	m, ds := newRuleFixture(t)
	m.list.all[0].RepositoryIdentity, m.list.all[0].RepositoryShown = "github.com/acme/\x1b[2Jpool", `github.com/acme/\x1b[2Jpool`
	send(t, m, keyPress(ruleNewKey))
	drain(t, m, m.dialog.action("wk:sec_gh"), 0)
	drain(t, m, m.dialog.action(ruleForTyped), 0)
	if strings.Contains(rawFrame(m), "\x1b[2J") {
		t.Fatal("a discobox's repository reached the terminal raw")
	}
	send(t, m, keyPress("up"), keyPress("up"), keyPress("up"), keyPress("up"))
	send(t, m, keyPress(" "))
	send(t, m, tea.PasteMsg{Content: "github.com/pasted/repo"})
	if !strings.Contains(m.dialog.emphasis, "a paste goes in a line") || m.dialog.typed() != "" {
		t.Fatalf("emphasis = %q, field = %q; want the paste refused where it landed", m.dialog.emphasis, m.dialog.typed())
	}
	send(t, m, keyPress("down"), keyPress("down"), keyPress("down"), keyPress("down"))
	drain(t, m, m.dialog.action(""), 0)
	typeLines(t, m, "Read issues")
	drain(t, m, m.dialog.action("3600"), 0)
	drain(t, m, m.dialog.action("0"), 0)
	send(t, m, keyPress("y"))
	if len(ds.madeRules) != 1 || !slices.Contains(ds.madeRules[0].Repositories, "github.com/acme/\x1b[2Jpool") {
		t.Fatalf("made = %+v, want the identity sent as the discobox has it", ds.madeRules)
	}
}

// setField types into the field a card's keys are in: the row being edited
// in place, or the field under the card.
func setField(m *Model, value string) {
	if m.dialog.editor != nil {
		m.dialog.editor.SetValue(value)
		return
	}
	m.dialog.input.SetValue(value)
}

// The uses are a numbered list, right-aligned past nine, the new line the
// next number; the line being edited is its own editor in place, wrapped
// whole with its start in view, and drawn once.
func TestTheUsesAreNumberedAndEditedInPlace(t *testing.T) {
	t.Parallel()
	m, _ := newRuleFixture(t)
	send(t, m, keyPress(ruleNewKey))
	drain(t, m, m.dialog.action("wk:sec_gh"), 0)
	drain(t, m, m.dialog.action(ruleForEvery), 0)
	long := "Open, update, comment on and close pull requests against org/repo from any branch named discobox/<slug>, and never push to main, delete a branch or change the repository's settings"
	drain(t, m, m.dialog.action(long), 0)
	for i := range 9 {
		drain(t, m, m.dialog.action("use "+itoa(i+2)), 0)
	}
	card := wholeCard(m)
	for _, want := range []string{" 1.  Open, update", " 9.  use 9", "10.  use 10", "11."} {
		if !strings.Contains(card, want) {
			t.Fatalf("card = %q, want %q", card, want)
		}
	}
	// Walk up to the long line: it is the editor, drawn whole, wrapped
	// under its text, its start and its end both on the card, once.
	for range 10 {
		send(t, m, keyPress("up"))
	}
	if m.dialog.editAt != 0 || m.dialog.typed() != long {
		t.Fatalf("editing %d %q, want the first use", m.dialog.editAt, m.dialog.typed())
	}
	words := cardWords(m)
	if !strings.Contains(words, "❯ 1. Open, update, comment") || !strings.Contains(words, "repository's settings") || strings.Count(words, "Open, update") != 1 {
		t.Fatalf("card = %q, want the line edited in place, whole, once", words)
	}
}

// A grant read on its card is made into a rule the way auto approve would
// have made one from the request it answered: the server proposes it from the
// grant, the new rule's steps open on that — its credential the grant's and
// not asked, the asking discobox's repository ticked, the judge's uses, each
// grant as long as this one — and Esc on the first step is the grant again.
func TestAGrantIsMadeIntoARule(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)
	ds.mu.Lock()
	ds.projectGrants[0].Expires = ds.projectGrants[0].Granted.Add(2 * time.Hour)
	ds.proposals = RuleProposals{
		Credential: "gh", Hosts: []string{"api.github.com"},
		Repository: "github.com/org/repo", RepositoryStored: "github.com/org/repo",
		Written: &WrittenUses{Uses: []string{"Open a PR against org/repo"}, Stored: []string{"Open a PR against org/repo"},
			Covers: true, Reason: "opening this one is within it"},
	}
	ds.mu.Unlock()
	send(t, m, keyPress(secretsKey), keyPress(secretsKey))
	send(t, m, keyPress("enter"))
	drain(t, m, m.dialog.action("grant_1"), 0)
	mustBeOn(t, m, "Grant")
	if row := m.dialog.items[0]; row.key != grantRuleItem || !row.enabled {
		t.Fatalf("row = %+v, want making a rule offered", row)
	}

	send(t, m, keyPress(ruleNewKey))
	mustBeOn(t, m, "New rule: which discoboxes")
	if !slices.Equal(ds.grantProposed, []string{"grant_1"}) || !strings.Contains(dialogText(m), "grant_1, which stays") {
		t.Fatalf("proposed from %q, card %q; want the grant asked about and named", ds.grantProposed, dialogText(m))
	}
	send(t, m, keyPress("esc"))
	mustBeOn(t, m, "Grant")

	send(t, m, keyPress(ruleNewKey))
	drain(t, m, m.dialog.action(ruleForTyped), 0)
	typeLines(t, m)
	mustBeOn(t, m, "New rule: what it approves")
	for _, want := range []string{"Open a PR against org/repo", "the judge found they cover the request this grant answered"} {
		if !strings.Contains(dialogText(m), want) {
			t.Fatalf("uses step = %q, want %q", dialogText(m), want)
		}
	}
	typeLines(t, m)
	drain(t, m, m.dialog.action("7200"), 0)
	drain(t, m, m.dialog.action("0"), 0)
	send(t, m, keyPress("y"))
	if len(ds.madeRules) != 1 {
		t.Fatalf("made = %+v, want one rule", ds.madeRules)
	}
	made := ds.madeRules[0]
	if made.SecretID != "sec_gh" || !slices.Equal(made.Hosts, []string{"api.github.com"}) || made.Delegate ||
		!slices.Equal(made.Repositories, []string{"github.com/org/repo"}) || !slices.Equal(made.Uses, []string{"Open a PR against org/repo"}) ||
		made.MaxGrantTTL != 2*time.Hour {
		t.Fatalf("made = %+v, want the grant's credential and hosts, its discobox's repository, the judge's uses, and its lifetime", made)
	}
}

// A grant that names no uses has nothing a rule could approve, and its card
// says so rather than offering it.
func TestAGrantWithNoUsesIsNotMadeIntoARule(t *testing.T) {
	t.Parallel()
	m, ds := secretsFixture(t)
	send(t, m, keyPress("enter"))
	drain(t, m, m.dialog.action("grant_2"), 0)
	mustBeOn(t, m, "Grant")
	if row := m.dialog.items[0]; row.enabled || !strings.Contains(row.why, "names none") {
		t.Fatalf("row = %+v, want it refused for a grant with no uses", row)
	}
	send(t, m, keyPress(ruleNewKey))
	if len(ds.grantProposed) != 0 {
		t.Fatalf("proposed from %q, want nothing asked", ds.grantProposed)
	}
}

// grantRuleFixture is the secrets screen with grant_1 standing on the second
// GitHub token, sec_loose, from a request that asked for com.github.api, and
// a proposal from it that covers; mark says whether sec_gh is marked for the
// ID.
func grantRuleFixture(t *testing.T, mark bool) (*Model, *fakeSource) {
	t.Helper()
	m, ds := secretsFixture(t)
	ds.mu.Lock()
	if mark {
		ds.projectSecrets[0].WellKnownID = "com.github.api"
	}
	ds.projectGrants[0].SecretID = "sec_loose"
	asked := "com.github.api"
	ds.proposals = RuleProposals{
		Credential: asked, WellKnownID: asked, Hosts: []string{"api.github.com"},
		Written: &WrittenUses{Uses: []string{"Open a PR against org/repo"}, Stored: []string{"Open a PR against org/repo"}, Covers: true},
	}
	ds.mu.Unlock()
	send(t, m, keyPress(secretsKey), keyPress(secretsKey))
	send(t, m, keyPress("enter"))
	drain(t, m, m.dialog.action("grant_1"), 0)
	mustBeOn(t, m, "Grant")
	send(t, m, keyPress(ruleNewKey))
	return m, ds
}

// A grant for a request that asked for a well-known ID makes a rule for the
// ID — the rule the judge read — even when the grant stands on another
// secret than the one marked for it, and every card says the rule answers
// with the marked secret.
func TestAGrantOnAnUnmarkedSecretMakesARuleForTheID(t *testing.T) {
	t.Parallel()
	m, ds := grantRuleFixture(t, true)
	mustBeOn(t, m, "New rule: which discoboxes")
	says := "answers with the marked secret gh, not OpenAI key"
	if !strings.Contains(dialogText(m), says) {
		t.Fatalf("card = %q, want %q", dialogText(m), says)
	}
	drain(t, m, m.dialog.action(ruleForEvery), 0)
	typeLines(t, m)
	drain(t, m, m.dialog.action("3600"), 0)
	drain(t, m, m.dialog.action("0"), 0)
	mustBeOn(t, m, "New rule: the rule")
	if !strings.Contains(dialogText(m), says) {
		t.Fatalf("last card = %q, want %q", dialogText(m), says)
	}
	send(t, m, keyPress("y"))
	if len(ds.madeRules) != 1 || ds.madeRules[0].WellKnownID != "com.github.api" || ds.madeRules[0].SecretID != "" {
		t.Fatalf("made = %+v, want a rule for the ID alone", ds.madeRules)
	}
}

// With no secret marked for the ID the request asked for, no rule could
// answer like it, and none is started: the card says why, and Esc is the
// grant.
func TestAGrantForAnIDNothingIsMarkedForMakesNoRule(t *testing.T) {
	t.Parallel()
	m, ds := grantRuleFixture(t, false)
	if m.dialog == nil || m.dialog.title != "Could not make a rule from this grant" || !strings.Contains(dialogText(m), "none marked") {
		t.Fatalf("dialog = %s %q, want the refusal", describe(m.dialog), dialogText(m))
	}
	send(t, m, keyPress("esc"))
	mustBeOn(t, m, "Grant")
	if len(ds.madeRules) != 0 {
		t.Fatalf("made = %+v, want nothing", ds.madeRules)
	}
}

// A use breaks at the same places whether it is the line being edited or
// not: the editor's wrapping is the list's, the column it keeps for the
// cursor included. Every card width in a range is drawn both ways, which
// takes in rows whose last word ends exactly where the editor's text ends.
func TestALineWrapsTheSameEditedOrNot(t *testing.T) {
	t.Parallel()
	m, _ := newRuleFixture(t)
	send(t, m, keyPress(ruleNewKey))
	drain(t, m, m.dialog.action("wk:sec_gh"), 0)
	drain(t, m, m.dialog.action(ruleForEvery), 0)
	long := `gh pr create --repo discobox-ai/discobox --base main --head discobox/<slug> --draft --title <title> --body "$(cat <file>)", and gh pr view, gh pr list, gh pr edit, gh pr ready and gh pr comment on that pull request; never merge it or enable auto-merge`
	drain(t, m, m.dialog.action(long), 0)
	drain(t, m, m.dialog.action("use 2"), 0)

	full := false
	for width := 60; width <= 130; width++ {
		// On the second line, the first is drawn as text.
		for m.dialog.editAt > 1 {
			send(t, m, keyPress("up"))
		}
		for m.dialog.editAt < 1 {
			send(t, m, keyPress("down"))
		}
		plain := lineRows(t, m, width)
		send(t, m, keyPress("up"))
		if m.dialog.editAt != 0 {
			t.Fatalf("editing %d, want the long use", m.dialog.editAt)
		}
		edited := lineRows(t, m, width)
		if !slices.Equal(plain, edited) {
			t.Fatalf("at width %d the use breaks\n%q\nwhen not edited, and\n%q\nwhen edited", width, plain, edited)
		}
		room := m.dialog.editor.Width()
		for _, row := range plain[:len(plain)-1] {
			if len([]rune(row)) == room-1 {
				full = true
			}
		}
	}
	if !full {
		t.Fatal("no width drew a row whose last word ends where the editor's text does")
	}
}

// lineRows are the rows the first use is drawn on in a card width cells
// wide, its number and chevron taken off: the rows from "1." to "2.".
func lineRows(t *testing.T, m *Model, width int) []string {
	t.Helper()
	card := strings.Split(ansi.Strip(m.dialog.view(m.st, &m.zones, width, 60)), "\n")
	var rows []string
	in := false
	for _, row := range card {
		row = strings.Trim(row, "│ ")
		row = strings.TrimPrefix(row, "❯ ")
		switch {
		case strings.HasPrefix(row, "1.  "):
			in = true
			row = strings.TrimPrefix(row, "1.  ")
		case strings.HasPrefix(row, "2.  ") || strings.HasPrefix(row, "2."):
			in = false
		}
		if in {
			rows = append(rows, strings.TrimSpace(row))
		}
	}
	if len(rows) < 2 {
		t.Fatalf("at width %d the use is not drawn wrapped:\n%s", width, strings.Join(card, "\n"))
	}
	return rows
}
