package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/discobox-ai/discobox/policy"
	"github.com/discobox-ai/discobox/wellknown"
)

// A rule answers requests nobody is shown, so the rules a project holds have
// to be readable where its credentials are: a table under the requests they
// answer, each read whole with the grants it minted, and turned off, deleted,
// or undone a grant at a time.

// longStatement is a statement too long for one row of any card, which is the
// kind a model writes, and readStatement a short one.
const (
	longStatement = `allow create, update, comment on pull request created by $this in repository $primary.repo when "its head branch is named discobox/<slug>, and it never pushes to main, deletes a branch or changes the repository's settings"`
	readStatement = `allow read on issue in repository "org/repo"`
)

// longCondition is longStatement's condition, as a card draws it whole.
const longCondition = `when "its head branch is named discobox/<slug>, and it never pushes to main, deletes a branch or changes the repository's settings"`

// rulePolicy is the first fixture rule's policy: the long statement and the
// short one, with the definitions of their words.
func rulePolicy() *policy.Policy {
	return mustPolicy(longStatement, readStatement, "comment = add a comment to it", "issue = a GitHub issue",
		"pull request = a GitHub pull request", "repository = a GitHub repository")
}

// rulePolicyText is rulePolicy as the list an edit opens on.
var rulePolicyText = []string{longStatement, readStatement, "comment = add a comment to it", "issue = a GitHub issue",
	"pull request = a GitHub pull request", "repository = a GitHub repository"}

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
			ApprovalRule: ApprovalRule{Repositories: []string{"github.com/org/repo", "github.com/org/tools"}, Policy: rulePolicy()},
			WellKnownID:  "com.github.api", SecretID: "sec_gh", Hosts: []string{"github.com", "api.github.com"},
			MaxGrantTTL: 8 * time.Hour, CreatedBy: "user:darren", Created: now.Add(-3 * time.Hour), Enabled: true,
		},
		{
			ID:           "rule_2",
			ApprovalRule: ApprovalRule{Repositories: []string{"gitlab.example.com/team/models"}, Policy: mustPolicy(`allow create on completion in service "openai"`, "completion = a model completion", "service = an API service")},
			SecretID:     "sec_loose", Hosts: []string{"api.openai.com"}, Created: now.Add(-time.Hour),
		},
		{
			ID:           "rule_3",
			ApprovalRule: ApprovalRule{EveryDiscobox: true, Policy: mustPolicy(readStatement, "issue = a GitHub issue", "repository = a GitHub repository")},
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
	// The rules arrive with the screen's own read, and again whenever it
	// re-reads.
	send(t, m, keyPress("r"))
	return m, ds
}

// onRule puts the keys in the rules table, on one rule's row.
func onRule(t *testing.T, m *Model, ruleID string) {
	t.Helper()
	for i, rule := range m.rules.all {
		if rule.ID == ruleID {
			m.table = tableRules
			m.rules.moveTo(i)
			return
		}
	}
	t.Fatalf("the table has no %s: %+v", ruleID, m.rules.all)
}

// openRule goes on from the rule's row to its card.
func openRule(t *testing.T, m *Model, ruleID string) {
	t.Helper()
	onRule(t, m, ruleID)
	send(t, m, keyPress("enter"))
	if m.dialog == nil || m.dialog.title != "Approval rule" || m.dialog.titleRight != ruleID {
		t.Fatalf("dialog = %s, want the card of %s", describe(m.dialog), ruleID)
	}
}

// onRuleRow is the screen with no card over it and the cursor on a rule.
func onRuleRow(m *Model, ruleID string) bool {
	rule := m.rules.current()
	return m.dialog == nil && m.secretsOpen && m.table == tableRules && rule != nil && rule.ID == ruleID
}

// plainRows is the frame as rows of text: a focused table's title is drawn
// bold, and the escapes would split the words these tests look for.
func plainRows(m *Model) []string { return strings.Split(plainFrame(m), "\n") }

// titleRow is the row of the frame a table's title is on.
func titleRow(rows []string, title string) int {
	for i, row := range rows {
		if strings.Contains(row, " "+title+" ") {
			return i
		}
	}
	return -1
}

func itemKeys(d *dialog) []string {
	keys := make([]string, 0, len(d.items))
	for _, it := range d.items {
		keys = append(keys, it.key)
	}
	return keys
}

// The rules are a table of the screen that owns credentials, under the
// requests they answer: nothing has to be opened to see that a project has
// them, how many, and which are answering. A row is how one rule is told from
// the next: its credential, its state, and the discoboxes it is for.
func TestTheRulesAreAThirdTableUnderTheRequests(t *testing.T) {
	t.Parallel()
	m, _ := rulesFixture(t)

	rows := plainRows(m)
	secrets, requests, rules := titleRow(rows, "Secrets"), titleRow(rows, "Requests"), titleRow(rows, "Rules")
	if secrets < 0 || requests <= secrets || rules <= requests {
		t.Fatalf("titles on rows %d, %d, %d; want Secrets, Requests, then Rules:\n%s", secrets, requests, rules, strings.Join(rows, "\n"))
	}
	// Nothing waiting is one row, then two blank rows, then this table's title.
	if want := requests + 4; rules != want {
		t.Fatalf("rules title on row %d, want %d:\n%s", rules, want, strings.Join(rows, "\n"))
	}
	if !strings.Contains(rows[rules], "3 rules") {
		t.Fatalf("title = %q, want it to count the rules", rows[rules])
	}
	for i, want := range [][]string{
		// Repositories longer than their column are cut; the policy takes
		// the rest of the row, as much of it as fits.
		{"com.github.api", "enabled", "github.com/org/repo, github.com/o…", "allow create, update, comment on", "no expiry"},
		// A rule for a project secret is named by the secret, not its ID.
		{"OpenAI key", "disabled", "gitlab.example.com/team/models", "allow create on completion"},
		{"com.github.api", "lapsed", "every discobox", "allow read on issue in", "lapsed 1h ago"},
	} {
		row := rows[rules+1+i]
		for _, word := range want {
			if !strings.Contains(row, word) {
				t.Fatalf("row %d = %q, want it to carry %q", i, row, word)
			}
		}
	}
	// The table is the list: no key opens another.
	send(t, m, keyPress("a"))
	if m.dialog != nil {
		t.Fatalf("dialog = %s, want no card over the screen", describe(m.dialog))
	}
}

// A narrow window gives up the columns that say least, from the right, and
// shares what is left between the repositories and the policy, cutting both.
// The credential and whether the rule is answering are what the row is for,
// and stay.
func TestARuleRowKeepsItsCredentialAndStateInANarrowWindow(t *testing.T) {
	t.Parallel()
	m, _ := rulesFixture(t)
	send(t, m, sizeMsg(72, 40))

	rows := plainRows(m)
	row := rows[titleRow(rows, "Rules")+1]
	for _, want := range []string{"com.github.api", "enabled", "github.com/or", "allow create"} {
		if !strings.Contains(row, want) {
			t.Fatalf("row = %q, want it to keep %q", row, want)
		}
	}
	if strings.Contains(row, "org/tools") || strings.Contains(row, "no expiry") {
		t.Fatalf("row = %q, want the repositories cut and the last column dropped", row)
	}
	for _, line := range rows {
		if w := lipgloss.Width(line); w > 72 {
			t.Fatalf("a row is %d cells wide in a window of 72: %q", w, line)
		}
	}
}

// An empty table that said nothing would read as a listing that did not
// arrive. A project with no rules says so, and how one is made: the empty
// table takes the cursor, its key line offers n, and n starts a rule.
func TestAProjectWithNoRulesSaysSoAndHowOneIsMade(t *testing.T) {
	t.Parallel()
	m, ds := rulesFixture(t)
	ds.rules = nil
	send(t, m, keyPress("r"))

	rows := plainRows(m)
	rules := titleRow(rows, "Rules")
	if rules < 0 {
		t.Fatalf("the table is not drawn:\n%s", strings.Join(rows, "\n"))
	}
	for _, want := range []string{"no rules", "n makes one", "auto approve"} {
		if !strings.Contains(rows[rules+1], want) {
			t.Fatalf("empty line = %q, want it to carry %q", rows[rules+1], want)
		}
	}
	// The line takes the cursor, and the key line there is making one.
	send(t, m, keyPress("end"), keyPress("down"))
	if m.table != tableRules {
		t.Fatalf("the keys are in table %d, want them on the empty rules", m.table)
	}
	rows = plainRows(m)
	if !strings.Contains(rows[titleRow(rows, "Rules")+1], "❯") {
		t.Fatalf("empty line = %q, want the cursor on it", rows[titleRow(rows, "Rules")+1])
	}
	frame := plainFrame(m)
	if !strings.Contains(frame, "n new rule") || strings.Contains(frame, "enter read it") || strings.Contains(frame, "d delete") {
		t.Fatalf("key line does not offer only a new rule:\n%s", frame)
	}
	send(t, m, keyPress(ruleNewKey))
	if m.dialog == nil || m.dialog.title != "New rule: which credential" {
		t.Fatalf("dialog = %s, want n to start a rule", describe(m.dialog))
	}
	// Leaving it is back on the table, with nothing made.
	send(t, m, keyPress("esc"))
	if m.dialog != nil || len(ds.madeRules) != 0 {
		t.Fatalf("dialog = %s, made %v; want the table back and nothing made", describe(m.dialog), ds.madeRules)
	}
	// Up from it is the table above.
	send(t, m, keyPress("up"))
	if m.table != tableSecrets {
		t.Fatalf("the keys are in table %d, want the secrets above", m.table)
	}
}

// The three tables are one screen read downwards: off the bottom of one is
// the next that has a row, off the top is the one above, Tab goes round, and a
// press says which table it landed in.
func TestFocusMovesThroughTheThreeTables(t *testing.T) {
	t.Parallel()
	m, ds := rulesFixture(t)
	slowClock(m)

	// With nothing waiting, the rules are what is under the secrets.
	send(t, m, keyPress("end"), keyPress("down"))
	if !onRuleRow(m, "rule_1") {
		t.Fatalf("down past the last secret left the keys in table %d, want the first rule", m.table)
	}
	send(t, m, keyPress("up"))
	if m.table != tableSecrets {
		t.Fatalf("up past the first rule left the keys in table %d, want the secrets", m.table)
	}

	ds.mu.Lock()
	ds.requests = []CredentialRequest{waitingRequest()}
	ds.mu.Unlock()
	send(t, m, tickMsg{})
	send(t, m, keyPress("end"), keyPress("down"))
	if m.table != tableRequests {
		t.Fatalf("down past the last secret left the keys in table %d, want the requests", m.table)
	}
	send(t, m, keyPress("down"))
	if !onRuleRow(m, "rule_1") {
		t.Fatalf("down past the last request left the keys in table %d, want the first rule", m.table)
	}
	send(t, m, keyPress("down"), keyPress("down"), keyPress("down"))
	if !onRuleRow(m, "rule_3") {
		t.Fatalf("the cursor is on %+v, want it stopped on the last rule", m.rules.current())
	}
	send(t, m, keyPress("home"), keyPress("up"))
	if m.table != tableRequests {
		t.Fatalf("up past the first rule left the keys in table %d, want the requests", m.table)
	}
	// Tab goes down the screen and round.
	for _, want := range []secretsTable{tableRules, tableSecrets, tableRequests} {
		send(t, m, keyPress("tab"))
		if m.table != want {
			t.Fatalf("tab left the keys in table %d, want %d", m.table, want)
		}
	}

	// A press on a rule's row is in that table, on that rule.
	x, y := at(t, m, "gitlab.example.com/team/models")
	tap(t, m, x, y)
	if !onRuleRow(m, "rule_2") {
		t.Fatalf("a press on a rule left the keys in table %d on %+v", m.table, m.rules.current())
	}
}

// The key line is what the row under the cursor can take, so on a rule it is
// the rule's: read it, turn it off or on — named for which — and delete it,
// each a button for its key.
func TestTheKeyLineOnARuleRowIsTheRules(t *testing.T) {
	t.Parallel()
	m, _ := rulesFixture(t)

	onRule(t, m, "rule_1")
	line := hintLine(m.secretHints())
	for _, want := range []string{"enter read it", ruleToggleKey + " disable", ruleDeleteKey + " delete", "tab secrets", "esc back"} {
		if !strings.Contains(line, want) {
			t.Fatalf("key line = %q, want it to offer %q", line, want)
		}
	}
	for _, h := range m.secretHints() {
		if strings.HasPrefix(h.text, ruleToggleKey+" ") && !slices.Equal(h.keys, []string{ruleToggleKey}) {
			t.Fatalf("the offer %q presses %v, want its own key", h.text, h.keys)
		}
	}
	if !strings.Contains(frameText(m), ruleToggleKey+" disable") {
		t.Fatalf("the frame's key line does not carry the rule's offers:\n%s", frameText(m))
	}
	onRule(t, m, "rule_2")
	if line := hintLine(m.secretHints()); !strings.Contains(line, ruleToggleKey+" enable") {
		t.Fatalf("key line on a disabled rule = %q, want it to offer turning it on", line)
	}
	// And from the secrets, Tab is offered for the table it reaches.
	m.table = tableSecrets
	if line := hintLine(m.secretHints()); !strings.Contains(line, "tab rules") {
		t.Fatalf("key line on a secret = %q, want Tab to name the rules", line)
	}
	if !strings.Contains(m.helpText(), "on a rule, disable or enable it") {
		t.Fatal("the help does not list the rule's keys")
	}
}

// A rule is turned off, or deleted, from its row the way a secret is edited
// from its own: the screen stays, re-reads, and keeps the cursor on the rule
// — or, when it is gone, on the row that took its place.
func TestARuleIsEnabledAndDeletedFromItsRow(t *testing.T) {
	t.Parallel()
	m, ds := rulesFixture(t)

	onRule(t, m, "rule_2")
	send(t, m, keyPress(ruleToggleKey))
	if !slices.Equal(ds.ruleEdits, []string{"rule_2 enabled=true"}) {
		t.Fatalf("edits = %v, want the rule under the cursor switched on", ds.ruleEdits)
	}
	if !onRuleRow(m, "rule_2") || !m.rules.current().Enabled {
		t.Fatalf("dialog = %s, cursor on %+v; want the screen, on the rule, showing it enabled", describe(m.dialog), m.rules.current())
	}

	send(t, m, keyPress(ruleDeleteKey))
	if m.dialog == nil || m.dialog.kind != dlgConfirm || !m.dialog.defaultNo || m.dialog.titleRight != "rule_2" {
		t.Fatalf("dialog = %s, want the question for that rule, defaulting to no", describe(m.dialog))
	}
	send(t, m, keyPress("enter"))
	if len(ds.deletedRules) != 0 || !onRuleRow(m, "rule_2") {
		t.Fatalf("deleted = %v, dialog = %s; want No to leave the rule and its row", ds.deletedRules, describe(m.dialog))
	}
	send(t, m, keyPress(ruleDeleteKey), keyPress("y"))
	if !slices.Equal(ds.deletedRules, []string{"rule_2"}) {
		t.Fatalf("deleted = %v, want the rule under the cursor", ds.deletedRules)
	}
	if !onRuleRow(m, "rule_3") || len(m.rules.all) != 2 {
		t.Fatalf("cursor on %+v of %d rules; want the row that took its place", m.rules.current(), len(m.rules.all))
	}
}

// A window too short for a row of all three tables leaves one of the lower
// two out rather than drawing past its own foot: the rules until the keys go
// there, and then the requests. Nothing is marked where it is not drawn.
func TestAShortWindowShowsTheTableWithTheKeys(t *testing.T) {
	t.Parallel()
	m, ds := rulesFixture(t)
	ds.mu.Lock()
	ds.requests = []CredentialRequest{waitingRequest()}
	ds.mu.Unlock()
	send(t, m, tickMsg{})
	send(t, m, sizeMsg(120, 16))

	rows := plainRows(m)
	if len(rows) > 16 {
		t.Fatalf("the frame is %d rows in a window of 16:\n%s", len(rows), strings.Join(rows, "\n"))
	}
	if titleRow(rows, "Requests") < 0 || titleRow(rows, "Rules") >= 0 {
		t.Fatalf("want the requests drawn and the rules left out:\n%s", strings.Join(rows, "\n"))
	}
	// Tab still reaches them, and they are drawn in the requests' place.
	send(t, m, keyPress("tab"), keyPress("tab"))
	rows = plainRows(m)
	if !onRuleRow(m, "rule_1") || titleRow(rows, "Rules") < 0 || titleRow(rows, "Requests") >= 0 {
		t.Fatalf("keys in table %d; want the rules drawn in the requests' place:\n%s", m.table, strings.Join(rows, "\n"))
	}
	if len(rows) > 16 {
		t.Fatalf("the frame is %d rows in a window of 16:\n%s", len(rows), strings.Join(rows, "\n"))
	}
	// Its share is fewer rows than it has rules, windowed on the cursor like
	// any list here.
	send(t, m, keyPress("end"))
	if rows = plainRows(m); !strings.Contains(rows[titleRow(rows, "Rules")+1], "gitlab.example.com/team/models") || !strings.Contains(rows[titleRow(rows, "Rules")+2], "every discobox") {
		t.Fatalf("the rows under the title do not follow the cursor:\n%s", strings.Join(rows, "\n"))
	}
	// A press lands on the table that is drawn, not on one that is not.
	slowClock(m)
	x, y := at(t, m, "gitlab.example.com/team/models")
	tap(t, m, x, y)
	if !onRuleRow(m, "rule_2") {
		t.Fatalf("the press left the keys in table %d on %+v", m.table, m.rules.current())
	}
}

// A rule's text is the authorization, so its card is the whole of it: its
// policy to its last word, what the server holds around it, and the grants it
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
		"discoboxes of github.com/org/repo", "discoboxes of github.com/org/tools",
		"in repository (a GitHub repository) $primary.repo",
		"allow create, update, comment (add a comment to it) on pull request (a GitHub pull request) created by $this",
		longCondition, `in repository "org/repo"`, "allow read on issue (a GitHub issue)",
		"each variable is bound to whichever discobox asks",
		"1 grant it minted still stands",
		"fix flaky pool reaper tests · sandbox sbx_one api.github.com · expires in",
		// An edit is offered, and what happens to one.
		"Edit this rule", "an edit is read whole before it is saved",
	} {
		if !strings.Contains(card, want) {
			t.Fatalf("card does not carry %q:\n%s", want, wholeCard(m))
		}
	}
	// A grant that has lapsed authorizes nothing, and one another rule or a
	// person made is not this rule's.
	if got := itemKeys(m.dialog); !slices.Equal(got, []string{ruleEditItem, ruleToggleItem, ruleDeleteItem, "grant_r1"}) {
		t.Fatalf("rows = %v, want the three actions and the one grant still standing", got)
	}

	// Enter on a grant reads it, and leaving it comes back to the rule.
	drain(t, m, m.dialog.action("grant_r1"), 0)
	if m.dialog == nil || m.dialog.title != "Grant" || !strings.Contains(dialogText(m), "approval rule rule_1") {
		t.Fatalf("dialog = %s, want the grant read, naming the rule that minted it:\n%s", describe(m.dialog), dialogText(m))
	}
	send(t, m, keyPress("esc"))
	if m.dialog == nil || m.dialog.titleRight != "rule_1" {
		t.Fatalf("dialog = %s, want the rule back", describe(m.dialog))
	}
	// And leaving the rule is back on the screen, on the row it was opened
	// from, with nothing in between.
	send(t, m, keyPress("esc"))
	if !onRuleRow(m, "rule_1") {
		t.Fatalf("dialog = %s, table %d; want the screen back on the rule's row", describe(m.dialog), m.table)
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
	if !slices.Contains(itemKeys(m.dialog), "grant_r1") || m.dialog.items[1].label != "Enable this rule" {
		t.Fatalf("rows = %v (toggle %q), want the grant kept and the way back on", itemKeys(m.dialog), m.dialog.items[1].label)
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
	// It has no card to come back to, so the screen is where this lands, on
	// the row that took its place.
	if !onRuleRow(m, "rule_2") || len(m.rules.all) != 2 {
		t.Fatalf("dialog = %s, cursor on %+v of %d rules; want the table without it", describe(m.dialog), m.rules.current(), len(m.rules.all))
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

	send(t, m, keyPress("down"), keyPress("down"), keyPress("down"), keyPress(grantRevokeKey))
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

// A rule that names no repository approves for discoboxes that do not exist
// yet. That is the widest thing on this screen, so it is said in the words the
// decision to allow it was recorded in, on its card and before it is deleted.
func TestARuleForEveryDiscoboxSaysWhatThatMeans(t *testing.T) {
	t.Parallel()
	m, _ := rulesFixture(t)
	openRule(t, m, "rule_3")
	card := cardWords(m)
	for _, want := range []string{"every discobox in this project", everyDiscobox, "state lapsed", "expires lapsed 1h ago", "the secret's own limit"} {
		if !strings.Contains(card, want) {
			t.Fatalf("card does not carry %q:\n%s", want, wholeCard(m))
		}
	}
	send(t, m, keyPress(ruleDeleteKey))
	if !strings.Contains(cardWords(m), "every discobox in this project; "+everyDiscobox) {
		t.Fatalf("the question does not say what the rule was for:\n%s", wholeCard(m))
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
		ApprovalRule: ApprovalRule{Repositories: []string{"github.com/org/repo"}, Policy: mustPolicy(`allow create on discobox in repository $primary.repo`, "discobox = a discobox delivering an issue", "repository = a GitHub repository")}}}
	send(t, m, keyPress("r"))
	// Its row says it delegates: a rule to use the same credential is another.
	if !strings.Contains(frameText(m), "delegates · github.com/org/repo") {
		t.Fatalf("the row does not say the rule delegates:\n%s", frameText(m))
	}
	openRule(t, m, "rule_gate")

	card := cardWords(m)
	for _, want := range []string{gateAnswers, "it may create discoboxes, and hand them credentials", delegationPurpose, "what it would delegate"} {
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

	// The grants a rule minted are half of its card: without them it is not
	// put up as though it had none.
	onRule(t, m, "rule_1")
	ds.grantsErr = errors.New("grants unavailable")
	send(t, m, keyPress("enter"))
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
	send(t, m, keyPress("esc"), keyPress("esc"))

	// From its row the same refusal is shown the same way, and the row stays.
	send(t, m, keyPress(ruleToggleKey))
	if m.dialog == nil || !m.dialog.err || !strings.Contains(dialogText(m), "the server refused the edit") {
		t.Fatalf("dialog = %s, want the refusal shown", describe(m.dialog))
	}
	send(t, m, keyPress("esc"))
	if !onRuleRow(m, "rule_1") || !m.rules.current().Enabled {
		t.Fatalf("dialog = %s, cursor on %+v; want the row back unchanged", describe(m.dialog), m.rules.current())
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

// A window lists several servers, and one may predate the rules. Its secrets
// and requests are still what somebody opened the screen for, so a rules read
// that fails says so in the rules' own place and takes nothing else with it —
// and holds no row, since a rule drawn from an earlier read could be acted on.
func TestAFailedRulesReadLeavesTheRestOfTheScreen(t *testing.T) {
	t.Parallel()
	m, ds := rulesFixture(t)
	ds.mu.Lock()
	ds.requests = []CredentialRequest{waitingRequest()}
	ds.rulesErr = errors.New("404 Not Found:\n  no route /approval-rules")
	ds.mu.Unlock()
	send(t, m, tickMsg{}, keyPress("r"))

	rows := plainRows(m)
	rules := titleRow(rows, "Rules")
	for _, want := range []string{"gh", "OpenAI key", "1 waiting", "api.github.com"} {
		if !strings.Contains(strings.Join(rows[:max(rules, 0)], "\n"), want) {
			t.Fatalf("the screen above the rules does not carry %q:\n%s", want, strings.Join(rows, "\n"))
		}
	}
	if rules < 0 || !strings.Contains(rows[rules+1], "cannot read rules: 404 Not Found: no route /approval-rules") {
		t.Fatalf("the rules table does not say the read failed, on one line:\n%s", strings.Join(rows, "\n"))
	}
	if strings.Contains(rows[rules], "rule") || len(m.rules.all) != 0 {
		t.Fatalf("title = %q with %d rules held; want none counted or kept", rows[rules], len(m.rules.all))
	}
	// Nothing puts the keys in a table with no row.
	send(t, m, keyPress("end"), keyPress("down"), keyPress("end"), keyPress("down"))
	if m.table != tableRequests {
		t.Fatalf("down left the keys in table %d, want them stopped in the requests", m.table)
	}
	send(t, m, keyPress("tab"))
	if m.table != tableSecrets {
		t.Fatalf("tab left the keys in table %d, want the secrets", m.table)
	}
	slowClock(m)
	x, y := at(t, m, "cannot read rules")
	tap(t, m, x, y)
	if m.table != tableSecrets {
		t.Fatalf("a press on the error line left the keys in table %d, want the secrets", m.table)
	}

	// The next read tries again.
	ds.mu.Lock()
	ds.rulesErr = nil
	ds.mu.Unlock()
	send(t, m, keyPress(secretsKey), keyPress(secretsKey))
	if rows = plainRows(m); !strings.Contains(rows[titleRow(rows, "Rules")], "3 rules") {
		t.Fatalf("the rules did not come back with the next read:\n%s", strings.Join(rows, "\n"))
	}
}

// A rules table that loses its last row keeps the keys on its empty line,
// however it lost it, and its key line offers only what that line does —
// making a rule — never a rule's keys that would do nothing.
func TestARulesTableThatHasNoRowLeftOffersOnlyANewRule(t *testing.T) {
	t.Parallel()
	m, ds := rulesFixture(t)
	onRule(t, m, "rule_1")
	// Deleted somewhere else, then opened here.
	ds.mu.Lock()
	ds.rules = nil
	ds.mu.Unlock()
	send(t, m, keyPress("enter"))
	if m.dialog != nil || m.table != tableRules || len(m.rules.all) != 0 {
		t.Fatalf("dialog = %s, table = %d with %d rules; want the empty rules and no card", describe(m.dialog), m.table, len(m.rules.all))
	}
	if frame := plainFrame(m); strings.Contains(frame, "enter read it") || !strings.Contains(frame, "n new rule") {
		t.Fatalf("the key line offers a rule's keys, or not a new one:\n%s", frame)
	}
	// A listing that failed has no line to act on, and gives the keys back.
	ds.mu.Lock()
	ds.rulesErr = errors.New("the server is gone")
	ds.mu.Unlock()
	send(t, m, keyPress("r"))
	if m.table != tableSecrets {
		t.Fatalf("table = %d, want the secrets once the rules cannot be read", m.table)
	}
}

// A table scrolled to its cursor in a short window is not left scrolled when
// the window grows: the rows there is now room for are drawn.
func TestAGrownWindowShowsTheRulesItHasRoomFor(t *testing.T) {
	t.Parallel()
	m, _ := rulesFixture(t)
	onRule(t, m, "rule_3")
	send(t, m, tea.WindowSizeMsg{Width: 120, Height: 16})
	send(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	frame := plainFrame(m)
	for _, want := range []string{"gitlab.example.com/team/models", "every discobox", "github.com/org/repo, github.com/o"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("the grown window does not draw the rule with %q:\n%s", want, frame)
		}
	}
}
