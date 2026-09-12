package tui

import (
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/discobox-ai/discobox/sandboxservices"
)

// sandboxList is the upper pane: every sandbox in the project, newest-created
// first.
//
// A row is one line: a glyph for the state, the name, the harness, where it
// came from, how old it is and what it has changed. The fixed width
// columns drop off the right end as the terminal narrows — the name and the
// glyph are what the eye actually picks a sandbox by, and neither ever goes.
type sandboxList struct {
	session Session
	// resources is what the machine has and what Discobox is using of it,
	// drawn on the title band. It is the machine's, not this list's: the list
	// is filtered to a folder and the machine is not.
	resources Resources
	all       []Sandbox
	cursor    int
	offset    int
	selected  map[string]bool

	// folder is the folder the list is filtered to, chosen in the header. With
	// no key it is every folder, which is the one choice that is not a place.
	folder folder

	// Archived sandboxes are history: kept, listed on request, and out of the
	// way until then.
	showArchived bool

	// unreachable are the servers the last listing could not reach, which get
	// a section of their own with no rows: a server's discoboxes going missing
	// says why rather than looking like discoboxes that are gone.
	unreachable []string

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

	// now is when the frame is being drawn, so the age column is a pure
	// function of the model and a test can render a fixed one.
	now func() time.Time

	// drawn is which rows the last render actually put on screen, recorded by
	// the loop that drew them so the mouse hits exactly what is there. See
	// zones.go.
	drawn drawn

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
// setPending stamps each row with how many credential requests are waiting on
// it. The count lives on the row rather than being looked up while drawing, so
// the draw stays a pure function of what the list holds.
func (l *sandboxList) setPending(byBox map[string][]CredentialRequest) {
	for i := range l.all {
		l.all[i].PendingRequests = len(byBox[l.all[i].ID])
	}
}

func (l *sandboxList) rows() []Sandbox {
	out := make([]Sandbox, 0, len(l.all))
	for _, s := range l.all {
		if !l.folder.holds(s, l.session) {
			continue
		}
		if !l.showArchived && s.State == StateArchived {
			continue
		}
		out = append(out, s)
	}
	// Grouped by server, and newest-first inside a section as everywhere else:
	// the sort is stable, so the order the listing arrived in is what orders a
	// section (ADR 0114 §4).
	if l.grouped() {
		sort.SliceStable(out, func(i, j int) bool {
			return l.section(out[i].Server) < l.section(out[j].Server)
		})
	}
	return out
}

// setUnreachable takes the servers the last listing could not reach.
func (l *sandboxList) setUnreachable(servers []string) {
	l.unreachable = append(l.unreachable[:0], servers...)
}

// grouped reports whether the list is drawn as one section per server, which
// it is once there is more than one server to tell apart. With one, naming it
// on every row — or over them — says nothing.
func (l *sandboxList) grouped() bool {
	return len(l.session.Servers) > 1
}

// sectioned reports whether the body carries any line that is not a row: the
// server headers when it is grouped, and the servers that did not answer
// whenever there are any. The listing and the session arrive on their own
// schedules, so a server can be missing from a list the window does not yet
// know is grouped, and it is still missing.
func (l *sandboxList) sectioned() bool {
	return l.grouped() || len(l.unreachable) > 0
}

// section is where a server's rows go: the order the session lists its
// servers, the primary first. A row whose server is not among them sorts last,
// which is what a listing that arrived before the session does.
func (l *sandboxList) section(server string) int {
	for i, name := range l.session.Servers {
		if name == server {
			return i
		}
	}
	return len(l.session.Servers)
}

// folders are what the header can filter to (ADR 0111): the window's own
// folder first, whether or not anything was cut from it yet, then every other
// folder the project's discoboxes are filed in, newest first, then the one the
// header is on if nothing is filed in it yet. The order does not follow the
// selection, so left and right walk the same ring whichever folder they start
// from. This machine's own key is never one of them: its discoboxes with no
// source are in every folder of this machine's already.
//
// It is derived from the listing rather than asked for separately: the folders
// worth offering are exactly the ones something is sitting in.
func (l *sandboxList) folders() []folder {
	seen := map[string]bool{"": true, l.session.HostKey: true}
	out := []folder{}
	add := func(f folder) {
		if seen[f.key] {
			return
		}
		seen[f.key] = true
		out = append(out, f)
	}
	add(l.session.folder())
	for _, s := range l.all {
		add(s.folder(l.session))
	}
	add(l.folder)
	return out
}

// sources are what the run options can cut a new discobox from: every source
// the project's discoboxes were cut from, newest sandbox first.
//
// It is the folder list's counterpart and not the same list. A folder is where
// a discobox is filed — a machine and a source — so one source cut on two
// machines is two folders; a source is what can be cut from here, which is a
// repository URL as often as a path. Each carries where a discobox cut from it
// on this machine is filed. A discobox created with no source at all
// contributes nothing, because "no source" is an answer the row already offers
// on its own.
func (l *sandboxList) sources() []Source {
	seen := map[string]bool{}
	out := []Source{}
	for _, s := range l.all {
		if s.Source == "" || seen[s.Source] {
			continue
		}
		seen[s.Source] = true
		out = append(out, Source{Value: s.Source, Remote: s.SourceRemote, OriginKey: s.SourceOriginKey})
	}
	return out
}

// archivedCount is what the title bar offers when they are hidden: a number
// worth pressing a key for, or nothing to say.
func (l *sandboxList) archivedCount() int {
	n := 0
	for _, s := range l.all {
		if s.State == StateArchived && l.folder.holds(s, l.session) {
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

func (l *sandboxList) view(st *styles, z *zones, focused bool) string {
	titleStyle := st.titleList
	if !focused {
		titleStyle = st.titleDim
	}
	rows := l.rows()
	right := plural(len(rows), "box", "boxes")
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
	if n := l.archivedCount(); n > 0 && !l.showArchived {
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
	if l.height > 1 {
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

	// A server that did not answer takes its line the same way the machine line
	// and the column header take theirs: out of the budget, before the rows are
	// drawn. Appended after them instead, it is drawn only while the rows leave
	// room — so exactly the list the launcher is for, one longer than the
	// window, would never say a server is missing, and nothing could scroll to
	// it, because offset and cursor walk rows.
	missing := min(len(l.unreachable), max(rowBudget, 0))
	rowBudget -= missing
	bodyBudget := rowBudget + missing

	// The cursor has to be inside the window, and what the rows above it cost
	// is only known here: a section header is a line, and how many there are
	// between the offset and the cursor depends on where the sections fall.
	// clamp keeps the cursor on a row; this keeps the row on the screen.
	for l.grouped() && l.cursor > l.offset && l.cursor < len(rows) && l.lineSpan(rows, l.offset, l.cursor) > rowBudget {
		l.offset++
	}

	body := make([]string, 0, max(bodyBudget, 0))
	// The invitation is for a list with nothing in it at all: rows, and the
	// sections that say where the missing ones went, are both something drawn.
	if len(rows) == 0 && len(l.unreachable) == 0 {
		body = append(body, st.dimText.Render(pad("  no discoboxes here yet — type a prompt below", l.width)))
	}
	l.drawn = drawn{top: len(out), first: l.offset}
	if l.sectioned() {
		l.drawn.rows = make([]int, 0, max(bodyBudget, 0))
	}
	// How many each server has, for its band to say, counted once rather than
	// per header: a section scrolled into twice is one section.
	sectionCounts := map[string]int{}
	if l.grouped() {
		for _, s := range rows {
			sectionCounts[s.Server]++
		}
	}
	drawnSection := ""
	for i := l.offset; i < len(rows) && len(body) < rowBudget; i++ {
		// A header in front of each server's rows, and in front of the first
		// row drawn whichever section it is in: a window scrolled into the
		// middle of one still says which server is on screen.
		if l.grouped() && rows[i].Server != drawnSection {
			if len(body)+1 >= rowBudget {
				break
			}
			body = append(body, l.sectionHeader(st, rows[i].Server, "", sectionCounts[rows[i].Server]))
			l.drawn.rows = append(l.drawn.rows, -1)
			drawnSection = rows[i].Server
		}
		body = append(body, l.row(st, rows[i], i, focused))
		if l.drawn.rows != nil {
			l.drawn.rows = append(l.drawn.rows, i)
		}
		l.drawn.count++
	}
	// The servers that did not answer, after the ones that did: their rows are
	// missing from this listing rather than gone, and a section saying so is
	// where a reader looks for them.
	for _, name := range l.unreachable {
		if len(body) >= bodyBudget {
			break
		}
		body = append(body, l.sectionHeader(st, name, "not answering", 0))
		l.drawn.rows = append(l.drawn.rows, -1)
	}
	for len(body) < bodyBudget {
		body = append(body, blank)
	}
	// One blank after the last row, so a list long enough to reach the composer
	// still has air between them.
	block := append(append(out, body...), blank)
	z.markList(hitRow, l.drawn, l.width, len(block))
	return lipgloss.JoinVertical(lipgloss.Left, block...)
}

// lineSpan is how many lines rows from through to take, headers included. It
// is what tells the window whether the cursor still fits, and it counts the
// way the body draws: a header in front of the first row, and one wherever the
// server changes.
func (l *sandboxList) lineSpan(rows []Sandbox, from, to int) int {
	if from < 0 || to >= len(rows) || from > to {
		return 0
	}
	lines := to - from + 1
	if !l.grouped() {
		return lines
	}
	lines++
	for i := from + 1; i <= to; i++ {
		if rows[i].Server != rows[i-1].Server {
			lines++
		}
	}
	return lines
}

// sectionHeader introduces one server's rows: the band the list's own title is
// drawn as, in the dim of the two, so a section reads as a header of the same
// kind rather than as a row whose glyph went missing. It says "server" because
// a bare name over a list of discoboxes is one more name among them.
//
// It is a label rather than a row: nothing acts on it, the cursor never lands
// on it, and the mouse walks past it onto the list (zones.go).
func (l *sandboxList) sectionHeader(st *styles, server, note string, count int) string {
	name := strings.TrimSpace(server)
	if name == "" {
		name = "this server"
	}
	right := note
	if right == "" {
		right = plural(count, "box", "boxes")
	}
	return renderTitle(st.titleDim, "server "+name, right, l.width)
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

	cols := &tailColumns{width: l.width}
	addCol := cols.add

	if !glyph {
		addCol("  "+pad(string(s.State), 8), 10)
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
	addCol("  "+st.dimText.Render(pad(s.Harness, 7)), 9)
	addCol(up, 2)
	addCol(req, 2)

	// Where the work sits in git — the reported position once the sandbox's
	// agent has spoken, the spawn commit until then. Where it came from is not
	// a column: it is the header's dropdown, and every row on screen has
	// already been filtered to it — a column repeating the same value on every
	// row is a column spent saying nothing.
	//
	baseStyle := gitStyle(st, s)
	addCol(baseStyle.Render(pad(s.base(), 14)), 15)
	// The mark spelled out, in the mark's own color. It is added right after
	// the position so the two survive a narrowing terminal together.
	addCol(baseStyle.Render(pad(s.changes(), 7)), 8)
	addCol(st.dimText.Render(pad(createdText(s, l.now()), 7)), 8)
	addCol(usage(st, s), usageWidth+1)
	addCol(padANSI(diffText(st, s), 11), 11)
	tail := cols.text

	// The cursor is a chevron, and selection is a background —
	// discobox-review's file list, which is a picker like this one rather than
	// a diff.
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

	// A discobox created on another machine says so after its name, dim,
	// because it is a qualifier on the name rather than a fact of its own —
	// what it answers is "why is this here", which is a question about the
	// name it is sitting beside. It shares the name's column instead of taking
	// one: no other row has anything to put in such a column.
	//
	// It comes out of the name's own width, and goes rather than squeezing the
	// name below what a name is readable in. The name never goes.
	from := ""
	if text := s.elsewhere(l.session); text != "" {
		// The floor is the one the tail columns hold to, less the head, which
		// is the part of that reserve the name does not get either way.
		if room := nameW - lipgloss.Width(text) - 1; room >= nameReserve-lipgloss.Width(head) {
			from = st.dimText.Render(" " + text)
		}
	}
	nameW -= lipgloss.Width(from)

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

	cell := padANSI(nameStyle.Render(truncate(name, nameW))+from, nameW+lipgloss.Width(from))
	line := padANSI(head+cell+tail, l.width)
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

// gitStyle is the color the git position and its spelled-out mark are drawn
// in: the state of the work. Warning for uncommitted content that only the
// sandbox holds, green for a head commit an apply has landed, and the default
// text for committed work no apply has landed — the state to notice before
// archiving, so it stands against rows that are dim because nothing on them is
// at stake.
//
// It is one function rather than a switch per drawing site so the list row and
// the workspace header cannot drift apart on what a color means.
func gitStyle(st *styles, s Sandbox) lipgloss.Style {
	switch {
	case s.dirty():
		return st.statusWA
	case s.Git.Applied:
		return st.add
	case s.ahead():
		return st.name
	default:
		return st.dimText
	}
}

// diffText is the diffstat as both the row and the workspace header draw it:
// what the sandbox has changed, in the diff's own two colors. Empty when
// nothing has been reported or nothing has changed, which is the same answer
// as far as anything drawing it is concerned.
func diffText(st *styles, s Sandbox) string {
	if !s.hasDiff() {
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
func portsField(st *styles, s Sandbox, forwarded map[portKey]int) paneHeaderField {
	if len(s.Ports) == 0 {
		return paneHeaderField{}
	}
	groups := map[string][]Port{}
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
			field.spans = append(field.spans, portSpan(st, " · ", ""))
		}
		ports := groups[protocol]
		sort.Slice(ports, func(i, j int) bool { return ports[i].Number < ports[j].Number })
		field.spans = append(field.spans, portSpan(st, protocolLabel(protocol)+":", ""))
		for i, port := range ports {
			if i > 0 {
				field.spans = append(field.spans, portSpan(st, ",", ""))
			}
			field.spans = append(field.spans, portEntry(st, port, forwarded))
		}
	}
	return field
}

// portSpan is one piece of the ports field: the text in the field's own color,
// linked when there is somewhere for it to point.
func portSpan(st *styles, text, url string) headerSpan {
	if url == "" {
		return headerSpan{text: st.info.Render(text), label: text}
	}
	return headerSpan{text: st.info.Render(hyperlink(url, text)), label: text, url: url}
}

// desktopField is the sandbox's graphical desktop, as a link to the local end of
// its forward.
//
// Its own field rather than a number in the protocol groups, because it is a
// different kind of thing: a desktop to open, not a port to connect something
// to. The client recognizes it by the id the image declared it under
// (sandboxservices.DesktopID) rather than by a port number, so nothing here
// hardcodes 6900 — see ADR 0094 on image services.
//
// Drawn only when the forward has bound it, for the reason portEntry links only
// forwarded ports: an offer to open a desktop that cannot be reached is worse
// than not offering. The label is the declaration's own name, so the sandbox
// says what to call it.
func desktopField(st *styles, s Sandbox, forwarded map[portKey]int) paneHeaderField {
	for _, port := range s.Ports {
		if port.ServiceID != sandboxservices.DesktopID {
			continue
		}
		local, ok := forwarded[port.key()]
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
		return paneHeaderField{spans: []headerSpan{portSpan(st, label, scheme+"://localhost:"+itoa(local))}}
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
func portEntry(st *styles, port Port, forwarded map[portKey]int) headerSpan {
	local, ok := forwarded[port.key()]
	if !ok {
		return portSpan(st, itoa(port.Number), "")
	}
	text := itoa(local)
	if local != port.Number {
		text += "->" + itoa(port.Number)
	}
	scheme, web := portScheme(port.Protocol)
	if !web {
		return portSpan(st, text, "")
	}
	return portSpan(st, text, scheme+"://localhost:"+itoa(local))
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
// marker and, with color, the state dot, then the name filling the slack.
func (l *sandboxList) headOffset(glyph bool) int {
	if glyph {
		return 4
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
