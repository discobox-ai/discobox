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

func TestControlByte(t *testing.T) {
	for name, want := range map[string]byte{
		"ctrl+a": 0x01,
		"ctrl+d": 0x04,
		"ctrl+z": 0x1a,
		"a":      0,
		"ctrl+5": 0,
		"ctrl+":  0,
		"":       0,
	} {
		if got := ControlByte(name); got != want {
			t.Errorf("ControlByte(%q) = %#x, want %#x", name, got, want)
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
