// Package keys owns the leader: the one prefix key discobox reserves in a
// terminal it is showing you.
//
// There are two such terminals and they have to agree. The launcher draws a
// sandbox's terminal in a pane, where the leader carries the window's own
// commands; `discobox attach` hands the real terminal over, where the leader
// carries the way back out. A leader the window uses and the attach does not
// would be two things to learn and two things to change, so neither package
// owns it and this one does.
//
// Key names here are Bubble Tea's, as [tea.KeyPressMsg.String] reports them —
// "ctrl+a", "alt+g", "d" — because that is the spelling the pane already
// matches against. [Sequence] is how the same key reaches a raw byte stream.
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

// The modifiers a leader may carry, as the key names spell them.
const (
	ctrl = "ctrl+"
	alt  = "alt+"
)

// altSpellings are the other names Alt goes by — Meta in Emacs and the
// terminals that send it, Option on a Mac's keyboard — accepted so a leader can
// be written the way its own keyboard labels the key.
var altSpellings = []string{"meta+", "option+", "opt+"}

// NormalizeLeader turns what a user typed into the key name the leader reserves.
//
// A bare character is the usual spelling — "x" means Ctrl-X, because a leader
// that is not a chord would be a character you could never type — and the full
// "ctrl+x" is accepted for anyone who writes it out. "alt+x" is the other
// chord, for when every Ctrl letter is spoken for by what runs inside: it
// collides with less, at the cost of an outer terminal that has to send Option
// as Meta on a Mac. Empty is the default.
//
// Only a letter is allowed. The leader has to survive being turned back into
// the bytes a terminal sends ([Sequence]), and the chords that are not letters
// are the ones no one wants anyway: Ctrl-[ is Escape, which would eat every
// arrow key, Ctrl-\ is quit, and Alt-[ is how every arrow key begins.
func NormalizeLeader(leader string) (string, error) {
	leader = strings.TrimSpace(strings.ToLower(leader))
	if leader == "" {
		return DefaultLeader, nil
	}
	for _, spelling := range altSpellings {
		if rest, ok := strings.CutPrefix(leader, spelling); ok {
			leader = alt + rest
		}
	}
	key := leader
	if !strings.HasPrefix(key, alt) {
		key = ctrl + strings.TrimPrefix(key, ctrl)
	}
	if key == Interrupt {
		return "", fmt.Errorf("leader cannot be %s: that is the application's interrupt", Interrupt)
	}
	if Sequence(key) == nil {
		return "", fmt.Errorf("leader must be a single letter, or ctrl+ or alt+ one: got %q", leader)
	}
	return key, nil
}

// Modifier is the modifier a leader is held with, as the key names spell it:
// "ctrl+" for "ctrl+a", "alt+" for "alt+g", empty for anything else.
//
// It is what a second keystroke typed with the leader's modifier still held
// is spelled with, which both terminals tolerate: letting go precisely between
// the two keys of a chord is a skill nobody should have to acquire.
func Modifier(name string) string {
	for _, mod := range []string{ctrl, alt} {
		if strings.HasPrefix(name, mod) {
			return mod
		}
	}
	return ""
}

// Sequence is the bytes a terminal sends for a letter key, bare or with one
// modifier: "d" is 'd', "ctrl+a" is 0x01, and "alt+g" is Escape then 'g',
// which is how a terminal sends Alt (Meta) with a key. It is nil for anything
// else, which no normalized leader is.
//
// It is what a raw attach matches on. There the keystrokes are bytes off the
// terminal rather than the key messages a Bubble Tea pane is handed, and the
// leader has to be recognizable in both.
func Sequence(name string) []byte {
	mod := Modifier(name)
	key := strings.TrimPrefix(name, mod)
	if len(key) != 1 || key[0] < 'a' || key[0] > 'z' {
		return nil
	}
	switch mod {
	case ctrl:
		return []byte{key[0] - 'a' + 1}
	case alt:
		return []byte{0x1b, key[0]}
	}
	return []byte{key[0]}
}

// Describe spells a key name the way prose does: "ctrl+a" is "Ctrl-A", and
// "alt+g" is "Alt-G".
func Describe(name string) string {
	switch mod := Modifier(name); mod {
	case ctrl:
		return "Ctrl-" + strings.ToUpper(strings.TrimPrefix(name, mod))
	case alt:
		return "Alt-" + strings.ToUpper(strings.TrimPrefix(name, mod))
	}
	return name
}
