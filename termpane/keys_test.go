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

		// A modifier bit the parameter has no room for is not counted into it.
		// Windows sets ModCapsLock on every key pressed while Caps Lock is on,
		// and a parameter that carried it would read ";1" for a bare arrow key.
		{"capslock+ctrl+left", tea.KeyPressMsg{Code: uv.KeyLeft, Mod: uv.ModCapsLock | uv.ModCtrl}, "\x1b[1;5D"},

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
