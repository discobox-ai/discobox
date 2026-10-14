package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
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
	if m.dialog.input.Value() != "Read any issue in org/repo" {
		t.Fatalf("field = %q, want the last use to edit", m.dialog.input.Value())
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
	m.dialog.input.SetValue("four, half")

	frame := plainFrame(m)
	if !strings.Contains(frame, "↑ edit a line") || !strings.Contains(frame, "Enter adds; empty, finishes") {
		t.Fatalf("key line on the new line:\n%s", frame)
	}
	send(t, m, keyPress("up"), keyPress("up"))
	if m.dialog.input.Value() != "two" || !strings.Contains(cardWords(m), "❯ · two ← in the field below") ||
		!strings.Contains(m.dialog.answerLabel, "line 2 of 3") {
		t.Fatalf("field = %q, card = %q; want line 2 up for editing", m.dialog.input.Value(), wholeCard(m))
	}
	if frame := plainFrame(m); !strings.Contains(frame, "Enter keeps; emptied, removes") || !strings.Contains(frame, "Esc back, kept") {
		t.Fatalf("key line on a line:\n%s", frame)
	}
	// Edited, and left unconfirmed by Esc: kept, field and all.
	m.dialog.input.SetValue("TWO")
	send(t, m, keyPress("esc"))
	mustBeOn(t, m, "New rule: which discoboxes")
	drain(t, m, m.dialog.action(ruleForEvery), 0)
	if m.dialog.input.Value() != "TWO" || !strings.Contains(m.dialog.answerLabel, "line 2 of 3") {
		t.Fatalf("field = %q at %q, want the unconfirmed edit kept", m.dialog.input.Value(), m.dialog.answerLabel)
	}
	drain(t, m, m.dialog.action("TWO"), 0)
	// Back on the new line, with what was half-typed there.
	if m.dialog.input.Value() != "four, half" {
		t.Fatalf("field = %q, want the new line as it was left", m.dialog.input.Value())
	}
	// A line emptied and moved off is left as it was; emptied and Entered,
	// it goes.
	send(t, m, keyPress("up"))
	m.dialog.input.SetValue("")
	send(t, m, keyPress("down"))
	if !strings.Contains(wholeCard(m), "three") {
		t.Fatalf("card = %q, want a line moved off kept", wholeCard(m))
	}
	send(t, m, keyPress("up"), keyPress("up"), keyPress("up"))
	drain(t, m, m.dialog.action(""), 0)
	m.dialog.input.SetValue("")
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
	if card := wholeCard(m); strings.Count(card, "github.com/org/tools") != 1 || !strings.Contains(card, "· github.com/new/thing") {
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

// Auto approve's hand-written uses ride every step back and forward: Esc to
// the menu of uses, Esc again to which discoboxes, and forward again, and
// writing by hand opens on them as they were, half-typed line included.
func TestAutoApprovesHandWrittenUsesSurviveStepsBack(t *testing.T) {
	t.Parallel()
	m, _ := sourceWithProposals(t)
	toRuleDiscoboxes(t, m)
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	send(t, m, keyPress("w"))
	drain(t, m, m.dialog.action("Open pull requests against acme/pool"), 0)
	drain(t, m, m.dialog.action("Never push to main"), 0)
	m.dialog.input.SetValue("Comment on th")
	send(t, m, keyPress("esc"), keyPress("esc"))
	if !onRuleStep(m, "which discoboxes") {
		t.Fatalf("dialog = %s, want which discoboxes after Esc twice", describe(m.dialog))
	}
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	send(t, m, keyPress("w"))
	if card := cardWords(m); !strings.Contains(card, "Open pull requests against acme/pool") || !strings.Contains(card, "Never push to main") ||
		m.dialog.input.Value() != "Comment on th" {
		t.Fatalf("card = %q, field = %q; want every line and the half-typed one", card, m.dialog.input.Value())
	}
	// And back from the lifetime, then the uses menu, the same.
	drain(t, m, m.dialog.action("Comment on the issue"), 0)
	drain(t, m, m.dialog.action(""), 0)
	send(t, m, keyPress("esc"), keyPress("esc"), keyPress("esc"))
	if !onRuleStep(m, "which discoboxes") {
		t.Fatalf("dialog = %s, want which discoboxes", describe(m.dialog))
	}
	drain(t, m, m.dialog.action(ruleForRepository), 0)
	send(t, m, keyPress("w"))
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
		// A press on a pick drawn ticks it.
		x, y := at(t, m, "[ ] github.com/acme/pool")
		tap(t, m, x, y)
		if got := pickLabels(m); got[0] != "[x] github.com/acme/pool" {
			t.Fatalf("height %d: picks = %q, want the pressed one ticked", height, got)
		}
		// A press on a line drawn puts it in the field, keeping what the
		// new line held. The press on the pick put the cursor there, so the
		// field is reached first.
		for m.dialog.cursor >= 0 {
			send(t, m, keyPress("down"))
		}
		m.dialog.input.SetValue("half")
		x, y = at(t, m, "· github.com/new/thing")
		tap(t, m, x, y)
		if v := m.dialog.input.Value(); !strings.HasPrefix(v, "github.com/new/thing") || !strings.Contains(m.dialog.answerLabel, "being edited") {
			t.Fatalf("height %d: field = %q at %q, want the pressed line to edit", height, v, m.dialog.answerLabel)
		}
		send(t, m, keyPress("down"), keyPress("down"), keyPress("down"), keyPress("down"), keyPress("down"), keyPress("down"), keyPress("down"))
		if m.dialog.input.Value() != "half" {
			t.Fatalf("height %d: field = %q, want the new line as it was left", height, m.dialog.input.Value())
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
	if !strings.Contains(m.dialog.emphasis, "a paste goes in the field") || m.dialog.input.Value() != "" {
		t.Fatalf("emphasis = %q, field = %q; want the paste refused where it landed", m.dialog.emphasis, m.dialog.input.Value())
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
