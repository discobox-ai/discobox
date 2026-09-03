package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// The prompt survives the window. What was in the composer when it closed is
// what the next window in that folder opens holding, because a prompt is
// usually the most considered thing on the screen and closing a terminal is
// not a decision to throw one away.
func TestThePromptComesBackWithTheSession(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.session.Draft = "finish the pool reaper\nand test it"
	m := newTestModel(t, ds)

	if got := m.prompt.Value(); got != ds.session.Draft {
		t.Fatalf("prompt = %q, want the draft back", got)
	}
	// The cursor is where you left off, not in front of what you wrote.
	if got, want := m.prompt.Line(), m.prompt.LineCount()-1; got != want {
		t.Errorf("cursor on line %d, want the last line %d", got, want)
	}
	if !strings.Contains(frameText(m), "the prompt you left here is back") {
		t.Errorf("the window should say why there is text in the field:\n%s", frameText(m))
	}
	// It is a draft, not a save: Enter still runs it, and the composer has
	// grown to hold it.
	if m.prompt.Height() != 2 {
		t.Errorf("the composer is %d rows, want the two the draft needs", m.prompt.Height())
	}
}

// The session lands a moment after the window is up, and the cursor is in the
// composer from the first frame. Anything typed in that moment is what is
// being written now, and a draft must never land on top of it.
func TestARestoredDraftNeverOverwritesWhatIsBeingTyped(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.session.Draft = "the old one"
	m := newTestModel(t, ds)
	m.prompt.SetValue("")
	m.draft = ""

	send(t, m, typeString("the new one")...)
	send(t, m, sessionLoadedMsg{session: ds.session})

	if got := m.prompt.Value(); got != "the new one" {
		t.Fatalf("prompt = %q, want what was being typed", got)
	}
}

// The draft is written on the listing's clock, so a window that is killed
// outright loses at most the last few seconds of it — and only when there is
// something new to write, so an idle window writes nothing at all.
func TestTheDraftIsSavedOnTheTick(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)

	send(t, m, tickMsg{})
	if len(ds.drafts) != 0 {
		t.Fatalf("an untouched prompt wrote %v", ds.drafts)
	}

	send(t, m, typeString("half a thought")...)
	send(t, m, tickMsg{})
	if want := []string{"/src/disco2 half a thought"}; !slices.Equal(ds.drafts, want) {
		t.Fatalf("drafts = %v, want %v", ds.drafts, want)
	}

	// Nothing has changed since, so nothing is written again.
	send(t, m, tickMsg{})
	if len(ds.drafts) != 1 {
		t.Fatalf("an unchanged prompt wrote again: %v", ds.drafts)
	}
}

// The keys that close the window save it themselves, rather than leaving it to
// a clock that will not tick again: a command batched with the quit races the
// runtime shutting down.
func TestClosingTheWindowSavesTheDraft(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	send(t, m, typeString("unsent")...)

	send(t, m, keyPress("ctrl+c"))
	if !m.quit {
		t.Fatal("ctrl+c should close the window")
	}
	if want := []string{"/src/disco2 unsent"}; !slices.Equal(ds.drafts, want) {
		t.Fatalf("drafts = %v, want %v", ds.drafts, want)
	}
}

// Deleting the draft counts too: quitting on the empty composer you were left
// with has to drop what was stored, or the window comes back holding a prompt
// you threw away.
func TestQuittingOnAnEmptiedPromptDropsTheDraft(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.session.Draft = "an old thought"
	m := newTestModel(t, ds)
	if m.prompt.Value() == "" {
		t.Fatal("this test needs the draft restored")
	}

	for range len(ds.session.Draft) {
		send(t, m, keyPress("backspace"))
	}
	// Ctrl-D is the shell's EOF, and quits on an empty prompt.
	send(t, m, keyPress("ctrl+d"))
	if !m.quit {
		t.Fatal("ctrl+d on an empty prompt should close the window")
	}
	if want := []string{"/src/disco2 "}; !slices.Equal(ds.drafts, want) {
		t.Fatalf("drafts = %v, want the draft dropped", ds.drafts)
	}
}

// A prompt that has been run is not a prompt you are still writing, so the
// draft goes with it. Otherwise every window after it would open holding a
// prompt that already has a discobox running it.
func TestRunningThePromptDropsTheDraft(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	send(t, m, typeString("build it")...)
	send(t, m, tickMsg{})

	send(t, m, createdMsg{sandbox: Sandbox{ID: "sbx_created"}, req: RunRequest{Detach: true}})
	send(t, m, tickMsg{})

	if got := ds.drafts[len(ds.drafts)-1]; got != "/src/disco2 " {
		t.Fatalf("last write = %q, want the draft dropped", got)
	}
}

// A store that cannot take the draft says so. It is the one thing the window
// cannot quietly get wrong: silence would read as saved.
func TestAFailedDraftSaveIsReported(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.draftErr = errors.New("read-only file system")
	m := newTestModel(t, ds)

	send(t, m, typeString("something")...)
	send(t, m, tickMsg{})

	if !strings.Contains(frameText(m), "cannot save the prompt") {
		t.Errorf("the window should report a draft it could not save:\n%s", frameText(m))
	}
}

// A window with no folder resolved has nothing to key a draft by, and a draft
// nothing can be keyed by is one nothing can return.
func TestNoFolderNoDraft(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.session.Directory = ""
	m := newTestModel(t, ds)

	send(t, m, typeString("nowhere")...)
	send(t, m, tickMsg{})
	send(t, m, keyPress("ctrl+c"))

	if len(ds.drafts) != 0 {
		t.Fatalf("drafts = %v, want none written", ds.drafts)
	}
}
