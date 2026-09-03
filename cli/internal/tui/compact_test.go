package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// newCompactModel builds the window as it actually opens, before anything has
// asked it to open out.
func newCompactModel(t *testing.T, ds DataSource) (*driver, *Model) {
	t.Helper()
	t.Setenv("NO_COLOR", "1")
	m := New(t.Context(), ds)
	m.logo = logo{rows: []string{"aa", "bb", "cc", "dd", "ee", "ff"}, width: 2}
	d := newDriver(t, m)
	d.start()
	// Both, and the session especially: it carries the folder the list is
	// filtered to, and a test that looks before it lands sees an unfiltered
	// list that is about to change under it.
	d.wait("the session and the listing", func() bool {
		return m.session.Directory != "" && m.list.all != nil
	})
	return d, m
}

// The window opens as a prompt and nothing else: no sandboxes, no alternate
// screen, and only the rows it needs.
func TestTheWindowOpensAsAPrompt(t *testing.T) {
	_, m := newCompactModel(t, newFakeSource(testSandboxes()...))

	if m.expanded {
		t.Fatal("the window should open compact")
	}
	view := m.View()
	if view.AltScreen {
		t.Fatal("the opening prompt is inline, not the alternate screen")
	}
	if got := lipgloss.Height(view.Content); got >= m.height {
		t.Fatalf("the opening window is %d rows in a %d-row terminal: it should take only what it needs", got, m.height)
	}

	// The sandboxes are not on it — that is the whole point of the size.
	frame := plainFrame(m)
	for _, row := range testSandboxes() {
		if strings.Contains(frame, row.Name) {
			t.Errorf("the opening window should not list sandboxes:\n%s", frame)
		}
	}
	// What it does have is the prompt, the mark beside it, and — because
	// nothing else on it hints that there is more behind it — the way to the
	// rest, said right above the field.
	if !strings.Contains(frame, "What should the new discobox do?") {
		t.Errorf("the opening window should be a prompt:\n%s", frame)
	}
	if !strings.Contains(frame, "aa") {
		t.Errorf("the mark should be beside the prompt:\n%s", frame)
	}
	if !strings.Contains(frame, "Tab or ↑ for the discoboxes you already have") {
		t.Errorf("the opening window should say how to reach the rest:\n%s", frame)
	}
}

// ↑ opens the window out only from an empty prompt. Having typed something,
// holding ↑ to walk back through it would otherwise throw the whole window
// open behind your own words, which is not what the key was pressed for.
func TestUpDoesNotOpenTheWindowOutWithTextInThePrompt(t *testing.T) {
	d, m := newCompactModel(t, newFakeSource(testSandboxes()...))

	for _, msg := range typeString("a plan") {
		d.dispatch(msg)
	}
	d.key("ctrl+j")
	for _, msg := range typeString("and more") {
		d.dispatch(msg)
	}
	d.settle()

	for range 4 {
		d.key("up")
		if m.expanded {
			t.Fatal("↑ should stay in a prompt that has text in it")
		}
	}
	if got := m.prompt.Value(); got != "a plan\nand more" {
		t.Fatalf("prompt = %q, want the text untouched", got)
	}
	// The frame offers the key that does work, and only that one.
	if frame := frameText(m); !strings.Contains(frame, "Tab for the discoboxes you already have") {
		t.Errorf("the opening window should offer Tab alone:\n%s", frame)
	}

	d.key("tab")
	if !m.expanded {
		t.Fatal("tab should still open the window out")
	}
}

// Reaching past the prompt is the ask for everything behind it: the window
// opens out to full screen with the sandboxes on it.
func TestReachingPastThePromptOpensTheWindowOut(t *testing.T) {
	// Down is not among them: the prompt is the bottom of the window.
	for _, key := range []string{"up", "tab"} {
		t.Run(key, func(t *testing.T) {
			d, m := newCompactModel(t, newFakeSource(testSandboxes()...))
			d.key(key)

			if !m.expanded {
				t.Fatalf("%s should open the window out", key)
			}
			if !m.View().AltScreen {
				t.Fatal("the full window takes the alternate screen")
			}
			if got := lipgloss.Height(m.View().Content); got != m.height {
				t.Fatalf("the full window is %d rows in a %d-row terminal", got, m.height)
			}
			// The name may be ellipsized at this width — the git and usage
			// columns get their cells before the name gets its tail.
			if !strings.Contains(frameText(m), "fix flaky pool") {
				t.Errorf("the sandboxes should be on it:\n%s", frameText(m))
			}
			// It lands at the top whichever key opened it out. Up means "the
			// row nearest the prompt" only once there are rows on screen to be
			// near, and opening the window out there were none.
			if m.focus != focusList {
				t.Fatalf("focus = %v, want the list", m.focus)
			}
			if m.list.cursor != 0 {
				t.Fatalf("%s landed on row %d, want the top", key, m.list.cursor)
			}
		})
	}
}

// The opening prompt is printed on the screen the window was started from, and
// everything else the window draws is on the other one. What was printed comes
// off before it goes: it stays on the primary screen otherwise, and whatever
// the window later drops back there — a harness setup that takes the terminal —
// lands in the middle of it.
func TestThePromptComesOffTheScreenBeforeTheWindowTakesIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(d *driver)
	}{
		// Both ways to the alternate screen from the opening prompt: opening
		// the window out, and a modal standing in place of it.
		{"opening out", func(d *driver) { d.dispatch(keyPress("tab")) }},
		{"a modal over it", func(d *driver) { d.dispatch(keyPress("enter")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := newFakeSource(testSandboxes()...)
			// Enter on a dirty working tree asks about it, in a modal.
			ds.workspace = SourceWorkspace{Directory: "/home/ada/src/web", Repository: true, Carries: true}
			d, m := newCompactModel(t, ds)
			if view := m.View(); view.AltScreen || view.Content == "" {
				t.Fatal("the window should open inline, with the prompt printed on it")
			}

			tc.open(d)
			d.wait("the prompt to come off the screen", func() bool { return m.View().Content == "" })
			if m.View().AltScreen {
				t.Fatal("the frame that erases the prompt has to be inline: the rows it erases are")
			}

			d.settle()
			view := m.View()
			if !view.AltScreen || view.Content == "" {
				t.Fatalf("the window should take the screen once the prompt is off it: altScreen=%v, content=%q",
					view.AltScreen, view.Content)
			}
		})
	}
}

// Typing a prompt and running it does not open the window out on its own — but
// the terminal it attaches to does, because a terminal wants the whole screen.
func TestRunningFromTheOpeningPromptOpensOutForTheTerminal(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m := newCompactModel(t, ds)

	for _, msg := range typeString("fix the reaper") {
		d.dispatch(msg)
	}
	if m.expanded {
		t.Fatal("typing should not open the window out")
	}

	d.key("enter")
	d.wait("the pane", func() bool { return m.focus == focusPane })
	if !m.expanded || !m.View().AltScreen {
		t.Fatal("a terminal takes the whole screen")
	}
}

// Once it has opened out it stays open: having asked for the sandboxes once,
// flipping the screen back and forth around them would be the window arguing
// with you.
func TestTheWindowStaysOpenOnceItHasOpenedOut(t *testing.T) {
	d, m := newCompactModel(t, newFakeSource(testSandboxes()...))
	d.key("tab")
	if !m.expanded {
		t.Fatal("tab should open the window out")
	}

	// Back to the prompt, by every route there is.
	d.key("esc")
	if m.focus != focusPrompt {
		t.Fatalf("focus = %v, want the prompt", m.focus)
	}
	if !m.expanded || !m.View().AltScreen {
		t.Fatal("the window should stay open")
	}
}

// An empty list has no rows to reach, so leaving the prompt lands on the folder
// filter — and that still opens the window out, since the filter is drawn in
// the full window's header.
func TestOpeningOutWithNothingToShow(t *testing.T) {
	d, m := newCompactModel(t, newFakeSource(Sandbox{ID: "sbx_one", Name: "one", State: StateRunning, Folder: "/src/elsewhere"}))
	d.key("tab")

	if !m.expanded {
		t.Fatal("the window should open out even with nothing to list")
	}
	if m.focus != focusFolder {
		t.Fatalf("focus = %v, want the folder filter", m.focus)
	}
}

// The opening glint runs once over the word the window is named for, and stops
// the instant there is anything to do.
func TestTheOpeningShimmerRunsOnceAndYields(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := New(t.Context(), ds)
	m.st = newStyles(true) // it only runs where there is color to run it on
	m.logo = logo{}
	m.width, m.height, m.ready = 100, 40, true
	m.layout()

	if cmd := m.startShimmer(); cmd == nil {
		t.Fatal("the glint should start")
	}
	if m.shimmer != 1 {
		t.Fatalf("shimmer = %d, want the first frame", m.shimmer)
	}
	// While it runs the word is lit: every letter carries its own color, and
	// the first character stays bare so the cursor does not land on an escape.
	lit := m.placeholder()
	if strings.HasPrefix(lit, "\x1b") {
		t.Fatal("the placeholder must not start with an escape: the cursor is placed on its first grapheme")
	}
	if !strings.Contains(lit, "\x1b[38;5;") {
		t.Fatalf("the word should be lit: %q", lit)
	}
	if ansi.Strip(lit) != "What should the new discobox do?" {
		t.Fatalf("the glint changed the words: %q", ansi.Strip(lit))
	}

	// It runs to the end on its own.
	for range shimmerFrames + 2 {
		if m.shimmer == 0 {
			break
		}
		m.advanceShimmer(shimmerTickMsg{frame: m.shimmer})
	}
	if m.shimmer != 0 {
		t.Fatalf("the glint should end on its own, stopped at %d", m.shimmer)
	}
	if got := m.placeholder(); got != "What should the new discobox do?" {
		t.Fatalf("at rest the placeholder should be plain: %q", got)
	}
}

// Typing ends it: a glint playing under your own words is a distraction.
func TestTypingEndsTheShimmer(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := New(t.Context(), ds)
	m.st = newStyles(true)
	m.logo = logo{}
	m.width, m.height, m.ready = 100, 40, true
	m.layout()
	m.startShimmer()

	m.Update(keyPress("f"))
	if m.shimmer != 0 {
		t.Fatal("the first keystroke should end the glint")
	}
	if got := m.prompt.Placeholder; got != "What should the new discobox do?" {
		t.Fatalf("the placeholder should be plain again: %q", got)
	}
	// And a tick still in flight from before does not restart it.
	if cmd := m.advanceShimmer(shimmerTickMsg{frame: 3}); cmd != nil || m.shimmer != 0 {
		t.Fatal("a stale tick should be ignored")
	}
}

// A terminal with no color has nothing to run it on.
func TestNoShimmerWithoutColor(t *testing.T) {
	_, m := newCompactModel(t, newFakeSource(testSandboxes()...))
	if cmd := m.startShimmer(); cmd != nil || m.shimmer != 0 {
		t.Fatal("there is no glint without color")
	}
	if got := m.placeholder(); got != "What should the new discobox do?" {
		t.Fatalf("placeholder = %q", got)
	}
}

// The way to everything behind the prompt is said in the very top line, laid
// into the border. That line has nothing else on it, which is what keeps a
// centered word from being squeezed out by whatever is beside it — the header
// below carries the path on one side and the keys on the other, and at 80 or
// 100 columns there is no room between them.
func TestTheOpeningHintIsInTheTopBorder(t *testing.T) {
	const hint = "Tab or ↑ for the discoboxes you already have"

	for _, width := range []int{80, 100, 120, 160} {
		d, m := newCompactModel(t, newFakeSource(testSandboxes()...))
		d.dispatch(sizeMsg(width, 40))

		top := ansi.Strip(strings.Split(rawFrame(m), "\n")[0])
		if !strings.Contains(top, hint) {
			t.Fatalf("at %d columns the hint is missing from the top line: %q", width, top)
		}
		if !strings.HasPrefix(top, "╭") || !strings.HasSuffix(top, "╮") {
			t.Fatalf("at %d columns the top line is not a border: %q", width, top)
		}
		cells := []rune(top)
		start := len([]rune(top[:strings.Index(top, hint)]))
		if want := (len(cells) - len([]rune(hint))) / 2; start < want-2 || start > want+2 {
			t.Fatalf("at %d columns the hint starts at cell %d, want about %d", width, start, want)
		}

		// Once the window has opened out the discoboxes speak for themselves.
		d.key("tab")
		if strings.Contains(plainFrame(m), hint) {
			t.Errorf("at %d columns the hint should go once the discoboxes are on screen", width)
		}
	}
}

// Up means "the row nearest the prompt" only once there are rows on screen to
// be near. Opening the window out there are none, so it lands at the top — and
// only afterwards does the last row become what Up reaches for.
func TestOpeningOutLandsAtTheTopEvenFromUp(t *testing.T) {
	d, m := newCompactModel(t, newFakeSource(testSandboxes()...))
	if len(m.list.rows()) < 2 {
		t.Fatalf("this test needs more than one row, got %d", len(m.list.rows()))
	}

	d.key("up")
	if m.list.cursor != 0 {
		t.Fatalf("opening out landed on row %d, want the top", m.list.cursor)
	}

	// Once it is open, Up is a direction into a list that is on screen, and
	// goes back to where the cursor was left.
	d.key("down")
	on := m.list.current().ID
	d.key("esc")
	d.key("up")
	if got := m.list.current().ID; got != on {
		t.Fatalf("cursor on %s, want %s — where it was left", got, on)
	}
}
