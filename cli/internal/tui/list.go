package tui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/pkg/programstatus"
	"github.com/discobox-ai/discobox/pkg/sandboxservices"
)

// sandboxList is the upper pane: every sandbox in the project, as the
// console's list orders, sections, nests and filters them (console.List, ADR
// 26-11-07-876 §1). What it adds is the renderer's: the cursor, the marks and
// the visual range, scrolling, and the drawing.
//
// A row is one line (two in the workspace tree, for a tagged one: inTree):
// a glyph for the state, the name, the harness, where it
// came from, how old it is and what it has changed — in the tree only the
// glyph, the name and the marks (inTree). The fixed width
// columns drop off the right end as the terminal narrows — the name and the
// glyph are what the eye actually picks a sandbox by, and neither ever goes.
type sandboxList struct {
	console.List

	// resources is what the machine has and what Discobox is using of it,
	// drawn on the title band. It is the machine's, not this list's: the list
	// is filtered to a folder and the machine is not.
	resources console.Resources
	cursor    int
	offset    int
	selected  map[console.BoxRef]bool

	// inTree is the list drawn as the workspace tree's narrow column. A
	// row's tags go on a line of their own under it rather than beside the
	// name: a column that narrow gives the name all of its width and the tags
	// none (tagLine). And a row's tail is only its marks (row): there the
	// name is worth more than the harness or anything after it.
	inTree bool

	// Visual mode, lifted from discobox-review's diff: V anchors here, moving
	// extends the range, and a command acts on the whole of it.
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

	// drawn is which rows the last render actually put on screen, recorded by
	// the loop that drew them so the mouse hits exactly what is there. See
	// zones.go.
	drawn drawn

	width, height int
}

func newSandboxList(session console.Session) *sandboxList {
	return &sandboxList{List: console.NewList(session, listingStaleAfter), selected: map[console.BoxRef]bool{}}
}

// setAll takes a refreshed listing, keeping the cursor on the sandbox it was
// on rather than on the row number it was at: a list that reorders under you
// while something starts up would otherwise move the cursor onto a different
// sandbox between the key press and the action.
func (l *sandboxList) setAll(all []console.Sandbox) {
	l.keepingCursor(func() { l.SetAll(all) })
}

// keepingCursor makes a change to the rows and puts the cursor back on the box
// it was on. The box is read before the change: the rows may be changed in
// place (markArchiving), and read after, a box that has left the list is one
// the cursor was never on. A cursor whose box has left stays at its row
// number, on the row that takes its place.
func (l *sandboxList) keepingCursor(change func()) {
	var on console.BoxRef
	if s := l.current(); s != nil {
		on = console.BoxRef{Server: s.Server, ID: s.ID}
	}
	change()
	if on.ID != "" {
		for i, s := range l.Rows() {
			if (console.BoxRef{Server: s.Server, ID: s.ID}) == on {
				l.cursor = i
				break
			}
		}
	}
	l.clamp()
}

// markArchiving lists the discoboxes an archive is going out for as archived
// (console.List.MarkArchiving). The cursor stays on the box it was on; one on
// an archived box goes to the row that takes its place, as neighbor says.
func (l *sandboxList) markArchiving(targets []console.Sandbox) {
	l.keepingCursor(func() { l.MarkArchiving(targets) })
}

// unmarkArchiving drops the marks of archives that failed, putting the rows
// back in the state the latest listing gave them, and the cursor where it was.
func (l *sandboxList) unmarkArchiving(targets []console.Sandbox) {
	l.keepingCursor(func() { l.UnmarkArchiving(targets) })
}

// tree is the branch drawn in front of a row's name, from where the core
// nests it (console.Nest): empty for a row nested under nothing.
func (l *sandboxList) tree(s console.Sandbox) string {
	n := l.Nesting(s)
	if n.Depth == 0 {
		return ""
	}
	var b strings.Builder
	for _, rule := range n.Rules {
		if rule {
			b.WriteString(treeRule)
		} else {
			b.WriteString(treeGap)
		}
	}
	if n.Last {
		b.WriteString(treeLast)
	} else {
		b.WriteString(treeBranch)
	}
	return b.String()
}

// The branch in front of a nested row's name: one cell of line and one of
// air, so a level of nesting costs the name two cells.
const (
	treeBranch = "├ "
	treeLast   = "└ "
	treeRule   = "│ "
	treeGap    = "  "
)

func (l *sandboxList) current() *console.Sandbox {
	rows := l.Rows()
	if l.cursor < 0 || l.cursor >= len(rows) {
		return nil
	}
	return &rows[l.cursor]
}

// targets returns the sandboxes a command should act on: the visual range
// while one is being drawn, otherwise everything selected, otherwise the row
// under the cursor.
func (l *sandboxList) targets() []console.Sandbox {
	rows := l.Rows()
	if l.visual && len(rows) > 0 {
		lo, hi := l.visualRange()
		return append([]console.Sandbox(nil), rows[lo:hi+1]...)
	}
	var out []console.Sandbox
	for _, s := range rows {
		if l.selected[console.BoxRef{Server: s.Server, ID: s.ID}] {
			out = append(out, s)
		}
	}
	if len(out) > 0 {
		return out
	}
	if s := l.current(); s != nil {
		return []console.Sandbox{*s}
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
	rows := l.Rows()
	if len(rows) == 0 {
		l.visual = false
		return 0
	}
	for _, s := range rows[lo : hi+1] {
		l.selected[console.BoxRef{Server: s.Server, ID: s.ID}] = true
	}
	l.visual = false
	return hi - lo + 1
}

func (l *sandboxList) selectionCount() int {
	n := 0
	for _, s := range l.Rows() {
		if l.selected[console.BoxRef{Server: s.Server, ID: s.ID}] {
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
	if l.selected[console.BoxRef{Server: s.Server, ID: s.ID}] {
		delete(l.selected, console.BoxRef{Server: s.Server, ID: s.ID})
		return
	}
	l.selected[console.BoxRef{Server: s.Server, ID: s.ID}] = true
}

func (l *sandboxList) clearSelection() { l.selected = map[console.BoxRef]bool{} }

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
	n := len(l.Rows())
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

func (l *sandboxList) stateStyle(st *styles, s console.Sandbox) lipgloss.Style {
	switch s.State {
	case console.StateRunning:
		return st.stateRun
	case console.StateStarting, console.StateRepairing, console.StateSuspended:
		return st.stateBusy
	case console.StateError:
		return st.stateErr
	default:
		return st.stateOff
	}
}

// agentGlyph draws what a discobox's agent reports (ADR 26-10-16-424 §5), a
// blank cell for no report. Each is its own shape as well as its own color,
// so working and blocked are told apart where the colors are close.
func agentGlyph(st *styles, state programstatus.State) string {
	mark := agentMark(state)
	switch state {
	case programstatus.Working:
		return st.name.Render(mark)
	case programstatus.Idle:
		return st.dimText.Render(mark)
	case programstatus.Blocked, programstatus.Error:
		return st.statusER.Render(mark)
	case programstatus.Done:
		return st.add.Render(mark)
	default:
		return mark
	}
}

// agentMark is the shape agentGlyph draws, uncolored, for the places that
// take no color: the window title (statusTitlePrefix).
func agentMark(state programstatus.State) string {
	switch state {
	case programstatus.Working:
		return "▶"
	case programstatus.Idle:
		return "◇"
	case programstatus.Blocked:
		return "◆"
	case programstatus.Done:
		return "✓"
	case programstatus.Error:
		return "×"
	default:
		return " "
	}
}

func stateDot(s console.Sandbox) string {
	switch s.State {
	case console.StateRunning:
		return "●"
	case console.StateStarting, console.StateRepairing:
		return "◐"
	case console.StateSuspended:
		return "‖"
	case console.StateError:
		return "✗"
	case console.StateArchived:
		return "▪"
	default:
		return "○"
	}
}

func (l *sandboxList) view(st *styles, z *zones, focused bool) string {
	titleStyle := st.titleList
	if !focused {
		titleStyle = st.titleDim
	}
	rows := l.Rows()
	right := plural(len(rows), "box", "boxes")
	// A refresh out longer than the window waits before saying so takes the
	// count's place: the count is exactly what is being refreshed, and this is
	// the one place a window with one server has to say it. Nothing is drawn
	// here on an ordinary refresh, which is the point — an indicator that is
	// always on says nothing when it matters.
	if l.Slow {
		right = "still listing"
	}
	// Rows that have stopped keeping up say how far behind they are, which
	// outranks "still listing": it is the same news with the size of it.
	// With several servers on screen it is one of them that is behind, and
	// its own note says so instead (console.List.Behind).
	if l.OneServer() {
		if age := l.ListedAgo(l.Filter.Server); age != "" {
			right = age
		}
	}
	// The marks are worth a press of their own: they are the one thing on the
	// band that is a state you got into rather than a fact about the project,
	// and c is how you get out of it.
	marks := ""
	if n := l.selectionCount(); n > 0 {
		marks = plural(n, "selected", "selected")
		right = marks + "  ·  " + right
	}
	// The folder the list is filtered to is named in the header, which is where
	// it is chosen, so the title bar does not say it twice.
	scope, offer := "Discoboxes", ""
	if n := l.ArchivedCount(); n > 0 && !l.Filter.Archived {
		offer = plural(n, "archived", "archived") + ", A shows them"
		scope += "   " + offer
	}
	blank := strings.Repeat(" ", max(l.width, 0))

	// The machine goes above the band, not below the rows: it is the frame the
	// list is read inside — how much room there is — and a frame belongs before
	// the thing it frames.
	//
	// It and the column labels each cost a row, and both come out of the body
	// rather than out of the window: the list occupies the height it was given
	// whatever it chooses to put there.
	rowBudget := l.height
	var out []string
	if l.height > 1 && l.OnPrimary() {
		if machine := l.machine(st); machine != "" {
			out = append(out, machine)
			rowBudget--
		}
	}
	// A band that names a key is a button for it, wherever the keyboard
	// happens to be: pressing "A shows them" from the composer must show the
	// archived rather than type an A into the prompt. See zones.go.
	l.markBand(z, len(out), offer, marks, right)
	out = append(out, renderTitle(titleStyle, scope, right, l.width))

	// Only where a row survives being labeled. A window this short is one
	// where the list is being squeezed out entirely, and spending its last
	// line on the names of columns that have nothing under them would push the
	// window past the terminal.
	if l.height > 1 {
		if header := l.header(st, st.color); header != "" {
			out = append(out, header)
			rowBudget--
		}
	}

	// A server that did not answer, or has not answered yet, takes its line the
	// same way the machine line and the column header take theirs: out of the
	// budget, before the rows are drawn. Appended after them instead, it is
	// drawn only while the rows leave room — so exactly the list the launcher
	// is for, one longer than the window, would never say a server is missing,
	// and nothing could scroll to it, because offset and cursor walk rows.
	notes := l.Notes()
	noted := min(len(notes), max(rowBudget, 0))
	rowBudget -= noted
	bodyBudget := rowBudget + noted

	// The cursor has to be inside the window, and what the rows above it cost
	// is only known here: a section header is a line, and how many there are
	// between the offset and the cursor depends on where the sections fall.
	// clamp keeps the cursor on a row; this keeps the row on the screen.
	for (l.Grouped() || l.inTree) && l.cursor > l.offset && l.cursor < len(rows) && l.lineSpan(rows, l.offset, l.cursor) > rowBudget {
		l.offset++
	}

	body := make([]string, 0, max(bodyBudget, 0))
	// The invitation is for a list with nothing in it at all: rows, and the
	// sections that say where the missing ones went, are both something drawn.
	// It waits for the first listing rather than for a fast one — "no
	// discoboxes here yet" is an answer about the project, and until something
	// has landed nobody has one. After that it stays put whether or not a
	// refresh is out: an invitation that blinked out every time a poll ran
	// long would be the one screen a new user is reading flickering at them.
	if l.Loaded() && len(rows) == 0 && len(l.Missing()) == 0 && len(l.WaitingOn()) == 0 {
		body = append(body, st.dimText.Render(pad("  no discoboxes here yet — type a prompt below", l.width)))
	}
	l.drawn = drawn{top: len(out), first: l.offset}
	if l.Sectioned() || l.inTree {
		l.drawn.rows = make([]int, 0, max(bodyBudget, 0))
	}
	// How many each section has, for its band to say, counted once rather
	// than per header: a section scrolled into twice is one section.
	sectionCounts := map[string]int{}
	if l.Grouped() {
		for _, s := range rows {
			sectionCounts[l.Section(s)]++
		}
	}
	var drawnSection *string
	var rowLinks []rowLink
	for i := l.offset; i < len(rows) && len(body) < rowBudget; i++ {
		// A header in front of each section's rows, and in front of the first
		// row drawn whichever section it is in: a window scrolled into the
		// middle of one still says which folder is on screen.
		if key := l.Section(rows[i]); l.Grouped() && (drawnSection == nil || key != *drawnSection) {
			if len(body)+1 >= rowBudget {
				break
			}
			body = append(body, l.sectionHeader(st, l.SectionTitle(rows[i]), plural(sectionCounts[key], "box", "boxes")))
			l.drawn.rows = append(l.drawn.rows, -1)
			drawnSection = &key
		}
		line, links := l.row(st, z, rows[i], i, len(out)+len(body), focused)
		rowLinks = append(rowLinks, links...)
		body = append(body, line)
		if l.drawn.rows != nil {
			l.drawn.rows = append(l.drawn.rows, i)
		}
		l.drawn.count++
		// Its tags, when it has a line for them, are part of the row: a press
		// on them points at it like a press on its name.
		if l.inTree && len(body) < rowBudget {
			if line, links, ok := l.tagLine(st, z, rows[i], i, len(out)+len(body), focused); ok {
				rowLinks = append(rowLinks, links...)
				body = append(body, line)
				l.drawn.rows = append(l.drawn.rows, i)
			}
		}
	}
	// The notes about servers, after the rows (console.List.Notes): the ones
	// that did not answer, whose rows are missing from this listing rather
	// than gone; the ones not answered yet, whose rows are on their way; and
	// the ones whose rows are on screen but have stopped keeping up, which sit
	// among every other server's in their folders, so the note is what says
	// which of them are old.
	for _, note := range notes {
		if len(body) >= bodyBudget {
			break
		}
		body = append(body, l.sectionHeader(st, note.Title, note.Detail))
		l.drawn.rows = append(l.drawn.rows, -1)
	}
	for len(body) < bodyBudget {
		body = append(body, blank)
	}
	// One blank after the last row, so a list long enough to reach the composer
	// still has air between them.
	block := append(append(out, body...), blank)
	z.markList(hitRow, l.drawn, l.width, len(block))
	// The links on the rows go over the rows, so a press on one opens it
	// rather than pointing at its row.
	for _, link := range rowLinks {
		z.mark(urlHit(link.url), link.x, link.y, link.width, 1)
	}
	return lipgloss.JoinVertical(lipgloss.Left, block...)
}

// lineSpan is how many lines rows from through to take, headers and tag lines
// included. It
// is what tells the window whether the cursor still fits, and it counts the
// way the body draws: a header in front of the first row, and one wherever the
// section changes.
func (l *sandboxList) lineSpan(rows []console.Sandbox, from, to int) int {
	if from < 0 || to >= len(rows) || from > to {
		return 0
	}
	lines := to - from + 1
	if l.inTree {
		for _, s := range rows[from : to+1] {
			if len(s.Tags) > 0 {
				lines++
			}
		}
	}
	if !l.Grouped() {
		return lines
	}
	lines++
	for i := from + 1; i <= to; i++ {
		if l.Section(rows[i]) != l.Section(rows[i-1]) {
			lines++
		}
	}
	return lines
}

// sectionHeader introduces one section's rows: the band the list's own title
// is drawn as, in the dim of the two, so a section reads as a header of the
// same kind rather than as a row whose glyph went missing.
//
// It is a label rather than a row: nothing acts on it, the cursor never lands
// on it, and the mouse walks past it onto the list (zones.go).
func (l *sandboxList) sectionHeader(st *styles, title, right string) string {
	return renderTitle(st.titleDim, title, right, l.width)
}

// markBand makes the two offers on the title band pressable: the archived
// count, which names A, and the number of marks, which names c. The band is
// drawn by renderTitle, which pads a cell on each side and pins the right
// fragment to the far end, so both spans are measured off that.
func (l *sandboxList) markBand(z *zones, y int, offer, marks, right string) {
	if offer != "" {
		// The offer is the tail of the left fragment.
		z.mark(listKeyHit("A"), 1+lipgloss.Width("Discoboxes   "), y, lipgloss.Width(offer), 1)
	}
	if marks != "" {
		z.mark(listKeyHit("c"), l.width-1-lipgloss.Width(right), y, lipgloss.Width(marks), 1)
	}
}

// row draws one sandbox. Widths are budgeted left to right and the columns
// drop off the right end as the terminal narrows: the age goes
// first, then the origin, then the diffstat, and the name never goes at all.
// row draws one discobox on line y of the list's block, and hands back the
// links on it for the list to mark over the row.
func (l *sandboxList) row(st *styles, z *zones, s console.Sandbox, i, y int, focused bool) (string, []rowLink) {
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
	selected := l.selected[console.BoxRef{Server: s.Server, ID: s.ID}] || inVisual

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

	cols := &tailColumns{width: l.width}
	addCol := cols.add

	if !glyph {
		// As wide as the longest state, `repairing`.
		addCol("  "+pad(s.StateWord(), 9), 11)
	}

	up := " "
	if s.Upgrade {
		up = st.statusWA.Render("↑")
	}
	// A credential request waiting on this discobox. It sits beside the
	// upgrade arrow because it is the same kind of fact — something about this
	// row wants a person — and it is drawn in the error color rather than the
	// warning one: an upgrade can wait, an agent asking for a credential is
	// blocked until it is answered.
	req := " "
	if s.PendingRequests > 0 {
		req = st.statusER.Render("!")
	}
	// The tree's column has room for the marks that want a person and
	// nothing else: the harness and every column after it would only take
	// the name's width, and the name is what a row there is picked by. The
	// columns are first-fit, so leaving out the harness alone would hand its
	// cells to the next one that fits rather than to the name.
	if !l.inTree {
		addCol("  "+st.dimText.Render(pad(s.Harness, 7)), 9)
	}
	addCol(up, 2)
	addCol(req, 2)

	if !l.inTree {
		// Where the work sits in git — the reported position once the sandbox's
		// agent has spoken, the spawn commit until then. Where it came from is not
		// a column: it is the header's filter, and every row on screen has
		// already been filtered to it — a column repeating the same value on every
		// row is a column spent saying nothing.
		//
		baseStyle := gitStyle(st, s)
		addCol(baseStyle.Render(pad(s.Base(), 14)), 15)
		// The mark spelled out, in the mark's own color. It is added right after
		// the position so the two survive a narrowing terminal together.
		addCol(baseStyle.Render(pad(s.Changes(), 7)), 8)
		addCol(st.dimText.Render(pad(createdText(s, l.Now()), 8)), 9)
		addCol(usage(st, s), usageWidth+1)
		addCol(padANSI(diffText(st, s), 11), 11)
	}
	tail := cols.text

	head := l.rowHead(st, s, atCursor)
	// The branch a discobox created by another hangs from, after the state so
	// the glyphs stay one column the eye runs down. It is part of the head: it
	// comes out of the name's width, and the name keeps its floor.
	if tree := l.tree(s); tree != "" {
		head += st.dimText.Render(tree)
	}
	nameW := max(l.width-lipgloss.Width(head)-lipgloss.Width(tail), 4)
	name := s.Name
	// Under the cursor the name gives way to what the state is about
	// (Sandbox.Remark).
	if remark := s.Remark(); remark.Text != "" && atCursor {
		name = remark.Text
		// A remark a command answers offers the command by its key here.
		if c, ok := console.Lookup(remark.Command); ok && len(c.Bindings.TUI.Keys) > 0 {
			name += ": " + c.Bindings.TUI.Keys[0] + " to " + remark.Verb
		}
	}

	// A discobox created on another machine says so after its name, dim,
	// because it is a qualifier on the name rather than a fact of its own —
	// what it answers is "why is this here", which is a question about the
	// name it is sitting beside. It shares the name's column instead of taking
	// one: no other row has anything to put in such a column.
	//
	// It comes out of the name's own width, and goes rather than squeezing the
	// name below what a name is readable in. The name never goes.
	//
	// Which rows say it is the core's (console.List.Origin): a row nested
	// under its creator does not.
	from := ""
	if text := l.Origin(s); text != "" {
		// The floor is the one the tail columns hold to, less the head, which
		// is the part of that reserve the name does not get either way.
		if room := nameW - lipgloss.Width(text) - 1; room >= nameReserve-lipgloss.Width(head) {
			from = st.dimText.Render(" " + text)
		}
	}
	nameW -= lipgloss.Width(from)

	// Its tags follow, dim, for the same reason and on the same terms as the
	// qualifier above: they say which box this is, so they sit by the name,
	// and they give way before the name does — as many as fit, and none when
	// even one would squeeze the name (ADR 0136). A status tag comes first,
	// so it is the last to give way: the tag is the only place the row says
	// it. The pull request and the issue follow it, ahead of the rest.
	var shown []string
	taggedW := 0
	if len(s.Tags) > 0 && !l.inTree {
		room := nameW - (nameReserve - lipgloss.Width(head))
		for _, tag := range s.RowTags() {
			next := taggedW + lipgloss.Width(" "+tagLabel(tag))
			if next > room {
				break
			}
			shown, taggedW = append(shown, tag), next
		}
	}
	nameW -= taggedW

	if atCursor {
		// The cursor row is the one that can be scrolled, so it is the one
		// whose measurements are worth keeping for the next key press. They
		// are the name's, not the cell's: the qualifier is not scrolled past
		// to read the rest of the name.
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

	named := nameStyle.Render(truncate(name, nameW))
	tagged, links := l.tagSpans(st, z, s, shown, lipgloss.Width(head)+lipgloss.Width(named)+lipgloss.Width(from), y)
	cell := padANSI(named+from+tagged, nameW+lipgloss.Width(from)+taggedW)
	return l.shade(st, padANSI(head+cell+tail, l.width), atCursor, selected), links
}

// rowHead is what a row draws in front of its tree branch and its name: the
// cursor's chevron, and in color the state dot and the agent's glyph. It is
// the one place that says how wide that is, so the tag line under a row
// (tagLine) starts where the row's name does.
func (l *sandboxList) rowHead(st *styles, s console.Sandbox, atCursor bool) string {
	// The cursor is a chevron, and selection is a background —
	// discobox-review's file list, which is a picker like this one rather than
	// a diff.
	head := "  "
	if atCursor {
		head = st.key.Render("❯") + " "
	}
	if st.color {
		// What the agent reports comes right after the box's own state, so
		// the two read as one column pair; a box whose agent reports nothing
		// leaves the cell blank rather than closing it up, so the names stay
		// in line.
		head += l.stateStyle(st, s).Render(stateDot(s)) + " " + agentGlyph(st, s.Agent.State) + " "
	}
	if s.Warned() {
		head += st.statusWA.Render("⚠") + " "
	}
	return head
}

// shade is a row's line with the background its cursor and its marks give it.
func (l *sandboxList) shade(st *styles, line string, atCursor, selected bool) string {
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

// tagLine is the line under a tagged row in the workspace tree (inTree): its
// tags, drawn as the row draws them beside the name (tagSpans) — the pull
// request status bold in its color and first, an issue or pull request a link,
// the rest dim — from under the name, with the tree's rule carried down past
// them. As many as fit, and how many more there are when that is not all of
// them. It reports false for a row with no tags, which has no such line.
func (l *sandboxList) tagLine(st *styles, z *zones, s console.Sandbox, i, y int, focused bool) (string, []rowLink, bool) {
	if len(s.Tags) == 0 {
		return "", nil, false
	}
	atCursor := i == l.cursor && focused
	selected := l.selected[console.BoxRef{Server: s.Server, ID: s.ID}]
	if l.visual {
		lo, hi := l.visualRange()
		selected = selected || (i >= lo && i <= hi)
	}

	// Under the marker and the glyphs, and under the name's branch the rule
	// it continues on, so a tree reads unbroken through its rows' tags.
	indent := strings.Repeat(" ", lipgloss.Width(l.rowHead(st, s, false)))
	cont := ""
	if lead, ok := strings.CutSuffix(l.tree(s), treeBranch); ok {
		cont = lead + treeRule
	} else if lead, ok := strings.CutSuffix(l.tree(s), treeLast); ok {
		cont = lead + treeGap
	}
	head := indent + st.dimText.Render(cont)

	// The space in front of the first tag is the gap to the name's column,
	// and a cell is kept clear of the right edge, as the row's columns keep
	// one.
	room := l.width - lipgloss.Width(head)
	tags := s.RowTags()
	var shown []string
	used := 0
	for n, tag := range tags {
		w := lipgloss.Width(" " + tagLabel(tag))
		// The last that fits leaves room to say how many did not, unless it
		// is the last of them.
		more := 0
		if n < len(tags)-1 {
			more = lipgloss.Width(fmt.Sprintf(" +%d", len(tags)-n-1))
		}
		if used+w+more > room {
			break
		}
		shown, used = append(shown, tag), used+w
	}
	tagged, links := l.tagSpans(st, z, s, shown, lipgloss.Width(head)-1, y)
	if rest := len(tags) - len(shown); rest > 0 {
		tagged += st.dimText.Render(fmt.Sprintf(" +%d", rest))
	}
	line := padANSI(ansi.Truncate(head, max(lipgloss.Width(head)-1, 0), "")+tagged, l.width)
	return l.shade(st, line, atCursor, selected), links, true
}

// rowLink is a link drawn on a row: the cells it takes in the list's block —
// the line the row was drawn on, and the columns from its left edge, the same
// numbers it was lit by — and where it points. The row cannot mark it itself — the list marks
// its rows once it knows which lines they landed on, and a link marked before
// its row would be underneath it — so it hands it back to be marked over the
// row (sandboxList.view).
type rowLink struct {
	x, y, width int
	url         string
}

// tagSpans draws the tags a row has room for, dim, from cell x of line y. A
// status tag (statusTags) is drawn bold, green for ready and red for the two
// waiting on a person, instead. A tag that numbers an issue or a pull request in the discobox's
// GitHub repository (Sandbox.tagURL) is a link to it instead, in the color the
// header's links are drawn in, and lit when the pointer is on it — the same
// terms the header's links are drawn on, since a click on one opens it.
func (l *sandboxList) tagSpans(st *styles, z *zones, s console.Sandbox, tags []string, x, y int) (string, []rowLink) {
	var out strings.Builder
	var links []rowLink
	for _, tag := range tags {
		label := tagLabel(tag)
		out.WriteString(st.dimText.Render(" "))
		x++
		width := lipgloss.Width(label)
		switch url := s.TagURL(tag); {
		case console.TagKey(tag) == console.StatusBlocked || console.TagKey(tag) == console.StatusNeedsInput:
			out.WriteString(st.blockedText.Render(label))
		case console.TagKey(tag) == console.StatusReady:
			out.WriteString(st.readyText.Render(label))
		case url == "":
			out.WriteString(st.dimText.Render(label))
		case z.hovering(x, y, width, 1):
			out.WriteString(hyperlink(url, st.hover.Render(label)))
			links = append(links, rowLink{x: x, y: y, width: width, url: url})
		default:
			out.WriteString(hyperlink(url, st.info.Render(label)))
			links = append(links, rowLink{x: x, y: y, width: width, url: url})
		}
		x += width
	}
	return out.String(), links
}

// gitStyle is the color the git position and its spelled-out mark are drawn
// in: the state of the work. Warning for uncommitted content that only the
// sandbox holds, green for a head commit an apply has landed, and the default
// text for committed work no apply has landed — the state to notice before
// archiving, so it stands against rows that are dim because nothing on them is
// at stake.
//
// It is one function rather than a switch per drawing site so the list row and
// the workspace header cannot drift apart on what a color means.
func gitStyle(st *styles, s console.Sandbox) lipgloss.Style {
	switch {
	case s.Uncommitted():
		return st.statusWA
	case s.Ahead():
		return st.name
	default:
		return st.dimText
	}
}

// diffText is the diffstat as both the row and the workspace header draw it:
// what the sandbox has changed, in the diff's own two colors. Empty when
// nothing has been reported or nothing has changed, which is the same answer
// as far as anything drawing it is concerned.
func diffText(st *styles, s console.Sandbox) string {
	if !s.HasDiff() {
		return ""
	}
	return st.add.Render("+"+itoa(s.Diff.Added)) + " " + st.del.Render("−"+itoa(s.Diff.Deleted))
}

// portsField is what the sandbox is serving, grouped by protocol:
//
//	http:3000,5173,8080 · https:8443 · tcp:22,5432,6379 · udp:53,5353
//
// Grouped rather than one `protocol/port` per listening port, because the
// protocol is the repetitive half: a sandbox running three dev servers said
// "http" three times for no information. Naming it once per group is what keeps
// this readable on a header row when a compose stack is up.
//
// The protocol leads its group because it is what decides whether a port is
// worth opening at all — `http:5173` is a page, `tcp:5432` is a database — and
// the groups run in that order of usefulness, web first. A protocol this CLI
// does not know, from a newer agent, keeps its own name and follows them: it is
// not this end's business to rename or drop what it was told.
//
// `unknown` is drawn as `?`. It is the longest word for the least information,
// and a port whose probe has not answered yet is exactly the case where the
// number is all there is to say.
//
// The separator is the hints line's own `·`, so a group reads as "and" rather
// than as a new banner field, which is two spaces.
//
// forwarded is the workspace's port forward: the local port standing in for
// each sandbox port, which turns that port's entry into `8082->8080` and, when
// it speaks a web protocol, into a link to it. It is empty everywhere else —
// the list has no forward, and a row that claimed one would be offering a port
// nothing is listening on.
//
// The sandbox's own bind address is still not shown. A forward dials from
// inside the sandbox, where a loopback-only listener answers exactly as a
// wildcard one does, so the address would be a column that never changes what
// you can do; the local port, which is the one you can type, is the half worth
// the space.
//
// The desktop is not among them: it has its own header field (desktopField), so
// a sandbox serving nothing else renders no port group at all.
//
// Empty when nothing is listening, which is also what a sandbox whose agent has
// not reported yet looks like — there is no third thing to say and no room to
// say it in.
func portsField(st *styles, s console.Sandbox, forwarded map[console.PortKey]int) paneHeaderField {
	if len(s.Ports) == 0 {
		return paneHeaderField{}
	}
	groups := map[string][]console.Port{}
	var order []string
	for _, port := range s.Ports {
		// The desktop is drawn as its own field, not as a number here. It is
		// not a port somebody's program is serving and there is nothing useful
		// to do with the number: "open the desktop" is the whole offer, and
		// listing `http:6900` beside a dev server invites opening it as if it
		// were one.
		if port.ServiceID == sandboxservices.DesktopID {
			continue
		}
		if _, seen := groups[port.Protocol]; !seen {
			order = append(order, port.Protocol)
		}
		groups[port.Protocol] = append(groups[port.Protocol], port)
	}
	if len(order) == 0 {
		return paneHeaderField{}
	}
	sort.SliceStable(order, func(i, j int) bool {
		return protocolRank(order[i]) < protocolRank(order[j])
	})

	// Drawn span by span rather than as one string, because the numbers in it
	// are separately pressable: every forwarded web port is a link of its own,
	// and the header lights and opens the one under the pointer. The rest of
	// the row — the protocol labels, the commas, the separators — is text.
	field := paneHeaderField{}
	for _, protocol := range order {
		if len(field.spans) > 0 {
			field.spans = append(field.spans, linkSpan(st, " · ", ""))
		}
		ports := groups[protocol]
		sort.Slice(ports, func(i, j int) bool { return ports[i].Number < ports[j].Number })
		field.spans = append(field.spans, linkSpan(st, protocolLabel(protocol)+":", ""))
		for i, port := range ports {
			if i > 0 {
				field.spans = append(field.spans, linkSpan(st, ",", ""))
			}
			field.spans = append(field.spans, portEntry(st, port, forwarded))
		}
	}
	return field
}

// linkSpan is one piece of a header field drawn in the links' color — the
// ports, the desktop, the issue and pull request — linked when there is
// somewhere for it to point.
func linkSpan(st *styles, text, url string) headerSpan {
	if url == "" {
		return headerSpan{text: st.info.Render(text), label: text}
	}
	return headerSpan{text: st.info.Render(hyperlink(url, text)), label: text, url: url}
}

// workField is the issue and the pull request the discobox's `issue=` and
// `pr=` tags number, each a link to it on GitHub (Sandbox.tagURL): the work
// this discobox is doing, one press from what is being said about it. Empty
// when it has neither, or no GitHub repository for them to be numbered in.
//
// Each opens as a workspace tab on a plain click (the issue tab) and on the
// page on a Ctrl-click.
func workField(st *styles, s console.Sandbox) paneHeaderField {
	var field paneHeaderField
	for _, tag := range s.Tags {
		url := s.TagURL(tag)
		if url == "" {
			continue
		}
		key, number, _ := strings.Cut(tag, "=")
		label := "issue #" + number
		if key == "pr" {
			label = "PR #" + number
		}
		if !field.empty() {
			field.spans = append(field.spans, linkSpan(st, " · ", ""))
		}
		span := linkSpan(st, label, url)
		span.issue, _ = strconv.Atoi(number)
		span.pull = key == "pr"
		field.spans = append(field.spans, span)
	}
	return field
}

// tagsField is the discobox's tags the header says nothing of otherwise, dim
// and in the order they are kept. The ones it says already are left out: an
// issue or pull request workField links, and a status tag, which the
// header says through the git word (paneHeaderFields), on terms of its own
// that a tag drawn beside it would contradict. Empty when that leaves none.
func tagsField(st *styles, s console.Sandbox) paneHeaderField {
	var labels []string
	for _, tag := range s.Tags {
		if s.TagURL(tag) == "" && !console.IsStatusTag(tag) {
			labels = append(labels, tagLabel(tag))
		}
	}
	if len(labels) == 0 {
		return paneHeaderField{}
	}
	return textField(st.dimText.Render(strings.Join(labels, " ")))
}

// desktopField is the sandbox's graphical desktop, as a link to the local end of
// its forward.
//
// Its own field rather than a number in the protocol groups, because it is a
// different kind of thing: a desktop to open, not a port to connect something
// to. The client recognizes it by the id the image declared it under
// (sandboxservices.DesktopID) rather than by a port number, so nothing here
// hardcodes 6900 — see ADR 26-09-05-409 on image services.
//
// Drawn only when the forward has bound it, for the reason portEntry links only
// forwarded ports: an offer to open a desktop that cannot be reached is worse
// than not offering. The label is the declaration's own name, so the sandbox
// says what to call it.
func desktopField(st *styles, s console.Sandbox, forwarded map[console.PortKey]int) paneHeaderField {
	for _, port := range s.Ports {
		if port.ServiceID != sandboxservices.DesktopID {
			continue
		}
		local, ok := forwarded[port.Key()]
		if !ok {
			return paneHeaderField{}
		}
		scheme, web := portScheme(port.Protocol)
		if !web {
			return paneHeaderField{}
		}
		label := port.ServiceName
		if label == "" {
			label = "Desktop"
		}
		return paneHeaderField{spans: []headerSpan{linkSpan(st, label, scheme+"://localhost:"+itoa(local))}}
	}
	return paneHeaderField{}
}

// portEntry is one port in its group: the number on its own, or `local->remote`
// when the workspace's forward has had to move the port.
//
// The arrow is there to answer "which number do I type here", so it is drawn
// only when the answer differs from the port the sandbox is serving. A forward
// that got the port it asked for has nothing to correct, and `1234->1234` costs
// header width to say the same number twice.
//
// A web port is also a link to the local end of it, so the port a sandbox is
// serving is one click away rather than a URL to assemble by hand — an OSC 8
// link for the terminal's Ctrl-click, and a marked span the header opens
// itself when it is clicked plainly (see paneHeaderFields.render). Only the
// forwarded ones: a link to a port nothing is listening on is worse than no
// link. Only the web ones: OSC 8 hands the URL to whatever opens
// `http://`, and there is nothing sensible for a browser to do with a Postgres
// socket.
func portEntry(st *styles, port console.Port, forwarded map[console.PortKey]int) headerSpan {
	local, ok := forwarded[port.Key()]
	if !ok {
		return linkSpan(st, itoa(port.Number), "")
	}
	text := itoa(local)
	if local != port.Number {
		text += "->" + itoa(port.Number)
	}
	scheme, web := portScheme(port.Protocol)
	if !web {
		return linkSpan(st, text, "")
	}
	return linkSpan(st, text, scheme+"://localhost:"+itoa(local))
}

// portScheme is the URL scheme a protocol is reachable under, and whether it is
// one at all. The forward is a byte pipe, so a port speaking https is https at
// the local end too — with the certificate's name not matching, which is
// inherent to forwarding it and not something a scheme choice here can fix.
func portScheme(protocol string) (string, bool) {
	switch protocol {
	case "http", "https":
		return protocol, true
	default:
		return "", false
	}
}

// protocolOrder is the order the groups are drawn in: what you would act on
// first. Anything not named here follows, in port order.
var protocolOrder = []string{"http", "https", "tcp", "udp", "unknown"}

func protocolRank(protocol string) int {
	for rank, known := range protocolOrder {
		if protocol == known {
			return rank
		}
	}
	return len(protocolOrder)
}

func protocolLabel(protocol string) string {
	if protocol == "" || protocol == "unknown" {
		return "?"
	}
	return protocol
}

// tailColumns budgets the fixed-width columns to the right of the name, left to
// right, dropping the ones that no longer leave the name room to be read.
//
// It is a type rather than a closure inside row so that the header labeling
// those columns is budgeted by exactly the same arithmetic. A header computed
// separately drifts out of line with its rows the moment a column drops on a
// narrowing window, and a label over the wrong column is worse than none.
type tailColumns struct {
	width int
	text  string
}

// nameReserve is what the head and the name keep between them: a tail column
// that would leave less than this is dropped instead of drawn. It is one
// number rather than one per caller because it is one rule — anything that
// wants cells the name is using holds to it, the qualifier on a discobox from
// another machine included (see row). Two numbers for "how narrow a name may
// get" drift the moment either is tuned.
const nameReserve = 20

func (t *tailColumns) add(text string, w int) {
	if t.width-lipgloss.Width(t.text)-w < nameReserve {
		return
	}
	t.text += padANSI(text, w)
}

// header labels the columns whose numbers do not say what they are. The name,
// the state and the git position are read without help; "1.2 GiB" beside "94%"
// is not, and the units alone do not say which is memory and which is disk.
//
// Only the usage columns are labeled. A header over every column would spell
// out what the rows already make plain, and cost a line to do it.
func (l *sandboxList) header(st *styles, glyph bool) string {
	cols := &tailColumns{width: l.width}
	if !glyph {
		cols.add("", 10)
	}
	cols.add("", 9)
	cols.add("", 2)
	cols.add("", 15)
	cols.add("", 8)
	cols.add("", 8)
	cols.add(usageHeader(st), usageWidth+1)
	cols.add("", 11)
	if strings.TrimSpace(ansi.Strip(cols.text)) == "" {
		// Every labeled column dropped off this window, so there is nothing
		// to label and the line is not worth spending.
		return ""
	}
	return padANSI(strings.Repeat(" ", l.headOffset(glyph))+padANSI("", l.nameSpace(glyph))+cols.text, l.width)
}

// headOffset and nameSpace are what row puts in front of the tail: the cursor
// marker and, with color, the state dot and the agent's glyph, then the name
// filling the slack.
func (l *sandboxList) headOffset(glyph bool) int {
	if glyph {
		return 6
	}
	return 2
}

func (l *sandboxList) nameSpace(glyph bool) int {
	cols := &tailColumns{width: l.width}
	if !glyph {
		cols.add("", 10)
	}
	cols.add("", 9)
	cols.add("", 2)
	cols.add("", 15)
	cols.add("", 8)
	cols.add("", 8)
	cols.add("", usageWidth+1)
	cols.add("", 11)
	return max(l.width-l.headOffset(glyph)-lipgloss.Width(cols.text), 4)
}

// machine is what this machine has and how much of it Discobox is using.
//
// A row of its own above the band, rather than squeezed onto it beside the
// count: the band was carrying two unrelated facts and reading as neither, and
// the count belongs to the list, which is filtered to a folder, where the
// machine does not.
//
// It is left-aligned and says what each figure is, because it cannot line up
// under the columns it belongs to — the figures are used-of-capacity pairs and
// the cells below them hold one number each.
func (l *sandboxList) machine(st *styles) string {
	machine := machineText(st, l.resources, max(l.width-12, 0))
	if machine == "" {
		return ""
	}
	return padANSI("  "+st.dimText.Render("machine")+"  "+machine, l.width)
}
