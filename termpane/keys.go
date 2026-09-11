package termpane

import (
	"strconv"

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

// unshiftBackspace folds Shift-Backspace onto Backspace.
//
// xterm has no modified form for Backspace: shifted or not, it sends the same
// DEL. The emulator matches keys by exact equality, so the shifted key would
// reach the application as silence — a Backspace that deletes nothing on any
// terminal that reports the modifier. Ctrl and Alt are left as they are.
func unshiftBackspace(key tea.Key) tea.Key {
	if key.Code == uv.KeyBackspace {
		key.Mod &^= uv.ModShift
	}
	return key
}
