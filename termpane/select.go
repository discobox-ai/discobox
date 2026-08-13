package termpane

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
)

// CopyMsg is a finished selection. Text is what the user selected; putting it
// on a clipboard — tea.SetClipboard, an OS clipboard, both — is the host's
// business. The selection stays highlighted in the pane until something
// replaces or clears it.
type CopyMsg struct{ Text string }

// WithHighlight replaces how selected cells are drawn. The default toggles
// reverse video, which stays legible over anything the application painted;
// a themed host can substitute its own transform.
func WithHighlight(f func(uv.Style) uv.Style) Option {
	return func(o *options) { o.highlight = f }
}

func reverseVideo(s uv.Style) uv.Style {
	s.Attrs ^= uv.AttrReverse
	return s
}

// WithWheelLines sets how many lines one wheel tick moves, both through the
// scrollback and as arrow keys on the alternate screen. The default is 3.
func WithWheelLines(lines int) Option {
	return func(o *options) { o.wheelLines = lines }
}

// copyChord catches the copy keys — ctrl+c, ctrl+shift+c, super+c — while a
// selection is showing, and only then: the highlight on screen is what makes
// the chord mean copy rather than whatever it means to the application.
// Copying clears the selection, so on ctrl+c — which is also the interrupt —
// a second press is the interrupt, and nobody is ever trapped behind a
// selection they made. It outranks the pane's own reserved keys for the same
// reason it outranks the application's interrupt: a host whose detach key is
// ctrl+c still detaches, one press later.
//
// The enhanced chords are matched by Keystroke, which reads the modifiers
// even when the terminal attached text to the key; classic terminals cannot
// distinguish ctrl+shift+c from ctrl+c at all, and deliver the form the first
// case already catches. A selection of nothing but trimmed padding falls
// through: swallowing an interrupt to copy an empty string helps nobody.
func (m *Model) copyChord(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	if !m.HasSelection() {
		return nil, false
	}
	switch msg.Key().Keystroke() {
	case "ctrl+c", "ctrl+shift+c", "super+c":
	default:
		return nil, false
	}
	text := m.SelectionText()
	m.clearSelection()
	if text == "" {
		return nil, false
	}
	return func() tea.Msg { return CopyMsg{Text: text} }, true
}

// HandleMouse routes one mouse event, in cells relative to the pane's grid —
// the same origin as [Model.SendMouse] and [Model.Cursor].
//
// The application's mouse wins: while it has asked for the mouse (see
// [Model.MouseMode]) and the host has not seized it, every event is forwarded
// and selection is inert. Otherwise the left button drives selection —
// press-drag-release, double-click for a word, triple-click for a line, Alt
// for a rectangular block — and a gesture that selected something returns a
// command carrying [CopyMsg]. Dragging past the top or bottom edge scrolls
// the view and keeps selecting.
//
// The wheel goes to whoever can actually scroll. An application that has the
// mouse is forwarded the event and scrolls itself. One that never asked but
// is on the alternate screen — a pager, most of the time — is sent arrow
// keys instead, which is xterm's alternate-scroll bargain and the only
// scrolling such an application understands; the alternate screen has no
// scrollback to offer instead. Everything else scrolls the pane's own
// scrollback ([Model.Scroll]). A host with a different policy handles wheel
// events itself and simply does not delegate them.
func (m *Model) HandleMouse(msg tea.MouseMsg) tea.Cmd {
	if m.emu == nil || m.sel == nil {
		return nil
	}
	if m.MouseMode() != MouseNone && !m.seized {
		m.SendMouse(msg)
		return nil
	}
	switch ev := msg.(type) {
	case tea.MouseClickMsg:
		if ev.Button != tea.MouseLeft ||
			ev.X < 0 || ev.X >= m.cols || ev.Y < 0 || ev.Y >= m.rows {
			return nil
		}
		m.sel.MouseDown(m.absLine(ev.Y), ev.X, ev.Mod&tea.ModAlt != 0)
		m.snapshotSelection()
	case tea.MouseMotionMsg:
		if ev.Button != tea.MouseLeft || !m.sel.Dragging() {
			return nil
		}
		m.sel.MouseDrag(m.absLine(m.dragScroll(ev.Y)), ev.X)
		m.snapshotSelection()
	case tea.MouseReleaseMsg:
		if ev.Button != tea.MouseLeft {
			return nil
		}
		if text, ok := m.sel.MouseUp(); ok {
			m.snapshotSelection()
			return func() tea.Msg { return CopyMsg{Text: text} }
		}
	case tea.MouseWheelMsg:
		m.wheel(ev)
	}
	return nil
}

// defaultWheelLines is how far one wheel tick moves, xterm's and tmux's
// convention.
const defaultWheelLines = 3

// wheel scrolls for an application that never asked for the mouse: through
// the pane's scrollback, or — on the alternate screen, where there is none —
// by sending the arrow keys the application does understand, encoded through
// the same path as typed ones so cursor-key mode is respected.
func (m *Model) wheel(ev tea.MouseWheelMsg) {
	lines := m.opts.wheelLines
	if lines <= 0 {
		lines = defaultWheelLines
	}
	up := false
	switch ev.Button {
	case tea.MouseWheelUp:
		up = true
	case tea.MouseWheelDown:
	default:
		// Horizontal wheels scroll nothing here, the same as every terminal's
		// alternate-scroll: there is no sideways scrollback to offer.
		return
	}
	if m.AltScreen() {
		code := tea.KeyDown
		if up {
			code = tea.KeyUp
		}
		key := tea.Key{Code: code}
		for range lines {
			m.SendKey(tea.KeyPressMsg(key))
		}
		return
	}
	if up {
		m.Scroll(lines)
		return
	}
	m.Scroll(-lines)
}

// SetSeized takes the mouse for selection even while the application is asking
// for it — the escape hatch for copying out of a full-screen program. While
// seized, nothing is forwarded; the application simply sees no mouse. What
// arms it is host policy, and so is showing that it is armed: a user whose
// clicks vim suddenly ignores needs the chrome to say why.
func (m *Model) SetSeized(seized bool) { m.seized = seized }

// Seized reports whether the host has taken the mouse for selection.
func (m *Model) Seized() bool { return m.seized }

// HasSelection reports whether a selection is on display.
func (m *Model) HasSelection() bool { return m.sel != nil && m.sel.Active() }

// SelectionText is the selected text, empty when there is no selection. It is
// what a [CopyMsg] would carry; exposed for hosts with their own moments to
// copy at.
func (m *Model) SelectionText() string {
	if m.sel == nil {
		return ""
	}
	return m.sel.Text()
}

// ClearSelection drops the selection, if any.
func (m *Model) ClearSelection() { m.clearSelection() }

func (m *Model) clearSelection() {
	if m.sel != nil {
		m.sel.Clear()
	}
	m.selShot = ""
}

// absLine converts a view row to the absolute line the selection layer
// addresses: lines that have scrolled into history keep their index, and the
// live screen begins where history ends. See ADR 0036 §4.
func (m *Model) absLine(viewRow int) int {
	return m.ScrollbackLen() + viewRow - m.scroll
}

// dragScroll turns a drag past the pane's vertical edge into scrolling, and
// returns the row the drag effectively points at.
func (m *Model) dragScroll(y int) int {
	if y < 0 {
		m.Scroll(-y)
		return 0
	}
	if y >= m.rows {
		m.Scroll(m.rows - 1 - y)
		return m.rows - 1
	}
	return y
}

// snapshotSelection records what the selection reads right now, so new output
// can be told apart from output that merely scrolled the selection into
// history: scrolled content reads back identical at the same absolute lines,
// overwritten content does not.
func (m *Model) snapshotSelection() {
	m.selShot = ""
	m.selAlt = m.AltScreen()
	if m.sel != nil && m.sel.Active() {
		m.selShot = m.sel.Text()
	}
}

// reconcileSelection is called after output is written: a selection whose
// coordinates no longer mean what they meant is cleared rather than left to
// slide onto text nobody selected. See ADR 0036 §4.
func (m *Model) reconcileSelection() {
	if m.sel == nil || !m.sel.Active() {
		return
	}
	if m.AltScreen() != m.selAlt {
		m.clearSelection()
		return
	}
	// A full scrollback evicts a line for every line pushed, shifting every
	// history index under a selection that touches it. Eviction itself is
	// invisible from here, so full-plus-output is the signal — conservative,
	// and honest where a slid highlight would be silently wrong.
	if from, _, ok := m.sel.Extent(); ok && from.Line < m.ScrollbackLen() && m.scrollbackFull() {
		m.clearSelection()
		return
	}
	if m.sel.Text() != m.selShot {
		m.clearSelection()
	}
}

func (m *Model) scrollbackFull() bool {
	maxLines := m.opts.scrollback
	if maxLines <= 0 {
		maxLines = vt.DefaultScrollbackSize
	}
	return m.ScrollbackLen() >= maxLines
}

// highlightRow re-renders one view row from its cells with the selection's
// span restyled, so the highlight is applied to the grid rather than spliced
// into a rendered string — the same cells extraction reads, which is what
// keeps what you see and what you copy the same thing. It reports false for
// rows the selection does not touch, which keeps the ordinary path free of
// per-cell work.
func (m *Model) highlightRow(viewRow int) (string, bool) {
	line := m.absLine(viewRow)
	start, end, ok := m.sel.Span(line)
	if !ok || line < 0 {
		return "", false
	}
	grid := emuGrid{m}
	highlight := m.opts.highlight
	if highlight == nil {
		highlight = reverseVideo
	}
	row := make(uv.Line, 0, m.cols)
	for col := range m.cols {
		cell := uv.EmptyCell
		if c := grid.Cell(line, col); c != nil {
			cell = *c
		}
		// Continuation columns of wide cells stay zero: their glyph is in
		// the primary cell, whose style already covers both columns.
		if col >= start && col <= end && !cell.IsZero() {
			cell.Style = highlight(cell.Style)
		}
		row = append(row, cell)
	}
	return strings.TrimSuffix(row.Render(), "\n"), true
}

// emuGrid adapts the emulator to [selection.Grid]: one absolute line space
// with the scrollback first and the live screen after it.
type emuGrid struct{ m *Model }

func (g emuGrid) Width() int { return g.m.cols }

func (g emuGrid) Lines() (first, count int) {
	return 0, g.m.ScrollbackLen() + g.m.rows
}

func (g emuGrid) Cell(line, col int) *uv.Cell {
	if g.m.emu == nil {
		return nil
	}
	sb := g.m.ScrollbackLen()
	if line < sb {
		return g.m.emu.ScrollbackCellAt(col, line)
	}
	return g.m.emu.CellAt(col, line-sb)
}

// Wrapped reports a soft wrap by whether the line's last cell holds content, a
// heuristic: the emulator does not record wrap, and a line that reached the
// last column is one that wrapped, while a hard newline leaves the tail blank.
// The one miss is a hard line exactly the width of the pane, which reads as
// wrapped. ADR 0036 §5 trades that for shipping ahead of the upstream wrap
// flag, behind this exact seam.
func (g emuGrid) Wrapped(line int) bool {
	c := g.Cell(line, g.m.cols-1)
	return c != nil && !c.Equal(&uv.EmptyCell)
}
