package shimruntime

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/vt"
)

// DefaultScrollbackLines bounds the history a repaint snapshot carries. A late
// attacher gets the current screen plus this many lines of prior output.
const DefaultScrollbackLines = 1000

// screenBuffer maintains an in-memory terminal emulator fed from the live PTY
// output so a client that attaches after a program has been running can be
// repainted with the current screen, recent scrollback, and the terminal modes
// the program set before the client connected.
//
// A running TUI emits its screen and its mode-setting (mouse, bracketed paste,
// cursor keys, cursor visibility) once at startup. A client attaching later
// never saw any of it, so the emulator reconstructs the visible state and a
// small mode tracker records the input/rendering modes the emulator does not
// expose, so both can be replayed on attach.
//
// screenBuffer is not safe for concurrent use. The Runtime serializes all
// access (write, resize, snapshot) under its mutex, keeping the emulator's view
// in lockstep with the broadcast output so a snapshot taken at an attacher's
// cutover reflects exactly the bytes broadcast before it.
type screenBuffer struct {
	emu   *vt.Emulator
	modes modeTracker
}

func newScreenBuffer(rows, cols uint16, scrollbackLines int) *screenBuffer {
	w, h := int(cols), int(rows)
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	emu := vt.NewEmulator(w, h)
	emu.SetScrollbackSize(scrollbackLines)
	return &screenBuffer{emu: emu}
}

func (s *screenBuffer) write(p []byte) {
	_, _ = s.emu.Write(p)
	s.modes.scan(p)
}

func (s *screenBuffer) resize(rows, cols uint16) {
	if rows == 0 || cols == 0 {
		return
	}
	s.emu.Resize(int(cols), int(rows))
}

// snapshot renders a self-contained escape sequence that repaints the current
// terminal state: restored input/rendering modes, then the screen (with recent
// scrollback when on the primary screen), then the cursor placed where the
// program left it. The returned bytes are streamed to a fresh attacher before
// its buffered live output is flushed.
func (s *screenBuffer) snapshot() []byte {
	var b strings.Builder

	// Restore the modes the running program set before this client attached so
	// mouse, paste, and cursor-key input work immediately rather than after the
	// program's next redraw (which a detach/attach never triggers on its own).
	b.WriteString(s.modes.sequences())

	if s.emu.IsAltScreen() {
		// Enter the alternate screen and paint it. The alternate screen has no
		// scrollback.
		b.WriteString("\x1b[?1049h")
		b.WriteString("\x1b[H\x1b[2J")
		writeScreen(&b, s.emu.Render())
	} else {
		// Leave any alternate screen and clear the client's screen and scrollback
		// so a re-attach starts from a clean slate rather than stacking history.
		b.WriteString("\x1b[?1049l")
		b.WriteString("\x1b[H\x1b[2J\x1b[3J")
		sb := s.emu.Scrollback()
		for i := 0; i < sb.Len(); i++ {
			// Reset SGR after each line: ultraviolet renders lines assuming a clean
			// pen at the start, but does not reset a line that ends mid-style, so
			// styling would otherwise bleed into the next line.
			b.WriteString(sb.Line(i).Render())
			b.WriteString("\x1b[m\r\n")
		}
		writeScreen(&b, s.emu.Render())
	}
	// Clear any style left active by the final rendered cell before positioning
	// the cursor.
	b.WriteString("\x1b[m")

	pos := s.emu.CursorPosition()
	fmt.Fprintf(&b, "\x1b[%d;%dH", pos.Y+1, pos.X+1)

	return []byte(b.String())
}

// writeScreen writes the emulator's rendered screen, translating the bare line
// feeds vt emits into CRLF so lines do not stair-step in the client's raw-mode
// terminal.
func writeScreen(b *strings.Builder, render string) {
	b.WriteString(strings.ReplaceAll(render, "\n", "\r\n"))
}

// trackedModes are the private DEC modes a running TUI sets that the emulator's
// exported state does not carry, so a repaint must restore them explicitly.
// Alt-screen and cursor position come from the emulator directly and are not
// tracked here.
var trackedModes = map[int]struct{}{
	1:    {}, // DECCKM: application cursor keys
	25:   {}, // DECTCEM: cursor visibility
	1000: {}, // mouse: normal tracking
	1002: {}, // mouse: button-event tracking
	1003: {}, // mouse: any-event tracking
	1006: {}, // mouse: SGR extended coordinates
	2004: {}, // bracketed paste
}

// modeOrder fixes a deterministic emission order for restored modes.
var modeOrder = []int{1, 25, 1000, 1002, 1003, 1006, 2004}

// maxCarry bounds the partial escape sequence carried between writes so a
// malformed, never-terminated sequence cannot grow the carry buffer without
// limit.
const maxCarry = 128

// modeTracker records the last set/reset of the tracked private DEC modes by
// scanning the raw output stream for `ESC [ ? params (h|l)` sequences. It only
// emits modes it has actually observed toggled, leaving untouched modes at the
// client terminal's defaults.
type modeTracker struct {
	set   map[int]bool // mode number -> currently set
	seen  map[int]bool // mode number -> observed at least once
	carry []byte       // partial trailing escape sequence spanning writes
}

func (m *modeTracker) scan(p []byte) {
	if m.set == nil {
		m.set = map[int]bool{}
		m.seen = map[int]bool{}
	}
	data := p
	if len(m.carry) > 0 {
		data = append(m.carry, p...)
		m.carry = nil
	}
	for i := 0; i < len(data); {
		if data[i] != 0x1b {
			i++
			continue
		}
		consumed, complete := m.parsePrivateCSI(data[i:])
		if !complete {
			if tail := data[i:]; len(tail) <= maxCarry {
				m.carry = append([]byte(nil), tail...)
			}
			return
		}
		i += consumed
	}
}

// parsePrivateCSI inspects a possible escape sequence starting at s[0] == ESC.
// It returns the number of bytes consumed and whether the sequence was complete.
// Non-private and unrecognized escapes consume only the ESC so byte scanning
// resumes immediately after it; an incomplete private CSI reports not-complete
// so the caller carries it to the next write.
func (m *modeTracker) parsePrivateCSI(s []byte) (int, bool) {
	if len(s) < 2 {
		return 0, false // could still become ESC [
	}
	if s[1] != '[' {
		return 1, true // not a CSI
	}
	if len(s) < 3 {
		return 0, false // could still become ESC [ ?
	}
	if s[2] != '?' {
		return 1, true // regular CSI; not a private mode
	}
	k := 3
	for k < len(s) && (s[k] >= '0' && s[k] <= '9' || s[k] == ';') {
		k++
	}
	if k >= len(s) {
		return 0, false // parameters not terminated yet
	}
	final := s[k]
	if final == 'h' || final == 'l' {
		m.apply(s[3:k], final == 'h')
	}
	return k + 1, true
}

func (m *modeTracker) apply(params []byte, set bool) {
	for _, ps := range bytes.Split(params, []byte{';'}) {
		if len(ps) == 0 {
			continue
		}
		n, err := strconv.Atoi(string(ps))
		if err != nil {
			continue
		}
		if _, ok := trackedModes[n]; !ok {
			continue
		}
		m.set[n] = set
		m.seen[n] = true
	}
}

func (m *modeTracker) sequences() string {
	if len(m.seen) == 0 {
		return ""
	}
	var b strings.Builder
	for _, n := range modeOrder {
		if !m.seen[n] {
			continue
		}
		verb := byte('l')
		if m.set[n] {
			verb = 'h'
		}
		fmt.Fprintf(&b, "\x1b[?%d%c", n, verb)
	}
	return b.String()
}
