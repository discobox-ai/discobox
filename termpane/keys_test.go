package termpane

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
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

// sentForKeyAfter is sentForKey with the far end having asked for something
// first: the sequence is written to the pane, and the marker behind it landing
// on the screen proves it was processed before the key is pressed.
func sentForKeyAfter(t *testing.T, seq string, key tea.KeyPressMsg) string {
	t.Helper()
	m, stream, cmd := attach(t, 40, 5)
	stream.send(seq + "READY")
	pump(t, m, cmd, "READY")
	m.Update(key)
	m.SendText(keyMarker)
	return strings.TrimSuffix(stream.sent(t, keyMarker), keyMarker)
}

// With no keymap, the encoder answers the protocol and nothing else. No
// protocol below Kitty and modifyOtherKeys encodes a modified Enter, so a
// modified Enter is a plain return — which is what a real terminal in the same
// position sends. The chord that writes a newline is a keymap; see
// TestTheKeymapComesBeforeTheEncoder.
func TestAPureEncoderReportsAModifiedEnterAsAReturn(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyPressMsg
		want string
	}{
		{"enter", tea.KeyPressMsg{Code: uv.KeyEnter}, "\r"},
		{"shift+enter", tea.KeyPressMsg{Code: uv.KeyEnter, Mod: uv.ModShift}, "\r"},
		{"ctrl+enter", tea.KeyPressMsg{Code: uv.KeyEnter, Mod: uv.ModCtrl}, "\r"},
		{"ctrl+shift+enter", tea.KeyPressMsg{Code: uv.KeyEnter, Mod: uv.ModCtrl | uv.ModShift}, "\r"},

		// Alt is the exception, because it has an encoding: the escape prefix.
		{"alt+enter", tea.KeyPressMsg{Code: uv.KeyEnter, Mod: uv.ModAlt}, "\x1b\r"},

		// And Ctrl-J is a control byte, not a modified Enter.
		{"ctrl+j", tea.KeyPressMsg{Code: 'j', Mod: uv.ModCtrl}, "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sentForKey(t, tc.key); got != tc.want {
				t.Fatalf("%s sent %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// A bound key is what the host says it is. The keymap is consulted before the
// encoder and before the printable-text path — but after the pane's reserved
// keys, which never reach it. TestTheReservedKeysOutrankTheKeymap pins that
// half, through Update, where the prefix exists.
func TestTheKeymapComesBeforeTheEncoder(t *testing.T) {
	keys := map[string]string{
		"shift+enter": "\x1b\r",
		"ctrl+enter":  "\n",
		"f5":          "hello",
	}
	for _, tc := range []struct {
		name string
		key  tea.KeyPressMsg
		want string
	}{
		{"shift+enter", tea.KeyPressMsg{Code: uv.KeyEnter, Mod: uv.ModShift}, "\x1b\r"},
		{"ctrl+enter", tea.KeyPressMsg{Code: uv.KeyEnter, Mod: uv.ModCtrl}, "\n"},
		{"f5", tea.KeyPressMsg{Code: uv.KeyF5}, "hello"},

		// Everything unbound is the encoder's, unchanged.
		{"enter", tea.KeyPressMsg{Code: uv.KeyEnter}, "\r"},
		{"f6", tea.KeyPressMsg{Code: uv.KeyF6}, "\x1b[17~"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, stream, _ := attach(t, 40, 5, WithKeys(keys))
			m.Update(tc.key)
			m.SendText(keyMarker)
			got := strings.TrimSuffix(stream.sent(t, keyMarker), keyMarker)
			if got != tc.want {
				t.Fatalf("%s sent %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// A modifier this terminal cannot encode costs the modifier, never the
// keystroke. The emulator matches keys by exact equality, so anything it has no
// case for would reach the application as silence: a Backspace that deletes
// nothing, a Tab that indents nothing.
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

// A key neither the emulator nor the pane has a sequence for is sent as
// nothing, which is what a terminal with no sequence for it sends. Handed over
// anyway it reaches the emulator's default branch, which writes string(code) —
// and every special code is above utf8.MaxRune, so the application would be
// typed U+FFFD instead.
//
// The pane's own half of that is plainKeySeqs, which is why the condition is
// both and not just the emulator; see
// TestAKeyTheEmulatorCannotEncodeStillHasAForm.
func TestAKeyWithNoSequenceTypesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyPressMsg
	}{
		{"f13", tea.KeyPressMsg{Code: uv.KeyF13}},
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

// The pane's reserved keys outrank the keymap. They are matched in Update,
// before SendKey is reached at all, so binding one through WithKeys does
// nothing — which is the safe way round: the prefix is the way out of a pane,
// and a keymap that could shadow it could lock the pane shut.
func TestTheReservedKeysOutrankTheKeymap(t *testing.T) {
	// Both reserved keys are bound to something visible, so a keymap that did
	// win would announce itself.
	keys := map[string]string{
		"ctrl+a":      "PREFIX-LOST",
		"ctrl+c":      "DETACH-LOST",
		"shift+enter": "\x1b\r",
	}
	opts := []Option{WithPrefix("ctrl+a", "ctrl+c"), WithKeys(keys)}

	t.Run("the detach key still detaches", func(t *testing.T) {
		m, _, _ := attach(t, 40, 5, opts...)
		_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: uv.ModCtrl})
		if cmd == nil {
			t.Fatal("the detach key emitted nothing: the keymap took it")
		}
		if _, ok := cmd().(DetachMsg); !ok {
			t.Fatal("the detach key should emit DetachMsg")
		}
	})

	t.Run("the prefix still arms", func(t *testing.T) {
		m, stream, _ := attach(t, 40, 5, opts...)
		m.Update(tea.KeyPressMsg{Code: 'a', Mod: uv.ModCtrl})
		if !m.PrefixArmed() {
			t.Fatal("the prefix did not arm: the keymap took the key")
		}
		// And behind the prefix the reserved key is typed literally, as the
		// control byte it is, not as whatever the keymap says.
		m.Update(tea.KeyPressMsg{Code: 'c', Mod: uv.ModCtrl})
		got := stream.sent(t, "\x03")
		if strings.Contains(got, "DETACH-LOST") {
			t.Fatalf("the keymap took a key behind the prefix: %q", got)
		}
		if !strings.Contains(got, "\x03") {
			t.Fatalf("sent %q, want the literal interrupt", got)
		}
	})

	t.Run("an unreserved bound key is still bound", func(t *testing.T) {
		m, stream, _ := attach(t, 40, 5, opts...)
		m.Update(tea.KeyPressMsg{Code: uv.KeyEnter, Mod: uv.ModShift})
		if got := stream.sent(t, "\x1b\r"); !strings.Contains(got, "\x1b\r") {
			t.Fatalf("sent %q, want the bound shift+enter", got)
		}
	})
}

// The numpad is the keys it is labeled with. A pane is handed keys rather than
// scancodes — the host terminal decoded the keystroke under its own keypad
// mode, the numeric one — so what the far end is sent is Enter, or 5, or Left.
// It is what the rest of the pane already calls them: String names every keypad
// key after its twin, so the keymap and the prefix bindings never told the two
// apart either.
func TestTheNumpadIsTheKeysItIsLabeledWith(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyPressMsg
		want string
	}{
		{"kpenter", tea.KeyPressMsg{Code: uv.KeyKpEnter}, "\r"},
		{"alt+kpenter", tea.KeyPressMsg{Code: uv.KeyKpEnter, Mod: uv.ModAlt}, "\x1b\r"},

		// Num Lock is on for the digits and the arithmetic, and reported with
		// every one of them; it is a lock, not a modifier, and costs nothing.
		{"kp5", tea.KeyPressMsg{Code: uv.KeyKp5, Mod: uv.ModNumLock}, "5"},
		{"kpplus", tea.KeyPressMsg{Code: uv.KeyKpPlus, Mod: uv.ModNumLock}, "+"},
		{"kpdivide", tea.KeyPressMsg{Code: uv.KeyKpDivide, Mod: uv.ModNumLock}, "/"},
		{"kpdecimal", tea.KeyPressMsg{Code: uv.KeyKpDecimal, Mod: uv.ModNumLock}, "."},

		// And with Num Lock off the same keys are the navigation cluster, which
		// the emulator has no case for at all: before the fold every one of
		// these was silence.
		{"kpleft", tea.KeyPressMsg{Code: uv.KeyKpLeft}, "\x1b[D"},
		{"ctrl+kpleft", tea.KeyPressMsg{Code: uv.KeyKpLeft, Mod: uv.ModCtrl}, "\x1b[1;5D"},
		{"kphome", tea.KeyPressMsg{Code: uv.KeyKpHome}, "\x1b[H"},
		{"kpdelete", tea.KeyPressMsg{Code: uv.KeyKpDelete}, "\x1b[3~"},
		{"shift+kppgup", tea.KeyPressMsg{Code: uv.KeyKpPgUp, Mod: uv.ModShift}, "\x1b[5;2~"},

		// The cluster's center is the one whose twin the emulator cannot encode
		// either; plainKeySeqs is what keeps it from being a key that works only
		// while Ctrl is held. See TestAKeyTheEmulatorCannotEncodeStillHasAForm.
		{"kpbegin", tea.KeyPressMsg{Code: uv.KeyKpBegin}, "\x1b[E"},
		{"ctrl+kpbegin", tea.KeyPressMsg{Code: uv.KeyKpBegin, Mod: uv.ModCtrl}, "\x1b[1;5E"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sentForKey(t, tc.key); got != tc.want {
				t.Fatalf("%s sent %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// Application keypad mode is the far end's answer for its own keypad, and this
// pane does not have one: the key it was handed was pressed on a terminal in
// numeric mode, and re-encoding it as "\x1bOM" invents a keystroke nobody made.
// Programs ask for the mode constantly — every zsh line editor does — and a
// shell that reads "\x1bOM" as nothing is a numpad whose Enter key is dead.
func TestApplicationKeypadModeDoesNotReachTheNumpad(t *testing.T) {
	for _, mode := range []struct{ name, seq string }{
		{"DECKPAM", "\x1b="},
		{"DECNKM", ansi.SetModeNumericKeypad},
	} {
		t.Run(mode.name, func(t *testing.T) {
			for _, tc := range []struct {
				name string
				key  tea.KeyPressMsg
				want string
			}{
				{"kpenter", tea.KeyPressMsg{Code: uv.KeyKpEnter}, "\r"},
				{"kp5", tea.KeyPressMsg{Code: uv.KeyKp5, Mod: uv.ModNumLock}, "5"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if got := sentForKeyAfter(t, mode.seq, tc.key); got != tc.want {
						t.Fatalf("%s under %s sent %q, want %q", tc.name, mode.name, got, tc.want)
					}
				})
			}
		})
	}
}

// A key the emulator cannot encode still gets the sequence a terminal sends for
// it, where the pane has one.
//
// Begin is the whole of that set, and without this it is a key that works only
// while Ctrl is held: modifiedKeySeq encodes it from csiFinals, and the bare
// press was silence.
//
// The form is the normal-cursor-mode one, which is a tradeoff and not a
// derivation: under DECCKM xterm sends this cluster as SS3, and the pane is
// knowingly not tracking that mode. See plainKeySeqs for what tracking it would
// cost.
//
// The sequence is named in plainKeySeqs rather than derived from csiFinals,
// because that table is xterm's modified finals: a rule reading "CSI, then the
// final" would answer a bare F1 with "\x1b[P".
func TestAKeyTheEmulatorCannotEncodeStillHasAForm(t *testing.T) {
	if encodable(uv.KeyBegin) {
		t.Fatal("the emulator now encodes Begin: this test and plainKeySeqs are about a key that it does not")
	}
	for _, tc := range []struct {
		name string
		key  tea.KeyPressMsg
		want string
	}{
		{"begin", tea.KeyPressMsg{Code: uv.KeyBegin}, "\x1b[E"},
		{"ctrl+begin", tea.KeyPressMsg{Code: uv.KeyBegin, Mod: uv.ModCtrl}, "\x1b[1;5E"},
		{"alt+begin", tea.KeyPressMsg{Code: uv.KeyBegin, Mod: uv.ModAlt}, "\x1b[1;3E"},

		// A lock is not a modifier here either: it folds off and leaves the
		// plain form, rather than the ";1" of a modified sequence with no
		// modifier.
		{"numlock+begin", tea.KeyPressMsg{Code: uv.KeyBegin, Mod: uv.ModNumLock}, "\x1b[E"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sentForKey(t, tc.key); got != tc.want {
				t.Fatalf("%s sent %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}
