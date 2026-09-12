package termpane

import (
	"strconv"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
)

// The emulator encodes keys the way the application has asked for them, which
// is the reason input goes through it at all — but it does not encode the
// modified special keys.
//
// Its encoder matches key events by exact equality, so a Left carrying Ctrl or
// Shift matches none of its cases and produces nothing at all: Ctrl-Left,
// Shift-Home, Ctrl-Delete and the rest reach the application as silence. (Its
// own source says as much: "TODO: Support Kitty, CSI u, and XTerm
// modifyOtherKeys".) Alt is the exception, because it is stripped and sent as an
// escape prefix rather than matched.
//
// So those are encoded here, in xterm's form, before the emulator is asked.
// Everything else — plain special keys, the control letters, Alt — is left to
// it, since what it does there already works and is the application's own
// negotiated mode rather than a guess.

// modifierParam is xterm's modifier encoding: one, plus a bit per modifier
// held.
func modifierParam(mod tea.KeyMod) int {
	param := 1
	if mod&uv.ModShift != 0 {
		param++
	}
	if mod&uv.ModAlt != 0 {
		param += 2
	}
	if mod&uv.ModCtrl != 0 {
		param += 4
	}
	if mod&uv.ModMeta != 0 {
		param += 8
	}
	return param
}

// csiFinals are the keys xterm encodes as CSI 1 ; <mod> <final>.
var csiFinals = map[rune]string{
	uv.KeyUp:    "A",
	uv.KeyDown:  "B",
	uv.KeyRight: "C",
	uv.KeyLeft:  "D",
	uv.KeyBegin: "E",
	uv.KeyEnd:   "F",
	uv.KeyHome:  "H",
	uv.KeyF1:    "P",
	uv.KeyF2:    "Q",
	uv.KeyF3:    "R",
	uv.KeyF4:    "S",
}

// tildeNumbers are the keys xterm encodes as CSI <n> ; <mod> ~.
var tildeNumbers = map[rune]int{
	uv.KeyInsert: 2,
	uv.KeyDelete: 3,
	uv.KeyPgUp:   5,
	uv.KeyPgDown: 6,
	uv.KeyF5:     15,
	uv.KeyF6:     17,
	uv.KeyF7:     18,
	uv.KeyF8:     19,
	uv.KeyF9:     20,
	uv.KeyF10:    21,
	uv.KeyF11:    23,
	uv.KeyF12:    24,
}

// modifiedKeySeq is the escape sequence for a special key held with Ctrl or
// Shift, or empty for anything the emulator should handle itself.
//
// Modified cursor keys take the CSI form whether or not the application has
// asked for application cursor keys: that is what xterm does, and the SS3 form
// has nowhere to put a modifier.
//
// Alt belongs in the parameter here, not in front of the sequence. Prefixing an
// escape is right for the keys with no parameter to put a modifier in — Alt-B
// is readline's backward-word and has been for forty years — but a special key
// prefixed that way arrives as "\x1b\x1b[A", which reads as the Escape key
// followed by the literal text "[A". xterm sends "\x1b[1;3A" for Alt-Up, and so
// does every terminal since.
func modifiedKeySeq(key tea.Key) string {
	// A key carrying no modifier this form can express is the emulator's: it
	// is the only one that knows whether the application asked for application
	// cursor mode, and the unmodified cursor keys are the only place that
	// changes what is sent.
	//
	// The test is against what modifierParam encodes rather than against zero,
	// because a keyboard reports more than that. Windows sets ModCapsLock on
	// every key pressed while Caps Lock is on, and a parameter with nowhere to
	// put it would read ";1" — a modified sequence claiming no modifier — for
	// every arrow key on the machine.
	if key.Mod&encodableMods == 0 {
		return ""
	}
	param := strconv.Itoa(modifierParam(key.Mod))
	if final, ok := csiFinals[key.Code]; ok {
		return "\x1b[1;" + param + final
	}
	if number, ok := tildeNumbers[key.Code]; ok {
		return "\x1b[" + strconv.Itoa(number) + ";" + param + "~"
	}
	return ""
}

// encodableMods are the modifiers modifierParam has a bit for. The rest of what
// a keyboard reports — the locks, Super, Hyper — has no place in an xterm
// modifier parameter, so a key carrying only those is an unmodified key.
const encodableMods = uv.ModShift | uv.ModAlt | uv.ModCtrl | uv.ModMeta

// keypadTwins is every keypad key against the key it is labeled with.
var keypadTwins = map[rune]rune{
	uv.KeyKpEnter:    uv.KeyEnter,
	uv.KeyKpEqual:    '=',
	uv.KeyKpMultiply: '*',
	uv.KeyKpPlus:     '+',
	uv.KeyKpComma:    ',',
	uv.KeyKpMinus:    '-',
	uv.KeyKpDecimal:  '.',
	uv.KeyKpDivide:   '/',
	uv.KeyKpSep:      ',',
	uv.KeyKp0:        '0', uv.KeyKp1: '1', uv.KeyKp2: '2', uv.KeyKp3: '3',
	uv.KeyKp4: '4', uv.KeyKp5: '5', uv.KeyKp6: '6', uv.KeyKp7: '7',
	uv.KeyKp8: '8', uv.KeyKp9: '9',
	uv.KeyKpUp:     uv.KeyUp,
	uv.KeyKpDown:   uv.KeyDown,
	uv.KeyKpLeft:   uv.KeyLeft,
	uv.KeyKpRight:  uv.KeyRight,
	uv.KeyKpBegin:  uv.KeyBegin,
	uv.KeyKpHome:   uv.KeyHome,
	uv.KeyKpEnd:    uv.KeyEnd,
	uv.KeyKpPgUp:   uv.KeyPgUp,
	uv.KeyKpPgDown: uv.KeyPgDown,
	uv.KeyKpInsert: uv.KeyInsert,
	uv.KeyKpDelete: uv.KeyDelete,
}

// foldKeypad sends a keypad key as the key it is labeled with.
//
// A pane is handed keys, not scancodes. The host terminal already decoded the
// keystroke under its own keypad mode — the numeric one, since nothing a Bubble
// Tea window runs asks for the other — so what arrives is Enter, or 5, or Left,
// pressed on the right-hand side of the keyboard. Handing that on as a keypad
// key lets the *far end's* keypad mode encode it again, and a program that has
// asked for application keypad keys turns Enter on the numpad into "\x1bOM":
// a sequence the user's own terminal would never have sent, and one a shell's
// line editor does not read as a return. The numpad then has one key that
// silently does nothing.
//
// For most of the numpad this is already how the rest of the pane sees it.
// [tea.KeyPressMsg.String] names the digits, Enter and the navigation cluster
// after their twins — "5", "enter", "left" — so the keymap, the prefix bindings
// and the window's own keys have never told those apart, and the encoder was
// the last place they differed. The arithmetic keys are the exception: it names
// those after themselves ("plus", "minus", "div", "mul", "equal", "period",
// "comma", "sep"), so a keymap can still bind numpad minus separately from "-"
// while the fold sends the same "-" either way. That split stays: the keymap is
// matched on the name in SendKey, before this runs, and the reserved keys are
// matched on it a step earlier still, so folding sooner would only move the
// disagreement rather than end it.
//
// It also settles the keys the emulator has no case for at all. Keypad
// navigation — the numpad with Num Lock off — was silence before, since
// encodable drops what the emulator would turn into U+FFFD; as its twin it is
// an arrow like any other, modifiers and all. The center of that cluster needs
// plainKeySeqs as well, its twin being Begin, which the emulator cannot encode
// either.
func foldKeypad(key tea.Key) tea.Key {
	if twin, ok := keypadTwins[key.Code]; ok {
		key.Code = twin
	}
	return key
}

// emulatorKeys are the special codes the emulator encodes itself. Every other
// code above [utf8.MaxRune] reaches its default branch, which writes
// string(code) — and a code that is not a rune comes out of that as U+FFFD.
//
// It is the emulator's own switch, copied by hand, so the keypad codes are here
// even though foldKeypad means none of them can reach this. Keeping the list a
// faithful copy is what makes it checkable against the emulator when the pin
// this repository holds it by moves.
var emulatorKeys = map[rune]bool{
	uv.KeyBackspace: true, uv.KeyDelete: true, uv.KeyDown: true,
	uv.KeyEnd: true, uv.KeyEnter: true, uv.KeyEscape: true,
	uv.KeyHome: true, uv.KeyInsert: true, uv.KeyLeft: true,
	uv.KeyPgDown: true, uv.KeyPgUp: true, uv.KeyRight: true,
	uv.KeySpace: true, uv.KeyTab: true, uv.KeyUp: true,
	uv.KeyF1: true, uv.KeyF2: true, uv.KeyF3: true, uv.KeyF4: true,
	uv.KeyF5: true, uv.KeyF6: true, uv.KeyF7: true, uv.KeyF8: true,
	uv.KeyF9: true, uv.KeyF10: true, uv.KeyF11: true, uv.KeyF12: true,
	uv.KeyKp0: true, uv.KeyKp1: true, uv.KeyKp2: true, uv.KeyKp3: true,
	uv.KeyKp4: true, uv.KeyKp5: true, uv.KeyKp6: true, uv.KeyKp7: true,
	uv.KeyKp8: true, uv.KeyKp9: true, uv.KeyKpComma: true,
	uv.KeyKpDecimal: true, uv.KeyKpEnter: true, uv.KeyKpEqual: true,
	uv.KeyKpMinus: true, uv.KeyKpMultiply: true, uv.KeyKpPlus: true,
}

// plainKeySeqs is the unmodified sequence for a key the emulator has no case
// for at all. An unmodified key is otherwise the emulator's, since it is the
// one that knows what the application negotiated.
//
// Begin is the whole list, and the reason there is one. It is in csiFinals and
// not in emulatorKeys, so a modified Begin encoded and a bare one was silence:
// numpad 5 with Num Lock off worked only while Ctrl was held, which is the
// failure foldToEncodable exists to end.
//
// It is a list of keys and not a rule over csiFinals, which is the table that
// looks like it would serve. That is xterm's *modified* final table: F1 to F4
// take those finals after CSI 1 ; <mod> and SS3 when bare, so "CSI, then the
// final" would answer F1 with "\x1b[P" — not F1, and DCH on the way back out.
// It being unreachable today, F1 to F4 being in emulatorKeys, is the argument
// against rather than for: a rule keyed on csiFinals widens itself exactly when
// the emulator loses a case, which is when it would start sending wrong bytes
// instead of none. A missing entry here sends nothing, as before.
//
// The form is the normal-cursor-mode one. Under DECCKM xterm sends this cluster
// as SS3, and the pane cannot ask which mode is set — the emulator exposes no
// accessor, and watching the stream for it the way watchMouseModes has to would
// duplicate emulator state, and drift from it on a reset, for one key. So this
// is the right bytes in the mode applications are in unless they ask otherwise,
// and a form the other mode still decodes; silence was wrong in both.
var plainKeySeqs = map[rune]string{
	uv.KeyBegin: "\x1b[E",
}

// encodable reports whether the emulator can send this key at all. A printable
// code it writes as itself; a special code it writes only if it has a case for
// it. Anything else it would turn into a replacement character, so the pane
// sends nothing instead — which is what a terminal with no sequence for a key
// sends.
func encodable(code rune) bool {
	return utf8.ValidRune(code) || emulatorKeys[code]
}

// ctrlEncodable reports whether the emulator has a control byte for this code:
// the letters, space, and the five symbols that fill out the C0 range.
func ctrlEncodable(code rune) bool {
	switch code {
	case uv.KeySpace, '[', '\\', ']', '^', '_':
		return true
	}
	return code >= 'a' && code <= 'z'
}

// foldToEncodable keeps only the modifiers the emulator can send, so a key held
// with one it cannot arrives unmodified rather than not at all.
//
// The emulator matches keys by exact equality, so every modifier it has no case
// for turns the keystroke into silence: Ctrl-Tab, Ctrl-Backspace, Shift-Escape
// and Ctrl-Shift-anything reach the application as nothing. A real terminal
// with no encoding for a modifier sends the unmodified key instead, which is
// what this does. It runs after modifiedKeySeq, so every key that does have a
// form keeps it.
//
// It is an allowlist rather than a list of modifiers to strip, because a
// keyboard reports more than the four an encoding exists for: the locks, Super
// and Hyper all reach here, and any one of them left on a key is the silence
// this exists to prevent. Alt always survives, as an escape prefix; Shift only
// on Tab, whose back-tab the emulator encodes; Ctrl only on the codes with a
// control byte.
func foldToEncodable(key tea.Key) tea.Key {
	mod := key.Mod & uv.ModAlt
	if key.Mod&uv.ModShift != 0 && key.Code == uv.KeyTab {
		mod |= uv.ModShift
	}
	if key.Mod&uv.ModCtrl != 0 && ctrlEncodable(key.Code) {
		mod |= uv.ModCtrl
	}
	key.Mod = mod
	return key
}
