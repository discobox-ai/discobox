package shimruntime

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/vt"
)

// DefaultScrollbackLines bounds the history a repaint snapshot carries. A late
// attacher gets the current screen plus this many lines of prior output.
const DefaultScrollbackLines = 1000

const (
	// screenBurstGap is how long output has to pause before the screen is
	// compared. A frame reaches the emulator in as many pieces as the PTY was
	// read in, and a frame that erases before it draws is, part-way through, a
	// screen the program never meant to show; comparing it there would count a
	// redraw of the same frame as two changes. Pieces of one frame arrive back
	// to back, so a pause this long is the end of one.
	screenBurstGap = 50 * time.Millisecond
	// screenBurstMax bounds how long output that never pauses goes uncompared,
	// so a program streaming without a break is still seen to be busy.
	screenBurstMax = time.Second
)

// screenBuffer maintains an in-memory terminal emulator fed from the live PTY
// output so a client that attaches after a program has been running can be
// repainted with the current screen, recent scrollback, and the terminal modes
// the program set before the client connected.
//
// A running TUI emits its screen, its mode-setting (mouse, bracketed paste,
// cursor keys, cursor visibility) and its window title once at startup. A
// client attaching later never saw any of it, so the emulator reconstructs the
// visible state, a small mode tracker records the input/rendering modes the
// emulator does not expose, and the title is held from the emulator's callback,
// so all three can be replayed on attach.
//
// screenBuffer is not safe for concurrent use. The Runtime serializes all
// access (write, resize, snapshot) under its mutex, keeping the emulator's view
// in lockstep with the broadcast output so a snapshot taken at an attacher's
// cutover reflects exactly the bytes broadcast before it.
type screenBuffer struct {
	emu   *vt.Emulator
	modes modeTracker

	// title and iconName are the last the program set, or empty for never. The
	// emulator parses the OSC that carries them but does not keep them, and
	// there is no accessor to read them back, so they are held here as they go
	// past. Doing it from the callback rather than by scanning the stream the
	// way modeTracker does is what gets OSC 0/1/2, both terminators, and
	// sequences split across writes for free.
	title    string
	iconName string

	// changedAt is when what the program shows last changed — the text on its
	// screen or its title — and zero for never. It is what the idle stop reads
	// as the program doing something (ADR 0124). Only a change counts, not
	// output: a TUI redrawing the screen it already has, a program blinking a
	// cursor it draws itself by toggling a cell's style, and a shell re-sending
	// its title on redraw all write bytes and change nothing a person would
	// see.
	changedAt time.Time
	// textSum is a hash of the screen's text as of the last compare.
	textSum uint64
	// burstAt and pendingAt are the first and latest writes since the last
	// compare, both zero when there have been none: the burst of output not yet
	// compared.
	burstAt   time.Time
	pendingAt time.Time
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
	s := &screenBuffer{emu: emu}
	s.textSum = s.sumText()
	// The callbacks fire from inside emu.Write, which is only ever reached
	// through screenBuffer.write under the Runtime's lock, so they touch these
	// fields on the same goroutine that reads them in snapshot.
	emu.SetCallbacks(vt.Callbacks{
		Title: func(title string) {
			if title != s.title {
				s.title = title
				s.markChanged(time.Now().UTC())
			}
		},
		IconName: func(name string) { s.iconName = name },
	})
	return s
}

func (s *screenBuffer) write(p []byte) {
	now := time.Now().UTC()
	// The burst before this write ended if output paused since it: compare it
	// now, before this write lands, so a change it made is dated to its last
	// write rather than to this one, however much later this one came.
	s.settle(now)
	_, _ = s.emu.Write(p)
	s.modes.scan(p)
	if s.burstAt.IsZero() {
		s.burstAt = now
	}
	s.pendingAt = now
	if now.Sub(s.burstAt) >= screenBurstMax {
		s.compare(now)
	}
}

// screenChangedAt is when what the program shows last changed, with a burst
// of output that has since paused compared first. One still arriving is left
// for its end, and is dated then.
func (s *screenBuffer) screenChangedAt() time.Time {
	s.settle(time.Now().UTC())
	return s.changedAt
}

// settle compares the burst of output not yet compared, if output has paused
// since its last write, dating any change it made to that write.
func (s *screenBuffer) settle(now time.Time) {
	if !s.pendingAt.IsZero() && now.Sub(s.pendingAt) >= screenBurstGap {
		s.compare(s.pendingAt)
	}
}

// compare reads the screen's text and records a change at at if it differs
// from the last compare.
func (s *screenBuffer) compare(at time.Time) {
	s.burstAt, s.pendingAt = time.Time{}, time.Time{}
	if sum := s.sumText(); sum != s.textSum {
		s.textSum = sum
		s.markChanged(at)
	}
}

func (s *screenBuffer) markChanged(at time.Time) {
	if at.After(s.changedAt) {
		s.changedAt = at
	}
}

// sumText hashes the text of the screen the program is on: cell content only,
// which is what keeps a self-drawn blinking cursor from counting.
//
// The scrollback is left out. The emulator copies the screen into it on every
// full-screen erase, so a program that clears and redraws the same frame would
// grow it each time; the cost is that identical lines scrolling past, with
// nothing else on screen changing, are not seen.
func (s *screenBuffer) sumText() uint64 {
	// FNV-1a, inline rather than through hash/fnv: this runs over every cell
	// several times a second while a program prints, and handing each cell's
	// string to an io.Writer copies it.
	const offset, prime = 14695981039346656037, 1099511628211
	sum := uint64(offset)
	add := func(b byte) { sum = (sum ^ uint64(b)) * prime }
	w, rows := s.emu.Width(), s.emu.Height()
	for y := 0; y < rows; y++ {
		for x := 0; x < w; x++ {
			if cell := s.emu.CellAt(x, y); cell != nil {
				for i := 0; i < len(cell.Content); i++ {
					add(cell.Content[i])
				}
			}
		}
		add('\n')
	}
	if s.emu.IsAltScreen() {
		add(1)
	}
	return sum
}

// resize lays the screen out at a new size. A resize to the size it already is
// is not one, and is skipped.
//
// The emulator's own Resize is not idempotent: it resets the scroll region to
// the whole screen and re-notifies a program that asked for in-band resize
// reports. A client re-asserting its size to ask for a repaint sends the size
// it already sent, and a program that had set margins would lose them out of
// this screen — and so out of the next snapshot taken from it — for a repaint
// that changed nothing.
func (s *screenBuffer) resize(rows, cols uint16) {
	if rows == 0 || cols == 0 {
		return
	}
	if s.emu.Width() == int(cols) && s.emu.Height() == int(rows) {
		return
	}
	// Laying the same text out at a new size is not the program showing
	// anything new: compare what came before, and take the new layout as the
	// text to compare against without counting it.
	if !s.pendingAt.IsZero() {
		s.compare(s.pendingAt)
	}
	s.emu.Resize(int(cols), int(rows))
	s.textSum = s.sumText()
}

// snapshot renders a self-contained escape sequence that repaints the current
// terminal state: the window title, restored input/rendering modes, then the
// screen (with recent scrollback when on the primary screen), then the cursor
// placed where the program left it. The returned bytes are streamed to a fresh
// attacher before its buffered live output is flushed.
func (s *screenBuffer) snapshot() []byte {
	var b strings.Builder

	// The title the program set, for the same reason as the modes below: it was
	// announced once, at startup, and a client that arrived after that never
	// saw it. A title that was never set writes nothing rather than an empty
	// one, which would clear whatever the client's own terminal had.
	writeOSC(&b, 1, s.iconName)
	writeOSC(&b, 2, s.title)

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

// maxOSCString bounds a replayed title. It is state carried on every attach,
// and a program is free to set a title as long as it likes; xterm truncates for
// the same reason.
const maxOSCString = 512

// writeOSC writes one OSC string command, terminated with BEL: it is what
// almost everything that sets a title emits, and every terminal that
// understands the sequence at all understands that form of it.
//
// The payload is written only if there is one, and only up to the first control
// character — a title carrying an ESC or a BEL would end the sequence early and
// leave the rest of it printing on the client's screen as text.
func writeOSC(b *strings.Builder, cmd int, s string) {
	if s == "" {
		return
	}
	if len(s) > maxOSCString {
		s = s[:maxOSCString]
	}
	if i := strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }); i >= 0 {
		s = s[:i]
		if s == "" {
			return
		}
	}
	fmt.Fprintf(b, "\x1b]%d;%s\a", cmd, s)
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
