package tui

import (
	"strings"
	"testing"
)

// The way out is one sequence wherever you are: the workspace already quits on
// leader-q, and the discoboxes now do too.
func TestLeaderQuitsFromTheList(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m := openList(t, ds)

	d.key("ctrl+a")
	if !m.leaderArmed {
		t.Fatal("the leader did not arm on the list")
	}
	d.key(paneQuitKey)
	if !m.quit {
		t.Fatal("leader q did not quit the window")
	}
}

// Ctrl-C still quits there. It is no longer what the window advertises, but
// nothing that used to work stopped working.
func TestCtrlCStillQuitsFromTheList(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m := openList(t, ds)

	d.key("ctrl+c")
	if !m.quit {
		t.Fatal("ctrl+c no longer quits the list")
	}
}

// A mistyped leader costs nothing: the key it preceded is handled as though
// the leader had never been pressed, the way it is in a pane.
func TestAMistypedLeaderOnTheListCostsNothing(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m := openList(t, ds)
	before := m.list.cursor

	d.key("ctrl+a")
	d.key("down")
	if m.leaderArmed {
		t.Fatal("the leader stayed armed past the key it preceded")
	}
	if m.quit {
		t.Fatal("a key after the leader quit the window")
	}
	if m.list.cursor == before {
		t.Fatal("the key after the leader did nothing; it should act as itself")
	}
}

// Not in the prompt: Ctrl-A is the composer's own start-of-line, and an
// editing key is not worth a quit Ctrl-C already does there.
func TestTheLeaderDoesNotArmInThePrompt(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := New(t.Context(), ds)
	m.logo = logo{}
	d := newDriver(t, m)
	d.start()
	d.wait("the listing", func() bool { return len(m.list.rows()) > 0 })

	if m.focus != focusPrompt {
		t.Fatalf("focus = %v, want the prompt", m.focus)
	}
	d.key("ctrl+a")
	if m.leaderArmed {
		t.Fatal("the leader armed in the prompt, taking the composer's key")
	}
}

// The header offers whichever quit works where you are.
func TestTheHeaderOffersTheQuitThatWorksHere(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := New(t.Context(), ds)
	m.logo = logo{}
	d := newDriver(t, m)
	d.start()
	d.wait("the listing", func() bool { return len(m.list.rows()) > 0 })

	if got := plainFrame(m); !strings.Contains(got, "Ctrl-C quit") {
		t.Errorf("the prompt should offer Ctrl-C:\n%s", got)
	}
	d.key("tab")
	d.wait("the list", func() bool { return m.focus == focusList })
	if got := plainFrame(m); !strings.Contains(got, "ctrl+a q quit") {
		t.Errorf("the list should offer the leader:\n%s", got)
	}
}

// openList opens the window and moves the keys onto the discoboxes.
func openList(t *testing.T, ds *fakeSource) (*driver, *Model) {
	t.Helper()
	t.Setenv("NO_COLOR", "1")
	m := New(t.Context(), ds)
	m.logo = logo{}
	m.expanded = true
	d := newDriver(t, m)
	d.start()
	d.wait("the listing", func() bool { return len(m.list.rows()) > 0 })
	d.key("tab")
	d.wait("the list", func() bool { return m.focus == focusList })
	return d, m
}
