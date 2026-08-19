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
func modifiedKeySeq(key tea.Key) string {
	// Alt alone is left to the emulator, which sends it as an escape prefix —
	// a form readline and its like have always understood. Only the modifiers
	// it drops are handled here.
	if key.Mod&(uv.ModCtrl|uv.ModShift) == 0 {
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
