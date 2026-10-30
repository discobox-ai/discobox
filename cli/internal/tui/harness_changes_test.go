package tui

import (
	"strings"
	"testing"
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
}

// Every settings field remains reachable above the fixed save/discard actions.
// Reading a long candidate must not silently truncate what the user will save.
func TestHarnessReviewScrollsEverySettingsField(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	var content strings.Builder
	for n := 0; n < 100; n++ {
		content.WriteString("+field-" + itoa(n) + ": value\n")
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

func TestHarnessChangesConflictRequiresConfirmation(t *testing.T) {
	for _, confirm := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "overwrite"}[confirm], func(t *testing.T) {
			ds := newFakeSource(testSandboxes()...)
			ds.harnessChanges = HarnessChanges{CandidateID: "reviewed", State: "pending", Files: []HarnessChangedFile{{Path: "settings.json", Action: "update"}}}
			ds.harnessChangesSaveErr = ErrHarnessChangesConflict
			m := newTestModel(t, ds)
			send(t, m, keyPress("tab"), keyPress("H"), keyPress("s"))
			if !strings.Contains(dialogText(m), "Overwrite harness changes?") {
				t.Fatalf("missing confirmation: %s", dialogText(m))
			}
			if len(ds.harnessChangesWrites) != 1 || ds.harnessChangesForces[0] {
				t.Fatal("forced without confirmation")
			}
			ds.harnessChanges.CandidateID = "new-candidate"
			if confirm {
				send(t, m, keyPress("enter"))
				if len(ds.harnessChangesWrites) != 2 || ds.harnessChangesWrites[1] != "save sbx_one reviewed" || !ds.harnessChangesForces[1] {
					t.Fatalf("wrong retry: %v %v", ds.harnessChangesWrites, ds.harnessChangesForces)
				}
			} else {
				send(t, m, keyPress("esc"))
				if len(ds.harnessChangesWrites) != 1 {
					t.Fatal("saved after cancellation")
				}
			}
		})
	}
}
