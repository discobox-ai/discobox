package selection

import (
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// fakeGrid is a grid built from strings. Wide runes occupy their real width: a
// primary cell holding the glyph and zero cells for its continuation columns,
// the same shape the emulator stores.
type fakeGrid struct {
	width   int
	lines   []uv.Line
	wrapped []bool
}

func newGrid(width int, rows ...string) *fakeGrid {
	g := &fakeGrid{width: width}
	for _, row := range rows {
		wrapped := false
		if len(row) > 0 && row[len(row)-1] == '\\' {
			wrapped, row = true, row[:len(row)-1]
		}
		line := make(uv.Line, 0, width)
		for _, r := range row {
			w := ansi.StringWidth(string(r))
			line = append(line, uv.Cell{Content: string(r), Width: w})
			for i := 1; i < w; i++ {
				line = append(line, uv.Cell{})
			}
		}
		for len(line) < width {
			line = append(line, uv.EmptyCell)
		}
		g.lines = append(g.lines, line[:width])
		g.wrapped = append(g.wrapped, wrapped)
	}
	return g
}

func (g *fakeGrid) Width() int        { return g.width }
func (g *fakeGrid) Lines() (int, int) { return 0, len(g.lines) }
func (g *fakeGrid) Wrapped(line int) bool {
	return line >= 0 && line < len(g.wrapped) && g.wrapped[line]
}
func (g *fakeGrid) Cell(line, col int) *uv.Cell {
	if line < 0 || line >= len(g.lines) || col < 0 || col >= g.width {
		return nil
	}
	return &g.lines[line][col]
}

// sel builds a selection over the grid with a stopped clock the tests advance.
func sel(g *fakeGrid) (*Model, *time.Time) {
	now := time.Unix(0, 0)
	m := New(g)
	m.now = func() time.Time { return now }
	return m, &now
}

// click presses n times at the same cell, inside the double-click window.
func click(m *Model, now *time.Time, n, line, col int) {
	for range n {
		*now = now.Add(10 * time.Millisecond)
		m.MouseDown(line, col, false)
	}
}

func TestDragSelectsCells(t *testing.T) {
	g := newGrid(10, "hello disc", "and more")
	m, now := sel(g)
	click(m, now, 1, 0, 2)
	if m.Active() {
		t.Fatal("active before any drag")
	}
	m.MouseDrag(0, 6)
	text, ok := m.MouseUp()
	if !ok || text != "llo d" {
		t.Fatalf("got %q, %v", text, ok)
	}
	if !m.Active() {
		t.Fatal("selection should persist after release")
	}
}

func TestClickWithoutDragSelectsNothing(t *testing.T) {
	g := newGrid(10, "hello")
	m, now := sel(g)
	click(m, now, 1, 0, 2)
	if text, ok := m.MouseUp(); ok || text != "" {
		t.Fatalf("got %q, %v", text, ok)
	}
	if m.Active() {
		t.Fatal("plain click left a selection")
	}
}

func TestReverseDragNormalizes(t *testing.T) {
	g := newGrid(10, "hello disc")
	m, now := sel(g)
	click(m, now, 1, 0, 6)
	m.MouseDrag(0, 2)
	if text, _ := m.MouseUp(); text != "llo d" {
		t.Fatalf("got %q", text)
	}
}

func TestHardLinesJoinWithNewlineAndTrim(t *testing.T) {
	g := newGrid(10, "first", "second")
	m, now := sel(g)
	click(m, now, 1, 0, 0)
	m.MouseDrag(1, 4)
	if text, _ := m.MouseUp(); text != "first\nsecon" {
		t.Fatalf("got %q", text)
	}
}

func TestWrappedLinesJoinWithoutNewline(t *testing.T) {
	g := newGrid(5, `wrapp\`, "ed")
	m, now := sel(g)
	click(m, now, 1, 0, 0)
	m.MouseDrag(1, 1)
	if text, _ := m.MouseUp(); text != "wrapped" {
		t.Fatalf("got %q", text)
	}
}

func TestDoubleClickSelectsWord(t *testing.T) {
	g := newGrid(20, "one two-three four")
	m, now := sel(g)
	click(m, now, 2, 0, 5)
	if text, _ := m.MouseUp(); text != "two" {
		t.Fatalf("got %q", text)
	}
}

func TestDoubleClickPunctuationGroupsOnlyItself(t *testing.T) {
	g := newGrid(20, "a --- b")
	m, now := sel(g)
	click(m, now, 2, 0, 3)
	if text, _ := m.MouseUp(); text != "---" {
		t.Fatalf("got %q", text)
	}
}

func TestDoubleClickWordAcrossWrap(t *testing.T) {
	g := newGrid(5, `ab cd\`, "ef gh")
	m, now := sel(g)
	// "cdef" is one word split by the wrap; click either half.
	click(m, now, 2, 1, 0)
	if text, _ := m.MouseUp(); text != "cdef" {
		t.Fatalf("got %q", text)
	}
}

func TestDoubleClickThenDragExtendsByWords(t *testing.T) {
	g := newGrid(20, "one two three")
	m, now := sel(g)
	click(m, now, 2, 0, 5)
	m.MouseDrag(0, 9)
	if text, _ := m.MouseUp(); text != "two three" {
		t.Fatalf("got %q", text)
	}
}

func TestTripleClickSelectsLogicalLine(t *testing.T) {
	g := newGrid(5, `long \`, "line", "next")
	m, now := sel(g)
	click(m, now, 3, 1, 2)
	if text, _ := m.MouseUp(); text != "long line" {
		t.Fatalf("got %q", text)
	}
}

func TestFourthClickCyclesBackToCell(t *testing.T) {
	g := newGrid(10, "hello")
	m, now := sel(g)
	click(m, now, 4, 0, 2)
	if m.Active() {
		t.Fatal("fourth click should be a plain click again")
	}
}

func TestClickWindowExpires(t *testing.T) {
	g := newGrid(10, "hello")
	m, now := sel(g)
	click(m, now, 1, 0, 2)
	m.MouseUp()
	*now = now.Add(time.Second)
	m.MouseDown(0, 2, false)
	if m.Active() {
		t.Fatal("a slow second click is a new single click")
	}
}

func TestBlockSelectsRectangle(t *testing.T) {
	g := newGrid(10, "abcdef", "ghijkl", "mnopqr")
	m, now := sel(g)
	_ = now
	m.MouseDown(0, 1, true)
	m.MouseDrag(2, 3)
	if text, _ := m.MouseUp(); text != "bcd\nhij\nnop" {
		t.Fatalf("got %q", text)
	}
}

func TestBlockNormalizesCorners(t *testing.T) {
	g := newGrid(10, "abcdef", "ghijkl")
	m, _ := sel(g)
	m.MouseDown(1, 3, true)
	m.MouseDrag(0, 1)
	if text, _ := m.MouseUp(); text != "bcd\nhij" {
		t.Fatalf("got %q", text)
	}
}

func TestWideRuneSnapAndExtract(t *testing.T) {
	// "日本" is four columns: glyph, continuation, glyph, continuation.
	g := newGrid(10, "日本 ok")
	m, now := sel(g)
	// Press on the continuation column of 日 snaps back onto it.
	click(m, now, 1, 0, 1)
	m.MouseDrag(0, 2)
	if text, _ := m.MouseUp(); text != "日本" {
		t.Fatalf("got %q", text)
	}
}

func TestSpanCoversWideCellEnd(t *testing.T) {
	g := newGrid(10, "a日b")
	m, now := sel(g)
	click(m, now, 1, 0, 0)
	m.MouseDrag(0, 1) // end on 日's primary column
	start, end, ok := m.Span(0)
	if !ok || start != 0 || end != 2 {
		t.Fatalf("span [%d,%d] %v, want [0,2]", start, end, ok)
	}
}

func TestSpansAcrossLines(t *testing.T) {
	g := newGrid(6, "abcdef", "ghijkl", "mnopqr")
	m, now := sel(g)
	click(m, now, 1, 0, 3)
	m.MouseDrag(2, 2)
	m.MouseUp()
	for _, want := range []struct{ line, start, end int }{
		{0, 3, 5}, {1, 0, 5}, {2, 0, 2},
	} {
		start, end, ok := m.Span(want.line)
		if !ok || start != want.start || end != want.end {
			t.Fatalf("line %d span [%d,%d] %v, want [%d,%d]",
				want.line, start, end, ok, want.start, want.end)
		}
	}
	if _, _, ok := m.Span(3); ok {
		t.Fatal("line past the selection reported a span")
	}
}

func TestSnapClampsToGrid(t *testing.T) {
	g := newGrid(5, "abc")
	m, _ := sel(g)
	m.MouseDown(9, 9, false)
	m.MouseDrag(-3, -3)
	if text, _ := m.MouseUp(); text != "abc" {
		t.Fatalf("got %q", text)
	}
}

func TestClearDropsSelection(t *testing.T) {
	g := newGrid(10, "hello")
	m, now := sel(g)
	click(m, now, 1, 0, 0)
	m.MouseDrag(0, 4)
	m.MouseUp()
	m.Clear()
	if m.Active() {
		t.Fatal("still active after Clear")
	}
	if _, _, ok := m.Span(0); ok {
		t.Fatal("span survived Clear")
	}
}
