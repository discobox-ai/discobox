package client

import (
	"fmt"
	"strings"
	"sync"
)

// displayState is what the remote has turned on in the caller's terminal and
// not yet turned off, read from the output stream as it is written. It is what
// makes handing the terminal back an undo rather than a reset: a remote that
// changed nothing is handed back a terminal nothing is written to, and one that
// left the alternate screen up is handed back exactly that much.
//
// A blanket reset is not harmless. Several of the sequences that turn a mode
// off also move the cursor when the mode was never on — leaving an alternate
// screen that was never entered restores a cursor that was never saved, and
// resetting the scrolling region homes the cursor — and others take state that
// belongs to the caller, like an entry of the kitty keyboard stack the shell
// pushed.
//
// The stream is read the way a UTF-8 terminal reads it, by a parser of its own
// rather than a general one: what matters is agreeing with the terminal, and a
// terminal decoding UTF-8 recognizes no C1 controls — 0x80-0x9F there are
// continuation bytes — and lets ESC begin a sequence whatever came before it,
// malformed text included. A parser that disagreed would miss the alternate
// screen a program entered, or invent one it never did and write the
// cursor-moving leave this exists to avoid.
type displayState struct {
	mu sync.Mutex

	// state is where the parser is in the stream; prefix, intermediate and
	// params are the sequence being collected.
	state        parseState
	prefix       byte
	intermediate byte
	// params are the CSI parameters, missingParam where one was left empty.
	params []int

	modes displayModes
}

type parseState uint8

const (
	stateGround parseState = iota
	// stateEscape follows ESC, collecting intermediates until the final byte.
	stateEscape
	// stateCSI collects a control sequence's prefix, parameters and
	// intermediates until its final byte.
	stateCSI
	// stateCSIIgnore skips a malformed control sequence to its final byte.
	stateCSIIgnore
	// stateString is inside a DCS, APC, PM or SOS string, which only ST ends.
	stateString
	// stateOSC is inside an OSC string, which BEL ends as well as ST.
	stateOSC
	// stateStringEscape follows ESC inside a string: a backslash ends it, and
	// anything else is a new escape sequence.
	stateStringEscape
)

const (
	missingParam = -1
	// maxParams bounds a sequence's parameters; the ones after it are dropped,
	// as a terminal drops them.
	maxParams = 32
	// maxParamValue bounds a parameter, so a run of digits cannot overflow.
	maxParamValue = 65535
)

// displayModes is the terminal state read out of the output stream.
type displayModes struct {
	// privateModes holds the last value the remote set for each DEC private
	// mode in resettableModes.
	privateModes map[int]bool
	// altScreen is the private mode the alternate screen was entered with
	// (47, 1047 or 1049), or zero on the main screen. Leaving it with the same
	// mode matters: 1049 is the one that restores the cursor it saved.
	altScreen int
	// kitty is the keyboard protocol state of each screen, which the protocol
	// keeps separately: index 0 the main screen, 1 the alternate.
	kitty [2]kittyKeyboard
	// scrollRegion is set while the remote's scrolling margins are not the
	// whole screen.
	scrollRegion bool
	// keypad is application keypad mode (DECKPAM).
	keypad bool
	// insert is insert mode (IRM).
	insert bool
	// cursorStyle is set while the cursor shape is not the terminal's default.
	cursorStyle bool
	// modifyOtherKeys is set while xterm's modifyOtherKeys is on.
	modifyOtherKeys bool
	// attributes is set once the remote has turned any text attribute on, and
	// charset once it has selected any character set but ASCII. Neither is
	// cleared by the remote turning them off again, because the terminal can
	// bring them back without another sequence to read: a cursor restore
	// (DECRC, and leaving the 1049 alternate screen — the undo's own leave
	// included) restores the attributes and character sets saved with it.
	// Their resets move nothing, so writing one that was not needed costs
	// nothing.
	attributes bool
	charset    bool
}

// kittyKeyboard is one screen's kitty keyboard protocol state.
type kittyKeyboard struct {
	// pushed is how many entries the remote pushed and did not pop.
	pushed int
	// flags is set while the remote left flags on in the entry below its own
	// pushes, which is the caller's.
	flags bool
}

// resettableModes are the DEC private modes a program turns on for itself and
// is expected to turn off on its way out, with each one's default. The
// alternate screen is tracked apart from them, because it is left before
// anything else is put back.
var resettableModes = []struct {
	mode int
	on   bool
}{
	{1, false},    // application cursor keys
	{5, false},    // reverse video
	{7, true},     // autowrap
	{9, false},    // X10 mouse reporting
	{25, true},    // cursor visible
	{66, false},   // application keypad (DECNKM)
	{1000, false}, // mouse click reporting
	{1001, false}, // mouse highlight tracking
	{1002, false}, // mouse drag reporting
	{1003, false}, // mouse motion reporting
	{1004, false}, // focus reporting
	{1005, false}, // UTF-8 mouse encoding
	{1006, false}, // SGR mouse encoding
	{1015, false}, // urxvt mouse encoding
	{1016, false}, // SGR pixel mouse encoding
	{2004, false}, // bracketed paste
	{2026, false}, // synchronized output
	{2031, false}, // color scheme change reports
	{2048, false}, // in-band resize reports
}

func newDisplayState() *displayState {
	return &displayState{modes: displayModes{privateModes: map[int]bool{}}}
}

// observe reads output the remote wrote to the caller's terminal.
func (d *displayState) observe(p []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, b := range p {
		d.advance(b)
	}
}

func (d *displayState) advance(b byte) {
	switch b {
	case 0x18, 0x1a: // CAN and SUB abandon any sequence
		d.state = stateGround
		return
	case 0x1b:
		if d.state == stateString || d.state == stateOSC {
			d.state = stateStringEscape
			return
		}
		d.beginEscape()
		return
	}
	switch d.state {
	case stateGround:
		if b < 0x20 {
			d.execute(b)
		}
	case stateEscape:
		switch {
		case b < 0x20:
			d.execute(b)
		case b < 0x30:
			d.intermediate = b
		case b < 0x7f:
			d.escape(b)
		case b > 0x7f:
			d.state = stateGround
		}
	case stateCSI:
		d.collectCSI(b)
	case stateCSIIgnore:
		switch {
		case b < 0x20:
			d.execute(b)
		case b >= 0x40 && b < 0x7f:
			d.state = stateGround
		}
	case stateOSC:
		if b == 0x07 {
			d.state = stateGround
		}
	case stateStringEscape:
		if b == '\\' {
			d.state = stateGround
			return
		}
		d.beginEscape()
		d.advance(b)
	}
}

func (d *displayState) beginEscape() {
	d.state = stateEscape
	d.intermediate = 0
}

func (d *displayState) collectCSI(b byte) {
	switch {
	case b < 0x20:
		d.execute(b)
	case b >= '0' && b <= '9':
		if d.intermediate != 0 {
			d.state = stateCSIIgnore
			return
		}
		if len(d.params) == 0 {
			d.params = append(d.params, missingParam)
		}
		last := &d.params[len(d.params)-1]
		if *last == missingParam {
			*last = 0
		}
		*last = min(*last*10+int(b-'0'), maxParamValue)
	case b == ';' || b == ':':
		if d.intermediate != 0 {
			d.state = stateCSIIgnore
			return
		}
		if len(d.params) == 0 {
			d.params = append(d.params, missingParam)
		}
		if len(d.params) < maxParams {
			d.params = append(d.params, missingParam)
		}
	case b >= '<' && b <= '?':
		if d.prefix != 0 || len(d.params) > 0 || d.intermediate != 0 {
			d.state = stateCSIIgnore
			return
		}
		d.prefix = b
	case b < 0x30:
		d.intermediate = b
	case b >= 0x40 && b < 0x7f:
		d.state = stateGround
		d.csi(b)
	case b > 0x7f:
		d.state = stateCSIIgnore
	}
}

// param is parameter i, or def where it is absent or left empty.
func (d *displayState) param(i, def int) int {
	if i >= len(d.params) || d.params[i] == missingParam {
		return def
	}
	return d.params[i]
}

func (d *displayState) execute(b byte) {
	if b == 0x0e { // SO selects G1, which is not ASCII
		d.modes.charset = true
	}
}

func (d *displayState) escape(final byte) {
	d.state = stateGround
	switch d.intermediate {
	case 0:
		switch final {
		case '[':
			d.state = stateCSI
			d.prefix, d.intermediate, d.params = 0, 0, d.params[:0]
		case ']':
			d.state = stateOSC
		case 'P', '_', '^', 'X':
			d.state = stateString
		case '=':
			d.modes.keypad = true
		case '>':
			d.modes.keypad = false
		case 'c':
			// RIS: the terminal is back at its defaults, and so is the undo.
			d.modes = displayModes{privateModes: map[int]bool{}}
		}
	case '(':
		if final != 'B' {
			d.modes.charset = true
		}
	}
}

func (d *displayState) csi(final byte) {
	prefix, intermediate := d.prefix, d.intermediate
	switch {
	case prefix == '?' && intermediate == 0 && (final == 'h' || final == 'l'):
		for i := range d.params {
			d.privateMode(d.param(i, 0), final == 'h')
		}
	case prefix == 0 && intermediate == 0 && (final == 'h' || final == 'l'):
		for i := range d.params {
			if d.param(i, 0) == 4 {
				d.modes.insert = final == 'h'
			}
		}
	case prefix == 0 && intermediate == 0 && final == 'r':
		d.modes.scrollRegion = d.param(0, 1) > 1 || d.param(1, 0) > 0
	case prefix == 0 && intermediate == 0 && final == 'm':
		for i := range d.params {
			if d.param(i, 0) != 0 {
				d.modes.attributes = true
				break
			}
		}
	case prefix == '>' && intermediate == 0 && final == 'm':
		if d.param(0, missingParam) == 4 {
			d.modes.modifyOtherKeys = d.param(1, 0) != 0
		}
	case prefix == 0 && intermediate == ' ' && final == 'q':
		d.modes.cursorStyle = d.param(0, 0) != 0
	case prefix == 0 && intermediate == '!' && final == 'p':
		// DECSTR puts back the modes a soft reset covers. Attributes and
		// character sets stay pending: the cursor the alternate screen saved
		// belongs to the other screen and survives it, and brings back what
		// was saved with it when the screen is left.
		for mode, on := range map[int]bool{1: false, 7: true, 25: true, 66: false} {
			d.modes.privateModes[mode] = on
		}
		d.modes.scrollRegion, d.modes.keypad, d.modes.insert = false, false, false
	case prefix == '>' && intermediate == 0 && final == 'u':
		d.modes.kitty[d.screen()].pushed++
	case prefix == '<' && intermediate == 0 && final == 'u':
		k := &d.modes.kitty[d.screen()]
		k.pushed = max(0, k.pushed-d.param(0, 1))
	case prefix == '=' && intermediate == 0 && final == 'u':
		k := &d.modes.kitty[d.screen()]
		if k.pushed > 0 {
			// It changes the remote's own entry, which the pop takes with it.
			return
		}
		switch flags := d.param(0, 0); d.param(1, 1) {
		case 1:
			k.flags = flags != 0
		case 2:
			k.flags = k.flags || flags != 0
		}
	}
}

func (d *displayState) privateMode(mode int, on bool) {
	switch mode {
	case 47, 1047, 1049:
		if on {
			d.modes.altScreen = mode
		} else {
			d.modes.altScreen = 0
		}
		return
	}
	for _, m := range resettableModes {
		if m.mode == mode {
			d.modes.privateModes[mode] = on
			return
		}
	}
}

// screen indexes the per-screen state of the screen being drawn on.
func (d *displayState) screen() int {
	if d.modes.altScreen != 0 {
		return 1
	}
	return 0
}

// undo returns the sequence that turns off what the remote left on, and is
// empty when it left nothing.
//
// A stream that ended part-way through a sequence — the attach cut off between
// two reads of the remote's terminal — leaves the caller's terminal inside it,
// where a string swallows everything written after it and a control sequence
// takes the next byte as its final. CAN abandons it and moves nothing, so it
// goes first, and only then.
//
// The rest is in the terminal's order, not the list's. The alternate screen's
// own keyboard stack goes with that screen, so it is popped while it is still
// the screen being drawn on; the screen is left before anything else, so the
// rest lands on the screen the caller keeps; and the scrolling region is put
// back between a cursor save and restore, before the attributes and character
// set are, because the restore brings back the ones it saved.
func (d *displayState) undo() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var b strings.Builder
	if d.state != stateGround {
		b.WriteByte(0x18)
	}
	if d.modes.altScreen != 0 {
		writeKittyUndo(&b, d.modes.kitty[1])
		fmt.Fprintf(&b, "\x1b[?%dl", d.modes.altScreen)
	}
	if d.modes.scrollRegion {
		b.WriteString("\x1b7\x1b[r\x1b8")
	}
	writeKittyUndo(&b, d.modes.kitty[0])
	for _, m := range resettableModes {
		on, seen := d.modes.privateModes[m.mode]
		if !seen || on == m.on {
			continue
		}
		verb := 'l'
		if m.on {
			verb = 'h'
		}
		fmt.Fprintf(&b, "\x1b[?%d%c", m.mode, verb)
	}
	if d.modes.modifyOtherKeys {
		b.WriteString("\x1b[>4;0m")
	}
	if d.modes.keypad {
		b.WriteString("\x1b>")
	}
	if d.modes.insert {
		b.WriteString("\x1b[4l")
	}
	if d.modes.cursorStyle {
		b.WriteString("\x1b[0 q")
	}
	if d.modes.attributes {
		b.WriteString("\x1b[m")
	}
	if d.modes.charset {
		b.WriteString("\x1b(B\x0f")
	}
	return b.String()
}

func writeKittyUndo(b *strings.Builder, k kittyKeyboard) {
	if k.pushed > 0 {
		fmt.Fprintf(b, "\x1b[<%du", k.pushed)
	}
	if k.flags {
		b.WriteString("\x1b[=0;1u")
	}
}
