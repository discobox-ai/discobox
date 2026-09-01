package tui

import (
	"strings"
	"testing"
)

// promptWith puts a model in front of the composer with text in it and the
// cursor at the end, which is where every one of these starts.
func promptWith(t *testing.T, text string) *Model {
	t.Helper()
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	m.prompt.SetValue(text)
	m.promptEnd()
	m.edits.reset()
	return m
}

func press(t *testing.T, m *Model, specs ...string) {
	t.Helper()
	for _, spec := range specs {
		send(t, m, key(spec))
	}
}

// Ctrl-← and Ctrl-→ are what a terminal sends for word motion, and what the
// distributed inputrc files bind. Bubbles' single-line input has had them all
// along; the textarea the composer is built on has not, which is why they
// worked in a dialog and did nothing here.
func TestCtrlArrowsMoveByWord(t *testing.T) {
	m := promptWith(t, "one two three")

	press(t, m, "ctrl+left")
	if got := m.prompt.Column(); got != 8 {
		t.Errorf("ctrl+left put the cursor at %d, want the start of \"three\" at 8", got)
	}
	press(t, m, "ctrl+left")
	if got := m.prompt.Column(); got != 4 {
		t.Errorf("ctrl+left put the cursor at %d, want the start of \"two\" at 4", got)
	}
	press(t, m, "ctrl+right")
	if got := m.prompt.Column(); got != 7 {
		t.Errorf("ctrl+right put the cursor at %d, want the end of \"two\" at 7", got)
	}
	// The emacs spellings are still there for the terminals where Option is
	// Meta; nothing was traded away for the arrows.
	press(t, m, "alt+b")
	if got := m.prompt.Column(); got != 4 {
		t.Errorf("alt+b put the cursor at %d, want 4", got)
	}
}

// A kill is not a delete: readline puts what it took where Ctrl-Y can bring it
// back from, and that is the half of the muscle memory the composer used to
// drop on the floor.
func TestKillAndYank(t *testing.T) {
	m := promptWith(t, "one two three")

	press(t, m, "ctrl+a", "ctrl+k")
	if got := m.prompt.Value(); got != "" {
		t.Fatalf("prompt = %q, want ctrl+k to have taken the line", got)
	}
	press(t, m, "ctrl+y")
	if got := m.prompt.Value(); got != "one two three" {
		t.Fatalf("prompt = %q, want ctrl+y to have put the line back", got)
	}
	// Yanked text lands at the cursor, and the ring keeps it for the next one.
	press(t, m, "ctrl+a", "ctrl+y")
	if got := m.prompt.Value(); got != "one two threeone two three" {
		t.Fatalf("prompt = %q, want a second yank at the front", got)
	}
}

// Consecutive kills accumulate into one entry, in the order the text was in.
// Taking a line apart a word at a time and putting the whole of it back with
// one Ctrl-Y is the reason the ring exists.
func TestConsecutiveKillsAccumulate(t *testing.T) {
	m := promptWith(t, "one two three")

	press(t, m, "ctrl+w", "ctrl+w")
	if got := m.prompt.Value(); got != "one " {
		t.Fatalf("prompt = %q, want two words killed", got)
	}
	press(t, m, "ctrl+y")
	if got := m.prompt.Value(); got != "one two three" {
		t.Fatalf("prompt = %q, want both kills back in the order they were in", got)
	}

	// A key that is not a kill ends the run, so the next one starts a fresh
	// entry rather than joining what came before it.
	press(t, m, "ctrl+a", "ctrl+e", "ctrl+w")
	press(t, m, "ctrl+y")
	if got := m.prompt.Value(); got != "one two three" {
		t.Fatalf("prompt = %q, want only the last kill yanked back", got)
	}
	if m.edits.kill != "three" {
		t.Errorf("kill ring = %q, want only the kill after the motion", m.edits.kill)
	}
}

// Backspace is a typo, not a kill: what it takes never reaches the ring, so a
// stray one cannot cost you the line you meant to move.
func TestBackspaceIsNotAKill(t *testing.T) {
	m := promptWith(t, "one two")

	press(t, m, "ctrl+w")
	press(t, m, "backspace")
	if got := m.prompt.Value(); got != "one" {
		t.Fatalf("prompt = %q", got)
	}
	press(t, m, "ctrl+y")
	if got := m.prompt.Value(); got != "onetwo" {
		t.Errorf("prompt = %q, want the ring to still hold the killed word", got)
	}
}

func TestYankWithAnEmptyRingSaysSo(t *testing.T) {
	m := promptWith(t, "one")

	press(t, m, "ctrl+y")
	if got := m.prompt.Value(); got != "one" {
		t.Errorf("prompt = %q, want nothing yanked", got)
	}
	if !strings.Contains(frameText(m), "nothing to yank") {
		t.Errorf("the window should say the ring is empty:\n%s", frameText(m))
	}
}

// Undo walks back a change at a time, and a run of typing is one change: an
// undo that gave a word back a letter at a time is not one anybody wants.
func TestUndoWalksBackAChangeAtATime(t *testing.T) {
	m := promptWith(t, "")

	send(t, m, typeString("reap the pool")...)
	press(t, m, "ctrl+w")
	if got := m.prompt.Value(); got != "reap the " {
		t.Fatalf("prompt = %q", got)
	}

	press(t, m, "ctrl+_")
	if got := m.prompt.Value(); got != "reap the pool" {
		t.Fatalf("prompt = %q, want the kill undone", got)
	}
	press(t, m, "ctrl+_")
	if got := m.prompt.Value(); got != "" {
		t.Fatalf("prompt = %q, want the whole typed run undone at once", got)
	}
	press(t, m, "ctrl+_")
	if !strings.Contains(frameText(m), "nothing to undo") {
		t.Errorf("the window should say there is nothing left:\n%s", frameText(m))
	}
}

// Undo puts the cursor back where it stood when the change was made, not where
// you have since wandered to.
func TestUndoRestoresTheCursor(t *testing.T) {
	m := promptWith(t, "one two three")

	press(t, m, "ctrl+w")
	press(t, m, "ctrl+a")
	press(t, m, "ctrl+_")
	if got := m.prompt.Value(); got != "one two three" {
		t.Fatalf("prompt = %q", got)
	}
	if got := m.prompt.Column(); got != 13 {
		t.Errorf("cursor at %d, want 13 — where it stood when the kill happened", got)
	}
}

// A prompt that has been run is gone, and undo must not reach behind it into
// something already sent.
func TestUndoDoesNotReachBehindARun(t *testing.T) {
	m := promptWith(t, "")

	send(t, m, typeString("reap the pool")...)
	send(t, m, createdMsg{req: RunRequest{Detach: true}, sandbox: testSandboxes()[0]})
	if got := m.prompt.Value(); got != "" {
		t.Fatalf("prompt = %q, want it cleared by the run", got)
	}
	press(t, m, "ctrl+_")
	if got := m.prompt.Value(); got != "" {
		t.Errorf("prompt = %q, want the run to have ended the history", got)
	}
}

// Alt-T drags the word before the cursor past the word after it, the way it
// does at a shell — and at the end of a line it takes the last two words,
// since there is no word ahead to be dragged past.
func TestTransposeWords(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		col   int
		want  string
		at    int
	}{
		{name: "at the end", value: "reap the pool", col: 13, want: "reap pool the", at: 13},
		{name: "between the two", value: "reap the pool", col: 8, want: "reap pool the", at: 13},
		{name: "one word only", value: "reap", col: 4, want: "reap", at: 4},
		{name: "punctuation rides along", value: "a b, c", col: 6, want: "a c b,", at: 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := promptWith(t, tc.value)
			m.prompt.SetCursorColumn(tc.col)
			press(t, m, "alt+t")
			if got := m.prompt.Value(); got != tc.want {
				t.Fatalf("prompt = %q, want %q", got, tc.want)
			}
			if got := m.prompt.Column(); got != tc.at {
				t.Errorf("cursor at %d, want %d", got, tc.at)
			}
		})
	}
}

// The window's own keys still come first. Ctrl-D on an empty prompt quits and
// Enter runs, and no readline key was allowed to take either of them.
func TestTheWindowsKeysStillWinInTheComposer(t *testing.T) {
	m := promptWith(t, "one two")

	press(t, m, "ctrl+a", "ctrl+k")
	if got := m.prompt.Value(); got != "" {
		t.Fatalf("prompt = %q", got)
	}
	// Ctrl-D on what is now an empty prompt is the shell's EOF, not the
	// textarea's delete-forward.
	if cmd := m.updateKey(key("ctrl+d")); cmd == nil {
		t.Error("ctrl+d on an empty prompt should still close the window")
	}
}

func TestRemovedIsTheSpanThatWentAway(t *testing.T) {
	for _, tc := range []struct{ before, after, want string }{
		{"one two", "one ", "two"},
		{"one two", " two", "one"},
		{"öne twö", "öne ", "twö"},
		{"one\ntwo", "onetwo", "\n"},
		{"one", "one", ""},
	} {
		if got := removed(tc.before, tc.after); got != tc.want {
			t.Errorf("removed(%q, %q) = %q, want %q", tc.before, tc.after, got, tc.want)
		}
	}
}
