// Package selection turns mouse gestures over a cell grid into a text
// selection: press-drag-release, double-click for a word, triple-click for a
// line, and a rectangular block mode. It reads cells and reports back spans to
// highlight and the text they hold; it never draws anything and never touches
// a clipboard.
//
// Coordinates are absolute: a line index that names the same content for as
// long as that content exists, however the view over it scrolls. The grid owner
// decides what absolute means — for a terminal emulator it is "lines ever
// scrolled off, then the screen" — and is responsible for clearing the
// selection when its coordinates stop meaning what they meant (content
// overwritten, buffer resized, history evicted). See ADR 0036.
package selection

import (
	"strings"
	"time"
	"unicode"

	uv "github.com/charmbracelet/ultraviolet"
)

// Grid is the cell surface a selection reads. All of it must be addressable by
// (line, col) whether or not it is currently on any screen.
type Grid interface {
	// Width is the grid width in cells.
	Width() int

	// Lines is the absolute range that exists: lines [first, first+count).
	Lines() (first, count int)

	// Cell is the cell at an absolute line and column, nil when there is
	// nothing there — past the stored end of a line, or outside the grid.
	// The continuation column of a wide cell is a zero cell.
	Cell(line, col int) *uv.Cell

	// Wrapped reports whether line continues onto line+1 — a soft wrap, so
	// extraction joins the two without a newline.
	Wrapped(line int) bool
}

// Mode is what one gesture is selecting by.
type Mode int

const (
	// ModeNone: no gesture yet, or the selection was cleared.
	ModeNone Mode = iota
	// ModeCell: press and drag, cell by cell.
	ModeCell
	// ModeWord: double-click, extended word by word.
	ModeWord
	// ModeLine: triple-click, extended by whole logical lines.
	ModeLine
	// ModeBlock: the rectangle between anchor and cursor.
	ModeBlock
)

// Point is one cell, addressed absolutely.
type Point struct {
	Line, Col int
}

func (p Point) before(q Point) bool {
	return p.Line < q.Line || (p.Line == q.Line && p.Col < q.Col)
}

// doubleClickWindow is how close together two presses on the same cell must be
// to count as one gesture growing. The conventional desktop default.
const doubleClickWindow = 400 * time.Millisecond

// Model is one selection over one grid. The zero value is not usable; build
// one with [New].
type Model struct {
	grid Grid

	// now is the clock, injectable so click counting is testable.
	now func() time.Time

	mode   Mode
	anchor Point
	cursor Point
	// moved is whether the gesture has dragged onto another cell. A plain
	// click selects nothing until it moves; word, line and block start
	// selected. It stays true after the release so the selection persists.
	moved bool
	// down is whether the button is currently held.
	down bool

	// Click counting: presses on the same cell inside the window grow the
	// gesture from cell to word to line.
	clicks   int
	lastDown time.Time
	lastAt   Point
}

// New builds a selection over a grid.
func New(grid Grid) *Model {
	return &Model{grid: grid, now: time.Now}
}

// MouseDown starts or grows a gesture at an absolute position. block is
// whether the block modifier was held, which selects the rectangle between
// here and wherever the drag goes.
//
// A press replaces whatever selection there was: clearing on click is how
// every selection since the first one has worked.
func (m *Model) MouseDown(line, col int, block bool) {
	at := m.snap(line, col)

	now := m.now()
	if now.Sub(m.lastDown) <= doubleClickWindow && at == m.lastAt {
		m.clicks++
	} else {
		m.clicks = 1
	}
	m.lastDown, m.lastAt = now, at

	m.down = true
	m.anchor, m.cursor = at, at
	switch {
	case block:
		m.mode, m.moved = ModeBlock, false
	case (m.clicks-1)%3 == 1:
		m.mode, m.moved = ModeWord, true
	case (m.clicks-1)%3 == 2:
		m.mode, m.moved = ModeLine, true
	default:
		m.mode, m.moved = ModeCell, false
	}
}

// MouseDrag extends the gesture to an absolute position.
func (m *Model) MouseDrag(line, col int) {
	if !m.down {
		return
	}
	at := m.snap(line, col)
	if at != m.cursor {
		m.moved = true
	}
	m.cursor = at
}

// MouseUp ends the gesture. It returns the selected text when the gesture
// selected something — this is the moment a host copies. The selection itself
// stays, highlighted, until the next press or [Model.Clear].
func (m *Model) MouseUp() (string, bool) {
	if !m.down {
		return "", false
	}
	m.down = false
	if !m.Active() {
		m.mode = ModeNone
		return "", false
	}
	text := m.Text()
	return text, text != ""
}

// Clear drops the selection.
func (m *Model) Clear() {
	m.mode, m.moved, m.down = ModeNone, false, false
}

// Active reports whether there is a selection to draw. A plain click that
// never moved is not one.
func (m *Model) Active() bool {
	return m.mode != ModeNone && m.moved
}

// Dragging reports whether the button is down mid-gesture.
func (m *Model) Dragging() bool { return m.down }

// Extent is the selection's normalized bounds, both ends inclusive. For block
// mode the two points are opposite corners of the rectangle; otherwise they
// are the first and last selected cell in reading order.
func (m *Model) Extent() (from, to Point, ok bool) {
	if !m.Active() {
		return Point{}, Point{}, false
	}
	from, to = m.anchor, m.cursor
	if m.mode == ModeBlock {
		if to.Line < from.Line {
			from.Line, to.Line = to.Line, from.Line
		}
		if to.Col < from.Col {
			from.Col, to.Col = to.Col, from.Col
		}
		return from, to, true
	}
	if to.before(from) {
		from, to = to, from
	}
	switch m.mode {
	case ModeWord:
		from = m.wordStart(from)
		to = m.wordEnd(to)
	case ModeLine:
		from = Point{Line: m.logicalStart(from.Line), Col: 0}
		to = Point{Line: m.logicalEnd(to.Line), Col: m.grid.Width() - 1}
	}
	return from, to, true
}

// Span is the columns of one line to highlight, both ends inclusive, and
// whether the line holds any of the selection. The end is extended over a wide
// cell's continuation column, so a highlight never cuts a glyph in half.
func (m *Model) Span(line int) (start, end int, ok bool) {
	from, to, ok := m.Extent()
	if !ok || line < from.Line || line > to.Line {
		return 0, 0, false
	}
	if m.mode == ModeBlock {
		start, end = from.Col, to.Col
	} else {
		start, end = 0, m.grid.Width()-1
		if line == from.Line {
			start = from.Col
		}
		if line == to.Line {
			end = to.Col
		}
	}
	if c := m.grid.Cell(line, end); c != nil && c.Width > 1 {
		end += c.Width - 1
	}
	return start, end, true
}

// Text is the selected text: soft-wrapped lines joined without a newline,
// trailing pad spaces trimmed from every hard line end, block rectangles read
// row by row.
func (m *Model) Text() string {
	from, to, ok := m.Extent()
	if !ok {
		return ""
	}

	var b strings.Builder
	if m.mode == ModeBlock {
		for line := from.Line; line <= to.Line; line++ {
			if line > from.Line {
				b.WriteByte('\n')
			}
			b.WriteString(strings.TrimRight(m.rowText(line, from.Col, to.Col), " "))
		}
		return b.String()
	}

	width := m.grid.Width()
	for line := from.Line; line <= to.Line; line++ {
		startCol, endCol := 0, width-1
		if line == from.Line {
			startCol = from.Col
		}
		if line == to.Line {
			endCol = to.Col
		}
		row := m.rowText(line, startCol, endCol)
		if line < to.Line && m.grid.Wrapped(line) {
			// A soft wrap: the next line is the same line of text, so
			// nothing is trimmed and nothing separates them. A wrapped row
			// filled every cell, so its trailing spaces are content.
			b.WriteString(row)
			continue
		}
		b.WriteString(strings.TrimRight(row, " "))
		if line < to.Line {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// rowText reads one line's cells between two columns, inclusive. Continuation
// columns of wide cells contribute nothing — their glyph is in the primary
// column — and cells past a line's stored end read as spaces.
func (m *Model) rowText(line, startCol, endCol int) string {
	var b strings.Builder
	for col := startCol; col <= endCol; col++ {
		c := m.grid.Cell(line, col)
		switch {
		case c == nil:
			b.WriteByte(' ')
		case c.IsZero():
			// Wide continuation; the primary cell already wrote the glyph.
		default:
			b.WriteString(c.Content)
		}
	}
	return b.String()
}

// snap clamps a position onto the grid and moves a hit on a wide cell's
// continuation column back onto the glyph itself.
func (m *Model) snap(line, col int) Point {
	first, count := m.grid.Lines()
	line = max(first, min(line, first+count-1))
	col = max(0, min(col, m.grid.Width()-1))
	for col > 0 {
		c := m.grid.Cell(line, col)
		if c != nil && c.IsZero() {
			col--
			continue
		}
		break
	}
	return Point{Line: line, Col: col}
}

// wordStart walks left from a point to the start of the run it is in,
// following a soft wrap up onto the previous line: a word split by wrapping is
// still one word.
func (m *Model) wordStart(p Point) Point {
	first, _ := m.grid.Lines()
	class := m.classAt(p)
	for {
		q := p
		if q.Col > 0 {
			q.Col--
		} else if q.Line > first && m.grid.Wrapped(q.Line-1) {
			q.Line, q.Col = q.Line-1, m.grid.Width()-1
		} else {
			return p
		}
		q = m.snap(q.Line, q.Col)
		if m.classAt(q) != class {
			return p
		}
		p = q
	}
}

// wordEnd walks right to the end of the run, following a soft wrap down.
func (m *Model) wordEnd(p Point) Point {
	first, count := m.grid.Lines()
	last := first + count - 1
	class := m.classAt(p)
	for {
		q := p
		if w := m.cellWidth(q); q.Col+w < m.grid.Width() {
			q.Col += w
		} else if q.Line < last && m.grid.Wrapped(q.Line) {
			q.Line, q.Col = q.Line+1, 0
		} else {
			return p
		}
		if m.classAt(q) != class {
			return p
		}
		p = q
	}
}

// logicalStart walks up to the first row of the logical line containing line.
func (m *Model) logicalStart(line int) int {
	first, _ := m.grid.Lines()
	for line > first && m.grid.Wrapped(line-1) {
		line--
	}
	return line
}

// logicalEnd walks down to the last row of the logical line containing line.
func (m *Model) logicalEnd(line int) int {
	first, count := m.grid.Lines()
	last := first + count - 1
	for line < last && m.grid.Wrapped(line) {
		line++
	}
	return line
}

// cellWidth is how many columns the cell at a point occupies, at least one.
func (m *Model) cellWidth(p Point) int {
	if c := m.grid.Cell(p.Line, p.Col); c != nil && c.Width > 1 {
		return c.Width
	}
	return 1
}

// classAt is the double-click character class of a cell: blanks group with
// blanks and word characters with word characters, while each distinct
// punctuation glyph groups only with itself — so a run of dashes is one
// selection and "foo(bar" breaks at the paren. That is xterm's default
// arrangement.
func (m *Model) classAt(p Point) string {
	c := m.grid.Cell(p.Line, p.Col)
	if c == nil || c.IsZero() || strings.TrimSpace(c.Content) == "" {
		return " "
	}
	r := []rune(c.Content)[0]
	if r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) {
		return "w"
	}
	return c.Content
}
