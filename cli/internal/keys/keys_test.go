package keys

import "testing"

// The leader is configurable because it is the key that collides: it has to be
// a chord nothing you run in a sandbox wants, and which that is depends on what
// you run.
func TestLeaderIsConfigurable(t *testing.T) {
	for _, given := range []string{"b", "B", "ctrl+b", " ctrl+b "} {
		got, err := NormalizeLeader(given)
		if err != nil {
			t.Fatalf("NormalizeLeader(%q): %v", given, err)
		}
		if got != "ctrl+b" {
			t.Errorf("NormalizeLeader(%q) = %q, want ctrl+b", given, got)
		}
	}
	if got, _ := NormalizeLeader(""); got != DefaultLeader {
		t.Errorf("an unset leader = %q, want %q", got, DefaultLeader)
	}

	// Ctrl-C is the application's in every terminal discobox shows, so it is
	// matched before the leader ever is and a leader bound to it would never be
	// reachable.
	if _, err := NormalizeLeader("c"); err == nil {
		t.Error("ctrl+c is the application's interrupt and cannot be the leader")
	}
	if _, err := NormalizeLeader("shift"); err == nil {
		t.Error("a leader must be a single character")
	}
	// A leader has to be a byte a terminal can actually send.
	for _, given := range []string{"5", "[", "\\"} {
		if _, err := NormalizeLeader(given); err == nil {
			t.Errorf("NormalizeLeader(%q) should be rejected: Ctrl cannot send it as a leader", given)
		}
	}
}

func TestSequence(t *testing.T) {
	for name, want := range map[string]string{
		"ctrl+a": "\x01",
		"ctrl+d": "\x04",
		"ctrl+z": "\x1a",
		"alt+g":  "\x1bg",
		"d":      "d",
		"ctrl+5": "",
		"alt+[":  "",
		"ctrl+":  "",
		"alt+":   "",
		"":       "",
	} {
		if got := string(Sequence(name)); got != want {
			t.Errorf("Sequence(%q) = %q, want %q", name, got, want)
		}
	}
}

// A Ctrl chord is not the only leader a hand can reach: Alt (Option on a Mac)
// with a letter collides with less of what runs inside a sandbox.
func TestLeaderMayBeAnAltChord(t *testing.T) {
	for _, given := range []string{"alt+g", "ALT+G", " alt+g "} {
		got, err := NormalizeLeader(given)
		if err != nil {
			t.Fatalf("NormalizeLeader(%q): %v", given, err)
		}
		if got != "alt+g" {
			t.Errorf("NormalizeLeader(%q) = %q, want alt+g", given, got)
		}
	}
	if got := Describe("alt+g"); got != "Alt-G" {
		t.Errorf("Describe(alt+g) = %q, want Alt-G", got)
	}

	// The key goes by other names on other keyboards, and the leader can be
	// written with any of them.
	for _, given := range []string{"meta+g", "option+g", "opt+g"} {
		if got, err := NormalizeLeader(given); err != nil || got != "alt+g" {
			t.Errorf("NormalizeLeader(%q) = %q, %v; want alt+g", given, got, err)
		}
	}
	// Alt is not Ctrl, so Alt-C is a leader like any other letter.
	if got, err := NormalizeLeader("alt+c"); err != nil || got != "alt+c" {
		t.Errorf("NormalizeLeader(alt+c) = %q, %v; want alt+c", got, err)
	}
	// Alt-[ is how an arrow key begins, and Alt with no letter is nothing.
	for _, given := range []string{"alt+[", "alt+", "alt+gg", "ctrl+alt+g"} {
		if _, err := NormalizeLeader(given); err == nil {
			t.Errorf("NormalizeLeader(%q) should be rejected", given)
		}
	}
}

func TestModifier(t *testing.T) {
	for name, want := range map[string]string{
		"ctrl+a": "ctrl+",
		"alt+g":  "alt+",
		"d":      "",
	} {
		if got := Modifier(name); got != want {
			t.Errorf("Modifier(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestDescribe(t *testing.T) {
	if got := Describe("ctrl+a"); got != "Ctrl-A" {
		t.Errorf("Describe(ctrl+a) = %q, want Ctrl-A", got)
	}
	if got := Describe("d"); got != "d" {
		t.Errorf("Describe(d) = %q, want d", got)
	}
}
