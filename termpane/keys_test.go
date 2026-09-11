package termpane

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
)

// keyMarker follows a key under test, because a key that encodes to nothing is
// indistinguishable from one whose bytes have not arrived yet. It is bracketed
// by a character no escape sequence uses: a bare letter would be found inside
// "\x1b[Z" and cut the very sequence being measured in half.
const keyMarker = "|end|"

// sentForKey is the bytes one key press puts on the far end, and only those.
func sentForKey(t *testing.T, key tea.KeyPressMsg) string {
	t.Helper()
	m, stream, _ := attach(t, 40, 5)
	m.Update(key)
	m.SendText(keyMarker)
	return strings.TrimSuffix(stream.sent(t, keyMarker), keyMarker)
}

// Alt goes in the modifier parameter of a special key, not in front of its
// sequence. Prefixed instead, Alt-Up arrives as "\x1b\x1b[A" — the Escape key
// followed by the literal text "[A", which is what a program in a pane showed.
func TestAltIsAParameterNotAPrefix(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyPressMsg
		want string
	}{
		{"alt+up", tea.KeyPressMsg{Code: uv.KeyUp, Mod: uv.ModAlt}, "\x1b[1;3A"},
		{"alt+down", tea.KeyPressMsg{Code: uv.KeyDown, Mod: uv.ModAlt}, "\x1b[1;3B"},
		{"alt+left", tea.KeyPressMsg{Code: uv.KeyLeft, Mod: uv.ModAlt}, "\x1b[1;3D"},
		{"alt+home", tea.KeyPressMsg{Code: uv.KeyHome, Mod: uv.ModAlt}, "\x1b[1;3H"},
		{"alt+f1", tea.KeyPressMsg{Code: uv.KeyF1, Mod: uv.ModAlt}, "\x1b[1;3P"},
		{"alt+delete", tea.KeyPressMsg{Code: uv.KeyDelete, Mod: uv.ModAlt}, "\x1b[3;3~"},
		{"alt+pgup", tea.KeyPressMsg{Code: uv.KeyPgUp, Mod: uv.ModAlt}, "\x1b[5;3~"},
		{"alt+shift+up", tea.KeyPressMsg{Code: uv.KeyUp, Mod: uv.ModAlt | uv.ModShift}, "\x1b[1;4A"},
		{"ctrl+alt+up", tea.KeyPressMsg{Code: uv.KeyUp, Mod: uv.ModAlt | uv.ModCtrl}, "\x1b[1;7A"},

		// A key with no parameter to carry a modifier keeps the escape prefix,
		// which is the form readline has always read.
		{"alt+b", tea.KeyPressMsg{Code: 'b', Mod: uv.ModAlt}, "\x1bb"},
		{"alt+backspace", tea.KeyPressMsg{Code: uv.KeyBackspace, Mod: uv.ModAlt}, "\x1b\x7f"},

		// And an unmodified key is still the emulator's, so application cursor
		// mode keeps deciding what it sends.
		{"up", tea.KeyPressMsg{Code: uv.KeyUp}, "\x1b[A"},
		{"delete", tea.KeyPressMsg{Code: uv.KeyDelete}, "\x1b[3~"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sentForKey(t, tc.key); got != tc.want {
				t.Fatalf("%s sent %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// A modifier this terminal cannot encode costs the modifier, never the
// keystroke. The emulator matches keys by exact equality, so anything it has no
// case for used to reach the application as silence: a Backspace that deleted
// nothing, a Tab that indented nothing.
func TestAnUnencodableModifierLeavesTheKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyPressMsg
		want string
	}{
		{"ctrl+tab", tea.KeyPressMsg{Code: uv.KeyTab, Mod: uv.ModCtrl}, "\t"},
		{"ctrl+backspace", tea.KeyPressMsg{Code: uv.KeyBackspace, Mod: uv.ModCtrl}, "\x7f"},
		{"shift+backspace", tea.KeyPressMsg{Code: uv.KeyBackspace, Mod: uv.ModShift}, "\x7f"},
		{"shift+escape", tea.KeyPressMsg{Code: uv.KeyEscape, Mod: uv.ModShift}, "\x1b"},
		{"ctrl+escape", tea.KeyPressMsg{Code: uv.KeyEscape, Mod: uv.ModCtrl}, "\x1b"},
		{"ctrl+1", tea.KeyPressMsg{Code: '1', Mod: uv.ModCtrl}, "1"},
		{"ctrl+shift+a", tea.KeyPressMsg{Code: 'a', Mod: uv.ModCtrl | uv.ModShift}, "\x01"},

		// The forms the emulator does have keep them.
		{"shift+tab", tea.KeyPressMsg{Code: uv.KeyTab, Mod: uv.ModShift}, "\x1b[Z"},
		{"ctrl+shift+tab", tea.KeyPressMsg{Code: uv.KeyTab, Mod: uv.ModCtrl | uv.ModShift}, "\x1b[Z"},
		{"ctrl+c", tea.KeyPressMsg{Code: 'c', Mod: uv.ModCtrl}, "\x03"},
		{"ctrl+space", tea.KeyPressMsg{Code: uv.KeySpace, Mod: uv.ModCtrl}, "\x00"},
		{"alt+backspace", tea.KeyPressMsg{Code: uv.KeyBackspace, Mod: uv.ModAlt}, "\x1b\x7f"},
		{"ctrl+shift+left", tea.KeyPressMsg{Code: uv.KeyLeft, Mod: uv.ModCtrl | uv.ModShift}, "\x1b[1;6D"},
		{"shift+left", tea.KeyPressMsg{Code: uv.KeyLeft, Mod: uv.ModShift}, "\x1b[1;2D"},
		{"ctrl+delete", tea.KeyPressMsg{Code: uv.KeyDelete, Mod: uv.ModCtrl}, "\x1b[3;5~"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sentForKey(t, tc.key); got != tc.want {
				t.Fatalf("%s sent %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// A modifier this terminal has no parameter for is not a modifier. Windows sets
// ModCapsLock on every key pressed while Caps Lock is on, and a modifier
// parameter with nowhere to put it would report ";1" — a modified sequence
// claiming no modifier — for every arrow key on the machine, application cursor
// mode bypassed with it.
func TestALockIsNotAModifier(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyPressMsg
		want string
	}{
		{"capslock+up", tea.KeyPressMsg{Code: uv.KeyUp, Mod: uv.ModCapsLock}, "\x1b[A"},
		{"numlock+home", tea.KeyPressMsg{Code: uv.KeyHome, Mod: uv.ModNumLock}, "\x1b[H"},
		{"scrolllock+left", tea.KeyPressMsg{Code: uv.KeyLeft, Mod: uv.ModScrollLock}, "\x1b[D"},
		{"super+left", tea.KeyPressMsg{Code: uv.KeyLeft, Mod: uv.ModSuper}, "\x1b[D"},
		{"hyper+left", tea.KeyPressMsg{Code: uv.KeyLeft, Mod: uv.ModHyper}, "\x1b[D"},

		// And a lock does not silence the keys with no CSI form either.
		{"capslock+enter", tea.KeyPressMsg{Code: uv.KeyEnter, Mod: uv.ModCapsLock}, "\r"},
		{"capslock+tab", tea.KeyPressMsg{Code: uv.KeyTab, Mod: uv.ModCapsLock}, "\t"},
		{"capslock+backspace", tea.KeyPressMsg{Code: uv.KeyBackspace, Mod: uv.ModCapsLock}, "\x7f"},
		{"capslock+escape", tea.KeyPressMsg{Code: uv.KeyEscape, Mod: uv.ModCapsLock}, "\x1b"},

		// A real modifier still works with a lock held alongside it.
		{"capslock+ctrl+left", tea.KeyPressMsg{Code: uv.KeyLeft, Mod: uv.ModCapsLock | uv.ModCtrl}, "\x1b[1;5D"},
		{"capslock+ctrl+c", tea.KeyPressMsg{Code: 'c', Mod: uv.ModCapsLock | uv.ModCtrl}, "\x03"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sentForKey(t, tc.key); got != tc.want {
				t.Fatalf("%s sent %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// A key the emulator cannot encode is sent as nothing, which is what a terminal
// with no sequence for it sends. Handed over anyway it reaches the emulator's
// default branch, which writes string(code) — and every special code is above
// utf8.MaxRune, so the application would be typed U+FFFD instead.
func TestAKeyWithNoSequenceTypesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyPressMsg
	}{
		{"ctrl+kpleft", tea.KeyPressMsg{Code: uv.KeyKpLeft, Mod: uv.ModCtrl}},
		{"kpleft", tea.KeyPressMsg{Code: uv.KeyKpLeft}},
		{"ctrl+f13", tea.KeyPressMsg{Code: uv.KeyF13, Mod: uv.ModCtrl}},
		{"shift+menu", tea.KeyPressMsg{Code: uv.KeyMenu, Mod: uv.ModShift}},
		{"mute", tea.KeyPressMsg{Code: uv.KeyMute}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sentForKey(t, tc.key); got != "" {
				t.Fatalf("%s sent %q, want nothing at all", tc.name, got)
			}
		})
	}
}
