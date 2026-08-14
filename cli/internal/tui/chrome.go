package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/obot-platform/discobox/termpane/selection"
)

// The chrome — the header with the sandbox id in it, the hints line, the
// borders — is text on screen, and text on screen should be selectable. It
// gets the same selection component the panes use, over a much simpler grid:
// the composed frame itself, parsed back into cells, flat screen rows with
// nothing wrapped and no scrollback. A gesture belongs to the chrome when no
// pane claimed its press, and the double-click word rules make the common
// case — the sandbox id — one double-click, since ids are word characters
// throughout.
//
// A chrome selection is fragile by nature: the frame is recomposed on every
// message, and a selection whose text no longer reads back identically is
// cleared rather than left highlighting whatever moved in under it. The
// header is static enough that selections there live; the status line churns,
// and selections there honestly die with it.

// frameGrid is the composed frame as a [selection.Grid].
type frameGrid struct {
	lines []uv.Line
	width int
}

func (g *frameGrid) Width() int        { return g.width }
func (g *frameGrid) Lines() (int, int) { return 0, len(g.lines) }
func (g *frameGrid) Wrapped(int) bool  { return false }

func (g *frameGrid) Cell(line, col int) *uv.Cell {
	if line < 0 || line >= len(g.lines) {
		return nil
	}
	return g.lines[line].At(col)
}

// parseChrome rebuilds the chrome grid from a composed frame, by drawing the
// frame into a screen buffer the way a terminal would. Drawing rather than
// [uv.StyledString.Lines], because Lines clips against an empty rectangle
// and returns almost nothing; the draw path takes real bounds and lays wide
// cells out with their continuation columns, which is the shape the
// selection's coordinates need.
func (m *Model) parseChrome(frame string) {
	rows := strings.Count(frame, "\n") + 1
	buf := uv.NewScreenBuffer(max(m.width, 1), rows)
	uv.NewStyledString(frame).Draw(buf, buf.Bounds())
	m.chromeGrid.lines = buf.Lines
	m.chromeGrid.width = m.width
}

// chromeMouse drives the chrome selection with an event no pane claimed.
// Coordinates need no translation: the chrome's grid is the frame itself.
func (m *Model) chromeMouse(msg tea.MouseMsg) tea.Cmd {
	switch ev := msg.(type) {
	case tea.MouseClickMsg:
		if ev.Button != tea.MouseLeft {
			return nil
		}
		// The grid must be what is on screen at the moment of the press.
		m.parseChrome(m.lastFrame)
		m.chromeCapture = true
		m.clearPaneSelections(nil)
		m.chromeSel.MouseDown(ev.Y, ev.X, ev.Mod&tea.ModAlt != 0)
		m.chromeShot = m.chromeSel.Text()
	case tea.MouseMotionMsg:
		if ev.Button != tea.MouseLeft || !m.chromeCapture {
			return nil
		}
		m.chromeSel.MouseDrag(ev.Y, ev.X)
		m.chromeShot = m.chromeSel.Text()
	case tea.MouseReleaseMsg:
		if ev.Button != tea.MouseLeft {
			return nil
		}
		m.chromeCapture = false
		if text, ok := m.chromeSel.MouseUp(); ok {
			m.chromeShot = m.chromeSel.Text()
			return m.copyText(text)
		}
	}
	return nil
}

// chromeChord catches the copy chords while a chrome selection is showing,
// the way the panes catch them over their own; see termpane's copyChord.
func (m *Model) chromeChord(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	if !m.chromeSel.Active() {
		return nil, false
	}
	switch msg.Key().Keystroke() {
	case "ctrl+c", "ctrl+shift+c", "super+c":
	default:
		return nil, false
	}
	text := m.chromeSel.Text()
	m.chromeSel.Clear()
	if text == "" {
		return nil, false
	}
	return m.copyText(text), true
}

// paintChrome keeps the chrome selection honest against a freshly composed
// frame and paints its highlight over the affected rows. The frame is only
// parsed while a selection exists; the rest of the time this is a string
// assignment.
func (m *Model) paintChrome(frame string) string {
	m.lastFrame = frame
	if !m.chromeSel.Active() {
		return frame
	}
	m.parseChrome(frame)
	if m.chromeSel.Text() != m.chromeShot {
		// The text the selection named is no longer what is there.
		m.chromeSel.Clear()
		return frame
	}
	rows := strings.Split(frame, "\n")
	for i := range rows {
		if start, end, ok := m.chromeSel.Span(i); ok {
			rows[i] = m.chromeRow(i, start, end)
		}
	}
	return strings.Join(rows, "\n")
}

// chromeRow re-renders one frame row from its cells with the selection span
// in reverse video — the same cells extraction reads, so what is highlighted
// and what is copied cannot disagree.
func (m *Model) chromeRow(line, start, end int) string {
	row := make(uv.Line, 0, m.width)
	for col := range m.width {
		cell := uv.EmptyCell
		if c := m.chromeGrid.Cell(line, col); c != nil {
			cell = *c
		}
		if col >= start && col <= end && !cell.IsZero() {
			cell.Style.Attrs ^= uv.AttrReverse
		}
		row = append(row, cell)
	}
	return row.Render()
}

// clearPaneSelections drops every pane's selection but the given one: one
// selection on screen at a time, or two highlights race to answer the next
// copy chord.
func (m *Model) clearPaneSelections(except *pane) {
	for _, p := range append([]*pane{m.overlay, m.terminal}, m.shells...) {
		if p != nil && p != except {
			p.term.ClearSelection()
		}
	}
}

var _ selection.Grid = (*frameGrid)(nil)
