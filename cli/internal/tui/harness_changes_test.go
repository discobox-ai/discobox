package tui

import (
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestHarnessChangesReviewSavesDisplayedCandidate(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.harnessChanges = HarnessChanges{CandidateID: "reviewed", State: "pending", Files: []HarnessChangedFile{{Path: ".claude/settings.json", Action: "update", Diff: "@@ -1 +1 @@\n-{\"theme\":\"dark\"}\n+{\"theme\":\"light\"}"}}, Credentials: []string{"anthropic ANTHROPIC_API_KEY (oauth)"}}
	m := newTestModel(t, ds)
	send(t, m, keyPress("tab"), keyPress("H"))
	card := dialogText(m)
	if !strings.Contains(card, `+{"theme":"light"}`) || !strings.Contains(card, `-{"theme":"dark"}`) || !strings.Contains(card, "update .claude/settings.json") || !strings.Contains(card, "ANTHROPIC_API_KEY") {
		t.Fatalf("review did not show the settings diff and credential identities:\n%s", card)
	}
	ds.mu.Lock()
	ds.harnessChanges.CandidateID = "new-login"
	ds.mu.Unlock()
	send(t, m, keyPress("s"))
	if len(ds.harnessChangesWrites) != 1 || ds.harnessChangesWrites[0] != "save sbx_one reviewed" {
		t.Fatalf("writes=%v", ds.harnessChangesWrites)
	}
}

func TestHarnessDiagnosticOnlyCanBeDiscardedButNotSaved(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.harnessChanges = HarnessChanges{CandidateID: "diagnostic", State: "pending", Diagnostics: []string{"unsupported local configuration"}}
	m := newTestModel(t, ds)
	send(t, m, keyPress("tab"), keyPress("H"))
	if m.dialog == nil || !strings.Contains(m.dialog.body, "No saveable") {
		t.Fatal("missing diagnostic-only explanation")
	}
	send(t, m, keyPress("s"))
	if len(ds.harnessChangesWrites) != 0 {
		t.Fatal("saved diagnostic-only candidate")
	}
	send(t, m, keyPress("d"))
	if len(ds.harnessChangesWrites) != 1 || ds.harnessChangesWrites[0] != "discard sbx_one diagnostic" {
		t.Fatalf("writes=%v", ds.harnessChangesWrites)
	}
}

func TestHarnessChangesRefreshIsExplicit(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.harnessChanges = HarnessChanges{CandidateID: "old", State: "pending", Files: []HarnessChangedFile{{Path: "old.json", Action: "add", Diff: "@@ -0,0 +1 @@\n+old"}}}
	m := newTestModel(t, ds)
	send(t, m, keyPress("tab"), keyPress("H"))
	ds.mu.Lock()
	ds.harnessChanges = HarnessChanges{CandidateID: "new", State: "pending", Files: []HarnessChangedFile{{Path: "new.json", Action: "add", Diff: "@@ -0,0 +1 @@\n+new"}}}
	ds.mu.Unlock()
	send(t, m, keyPress("r"))
	if !strings.Contains(dialogText(m), "new.json") {
		t.Fatal("refresh did not show new candidate")
	}
	send(t, m, keyPress("s"))
	if len(ds.harnessChangesWrites) != 1 || ds.harnessChangesWrites[0] != "save sbx_one new" {
		t.Fatalf("writes=%v", ds.harnessChangesWrites)
	}
}

func TestWorkspaceLeaderOpensHarnessReview(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		ds.harnessChanges = HarnessChanges{CandidateID: "workspace-review", State: "pending", Files: []HarnessChangedFile{{Path: "settings.json", Action: "add", Diff: "@@ -0,0 +1 @@\n+{}"}}}
		d, m, term := openWorkspace(t, ds, "enter")
		d.key("ctrl+a")
		d.key("H")
		d.wait("harness review", func() bool { return m.dialog != nil && strings.Contains(m.dialog.title, "Harness changes") })
		if got := term.typed("H"); strings.Contains(got, "H") {
			t.Fatalf("review key reached harness: %q", got)
		}
		d.key("d")
		d.wait("discarded candidate", func() bool { ds.mu.Lock(); defer ds.mu.Unlock(); return len(ds.harnessChangesWrites) == 1 })
		ds.mu.Lock()
		defer ds.mu.Unlock()
		if ds.harnessChangesWrites[0] != "discard sbx_one workspace-review" {
			t.Fatalf("writes=%v", ds.harnessChangesWrites)
		}
	})
}

// Every settings field remains reachable above the fixed save/discard actions.
// Reading a long candidate must not silently truncate what the user will save.
func TestHarnessReviewScrollsEverySettingsField(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	var content strings.Builder
	for n := 0; n < 100; n++ {
		content.WriteString("+field-")
		content.WriteString(itoa(n))
		content.WriteString(": value\n")
	}
	ds.harnessChanges = HarnessChanges{CandidateID: "long-review", State: "pending", Files: []HarnessChangedFile{{Path: "settings.json", Action: "add", Diff: "@@ -0,0 +1,100 @@\n" + content.String()}}}
	m := newTestModel(t, ds)
	send(t, m, keyPress("tab"), keyPress("H"))
	var seen strings.Builder
	for n := 0; n < 100; n++ {
		frame := draw(m.dialog)
		seen.WriteString(frame)
		if !strings.Contains(frame, "Save reviewed changes") || !strings.Contains(frame, "Discard candidate") {
			t.Fatalf("actions not visible:\n%s", frame)
		}
		if !strings.Contains(frame, "PgUp/PgDn to scroll") {
			t.Fatalf("scroll keys not advertised:\n%s", frame)
		}
		if _, closed := m.dialog.update(keyPress("pgdown")); closed {
			t.Fatal("paging closed review")
		}
	}
	for n := 0; n < 100; n++ {
		if !strings.Contains(seen.String(), "field-"+itoa(n)+": value") {
			t.Errorf("field %d was never visible", n)
		}
	}
	send(t, m, keyPress("s"))
	if len(ds.harnessChangesWrites) != 1 || ds.harnessChangesWrites[0] != "save sbx_one long-review" {
		t.Fatalf("writes=%v", ds.harnessChangesWrites)
	}
}

func TestHarnessSaveFailureIsReportedWithoutRetry(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.harnessChanges = HarnessChanges{CandidateID: "reviewed", State: "pending", Files: []HarnessChangedFile{{Path: "settings.json", Action: "update"}}}
	ds.harnessChangesSaveErr = errors.New("upgrade or recreate the sandbox")
	m := newTestModel(t, ds)
	send(t, m, keyPress("tab"), keyPress("H"), keyPress("s"))
	if text := dialogText(m); !strings.Contains(text, "upgrade or recreate the sandbox") || strings.Contains(text, "Overwrite") {
		t.Fatalf("wrong recovery dialog: %s", text)
	}
	send(t, m, keyPress("enter"))
	if len(ds.harnessChangesWrites) != 1 || ds.harnessChangesWrites[0] != "save sbx_one reviewed" {
		t.Fatalf("unexpected retry: %v", ds.harnessChangesWrites)
	}
}

// pendingSandboxes is the fixture's listing with its first box reporting a
// harness change to save.
func pendingSandboxes() []Sandbox {
	boxes := testSandboxes()
	boxes[0].HarnessName = "Claude Code"
	boxes[0].HarnessConfigPending, boxes[0].HarnessConfigCandidate = true, "cand_1"
	return boxes
}

// The band is the offer's green, not the alert's red: it says something can
// be saved, not that something is wrong.
func TestTheHarnessChangesBandIsGreen(t *testing.T) {
	t.Parallel()
	m, _ := workspaceWithHarnessChanges(t)
	m.st = newStyles(true)

	row := m.viewBanner(140)
	if !strings.Contains(row, "\x1b[48;5;"+colOfferBG+"m") || strings.Contains(row, "\x1b[48;5;"+colAlertBG+"m") {
		t.Fatalf("band = %q, want it painted in the offer's green and not the alert's red", row)
	}
	plain := ansi.Strip(row)
	for _, want := range []string{"Claude Code settings or login changed in this box", "click to review and save", "✕"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("band = %q, want %q", plain, want)
		}
	}
	if offerPalette.bg == alertPalette.bg || offerPalette.mark == alertPalette.mark {
		t.Fatal("the offer's palette is the alert's")
	}
}

// Pressing the band opens the review, where Save takes the change.
func TestPressingTheHarnessChangesBandOpensTheReview(t *testing.T) {
	t.Parallel()
	m, ds := workspaceWithHarnessChanges(t)
	ds.harnessChanges = HarnessChanges{CandidateID: "cand_1", State: "pending", Files: []HarnessChangedFile{{Path: ".claude/settings.json", Action: "update", Diff: "@@ -1 +1 @@\n-a\n+b"}}}
	m.viewBanner(140)

	drain(t, m, m.pressBanner(), 0)
	if m.dialog == nil || !strings.Contains(m.dialog.title, "Harness changes") {
		t.Fatalf("dialog = %s, want the review", describe(m.dialog))
	}
	send(t, m, keyPress("s"))
	if len(ds.harnessChangesWrites) != 1 || ds.harnessChangesWrites[0] != "save "+m.paneBox.ID+" cand_1" {
		t.Fatalf("writes = %v, want the reviewed candidate saved", ds.harnessChangesWrites)
	}
	// Saved, the band goes at once rather than when the box next reports,
	// though the listing still names the candidate.
	if got := m.bannerShowing(); got != bannerNone {
		t.Fatalf("banner = %v after the save, want it gone", got)
	}
}

// The first listing with a box that has a change to save explains saving,
// once, and records on the project that it has.
func TestTheFirstHarnessChangeIsExplainedOnce(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(pendingSandboxes()...)
	m := newTestModel(t, ds)
	m.explainHarnessChanges = true
	send(t, m, tickMsg{})

	if m.dialog == nil || m.dialog.title != harnessChangesExplainTitle {
		t.Fatalf("dialog = %s, want the explainer", describe(m.dialog))
	}
	for _, want := range []string{"Claude Code", "Nothing is shared until you save", "shared default every new launch", "same login"} {
		if !strings.Contains(m.dialog.body, want) {
			t.Fatalf("body = %q, want %q", m.dialog.body, want)
		}
	}
	if ds.explained != 1 {
		t.Fatalf("explained = %d, want it recorded once", ds.explained)
	}

	pastKeyQuiet(m)
	send(t, m, keyPress("n"))
	if m.dialog != nil {
		t.Fatalf("dialog = %s, want Not now to close it", describe(m.dialog))
	}
	send(t, m, tickMsg{})
	if m.dialog != nil || ds.explained != 1 {
		t.Fatalf("dialog = %s explained = %d, want it explained only once", describe(m.dialog), ds.explained)
	}
}

// Review changes goes from the explainer straight to the box's review, which
// holds its keys as the explainer did: typing that answered the explainer
// ("r" of "run tests") must not go on to save ("s"). Nor, once the hold has
// passed, does a letter or a bare Enter save: Save is chosen with the arrows.
func TestTheExplainerOpensTheReview(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(pendingSandboxes()...)
	ds.harnessChanges = HarnessChanges{CandidateID: "cand_1", State: "pending", Credentials: []string{"anthropic CLAUDE_CODE_OAUTH_TOKEN (oauth)"}}
	m := newTestModel(t, ds)
	m.explainHarnessChanges = true
	send(t, m, tickMsg{})
	pastKeyQuiet(m)
	send(t, m, keyPress("r"))

	if m.dialog == nil || !strings.Contains(m.dialog.title, "Harness changes") {
		t.Fatalf("dialog = %s, want the review", describe(m.dialog))
	}
	send(t, m, keyPress("s"))
	if len(ds.harnessChangesWrites) != 0 || m.dialog == nil {
		t.Fatalf("writes = %v, want a key typed into the review's hold swallowed", ds.harnessChangesWrites)
	}
	pastKeyQuiet(m)
	send(t, m, keyPress("s"))
	if len(ds.harnessChangesWrites) != 0 || m.dialog == nil {
		t.Fatalf("writes = %v, want no one-letter save on a review the explainer opened", ds.harnessChangesWrites)
	}
	send(t, m, keyPress("d"))
	if len(ds.harnessChangesWrites) != 0 || m.dialog == nil {
		t.Fatalf("writes = %v, want no one-letter discard on a review the explainer opened", ds.harnessChangesWrites)
	}
	if it := m.dialog.items[m.dialog.cursor]; it.key != "r" {
		t.Fatalf("cursor on %q, want Enter to land on Refresh, not Save", it.label)
	}
	// Enter refreshes, and the refreshed card is the same kind: held, on
	// Refresh, no letter that commits — so Enter after Enter never saves.
	send(t, m, keyPress("enter"))
	pastKeyQuiet(m)
	send(t, m, keyPress("enter"), keyPress("s"), keyPress("d"))
	pastKeyQuiet(m)
	send(t, m, keyPress("enter"))
	if len(ds.harnessChangesWrites) != 0 || m.dialog == nil || m.dialog.items[m.dialog.cursor].key != "r" {
		t.Fatalf("writes = %v dialog = %s, want refreshes to keep the review unasked", ds.harnessChangesWrites, describe(m.dialog))
	}
	pastKeyQuiet(m)
	send(t, m, keyPress("up"), keyPress("up"), keyPress("enter"))
	if len(ds.harnessChangesWrites) != 1 || ds.harnessChangesWrites[0] != "save sbx_one cand_1" {
		t.Fatalf("writes = %v, want Save chosen with the arrows to save", ds.harnessChangesWrites)
	}
}

// A project that has explained it already, or a listing with nothing to save,
// shows nothing.
func TestTheExplainerWaitsForAReason(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(pendingSandboxes()...)
	m := newTestModel(t, ds)
	send(t, m, tickMsg{})
	if m.dialog != nil {
		t.Fatalf("dialog = %s, want nothing on a project that has explained it", describe(m.dialog))
	}

	ds = newFakeSource(testSandboxes()...)
	m = newTestModel(t, ds)
	m.explainHarnessChanges = true
	send(t, m, tickMsg{})
	if m.dialog != nil || ds.explained != 0 {
		t.Fatalf("dialog = %s explained = %d, want nothing without a change to save", describe(m.dialog), ds.explained)
	}
}

// The explainer never goes up over another dialog: it waits,
// and comes on the first listing once the screen is free.
func TestTheExplainerWaitsForAFreeScreen(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(pendingSandboxes()...)
	m := newTestModel(t, ds)
	m.explainHarnessChanges = true
	m.dialog = errorDialog("Something else", "already up")
	send(t, m, tickMsg{})
	if m.dialog.title != "Something else" || ds.explained != 0 {
		t.Fatalf("dialog = %s, want the explainer to wait behind another dialog", describe(m.dialog))
	}

	m.dialog = nil
	send(t, m, tickMsg{})
	if m.dialog == nil || m.dialog.title != harnessChangesExplainTitle {
		t.Fatalf("dialog = %s, want the explainer once the screen is free", describe(m.dialog))
	}
}

// It comes up over the terminal that has the keys — the one the person just
// signed in at, the bare attach window's — rather than waiting for them to
// leave it, which in that window they may never do.
func TestTheExplainerComesUpOverATerminalWithTheKeys(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		boxes := pendingSandboxes()
		ds := newFakeSource(boxes...)
		d, m, _ := openWorkspace(t, ds, "enter")
		d.wait("the workspace", func() bool { return m.inPanes() })
		m.explainHarnessChanges = true
		if cmd := m.explainHarnessChangesIfOwed(); cmd == nil || m.dialog == nil || m.dialog.title != harnessChangesExplainTitle {
			t.Fatalf("dialog = %s, want the explainer over the terminal", describe(m.dialog))
		}
	})
}

// Its answers are single letters, and it comes up over typing: a key pressed
// before the keyboard has gone quiet is swallowed, and each one starts the
// quiet over, so a "n" typed into a sign-in never spends the explanation.
func TestTheExplainerTakesNoKeyUntilTypingStops(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(pendingSandboxes()...)
	m := newTestModel(t, ds)
	at := m.now()
	m.now = func() time.Time { return at }
	m.explainHarnessChanges = true
	send(t, m, tickMsg{})
	if m.dialog == nil || m.dialog.title != harnessChangesExplainTitle {
		t.Fatalf("dialog = %s, want the explainer", describe(m.dialog))
	}
	for range 3 {
		at = at.Add(keyQuiet / 2)
		send(t, m, keyPress("n"))
		if m.dialog == nil || m.dialog.title != harnessChangesExplainTitle {
			t.Fatalf("dialog = %s, want a key typed into the hold swallowed", describe(m.dialog))
		}
	}
	at = at.Add(keyQuiet)
	send(t, m, keyPress("n"))
	if m.dialog != nil {
		t.Fatalf("dialog = %s, want Not now answered once the keyboard was quiet", describe(m.dialog))
	}
}

// What the held card swallowed is not lost: it goes on to the terminal the
// card came up over, the leader excepted, so the first words typed after a
// sign-in still reach the harness.
func TestTheExplainerPassesHeldKeysToTheTerminal(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(pendingSandboxes()...)
		d, m := openAttached(t, ds, ds.sandboxes[0])
		d.wait("the primary terminal", func() bool { return m.primary() != nil && m.dialog == nil })
		term := ds.execTerm(ExecPrimary)
		m.explainHarnessChanges = true
		d.run(m.explainHarnessChangesIfOwed())
		if m.dialog == nil || m.dialog.title != harnessChangesExplainTitle {
			t.Fatalf("dialog = %s, want the explainer over the terminal", describe(m.dialog))
		}
		for _, key := range []string{"n", "o", m.leader(), "enter"} {
			d.key(key)
		}
		d.wait("the held keys typed at the terminal", func() bool { return term.inputSoFar() == "no\r" })
		if m.dialog == nil || m.dialog.title != harnessChangesExplainTitle {
			t.Fatalf("dialog = %s, want the explainer still up", describe(m.dialog))
		}
	})
}

// In the composer a held key that edits the prompt goes into it; Enter, which
// would send a prompt the card hides, does not.
func TestTheExplainerPassesHeldEditsToThePrompt(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(pendingSandboxes()...)
	m := newTestModel(t, ds)
	at := m.now()
	m.now = func() time.Time { return at }
	m.explainHarnessChanges = true
	send(t, m, tickMsg{})
	if m.focus != focusPrompt || m.dialog == nil || m.dialog.title != harnessChangesExplainTitle {
		t.Fatalf("focus = %v dialog = %s, want the explainer over the composer", m.focus, describe(m.dialog))
	}
	send(t, m, keyPress("h"), keyPress("i"), keyPress("enter"))
	if got := m.prompt.Value(); got != "hi" || m.submitting {
		t.Fatalf("prompt = %q submitting = %v, want the edits and not the Enter", got, m.submitting)
	}
}

// pastKeyQuiet moves m's clock past an unasked card's hold on keys, for a
// test that answers it.
func pastKeyQuiet(m *Model) {
	at := m.now().Add(keyQuiet)
	m.now = func() time.Time { return at }
}

func TestAnEmptyCurrentHarnessDiffSuppressesBandAndExplainer(t *testing.T) {
	m, _ := workspaceWithHarnessChanges(t)
	m.explainHarnessChanges = true
	for _, pending := range []bool{false, true, false, true} {
		boxes := m.list.all
		boxes[0].HarnessConfigPending = pending
		m.list.setAll(boxes)
		m.paneBox = boxes[0]
		want := bannerNone
		if pending {
			want = bannerHarnessChanges
		}
		if got := m.bannerShowing(); got != want {
			t.Fatalf("pending=%v banner=%v want=%v", pending, got, want)
		}
		if _, got := m.harnessChangesToExplain(); got != pending {
			t.Fatalf("pending=%v explainer=%v", pending, got)
		}
		if len(m.dismissed) != 0 {
			t.Fatal("checking current diff dismissed the immutable candidate")
		}
	}
}
