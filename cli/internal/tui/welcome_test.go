package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// newWelcomeModel is the window as a first-time user gets it: opened on the
// introduction, over the launcher it will reveal.
func newWelcomeModel(t *testing.T, ds DataSource) *Model {
	t.Helper()
	m := newTestModel(t, ds)
	m.welcoming = true
	return m
}

// The introduction is the whole screen, ahead of the launcher behind it.
func TestWelcomeTakesTheWindow(t *testing.T) {
	m := newWelcomeModel(t, newFakeSource(testSandboxes()...))

	view := m.View()
	if !view.AltScreen {
		t.Fatal("the introduction was drawn inline")
	}
	if !strings.Contains(view.Content, welcomeTitle) {
		t.Fatalf("the introduction is not on screen:\n%s", view.Content)
	}
	if strings.Contains(view.Content, "fix flaky pool reaper tests") {
		t.Fatal("the listing was drawn behind the introduction")
	}
	if !strings.Contains(view.Content, welcomeFooter) {
		t.Fatal("the introduction does not say how to leave it")
	}
}

// Enter is the way out, and it says so on the server: the next window on this
// project opens on the launcher.
func TestWelcomeIsDismissedByEnterAndRecorded(t *testing.T) {
	fake := newFakeSource(testSandboxes()...)
	m := newWelcomeModel(t, fake)

	send(t, m, keyPress("enter"))
	if m.welcoming {
		t.Fatal("Enter did not dismiss the introduction")
	}
	if fake.welcomed != 1 {
		t.Fatalf("the welcome was recorded %d times, want once", fake.welcomed)
	}
	// A prefix, since the row ellipsizes the name at this width.
	if view := m.View(); !strings.Contains(view.Content, "fix flaky pool") {
		t.Fatalf("the launcher was not revealed:\n%s", view.Content)
	}
}

// Every other key is swallowed. A press aimed at the introduction must not
// arrive at a screen the user has not seen yet — typing into a prompt that is
// not on screen is how a first character goes missing.
func TestWelcomeSwallowsEveryOtherKey(t *testing.T) {
	fake := newFakeSource(testSandboxes()...)
	m := newWelcomeModel(t, fake)

	send(t, m, keyPress("tab"), tea.KeyPressMsg{Code: 'x', Text: "x"}, keyPress("esc"))
	if !m.welcoming {
		t.Fatal("the introduction was dismissed by something other than Enter")
	}
	if fake.welcomed != 0 {
		t.Fatal("the welcome was recorded without being dismissed")
	}
	if m.prompt.Value() != "" {
		t.Fatalf("a key reached the composer behind it: %q", m.prompt.Value())
	}
	if m.focus != focusPrompt {
		t.Fatal("a key moved the focus behind the introduction")
	}
}

// Ctrl-C still quits: a screen you cannot leave without answering is not what
// an introduction is.
func TestWelcomeDoesNotTrapCtrlC(t *testing.T) {
	m := newWelcomeModel(t, newFakeSource(testSandboxes()...))

	send(t, m, keyPress("ctrl+c"))
	if !m.quit {
		t.Fatal("ctrl+c did not close the window from the introduction")
	}
}

// The write is behind the screen, not in front of it: the user has read the
// welcome whether or not the server heard about it, so a failure is reported
// and the window carries on.
func TestWelcomeSurvivesAFailedWrite(t *testing.T) {
	fake := newFakeSource(testSandboxes()...)
	fake.welcomeErr = errors.New("the server said no")

	m := newWelcomeModel(t, fake)
	send(t, m, keyPress("enter"))

	if m.welcoming {
		t.Fatal("a failed write held the introduction on screen")
	}
	if !m.statusE || !strings.Contains(m.status, "welcome") {
		t.Fatalf("status = %q (error %v), want the write reported", m.status, m.statusE)
	}
}

// WithWelcome is how the CLI opens the window on it, and it composes with the
// screen the window was going to open on rather than replacing it.
func TestWithWelcomeOpensOverTheScreenItWasGiven(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := New(t.Context(), newFakeSource(), WithWelcome(), WithHarnesses())
	if !m.welcoming || !m.harnessesOpen {
		t.Fatalf("welcoming = %v, harnessesOpen = %v; want both", m.welcoming, m.harnessesOpen)
	}
}
