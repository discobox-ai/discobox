package tui

import (
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

// sandboxList is the upper pane: every sandbox in the project, newest-created
// first.
//
// A row is one line: a glyph for the state, the name, the harness, where it
// came from, how old it is and what it has changed. The fixed width
// columns drop off the right end as the terminal narrows — the name and the
// glyph are what the eye actually picks a sandbox by, and neither ever goes.
type sandboxList struct {
	session  Session
	all      []Sandbox
	cursor   int
	offset   int
	selected map[string]bool

	// folder is the origin the list is filtered to, chosen in the header.
	// Empty is every folder, which is the one choice that is not a path.
	folder string

	// Archived sandboxes are history: kept, listed on request, and out of the
	// way until then.
	showArchived bool

	// Visual mode, lifted from difftui's diff: V anchors here, moving extends
	// the range, and a command acts on the whole of it.
	visual bool
	anchor int

	// visited records whether the cursor has been anywhere in this list yet.
	// Coming up out of the prompt lands on the row nearest it — the last one —
	// but only the first time: after that the cursor goes back where it was,
	// because leaving the list to type something and coming back is not the
	// same as arriving at it.
	visited bool

	// A name too long for its column is ellipsized, and the row under the
	// cursor can be scrolled sideways to read the rest. The measurements come
	// from the last render, which is the only place the column width is known.
	nameScroll int
	nameWidth  int
	nameFull   int

	// now is when the frame is being drawn, so the age column is a pure
	// function of the model and a test can render a fixed one.
	now func() time.Time

	width, height int
}

func newSandboxList(session Session) *sandboxList {
	return &sandboxList{session: session, selected: map[string]bool{}, now: time.Now}
}

// setAll takes a refreshed listing, keeping the cursor on the sandbox it was
// on rather than on the row number it was at: a list that reorders under you
// while something starts up would otherwise move the cursor onto a different
// sandbox between the key press and the action.
func (l *sandboxList) setAll(all []Sandbox) {
	var onID string
	if s := l.current(); s != nil {
		onID = s.ID
	}
	l.all = all

	if onID != "" {
		for i, s := range l.rows() {
			if s.ID == onID {
				l.cursor = i
				break
			}
		}
	}
	l.clamp()
}

// rows is what is actually displayed, after the two filters.
func (l *sandboxList) rows() []Sandbox {
	out := make([]Sandbox, 0, len(l.all))
	for _, s := range l.all {
		if l.folder != "" && s.Folder != l.folder {
			continue
		}
		if !l.showArchived && s.State == StateArchived {
			continue
		}
		out = append(out, s)
	}
	return out
}

// folders are the origins the header can filter to: every folder the project's
// sandboxes were started from, newest sandbox first, with this session's
// own directory leading whether or not anything was started from it yet.
//
// It is derived from the listing rather than asked for separately: the folders
// worth offering are exactly the ones something is sitting in.
func (l *sandboxList) folders() []string {
	seen := map[string]bool{}
	out := []string{}
	if dir := l.session.Directory; dir != "" {
		seen[dir] = true
		out = append(out, dir)
	}
	for _, s := range l.all {
		if s.Folder == "" || seen[s.Folder] {
			continue
		}
		seen[s.Folder] = true
		out = append(out, s.Folder)
	}
	return out
}

// archivedCount is what the title bar offers when they are hidden: a number
// worth pressing a key for, or nothing to say.
func (l *sandboxList) archivedCount() int {
	n := 0
	for _, s := range l.all {
		if s.State == StateArchived && (l.folder == "" || s.Folder == l.folder) {
			n++
		}
	}
	return n
}

func (l *sandboxList) current() *Sandbox {
	rows := l.rows()
	if l.cursor < 0 || l.cursor >= len(rows) {
		return nil
	}
	return &rows[l.cursor]
}

// targets returns the sandboxes a command should act on: the visual range
// while one is being drawn, otherwise everything selected, otherwise the row
// under the cursor.
func (l *sandboxList) targets() []Sandbox {
	rows := l.rows()
	if l.visual && len(rows) > 0 {
		lo, hi := l.visualRange()
		return rows[lo : hi+1]
	}
	var out []Sandbox
	for _, s := range rows {
		if l.selected[s.ID] {
			out = append(out, s)
		}
	}
	if len(out) > 0 {
		return out
	}
	if s := l.current(); s != nil {
		return []Sandbox{*s}
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
	if len(rows) == 0 {
		l.visual = false
		return 0
	}
	for _, s := range rows[lo : hi+1] {
		l.selected[s.ID] = true
	}
	l.visual = false
	return hi - lo + 1
}

func (l *sandboxList) selectionCount() int {
	n := 0
	for _, s := range l.rows() {
		if l.selected[s.ID] {
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
	if l.selected[s.ID] {
		delete(l.selected, s.ID)
		return
	}
	l.selected[s.ID] = true
}

func (l *sandboxList) clearSelection() { l.selected = map[string]bool{} }

func (l *sandboxList) move(delta int) {
	l.cursor += delta
	l.nameScroll = 0
	l.visited = true
	l.clamp()
}

func (l *sandboxList) moveTo(i int) {
	l.cursor = i
	l.nameScroll = 0
	l.visited = true
	l.clamp()
}

// resetCursor puts the cursor back at the top of a list that is now a different
// set of sandboxes, and forgets that it was ever anywhere: nobody has chosen a
// row in this list, so coming up from the prompt should land at its end again.
func (l *sandboxList) resetCursor() {
	l.moveTo(0)
	l.offset = 0
	l.visited = false
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
	if l.anchor >= n {
		l.anchor = max(n-1, 0)
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

func (l *sandboxList) stateStyle(st *styles, s Sandbox) lipgloss.Style {
	switch s.State {
	case StateRunning:
		return st.stateRun
	case StateStarting:
		return st.stateBusy
	case StateError:
		return st.stateErr
	default:
		return st.stateOff
	}
}

func stateDot(s Sandbox) string {
	switch s.State {
	case StateRunning:
		return "●"
	case StateStarting:
		return "◐"
	case StateError:
		return "✗"
	case StateArchived:
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
	right := plural(len(rows), "box", "boxes")
	if n := l.selectionCount(); n > 0 {
		right = plural(n, "selected", "selected") + "  ·  " + right
	}
	// The folder the list is filtered to is named in the header, which is where
	// it is chosen, so the title bar does not say it twice.
	scope := "Discoboxes"
	if n := l.archivedCount(); n > 0 && !l.showArchived {
		scope += "   " + plural(n, "archived", "archived") + ", A shows them"
	}
	blank := strings.Repeat(" ", max(l.width, 0))
	out := []string{renderTitle(titleStyle, scope, right, l.width)}

	body := make([]string, 0, l.height)
	if len(rows) == 0 {
		body = append(body, st.dimText.Render(pad("  no discoboxes here yet — type a prompt below", l.width)))
	}
	for i := l.offset; i < len(rows) && len(body) < l.height; i++ {
		body = append(body, l.row(st, rows[i], i, focused))
	}
	for len(body) < l.height {
		body = append(body, blank)
	}
	// One blank after the last row, so a list long enough to reach the composer
	// still has air between them. The title bar needs none above the rows: it
	// is a band of color, and that is edge enough on its own.
	return lipgloss.JoinVertical(lipgloss.Left, append(append(out, body...), blank)...)
}

// row draws one sandbox. Widths are budgeted left to right and the columns
// drop off the right end as the terminal narrows: the age goes
// first, then the origin, then the diffstat, and the name never goes at all.
func (l *sandboxList) row(st *styles, s Sandbox, i int, focused bool) string {
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
	selected := l.selected[s.ID] || inVisual

	// The state is the colored glyph in front of the name and nothing else:
	// spelling "running" out on every row costs a column that says the same
	// thing the dot already said, in the one place the eye is not looking.
	//
	// Without color that trade reverses. Half of what the glyphs carry is
	// their color — a stopped ○ and an archived ▪ are a pixel apart in
	// monochrome — so the glyph goes and the word comes back.
	//
	// Columns are added in the order they matter and the last that fits wins:
	// where it came from, then how old it is, then what it has
	// changed. The diffstat is the first to go, because it is the one thing
	// the apply action will tell you anyway.
	//
	// Where it came from is not among them; see the origin comment below.
	glyph := st.color

	tail := ""
	addCol := func(text string, w int) {
		if l.width-lipgloss.Width(tail)-w < 20 {
			return
		}
		tail += padANSI(text, w)
	}

	if !glyph {
		addCol("  "+pad(string(s.State), 8), 10)
	}

	up := " "
	if s.Upgrade {
		up = st.statusWA.Render("↑")
	}
	addCol("  "+st.dimText.Render(pad(s.Harness, 7)), 9)
	addCol(up, 2)

	// Where the work sits in git — the reported position once the sandbox's
	// agent has spoken, the spawn commit until then. Where it came from is not
	// a column: it is the header's dropdown, and every row on screen has
	// already been filtered to it — a column repeating the same value on every
	// row is a column spent saying nothing.
	//
	// The color is the state of the work: warning for uncommitted content that
	// only the sandbox holds, green for a head commit an apply has landed.
	baseStyle := st.dimText
	switch {
	case s.dirty():
		baseStyle = st.statusWA
	case s.Git.Applied:
		baseStyle = st.add
	case s.ahead():
		// Work no apply has landed is the state to notice before
		// archiving, so it stands in the default text against rows that are
		// dim because nothing on them is at stake.
		baseStyle = st.name
	}
	addCol(baseStyle.Render(pad(s.base(), 14)), 15)
	// The mark spelled out, in the mark's own color. It is added right after
	// the position so the two survive a narrowing terminal together.
	addCol(baseStyle.Render(pad(s.changes(), 7)), 8)
	addCol(st.dimText.Render(pad(createdText(s, l.now()), 7)), 8)
	addCol(usage(st, s), usageWidth+1)
	stat := ""
	if s.hasDiff() {
		stat = st.add.Render("+"+itoa(s.Diff.Added)) + " " + st.del.Render("−"+itoa(s.Diff.Deleted))
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
	name := s.Name
	if s.State == StateError && s.Message != "" && atCursor {
		name = s.Message
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
		return highlight(st, line, colBothBG)
	case selected:
		return highlight(st, line, colSelectedBG)
	case atCursor:
		return highlight(st, line, colHighlightBG)
	default:
		return line
	}
}
