package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// sandboxList is the upper pane: every sandbox in the project, most recently
// used first.
//
// A row is one line: a glyph for the state, the name, the harness, where it
// came from, when it was last used and what it has changed. The fixed width
// columns drop off the right end as the terminal narrows — the name and the
// glyph are what the eye actually picks a sandbox by, and neither ever goes.
type sandboxList struct {
	all      []sandbox
	cursor   int
	offset   int
	selected map[string]bool
	onlyHere bool

	// Archived sandboxes are history: kept, listed on request, and out of the
	// way until then.
	showArchived bool

	// Visual mode, lifted from difftui's diff: V anchors here, moving extends
	// the range, and a command acts on the whole of it.
	visual bool
	anchor int

	// A name too long for its column is ellipsized, and the row under the
	// cursor can be scrolled sideways to read the rest. The measurements come
	// from the last render, which is the only place the column width is known.
	nameScroll int
	nameWidth  int
	nameFull   int

	width, height int
}

func newSandboxList(all []sandbox) *sandboxList {
	return &sandboxList{all: all, selected: map[string]bool{}}
}

// rows is what is actually displayed, after the two filters.
func (l *sandboxList) rows() []sandbox {
	out := make([]sandbox, 0, len(l.all))
	for _, s := range l.all {
		if l.onlyHere && !s.here() {
			continue
		}
		if !l.showArchived && s.state == stateArchived {
			continue
		}
		out = append(out, s)
	}
	return out
}

// archivedCount is what the title bar offers when they are hidden: a number
// worth pressing a key for, or nothing to say.
func (l *sandboxList) archivedCount() int {
	n := 0
	for _, s := range l.all {
		if s.state == stateArchived && (!l.onlyHere || s.here()) {
			n++
		}
	}
	return n
}

func (l *sandboxList) current() *sandbox {
	rows := l.rows()
	if l.cursor < 0 || l.cursor >= len(rows) {
		return nil
	}
	return &rows[l.cursor]
}

// targets returns the sandboxes a command should act on: the visual range
// while one is being drawn, otherwise everything selected, otherwise the row
// under the cursor.
func (l *sandboxList) targets() []sandbox {
	rows := l.rows()
	if l.visual {
		lo, hi := l.visualRange()
		return rows[lo : hi+1]
	}
	var out []sandbox
	for _, s := range rows {
		if l.selected[s.id] {
			out = append(out, s)
		}
	}
	if len(out) > 0 {
		return out
	}
	if s := l.current(); s != nil {
		return []sandbox{*s}
	}
	return nil
}

// visualRange is the inclusive span between the anchor and the cursor.
func (l *sandboxList) visualRange() (int, int) {
	return min(l.anchor, l.cursor), max(l.anchor, l.cursor)
}

func (l *sandboxList) toggleVisual() {
	l.visual = !l.visual
	l.anchor = l.cursor
}

// commitVisual folds the range into the selection, which is what Space does:
// the range becomes marks that survive the mode ending.
func (l *sandboxList) commitVisual() int {
	lo, hi := l.visualRange()
	rows := l.rows()
	for _, s := range rows[lo : hi+1] {
		l.selected[s.id] = true
	}
	l.visual = false
	return hi - lo + 1
}

func (l *sandboxList) selectionCount() int {
	n := 0
	for _, s := range l.rows() {
		if l.selected[s.id] {
			n++
		}
	}
	return n
}

func (l *sandboxList) toggleSelect() {
	s := l.current()
	if s == nil {
		return
	}
	if l.selected[s.id] {
		delete(l.selected, s.id)
		return
	}
	l.selected[s.id] = true
}

func (l *sandboxList) clearSelection() { l.selected = map[string]bool{} }

func (l *sandboxList) move(delta int) {
	l.cursor += delta
	l.nameScroll = 0
	l.clamp()
}

func (l *sandboxList) moveTo(i int) {
	l.cursor = i
	l.nameScroll = 0
	l.clamp()
}

// scrollName walks a long name under the cursor sideways. The bound is what
// the last render measured: a name that fits does not move at all.
func (l *sandboxList) scrollName(delta int) bool {
	next := min(max(l.nameScroll+delta, 0), l.maxNameScroll())
	if next == l.nameScroll {
		return false
	}
	l.nameScroll = next
	return true
}

// maxNameScroll is how far the name can go. Once it has moved at all it wears
// a leading ellipsis, and that ellipsis occupies a cell of the column — so the
// end of the name is one cell further away than the overflow suggests. Without
// this the last character is permanently one press out of reach.
func (l *sandboxList) maxNameScroll() int {
	over := l.nameFull - l.nameWidth
	if over <= 0 {
		return 0
	}
	return over + 1
}

func (l *sandboxList) clamp() {
	n := len(l.rows())
	if l.cursor >= n {
		l.cursor = n - 1
	}
	if l.cursor < 0 {
		l.cursor = 0
	}
	if l.height <= 0 {
		return
	}
	if l.cursor < l.offset {
		l.offset = l.cursor
	}
	if l.cursor >= l.offset+l.height {
		l.offset = l.cursor - l.height + 1
	}
	if l.offset < 0 {
		l.offset = 0
	}
}

func (l *sandboxList) stateStyle(st *styles, s sandbox) lipgloss.Style {
	switch s.state {
	case stateRunning:
		return st.stateRun
	case stateStarting:
		return st.stateBusy
	case stateError:
		return st.stateErr
	default:
		return st.stateOff
	}
}

func stateDot(s sandbox) string {
	switch s.state {
	case stateRunning:
		return "●"
	case stateStarting:
		return "◐"
	case stateError:
		return "✗"
	case stateArchived:
		return "▪"
	default:
		return "○"
	}
}

func (l *sandboxList) view(st *styles, focused bool) string {
	titleStyle := st.titleList
	if !focused {
		titleStyle = st.titleDim
	}
	rows := l.rows()
	right := plural(len(rows), "sandbox", "sandboxes")
	if n := l.selectionCount(); n > 0 {
		right = plural(n, "selected", "selected") + "  ·  " + right
	}
	scope := "Sandboxes"
	if l.onlyHere {
		scope = "Sandboxes in " + currentDir
	}
	if n := l.archivedCount(); n > 0 && !l.showArchived {
		scope += "   " + plural(n, "archived", "archived") + ", A shows them"
	}
	blank := strings.Repeat(" ", max(l.width, 0))
	// The title bar is a band of colour; a row butted straight against it
	// reads as part of it. One blank line is enough to break that.
	out := []string{renderTitle(titleStyle, scope, right, l.width), blank}

	body := make([]string, 0, l.height)
	if len(rows) == 0 {
		body = append(body, st.dimText.Render(pad("  no sandboxes yet — type a prompt below", l.width)))
	}
	for i := l.offset; i < len(rows) && len(body) < l.height; i++ {
		body = append(body, l.row(st, rows[i], i, focused))
	}
	for len(body) < l.height {
		body = append(body, blank)
	}
	// And one after the last row, so a list long enough to reach the composer
	// still has air between them.
	return lipgloss.JoinVertical(lipgloss.Left, append(append(out, body...), blank)...)
}

// row draws one sandbox. Widths are budgeted left to right and the columns
// drop off the right end as the terminal narrows: the last used time goes
// first, then the origin, then the diffstat, and the name never goes at all.
func (l *sandboxList) row(st *styles, s sandbox, i int, focused bool) string {
	// The cursor belongs to the pane that has focus. With the prompt focused
	// there is nothing to act on, so nothing is drawn as picked out — only
	// the marks, which were put there deliberately and outlive the focus.
	atCursor := i == l.cursor && focused
	inVisual := false
	if l.visual {
		lo, hi := l.visualRange()
		inVisual = i >= lo && i <= hi
	}

	// Selection is a background rather than a column of bullets: it is the
	// state of the row, not a field of it, and a column spent on it is a
	// column the name does not get.
	selected := l.selected[s.id] || inVisual

	// The state is the coloured glyph in front of the name and nothing else:
	// spelling "running" out on every row costs a column that says the same
	// thing the dot already said, in the one place the eye is not looking.
	//
	// Without colour that trade reverses. Half of what the glyphs carry is
	// their colour — a stopped ○ and an archived ▪ are a pixel apart in
	// monochrome — so the glyph goes and the word comes back.
	//
	// Columns are added in the order they matter and the last that fits wins:
	// where it came from, then when it was last used, then what it has
	// changed. The diffstat is the first to go, because it is the one thing
	// the diff and apply actions will tell you anyway.
	glyph := colorEnabled()

	tail := ""
	addCol := func(text string, w int) {
		if l.width-lipgloss.Width(tail)-w < 20 {
			return
		}
		tail += padANSI(text, w)
	}

	if !glyph {
		addCol("  "+pad(string(s.state), 8), 10)
	}

	up := " "
	if s.upgrade {
		up = st.statusWA.Render("↑")
	}
	addCol("  "+st.dimText.Render(pad(s.harness, 7)), 9)
	addCol(up, 2)

	// Origin: the folder the sandbox came from and the commit it was cut at.
	// A folder other than this one is the thing most worth noticing on a row,
	// so it is the one dim column that is allowed a colour.
	originStyle := st.dimText
	if !s.here() {
		originStyle = st.info
	}
	addCol(originStyle.Render(pad(s.origin(), 12)), 13)
	baseStyle := st.dimText
	if s.dirty {
		baseStyle = st.statusWA
	}
	addCol(baseStyle.Render(pad(s.base(), 14)), 15)
	addCol(st.dimText.Render(pad(s.lastUsed+" ago", 7)), 8)
	addCol(usage(st, s), 19)
	stat := ""
	if s.hasDiff() {
		stat = st.add.Render("+"+itoa(s.add)) + " " + st.del.Render("−"+itoa(s.del))
	}
	addCol(padANSI(stat, 11), 11)

	// The cursor is a chevron, and selection is a background — difftui's
	// file list, which is a picker like this one rather than a diff.
	marker := "  "
	if atCursor {
		marker = st.key.Render("❯") + " "
	}

	head := marker
	if glyph {
		head += l.stateStyle(st, s).Render(stateDot(s)) + " "
	}
	nameW := max(l.width-lipgloss.Width(head)-lipgloss.Width(tail), 4)
	name := s.name
	if s.state == stateError && s.message != "" && atCursor {
		name = s.message
	}
	if atCursor {
		// The cursor row is the one that can be scrolled, so it is the one
		// whose measurements are worth keeping for the next key press.
		l.nameWidth, l.nameFull = nameW, lipgloss.Width(name)
		if l.nameScroll > 0 {
			l.nameScroll = min(l.nameScroll, l.maxNameScroll())
			name = "…" + trimLeft(name, l.nameScroll)
		}
	}
	nameStyle := st.name
	if atCursor || selected {
		nameStyle = st.cursorName
	}

	line := padANSI(head+padANSI(nameStyle.Render(truncate(name, nameW)), nameW)+tail, l.width)
	switch {
	case atCursor && selected:
		return highlight(line, colBothBG)
	case selected:
		return highlight(line, colSelectedBG)
	case atCursor:
		return highlight(line, colHighlightBG)
	default:
		return line
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
