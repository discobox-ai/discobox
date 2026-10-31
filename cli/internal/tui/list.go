package tui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/discobox-ai/discobox/pkg/programstatus"
	"github.com/discobox-ai/discobox/pkg/sandboxservices"
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
	// index is where each discobox is in all, which is how a row finds the
	// discobox that created it.
	index    map[boxRef]int
	cursor   int
	offset   int
	selected map[string]bool

	// folder is the folder the list is filtered to, chosen in the header. With
	// no key it is every folder, which is the one choice that is not a place.
	folder folder

	// tags are the tags the list is filtered to, chosen in the header after
	// the folder (tags.go), in order: a discobox is listed when it carries
	// every one. None is every discobox, tagged or not.
	tags []string

	// server is the server the list is filtered to, chosen in the header
	// beside the folder. Empty is every server at once, which is the one
	// choice that is not a server; it is also what a window with only the
	// primary to list holds, where there is nothing to filter.
	server string

	// Archived sandboxes are history: kept, listed on request, and out of the
	// way until then.
	showArchived bool

	// unreachable are the servers the last listing could not reach, which get
	// a section of their own with no rows: a server's discoboxes going missing
	// says why rather than looking like discoboxes that are gone.
	unreachable []string

	// waiting are the servers the last listing was still asking, which get the
	// same section saying so. A server slow enough to be named here has
	// already kept the poll waiting; one that answers on time is never
	// mentioned, which is what keeps this from being a spinner that is always
	// on.
	waiting []string

	// slow is whether the refresh itself is late — still out when the next one
	// was due (listingSlowAfter). It is what a window with one server has
	// instead of the sections above: there is no other server's rows to draw
	// and no name to put the note under, so the band says it.
	slow bool

	// listed is when each server's rows on screen were last answered
	// (Listing.Listed). Rows outlive a server that has stopped answering
	// quickly, and once they are older than listingStaleAfter they say so
	// (listedAgo): a row that looks current and is not is a window that
	// looks stuck.
	listed map[string]time.Time

	// loaded is whether a listing has ever landed here. Until one has, an
	// empty list is a question rather than an answer, and the invitation to
	// create the first discobox is an answer.
	loaded bool

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
	l.loaded = true
	var onID string
	var onUnder boxRef
	if s := l.current(); s != nil {
		onID, onUnder = s.ID, s.under
	}
	l.all = all
	l.index = make(map[boxRef]int, len(all))
	for i, s := range all {
		l.index[boxRef{s.Server, s.ID}] = i
	}

	// A discobox can be on two rows — under its creator, and in its own
	// folder — so the row is the one in the same tree, and failing that the
	// first one of that discobox.
	if onID != "" {
		found := false
		for i, s := range l.rows() {
			if s.ID != onID {
				continue
			}
			if !found || s.under == onUnder {
				l.cursor, found = i, true
			}
			if s.under == onUnder {
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
	out := l.nest()
	// Grouped by section, and newest-first inside one as everywhere else: the
	// sort is stable, so the order the listing arrived in is what orders a
	// section. A nested row is in the section of the row its tree hangs
	// from (top), so the sort moves a tree whole.
	if l.grouped() {
		folders := l.folderRanks()
		sort.SliceStable(out, func(i, j int) bool {
			ki, kj := l.sectionOf(out[i]), l.sectionOf(out[j])
			if fi, fj := folderRank(folders, ki), folderRank(folders, kj); fi != fj {
				return fi < fj
			}
			// Folders the filter does not offer share a rank, and each is
			// still a section of its own: kept apart by key, or their rows
			// interleave and every change of key draws another header.
			return ki < kj
		})
	}
	return out
}

// count is how many discoboxes the list draws, which is what everything that
// says how many there are says — the band, a section, the filter card, the
// archived offer — so they all count the same ones. It is of discoboxes rather
// than rows: one drawn twice (nest) is one.
func (l *sandboxList) count() int {
	return countBoxes(l.rows())
}

func countBoxes(rows []Sandbox) int {
	seen := make(map[boxRef]bool, len(rows))
	for _, s := range rows {
		seen[boxRef{s.Server, s.ID}] = true
	}
	return len(seen)
}

// boxRef names a discobox across every server the window lists: an ID is one
// server's.
type boxRef struct {
	server, id string
}

// creator is the discobox that created s, when a discobox did and the listing
// still holds it.
func (l *sandboxList) creator(s Sandbox) (Sandbox, bool) {
	if s.CreatedBy == "" {
		return Sandbox{}, false
	}
	i, ok := l.index[boxRef{s.Server, s.CreatedBy}]
	if !ok || i >= len(l.all) {
		return Sandbox{}, false
	}
	return l.all[i], true
}

// top is the row a row's tree hangs from, which is the row whose section it
// is drawn in: itself, for a row nested under nothing.
func (l *sandboxList) top(s Sandbox) Sandbox {
	if i, ok := l.index[s.under]; ok && i < len(l.all) {
		return l.all[i]
	}
	return s
}

// shown reports whether a discobox passes the filters that are about the
// discobox itself: the server, the tags, and whether the archived are listed.
// The folder is not one of them — a discobox is drawn under its creator in
// whichever folder that is (nest).
func (l *sandboxList) shown(s Sandbox) bool {
	return l.onServer(s) && l.tagged(s) && (l.showArchived || s.State != StateArchived)
}

// nest is the rows in the order they are drawn, as trees: the discoboxes in
// the folder the header names, in the order they arrived, and under each the
// discoboxes it created, their own creations under those in turn. Each row is
// given the branch it draws (Sandbox.tree) and the row its tree hangs from
// (Sandbox.under).
//
// A discobox is filed where the server filed it (ADR 0111 §3), and that is
// often not where its creator is: one cut from a checkout inside the creator
// records the creator as the machine. So it is drawn under its creator
// wherever the creator is drawn, whatever folder it is filed in itself, and
// in its own folder as well — once, when that is the section it is already
// nested in, and otherwise a second time there, on a row of its own that says
// where it is from (Sandbox.elsewhere).
//
// A row nests under the nearest creator that is shown, so one whose creator
// is archived and hidden, or lacks a tag the header names, moves up to the
// creator's own creator, or is drawn in its own folder alone.
func (l *sandboxList) nest() []Sandbox {
	type node struct {
		parent int // in l.all, -1 for none
		kids   []int
	}
	nodes := make([]node, len(l.all))
	var tops []int
	for i, s := range l.all {
		nodes[i].parent = -1
		if !l.shown(s) {
			continue
		}
		// No chain is longer than the listing, which is what ends the walk
		// on a listing that somehow holds a loop.
		by := s
		for range l.all {
			var ok bool
			if by, ok = l.creator(by); !ok {
				break
			}
			if j := l.index[boxRef{by.Server, by.ID}]; j != i && l.shown(by) {
				nodes[i].parent = j
				nodes[j].kids = append(nodes[j].kids, i)
				break
			}
		}
		if l.folder.holds(s, l.session) {
			tops = append(tops, i)
		}
	}

	// sameSection is whether a row above i in its chain is a row of i's own
	// section, in which case i is already drawn there, nested.
	sameSection := func(i int) bool {
		at := i
		for range l.all {
			if at = nodes[at].parent; at < 0 || at == i {
				return false
			}
			if a := l.all[at]; l.folder.holds(a, l.session) && (!l.grouped() || a.folderKey(l.session) == l.all[i].folderKey(l.session)) {
				return true
			}
		}
		return false
	}

	out := make([]Sandbox, 0, len(tops))
	// placed is what has a row; placing is what is above the row being
	// placed, which a row is not drawn under a second time.
	placed := make([]bool, len(l.all))
	placing := make([]bool, len(l.all))
	var place func(i int, under boxRef, lead, branch string)
	place = func(i int, under boxRef, lead, branch string) {
		if placing[i] {
			return
		}
		placing[i], placed[i] = true, true
		defer func() { placing[i] = false }()
		s := l.all[i]
		s.under, s.tree = under, lead+branch
		out = append(out, s)
		switch branch {
		case treeBranch:
			lead += treeRule
		case treeLast:
			lead += treeGap
		}
		for n, kid := range nodes[i].kids {
			if n == len(nodes[i].kids)-1 {
				place(kid, under, lead, treeLast)
			} else {
				place(kid, under, lead, treeBranch)
			}
		}
	}
	for _, i := range tops {
		if !sameSection(i) {
			place(i, boxRef{l.all[i].Server, l.all[i].ID}, "", "")
		}
	}
	// Rows that created each other are each above the other, and are still
	// rows.
	for _, i := range tops {
		if !placed[i] {
			place(i, boxRef{l.all[i].Server, l.all[i].ID}, "", "")
		}
	}
	return out
}

// The branch in front of a nested row's name: one cell of line and one of
// air, so a level of nesting costs the name two cells.
const (
	treeBranch = "├ "
	treeLast   = "└ "
	treeRule   = "│ "
	treeGap    = "  "
)

// onServer reports whether a discobox is on the server the header names. Every
// discobox is on every server at once.
func (l *sandboxList) onServer(s Sandbox) bool {
	return l.server == "" || s.Server == l.server
}

// setUnreachable takes the servers the last listing could not reach.
func (l *sandboxList) setUnreachable(servers []string) {
	l.unreachable = append(l.unreachable[:0], servers...)
}

// setListed takes when each server on the last listing last answered.
func (l *sandboxList) setListed(listed map[string]time.Time) {
	l.listed = listed
}

// listedAgo is how old a server's rows are, said only once they are old
// enough to be worth saying (listingStaleAfter): "listed 40s ago". Empty
// otherwise, including for a server with nothing on record — a server that has
// never answered is the "still listing" note's to say, not this.
func (l *sandboxList) listedAgo(server string) string {
	at, ok := l.listed[server]
	if !ok || at.IsZero() {
		return ""
	}
	age := l.now().Sub(at)
	switch {
	case age < listingStaleAfter:
		return ""
	case age < time.Minute:
		return fmt.Sprintf("listed %ds ago", int(age.Seconds()))
	default:
		return "listed " + ago(at, l.now())
	}
}

// setWaiting takes the servers the last listing was still asking.
func (l *sandboxList) setWaiting(servers []string) {
	l.waiting = append(l.waiting[:0], servers...)
}

// grouped reports whether the list is drawn in sections, one per folder,
// which it is on "all folders": the rows carry no folder column, since the
// header stands in for one, so a list of every folder has to say which rows
// came from where. A list narrowed to one folder has none — a band naming it
// over every row says what the header already says.
//
// Servers are not sections. Which server a discobox is on is answered by
// narrowing the header to that server, and a section per server split one
// repository's discoboxes across as many bands as there were servers.
func (l *sandboxList) grouped() bool {
	return l.folder.key == ""
}

// sectionOf is the section a row is drawn under: the folder it is in, or for
// a nested row the folder of the row its tree hangs from.
func (l *sandboxList) sectionOf(s Sandbox) string {
	return l.top(s).folderKey(l.session)
}

// folderRanks is where each folder's rows go: the order the header's filter
// offers them in, the window's own first.
func (l *sandboxList) folderRanks() map[string]int {
	folders := l.folders()
	ranks := make(map[string]int, len(folders))
	for i, f := range folders {
		ranks[f.key] = i
	}
	return ranks
}

// folderRank is a folder's place among ranks. A folder the filter does not
// offer — this machine's discoboxes with no source, which are in every folder
// of this machine's rather than one of their own — sorts after the ones it
// does.
func folderRank(ranks map[string]int, key string) int {
	if i, ok := ranks[key]; ok {
		return i
	}
	return len(ranks)
}

// missing is the servers the last listing could not reach, as this list has to
// say them: every one of them when it is showing every server, and otherwise
// only the one it is filtered to. A note about a server whose discoboxes are
// not on screen anyway is a note about somewhere else.
func (l *sandboxList) missing() []string {
	return onlyServer(l.unreachable, l.server)
}

// waitingOn is the servers still being asked, filtered the way missing is: a
// server whose rows would not be on screen anyway is somewhere else.
func (l *sandboxList) waitingOn() []string {
	return onlyServer(l.waiting, l.server)
}

// onlyServer is names as a list filtered to one server has to say them: all of
// them when it is showing every server, and otherwise only the one it is
// filtered to.
func onlyServer(names []string, server string) []string {
	if server == "" {
		return names
	}
	for i, name := range names {
		if name == server {
			return names[i : i+1]
		}
	}
	return nil
}

// sectioned reports whether the body carries any line that is not a row: the
// folder headers when it is grouped, and the notes about servers — not
// answering, still listing, or behind — whenever there are any.
func (l *sandboxList) sectioned() bool {
	return l.grouped() || len(l.missing()) > 0 || len(l.waitingOn()) > 0 || len(l.behind()) > 0
}

// oneServer reports whether the rows on screen are one server's: a window with
// one server, or a list filtered to one. Then how old they are is the band's
// to say, in place of the count.
func (l *sandboxList) oneServer() bool {
	return l.server != "" || len(l.session.Servers) <= 1
}

// behind is the servers whose rows are on screen but have stopped keeping up
// (listedAgo), when there is more than one server on screen: each gets a note
// after the rows, the way a server that did not answer does. One server's
// rows say it on the band instead.
func (l *sandboxList) behind() []string {
	if l.oneServer() {
		return nil
	}
	shown := map[string]bool{}
	for _, s := range l.rows() {
		shown[s.Server] = true
	}
	var names []string
	for name := range l.listed {
		if shown[name] && l.listedAgo(name) != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// folders are what the header can filter to (ADR 0111): the window's own
// folder first, whether or not anything was cut from it yet, then every other
// folder the project's discoboxes are in, newest first, then the one the
// header is on if nothing is in it yet. The order does not follow the
// selection, so the filter card's folders and the list's folder sections
// (folderRanks) keep their places whichever folder is chosen. This machine's own key is never one of them: its discoboxes with no
// source are in every folder of this machine's already.
//
// A GitHub repository is one folder however many places its discoboxes are
// filed under (Sandbox.folder), so the card offers the repository once rather
// than each directory and spelling of the URL it was cut from. It is this
// machine's when any of those places is.
//
// It is derived from the listing rather than asked for separately: the folders
// worth offering are exactly the ones something is sitting in.
func (l *sandboxList) folders() []folder {
	at := map[string]int{"": -1, l.session.HostKey: -1}
	out := []folder{}
	add := func(f folder) {
		if i, ok := at[f.key]; ok {
			if i >= 0 {
				out[i].local = out[i].local || f.local
			}
			return
		}
		at[f.key] = len(out)
		out = append(out, f)
	}
	add(l.session.folder())
	// The folders on the server the header names: the folder sits inside
	// the server, and a folder only another server has something in
	// is a choice that lists nothing.
	for _, s := range l.all {
		if l.onServer(s) {
			add(s.folder(l.session))
		}
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
		// One source is one entry, however each discobox spelled it.
		key := sourceKey(s.Source)
		if s.Source == "" || seen[key] {
			continue
		}
		seen[key] = true
		source := Source{Value: s.Source, Remote: s.SourceRemote, OriginKey: s.SourceOriginKey, RepositoryKey: s.RepositoryKey}
		if !s.SourceRemote {
			// What a local discobox records is its directory alone.
			source.Directory = s.Source
		}
		out = append(out, source)
	}
	return out
}

// archivedCount is what the title bar offers when they are hidden: a number
// worth pressing a key for, or nothing to say.
func (l *sandboxList) archivedCount() int {
	// The ones showing them would draw, a creator's among them: the list
	// with them shown, less the list without.
	with, without := *l, *l
	with.showArchived, without.showArchived = true, false
	return with.count() - without.count()
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
	// A discobox drawn twice — under its creator, and in its own folder — is
	// still one discobox to act on.
	seen := map[boxRef]bool{}
	var out []Sandbox
	take := func(s Sandbox) {
		if ref := (boxRef{s.Server, s.ID}); !seen[ref] {
			seen[ref] = true
			out = append(out, s)
		}
	}
	if l.visual && len(rows) > 0 {
		lo, hi := l.visualRange()
		for _, s := range rows[lo : hi+1] {
			take(s)
		}
		return out
	}
	for _, s := range rows {
		if l.selected[s.ID] {
			take(s)
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
	return countBoxes(rows[lo : hi+1])
}

func (l *sandboxList) selectionCount() int {
	seen := map[boxRef]bool{}
	for _, s := range l.rows() {
		if l.selected[s.ID] {
			seen[boxRef{s.Server, s.ID}] = true
		}
	}
	return len(seen)
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
	case StateStarting, StateRepairing, StateSuspended:
		return st.stateBusy
	case StateError:
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

// stateWord is the state as the row spells it without color, where the
// glyphs are gone. What a discobox's agent reports takes the place of
// `running`, which every report implies (a box that is not running carries
// none), so it costs no column of its own; a failed agent is `failed`, not
// the box's own `error`.
func stateWord(s Sandbox) string {
	if s.State != StateRunning || s.Agent.State == "" {
		return string(s.State)
	}
	if s.Agent.State == programstatus.Error {
		return "failed"
	}
	return string(s.Agent.State)
}

func stateDot(s Sandbox) string {
	switch s.State {
	case StateRunning:
		return "●"
	case StateStarting, StateRepairing:
		return "◐"
	case StateSuspended:
		return "‖"
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
	right := plural(countBoxes(rows), "box", "boxes")
	// A refresh out longer than the window waits before saying so takes the
	// count's place: the count is exactly what is being refreshed, and this is
	// the one place a window with one server has to say it. Nothing is drawn
	// here on an ordinary refresh, which is the point — an indicator that is
	// always on says nothing when it matters.
	if l.slow {
		right = "still listing"
	}
	// Rows that have stopped keeping up say how far behind they are, which
	// outranks "still listing": it is the same news with the size of it.
	// With several servers on screen it is one of them that is behind, and
	// its own note says so instead (behind).
	if l.oneServer() {
		if age := l.listedAgo(l.server); age != "" {
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
	if l.height > 1 && l.onPrimary() {
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
	absent, awaited, lagging := l.missing(), l.waitingOn(), l.behind()
	notes := min(len(absent)+len(awaited)+len(lagging), max(rowBudget, 0))
	rowBudget -= notes
	bodyBudget := rowBudget + notes

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
	// It waits for the first listing rather than for a fast one — "no
	// discoboxes here yet" is an answer about the project, and until something
	// has landed nobody has one. After that it stays put whether or not a
	// refresh is out: an invitation that blinked out every time a poll ran
	// long would be the one screen a new user is reading flickering at them.
	if l.loaded && len(rows) == 0 && len(absent) == 0 && len(awaited) == 0 {
		body = append(body, st.dimText.Render(pad("  no discoboxes here yet — type a prompt below", l.width)))
	}
	l.drawn = drawn{top: len(out), first: l.offset}
	if l.sectioned() {
		l.drawn.rows = make([]int, 0, max(bodyBudget, 0))
	}
	// How many each section has, for its band to say, counted once rather
	// than per header: a section scrolled into twice is one section.
	sectionCounts := map[string]int{}
	if l.grouped() {
		for _, s := range rows {
			sectionCounts[l.sectionOf(s)]++
		}
	}
	var drawnSection *string
	var rowLinks []rowLink
	for i := l.offset; i < len(rows) && len(body) < rowBudget; i++ {
		// A header in front of each section's rows, and in front of the first
		// row drawn whichever section it is in: a window scrolled into the
		// middle of one still says which folder is on screen.
		if key := l.sectionOf(rows[i]); l.grouped() && (drawnSection == nil || key != *drawnSection) {
			if len(body)+1 >= rowBudget {
				break
			}
			body = append(body, l.sectionHeader(st, l.sectionTitle(rows[i]), plural(sectionCounts[key], "box", "boxes")))
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
	}
	// The servers that did not answer, after the ones that did: their rows are
	// missing from this listing rather than gone, and a section saying so is
	// where a reader looks for them.
	for _, name := range absent {
		if len(body) >= bodyBudget {
			break
		}
		body = append(body, l.sectionHeader(st, serverTitle(name), "not answering"))
		l.drawn.rows = append(l.drawn.rows, -1)
	}
	// And the ones that have not answered yet, which is a different thing to
	// say: their rows are on their way rather than missing.
	for _, name := range awaited {
		if len(body) >= bodyBudget {
			break
		}
		body = append(body, l.sectionHeader(st, serverTitle(name), "still listing"))
		l.drawn.rows = append(l.drawn.rows, -1)
	}
	// And the ones whose rows are on screen but have stopped keeping up: the
	// rows sit among every other server's in their folders, so the note is
	// what says which of them are old.
	for _, name := range lagging {
		if len(body) >= bodyBudget {
			break
		}
		body = append(body, l.sectionHeader(st, serverTitle(name), l.listedAgo(name)))
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

// lineSpan is how many lines rows from through to take, headers included. It
// is what tells the window whether the cursor still fits, and it counts the
// way the body draws: a header in front of the first row, and one wherever the
// section changes.
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
		if l.sectionOf(rows[i]) != l.sectionOf(rows[i-1]) {
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

// sectionTitle names the section a row is drawn under: its folder — for a
// nested row, its tree's — the way the header's filter names it.
func (l *sandboxList) sectionTitle(s Sandbox) string {
	return l.top(s).folder(l.session).label
}

// serverTitle is a server as a band names it. It says "server" because a bare
// name over a list of discoboxes is one more name among them.
func serverTitle(server string) string {
	return "server " + sectionServer(server)
}

// sectionServer is a server's name as a band spells it, with the unnamed one —
// what a listing that arrived before the session carries — said as such.
func sectionServer(server string) string {
	if name := strings.TrimSpace(server); name != "" {
		return name
	}
	return "this server"
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
func (l *sandboxList) row(st *styles, z *zones, s Sandbox, i, y int, focused bool) (string, []rowLink) {
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
		// As wide as the longest state, `repairing`.
		addCol("  "+pad(stateWord(s), 9), 11)
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
	// a column: it is the header's filter, and every row on screen has
	// already been filtered to it — a column repeating the same value on every
	// row is a column spent saying nothing.
	//
	baseStyle := gitStyle(st, s)
	addCol(baseStyle.Render(pad(s.base(), 14)), 15)
	// The mark spelled out, in the mark's own color. It is added right after
	// the position so the two survive a narrowing terminal together.
	addCol(baseStyle.Render(pad(s.changes(), 7)), 8)
	addCol(st.dimText.Render(pad(createdText(s, l.now()), 8)), 9)
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
		// What the agent reports comes right after the box's own state, so
		// the two read as one column pair; a box whose agent reports nothing
		// leaves the cell blank rather than closing it up, so the names stay
		// in line.
		head += l.stateStyle(st, s).Render(stateDot(s)) + " " + agentGlyph(st, s.Agent.State) + " "
	}
	// The branch a discobox created by another hangs from, after the state so
	// the glyphs stay one column the eye runs down. It is part of the head: it
	// comes out of the name's width, and the name keeps its floor.
	if s.tree != "" {
		head += st.dimText.Render(s.tree)
	}
	nameW := max(l.width-lipgloss.Width(head)-lipgloss.Width(tail), 4)
	name := s.Name
	// Under the cursor the name gives way to what the state is about: the
	// error a box settled on, or what a repair nobody typed is answering.
	if (s.State == StateError || s.State == StateRepairing) && s.Message != "" && atCursor {
		name = s.Message
	} else if (s.Agent.State == programstatus.Blocked || s.Agent.State == programstatus.Error) && s.Agent.Msg != "" && atCursor {
		// And to what an agent blocked on a person, or failed, said about it.
		name = s.Agent.Msg
	} else if s.Provisioning != "" && atCursor {
		// And to what the box is still provisioning, or failed to.
		name = s.Provisioning
	} else if s.HarnessConfigPending && atCursor {
		name = "harness changes: H to review"
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
	// A row nested under its creator does not say it: the machine it would
	// name is that creator, which the branch already says.
	from := ""
	if text := s.elsewhere(l.session); text != "" && s.tree == "" {
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
	// even one would squeeze the name (ADR 0136). A pull request status comes
	// first, so it is the last to give way: the tag is the only place the row
	// says it.
	var shown []string
	taggedW := 0
	if len(s.Tags) > 0 {
		room := nameW - (nameReserve - lipgloss.Width(head))
		for _, tag := range s.rowTags() {
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
	line := padANSI(head+cell+tail, l.width)
	switch {
	case atCursor && selected:
		return highlight(st, line, colBothBG), links
	case selected:
		return highlight(st, line, colSelectedBG), links
	case atCursor:
		return highlight(st, line, colHighlightBG), links
	default:
		return line, links
	}
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
// pull request status (Sandbox.prStatus) is drawn bold, green for ready and
// red for blocked, instead. A tag that numbers an issue or a pull request in the discobox's
// GitHub repository (Sandbox.tagURL) is a link to it instead, in the color the
// header's links are drawn in, and lit when the pointer is on it — the same
// terms the header's links are drawn on, since a click on one opens it.
func (l *sandboxList) tagSpans(st *styles, z *zones, s Sandbox, tags []string, x, y int) (string, []rowLink) {
	var out strings.Builder
	var links []rowLink
	for _, tag := range tags {
		label := tagLabel(tag)
		out.WriteString(st.dimText.Render(" "))
		x++
		width := lipgloss.Width(label)
		switch url := s.tagURL(tag); {
		case tagKey(tag) == prBlocked:
			out.WriteString(st.blockedText.Render(label))
		case tagKey(tag) == prReady:
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
func gitStyle(st *styles, s Sandbox) lipgloss.Style {
	switch {
	case s.dirty():
		return st.statusWA
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
func workField(st *styles, s Sandbox) paneHeaderField {
	var field paneHeaderField
	for _, tag := range s.Tags {
		url := s.tagURL(tag)
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
func portEntry(st *styles, port Port, forwarded map[portKey]int) headerSpan {
	local, ok := forwarded[port.key()]
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

// onPrimary reports whether the list is showing the primary's discoboxes —
// alone or among every server's — which is when the machine line belongs above
// it. The figures are the primary's (DataSource.Resources), and above a list
// narrowed to another server an unnamed capacity line reads as that server's,
// which is the number somebody checks before creating there.
func (l *sandboxList) onPrimary() bool {
	return l.server == "" || len(l.session.Servers) == 0 || l.server == l.session.Servers[0]
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
