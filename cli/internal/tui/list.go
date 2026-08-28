package tui

import (
	"sort"
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

// sources are what the run options can cut a new discobox from: every source
// the project's discoboxes were cut from, newest sandbox first.
//
// It is the folder list's counterpart and not the same list. A folder is where
// a create was run from, which a remote-sourced discobox shares with every
// other one started in that directory; a source is what was actually
// materialized, which is a repository URL as often as a path. A discobox
// created with no source at all contributes nothing, because "no source" is an
// answer the row already offers on its own.
func (l *sandboxList) sources() []Source {
	seen := map[string]bool{}
	out := []Source{}
	for _, s := range l.all {
		if s.Source == "" || seen[s.Source] {
			continue
		}
		seen[s.Source] = true
		out = append(out, Source{Value: s.Source, Remote: s.SourceRemote})
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
	baseStyle := gitStyle(st, s)
	addCol(baseStyle.Render(pad(s.base(), 14)), 15)
	// The mark spelled out, in the mark's own color. It is added right after
	// the position so the two survive a narrowing terminal together.
	addCol(baseStyle.Render(pad(s.changes(), 7)), 8)
	addCol(st.dimText.Render(pad(createdText(s, l.now()), 7)), 8)
	addCol(usage(st, s), usageWidth+1)
	addCol(padANSI(diffText(st, s), 11), 11)

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

// portsText is what the sandbox is serving, grouped by protocol:
//
//	http:3000,5173,8080 · https:8443 · tcp:22,5432,6379
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
// Empty when nothing is listening, which is also what a sandbox whose agent has
// not reported yet looks like — there is no third thing to say and no room to
// say it in.
func portsText(st *styles, s Sandbox, forwarded map[int]int) string {
	if len(s.Ports) == 0 {
		return ""
	}
	groups := map[string][]Port{}
	var order []string
	for _, port := range s.Ports {
		if _, seen := groups[port.Protocol]; !seen {
			order = append(order, port.Protocol)
		}
		groups[port.Protocol] = append(groups[port.Protocol], port)
	}
	sort.SliceStable(order, func(i, j int) bool {
		return protocolRank(order[i]) < protocolRank(order[j])
	})

	parts := make([]string, 0, len(order))
	for _, protocol := range order {
		ports := groups[protocol]
		sort.Slice(ports, func(i, j int) bool { return ports[i].Number < ports[j].Number })
		text := make([]string, 0, len(ports))
		for _, port := range ports {
			text = append(text, portEntry(port, forwarded))
		}
		parts = append(parts, protocolLabel(protocol)+":"+strings.Join(text, ","))
	}
	return st.info.Render(strings.Join(parts, " · "))
}

// portEntry is one port in its group: the number on its own, or `local->remote`
// when the workspace's forward has given it a local port.
//
// Both numbers are drawn even when they are the same. `1234->1234` says the
// port is reachable here, which a bare `1234` — the shape every unforwarded
// port already has — cannot; the arrow is the mark of "this one is open", and a
// mark that disappears exactly when the forward got what it asked for would be
// the wrong way round.
//
// A web port is also a link to the local end of it, so the port a sandbox is
// serving is one click away rather than a URL to assemble by hand. Only the
// forwarded ones: a link to a port nothing is listening on is worse than no
// link. Only the web ones: OSC 8 hands the URL to whatever opens
// `http://`, and there is nothing sensible for a browser to do with a Postgres
// socket.
func portEntry(port Port, forwarded map[int]int) string {
	local, ok := forwarded[port.Number]
	if !ok {
		return itoa(port.Number)
	}
	text := itoa(local) + "->" + itoa(port.Number)
	scheme, web := portScheme(port.Protocol)
	if !web {
		return text
	}
	return hyperlink(scheme+"://localhost:"+itoa(local), text)
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
var protocolOrder = []string{"http", "https", "tcp", "unknown"}

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
