// Package keys owns the leader: the one prefix key discobox reserves in a
// terminal it is showing you.
//
// There are two such terminals and they have to agree. The launcher draws a
// sandbox's terminal in a pane, where the leader carries the window's own
// commands; `disco attach` hands the real terminal over, where the leader
// carries the way back out. A leader the window uses and the attach does not
// would be two things to learn and two things to change, so neither package
// owns it and this one does.
//
// Key names here are Bubble Tea's, as [tea.KeyPressMsg.String] reports them —
// "ctrl+a", "d" — because that is the spelling the pane already matches
// against. [ControlByte] is how the same key reaches a raw byte stream.
package keys

import (
	"fmt"
	"strings"
)

const (
	// DefaultLeader is the prefix key when nothing says otherwise. It is
	// screen's, which is the one most hands already know.
	DefaultLeader = "ctrl+a"
	// LeaderEnv overrides it, for when the default collides with something you
	// run inside your sandboxes.
	LeaderEnv = "DISCOBOX_LEADER"
	// Interrupt belongs to whatever is running, always, so it can never be the
	// leader: a leader that took it would take it from every program discobox
	// ever shows.
	Interrupt = "ctrl+c"
)

// NormalizeLeader turns what a user typed into the key name the leader reserves.
//
// A bare character is the usual spelling — "x" means Ctrl-X, because a leader
// that is not a chord would be a character you could never type — and the full
// "ctrl+x" is accepted for anyone who writes it out. Empty is the default.
//
// Only a letter is allowed. The leader has to survive being turned back into
// the byte a terminal sends ([ControlByte]), and the chords that are not
// letters are the ones no one wants anyway: Ctrl-[ is Escape, which would eat
// every arrow key, and Ctrl-\ is quit.
func NormalizeLeader(leader string) (string, error) {
	leader = strings.TrimSpace(strings.ToLower(leader))
	if leader == "" {
		return DefaultLeader, nil
	}
	key := "ctrl+" + strings.TrimPrefix(leader, "ctrl+")
	if key == Interrupt {
		return "", fmt.Errorf("leader cannot be %s: that is the application's interrupt", Interrupt)
	}
	if ControlByte(key) == 0 {
		return "", fmt.Errorf("leader must be a single letter, or ctrl+ one: got %q", leader)
	}
	return key, nil
}

// ControlByte is the byte a Ctrl chord sends: "ctrl+a" is 0x01. It is 0 for
// anything else, which no normalized leader is.
//
// It is what a raw attach matches on. There the keystrokes are bytes off the
// terminal rather than the key messages a Bubble Tea pane is handed, and the
// leader has to be recognizable in both.
func ControlByte(name string) byte {
	key, ok := strings.CutPrefix(name, "ctrl+")
	if !ok || len(key) != 1 || key[0] < 'a' || key[0] > 'z' {
		return 0
	}
	return key[0] - 'a' + 1
}

// Describe spells a key name the way prose does: "ctrl+a" is "Ctrl-A".
func Describe(name string) string {
	key, ok := strings.CutPrefix(name, "ctrl+")
	if !ok {
		return name
	}
	return "Ctrl-" + strings.ToUpper(key)
}
