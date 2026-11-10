package console

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/programstatus"
	"github.com/discobox-ai/discobox/pkg/sourceref"
)

// List is the box listing as the console shows it (ADR 26-11-07-876 §1): the
// last listing, what it is filtered to, and the rules that order it into
// folder sections and nest each box under the one that created it. Which row
// is under the cursor, which are selected, and how a row is drawn are the
// renderer's.
//
// It is a value so a renderer can try a filter on a copy (the filter card's
// counts) without touching the one on screen: Filter is copied whole, and
// reading a copy changes nothing the original reads. A copy shares the
// listing, though, so taking a listing, its credential marks or an archive's
// marks (SetAll, SetPending, MarkArchiving, UnmarkArchiving) is the
// original's to do, never a copy's.
type List struct {
	// Session is the window the list is read from: which machine and folder
	// are this one's, and the servers it lists.
	Session Session

	// Filter is what the list is narrowed to.
	Filter Filter

	// Now is when the list is being read, so how old a server's rows are is
	// a pure function of the list and a test can fix it.
	Now func() time.Time

	// StaleAfter is how old a server's rows may get before they say so
	// (ListedAgo): a few of the renderer's refreshes, which is its to set.
	StaleAfter time.Duration

	// Slow is whether the refresh itself is late — still out when the next
	// one was due. It is what a window with one server has instead of the
	// notes about servers: there is no other server's rows to draw and no
	// name to put a note under.
	Slow bool

	all []Sandbox
	// index is where each discobox is in all, which is how a row finds the
	// discobox that created it.
	index map[BoxRef]int
	// nesting is where each row of the last ordering hangs (Rows).
	nesting map[BoxRef]Nest

	// archiving is the discoboxes an archive was asked for here, listed as
	// archived from the press on rather than once the server has answered and
	// the list has been read again: the row leaves the list at once, as an
	// archived box does (MarkArchiving). A listing sent before the archive
	// was answered may still say the box is running, so the mark holds until
	// one sent after the answer lands (SettleArchives); which is which is
	// counted in listings sent, not timed. A failed archive drops it at once
	// (UnmarkArchiving).
	archiving map[BoxRef]archiveMark

	// unreachable are the servers the last listing could not reach, which
	// get a note of their own with no rows: a server's discoboxes going
	// missing says why rather than looking like discoboxes that are gone.
	unreachable []string
	// waiting are the servers the last listing was still asking. A server
	// slow enough to be named here has already kept the poll waiting; one
	// that answers on time is never mentioned, which is what keeps this from
	// being a spinner that is always on.
	waiting []string
	// listed is when each server's rows were last answered (Listing.Listed).
	listed map[string]time.Time

	// loaded is whether a listing has ever landed. Until one has, an empty
	// list is a question rather than an answer.
	loaded bool
}

// NewList is an empty list for a window.
func NewList(session Session, staleAfter time.Duration) List {
	return List{Session: session, Now: time.Now, StaleAfter: staleAfter}
}

// Filter is what the header narrows the list to: a server, a folder, the tags
// a box must carry, and whether the archived are listed. Its zero value is
// every server, every folder and every tag, archived boxes hidden.
type Filter struct {
	// Server is the server the list is filtered to. Empty is every server at
	// once, which is the one choice that is not a server.
	Server string
	// Folder is the folder the list is filtered to. With no key it is every
	// folder, which is the one choice that is not a place.
	Folder Folder
	// Tags are the tags the list is filtered to, in order: a discobox is
	// listed when it carries every one. None is every discobox, tagged or not.
	Tags []string
	// Archived is whether archived discoboxes are listed. They are history:
	// kept, listed on request, and out of the way until then.
	Archived bool
}

// AllFolders is the folder choice that is not a place: every sandbox in the
// project, wherever it was started. It is offered last among the folders
// rather than first, because the folder you are standing in is the one you
// almost always want.
const AllFolders = "all folders"

// EveryFolder is AllFolders as a choice: no key, so nothing is filtered out.
var EveryFolder = Folder{Label: AllFolders}

// Tagged reports whether a discobox carries every tag the filter names.
// Every discobox carries none at all.
func (f Filter) Tagged(s Sandbox) bool {
	for _, tag := range f.Tags {
		if !slices.Contains(s.Tags, tag) {
			return false
		}
	}
	return true
}

// ToggleTag chooses a tag, or lets go of it when it is chosen already. The
// chosen tags are kept in order, so two filters holding the same ones are
// equal.
func (f *Filter) ToggleTag(tag string) {
	if i := slices.Index(f.Tags, tag); i >= 0 {
		f.Tags = slices.Delete(slices.Clone(f.Tags), i, i+1)
		return
	}
	f.AddTag(tag)
}

// AddTag chooses a tag alongside the ones chosen already, in place of any
// other value of its key: a discobox's tags are a map, one value to a key and
// a bare `key` its empty one (sandboxmeta.TagStrings), so no discobox carries
// `ticket=ENG-12` and `ticket=ENG-13` both, and marking the two would list
// nothing.
func (f *Filter) AddTag(tag string) {
	if slices.Contains(f.Tags, tag) {
		return
	}
	key := TagKey(tag)
	// A fresh slice: a filter tried on a copy of the list must not mark the
	// live one through a shared backing array.
	f.Tags = slices.DeleteFunc(slices.Clone(f.Tags), func(t string) bool { return TagKey(t) == key })
	f.Tags = append(f.Tags, tag)
	slices.Sort(f.Tags)
}

// Revealing is the filter that shows box whatever the person's own filter
// hid (ADR 26-11-06-671 §5): every server, every folder, no tags, and the
// archived only when it is one. It is a view for navigating to a box, never a
// filter to save over the person's.
func Revealing(box Sandbox) Filter {
	return Filter{Folder: EveryFolder, Archived: box.State == StateArchived}
}

// View is the filter as it is saved (Session.View): the server, the folder
// and the tags. Every folder is no folder at all, so its label is not
// carried: the zero view is the default. Whether the archived are listed is
// not saved.
func (f Filter) View() ListView {
	view := ListView{Server: f.Server, Tags: f.Tags}
	if f.Folder.Key != "" {
		view.FolderKey, view.FolderLabel, view.FolderSource, view.FolderLocal = f.Folder.Key, f.Folder.Label, f.Folder.Source, f.Folder.Local
	}
	return view
}

// Restore is what a window opens on from a saved view: the view as this
// window holds it, and the filter it stands for.
//
// What the command line named outranks what was saved: --server is the
// primary. That is where the window opens, not a choice made in it, so the
// view is taken to have it — it is saved only once the filter moves off it.
//
// The window's own folder is the session's, not the copy saved: its label
// carries the branch checked out now, which is not part of the key the view
// was saved under; a view saved under its origin key before the window's
// folder was its repository opens on it too. A server is kept only when the
// window lists more than one and this is one of them.
func (s Session) Restore(view ListView) (ListView, Filter) {
	if s.ServerChosen && len(s.Servers) > 0 {
		view.Server = s.Servers[0]
	}
	var f Filter
	switch view.FolderKey {
	case "":
		f.Folder = EveryFolder
	case s.OriginKey, s.Folder().Key:
		f.Folder = s.Folder()
	default:
		f.Folder = Folder{Key: view.FolderKey, Label: view.FolderLabel, Source: view.FolderSource, Local: view.FolderLocal}
	}
	if view.Server != "" && len(s.Servers) > 1 && slices.Contains(s.Servers, view.Server) {
		f.Server = view.Server
	}
	f.Tags = view.Tags
	return view, f
}

// Saves reports whether a window on this session saves view over the one
// saved: it has moved off it, and the window was opened somewhere — before
// the session lands, or in a window opened nowhere, there is no view it
// opened on to keep. A window that never shows the list (a run's or an
// attach's own) saves nothing either; that is its frontend's to know.
func (s Session) Saves(view, saved ListView) bool {
	return s.Directory != "" && !view.Equal(saved)
}

// archiveMark is one discobox's archive that is out or not yet settled
// (List.archiving).
type archiveMark struct {
	// answered is whether the archive has been answered, and after is how
	// many listings had been sent by then: only a later one knows of it.
	answered bool
	after    uint64
	// was is the state the latest listing gave the box, which is what a
	// failed archive puts back.
	was State
}

func refOf(s Sandbox) BoxRef { return BoxRef{Server: s.Server, ID: s.ID} }

// SetAll takes a refreshed listing. A box whose archive is out is listed as
// archived however the listing has it (MarkArchiving).
func (l *List) SetAll(all []Sandbox) {
	l.loaded = true
	for i := range all {
		if mark, ok := l.archiving[refOf(all[i])]; ok && all[i].State != StateArchived {
			mark.was = all[i].State
			l.archiving[refOf(all[i])] = mark
			all[i].State = StateArchived
		}
	}
	l.all = all
	l.index = make(map[BoxRef]int, len(all))
	for i, s := range all {
		l.index[refOf(s)] = i
	}
}

// All is every discobox in the last listing, on every server, whatever the
// filter: what the palette searches and what a box is looked up in. It is the
// list's own slice.
func (l *List) All() []Sandbox { return l.all }

// Loaded reports whether a listing has ever landed. Until one has, an empty
// list is a question rather than an answer, and the invitation to create the
// first discobox is an answer.
func (l *List) Loaded() bool { return l.loaded }

// MarkArchiving lists the discoboxes an archive is going out for as archived
// (archiving).
func (l *List) MarkArchiving(targets []Sandbox) {
	if l.archiving == nil {
		l.archiving = map[BoxRef]archiveMark{}
	}
	for _, box := range targets {
		l.archiving[refOf(box)] = archiveMark{}
	}
	l.SetAll(l.all)
}

// AnsweredArchives records that the archives for targets were answered once
// after listings had been sent, so that the first listing sent after that
// is the one that settles them.
func (l *List) AnsweredArchives(targets []Sandbox, after uint64) {
	for _, box := range targets {
		if mark, ok := l.archiving[refOf(box)]; ok {
			mark.answered, mark.after = true, after
			l.archiving[refOf(box)] = mark
		}
	}
}

// UnmarkArchiving drops the marks of archives that failed, putting the rows
// back in the state the latest listing gave them: the box is where it was.
func (l *List) UnmarkArchiving(targets []Sandbox) {
	for _, box := range targets {
		id := refOf(box)
		mark, ok := l.archiving[id]
		if !ok {
			continue
		}
		delete(l.archiving, id)
		if i, ok := l.index[id]; ok && mark.was != "" {
			l.all[i].State = mark.was
		}
	}
}

// SettleArchives drops the marks of archives answered before listing number
// asked was sent: that listing has the server's word on them.
func (l *List) SettleArchives(asked uint64) {
	for id, mark := range l.archiving {
		if mark.answered && asked > mark.after {
			delete(l.archiving, id)
		}
	}
}

// Archiving is the discoboxes an archive is out or unsettled for.
func (l *List) Archiving() []BoxRef {
	out := make([]BoxRef, 0, len(l.archiving))
	for id := range l.archiving {
		out = append(out, id)
	}
	slices.SortFunc(out, func(a, b BoxRef) int {
		return strings.Compare(a.Server+"\x00"+a.ID, b.Server+"\x00"+b.ID)
	})
	return out
}

// SetPending stamps each row with how many credential requests are waiting on
// it. The count lives on the row rather than being looked up while drawing,
// so drawing stays a pure function of what the list holds.
func (l *List) SetPending(byBox map[string][]CredentialRequest) {
	for i := range l.all {
		l.all[i].PendingRequests = 0
		for _, req := range byBox[l.all[i].ID] {
			if l.Session.ServerName(req.Server) == l.Session.ServerName(l.all[i].Server) {
				l.all[i].PendingRequests++
			}
		}
	}
}

// SetUnreachable takes the servers the last listing could not reach.
func (l *List) SetUnreachable(servers []string) {
	l.unreachable = slices.Clone(servers)
}

// Unreachable is the servers the last listing could not reach, every one.
func (l *List) Unreachable() []string { return l.unreachable }

// SetWaiting takes the servers the last listing was still asking.
func (l *List) SetWaiting(servers []string) {
	l.waiting = slices.Clone(servers)
}

// Waiting is the servers the last listing was still asking, every one.
func (l *List) Waiting() []string { return l.waiting }

// SetListed takes when each server on the last listing last answered.
func (l *List) SetListed(listed map[string]time.Time) { l.listed = listed }

// Listed is when each server on the last listing last answered.
func (l *List) Listed() map[string]time.Time { return l.listed }

// Rows is what is displayed, after the filter, in the order it is drawn: the
// folder sections in the order Folders offers them when the list is Grouped,
// and in each the discoboxes newest-created first as the listing has them,
// each followed by the tree of discoboxes it created (Nesting).
func (l *List) Rows() []Sandbox {
	out := l.nest()
	// Grouped by section, and newest-first inside one as everywhere else: the
	// sort is stable, so the order the listing arrived in is what orders a
	// section. A nested row is in the section of the row its tree hangs
	// from (Top), so the sort moves a tree whole.
	if l.Grouped() {
		folders := l.folderRanks()
		sort.SliceStable(out, func(i, j int) bool {
			ki, kj := l.Section(out[i]), l.Section(out[j])
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

// Count is how many discoboxes the list draws, which is what everything that
// says how many there are says — the band, a section, the filter card, the
// archived offer — so they all count the same ones. Every discobox is on one
// row.
func (l *List) Count() int {
	return len(l.Rows())
}

// Find is the discobox a reference names, on any server and whatever the
// filter, with the reference's server spelled as Session.ServerName spells it.
func (l *List) Find(ref BoxRef) *Sandbox {
	for i := range l.all {
		if (BoxRef{Server: l.Session.ServerName(l.all[i].Server), ID: l.all[i].ID}) == ref {
			s := l.all[i]
			return &s
		}
	}
	return nil
}

// Creator is the discobox that created s, when a discobox did and the
// listing still holds it.
func (l *List) Creator(s Sandbox) (Sandbox, bool) {
	if s.CreatedBy == "" {
		return Sandbox{}, false
	}
	i, ok := l.index[BoxRef{Server: s.Server, ID: s.CreatedBy}]
	if !ok || i >= len(l.all) {
		return Sandbox{}, false
	}
	return l.all[i], true
}

// Nest is one row's place in the trees of the last ordering (Rows): how deep
// it is nested under the discobox that created it, and enough of the shape
// above it to draw the branches.
type Nest struct {
	// Under is the row its tree hangs from, which is the row whose section it
	// is in: itself, for a row nested under nothing.
	Under BoxRef
	// Depth is how many creators it is nested under: 0 for a top row.
	Depth int
	// Last is whether it is the last of its creator's rows.
	Last bool
	// Rules is, for each level between the top and its creator — Depth-1 of
	// them — whether a branch carries on past it there: whether the creator
	// at that level has rows after it of its own creator's.
	Rules []bool
}

// Nesting is where a row hangs in the last ordering (Rows).
func (l *List) Nesting(s Sandbox) Nest {
	if n, ok := l.nesting[refOf(s)]; ok {
		return n
	}
	return Nest{Under: refOf(s)}
}

// Top is the row a row's tree hangs from, which is the row whose section it
// is drawn in: itself, for a row nested under nothing.
func (l *List) Top(s Sandbox) Sandbox {
	if i, ok := l.index[l.Nesting(s).Under]; ok && i < len(l.all) {
		return l.all[i]
	}
	return s
}

// Shown reports whether a discobox passes the filters that are about the
// discobox itself: the server, the tags, and whether the archived are listed.
// The folder is not one of them — a discobox is drawn under its creator in
// whichever folder that is (Rows).
func (l *List) Shown(s Sandbox) bool {
	return l.OnServer(s) && l.Filter.Tagged(s) && (l.Filter.Archived || s.State != StateArchived)
}

// nest is the rows in the order they are drawn, as trees: the discoboxes in
// the folder the filter names, in the order they arrived, and under each the
// discoboxes it leads, their own creations under those in turn. Each row's
// place goes in nesting.
//
// A discobox is filed where the server filed it (ADR 0111 §3), and that is
// often not where its creator is: one cut from a checkout inside the creator
// records the creator as the machine. So it is drawn under its creator
// wherever the creator is drawn, whatever folder it is filed in itself, and
// only there: every discobox is on one row. It is a top in its own folder
// only when no creator of it is drawn.
//
// A row nests under the nearest creator that is shown, so one whose creator
// is archived and hidden, or lacks a tag the filter names, moves up to the
// creator's own creator, or is drawn in its own folder alone.
func (l *List) nest() []Sandbox {
	l.nesting = make(map[BoxRef]Nest, len(l.all))
	type node struct {
		parent int // in l.all, -1 for none
		kids   []int
	}
	nodes := make([]node, len(l.all))
	var tops []int
	for i, s := range l.all {
		nodes[i].parent = -1
		if !l.Shown(s) {
			continue
		}
		// No chain is longer than the listing, which is what ends the walk
		// on a listing that somehow holds a loop.
		by := s
		for range l.all {
			var ok bool
			if by, ok = l.Creator(by); !ok {
				break
			}
			if j := l.index[refOf(by)]; j != i && l.Shown(by) {
				nodes[i].parent = j
				nodes[j].kids = append(nodes[j].kids, i)
				break
			}
		}
		if l.Filter.Folder.Holds(s, l.Session) {
			tops = append(tops, i)
		}
	}

	// drawnAbove is whether a row above i in its chain is a top, in which
	// case i is drawn nested in that one's tree.
	drawnAbove := func(i int) bool {
		at := i
		for range l.all {
			if at = nodes[at].parent; at < 0 || at == i {
				return false
			}
			if l.Filter.Folder.Holds(l.all[at], l.Session) {
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
	var place func(i int, n Nest)
	place = func(i int, n Nest) {
		if placing[i] {
			return
		}
		placing[i], placed[i] = true, true
		defer func() { placing[i] = false }()
		s := l.all[i]
		l.nesting[refOf(s)] = n
		out = append(out, s)
		rules := n.Rules
		if n.Depth > 0 {
			// Below this row, its own level carries a branch on when it is
			// not its creator's last.
			rules = append(slices.Clone(n.Rules), !n.Last)
		}
		kids := nodes[i].kids
		for k, kid := range kids {
			place(kid, Nest{Under: n.Under, Depth: n.Depth + 1, Last: k == len(kids)-1, Rules: rules})
		}
	}
	for _, i := range tops {
		if !drawnAbove(i) {
			place(i, Nest{Under: refOf(l.all[i])})
		}
	}
	// Rows that created each other are each above the other, and are still
	// rows.
	for _, i := range tops {
		if !placed[i] {
			place(i, Nest{Under: refOf(l.all[i])})
		}
	}
	return out
}

// OnServer reports whether a discobox is on the server the filter names.
// Every discobox is on every server at once.
func (l *List) OnServer(s Sandbox) bool {
	return l.Filter.Server == "" || s.Server == l.Filter.Server
}

// Grouped reports whether the list is drawn in sections, one per folder,
// which it is on every folder: the rows carry no folder column, since the
// header stands in for one, so a list of every folder has to say which rows
// came from where. A list narrowed to one folder has none.
//
// Servers are not sections (ADR 26-10-14-585). Which server a discobox is on
// is answered by narrowing the filter to that server.
func (l *List) Grouped() bool {
	return l.Filter.Folder.Key == ""
}

// Section is the key of the section a row is drawn under: the folder it is
// in, or for a nested row the folder of the row its tree hangs from.
func (l *List) Section(s Sandbox) string {
	return l.Top(s).FolderKey(l.Session)
}

// SectionTitle names the section a row is drawn under: its folder — for a
// nested row, its tree's — the way the filter names it.
func (l *List) SectionTitle(s Sandbox) string {
	return l.Top(s).Folder(l.Session).Label
}

// folderRanks is where each folder's rows go: the order the filter offers
// them in, the window's own first.
func (l *List) folderRanks() map[string]int {
	folders := l.Folders()
	ranks := make(map[string]int, len(folders))
	for i, f := range folders {
		ranks[f.Key] = i
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

// ListedAgo is how old a server's rows are, said only once they are old
// enough to be worth saying (StaleAfter): "listed 40s ago". Empty otherwise,
// including for a server with nothing on record — a server that has never
// answered is the "still listing" note's to say, not this.
func (l *List) ListedAgo(server string) string {
	at, ok := l.listed[server]
	if !ok || at.IsZero() {
		return ""
	}
	age := l.Now().Sub(at)
	switch {
	case age < l.StaleAfter:
		return ""
	case age < time.Minute:
		return fmt.Sprintf("listed %ds ago", int(age.Seconds()))
	default:
		return "listed " + Ago(at, l.Now())
	}
}

// Missing is the servers the last listing could not reach, as this list has
// to say them: every one of them when it is showing every server, and
// otherwise only the one it is filtered to. A note about a server whose
// discoboxes are not on screen anyway is a note about somewhere else.
func (l *List) Missing() []string {
	return onlyServer(l.unreachable, l.Filter.Server)
}

// WaitingOn is the servers still being asked, filtered the way Missing is.
func (l *List) WaitingOn() []string {
	return onlyServer(l.waiting, l.Filter.Server)
}

// onlyServer is names as a list filtered to one server has to say them: all
// of them when it is showing every server, and otherwise only the one it is
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

// OneServer reports whether the rows on screen are one server's: a window
// with one server, or a list filtered to one. Then how old they are is the
// band's to say, in place of the count.
func (l *List) OneServer() bool {
	return l.Filter.Server != "" || len(l.Session.Servers) <= 1
}

// Behind is the servers whose rows are on screen but have stopped keeping up
// (ListedAgo), when there is more than one server on screen: each gets a note
// after the rows, the way a server that did not answer does. One server's
// rows say it on the band instead.
func (l *List) Behind() []string {
	if l.OneServer() {
		return nil
	}
	shown := map[string]bool{}
	for _, s := range l.Rows() {
		shown[s.Server] = true
	}
	var names []string
	for name := range l.listed {
		if shown[name] && l.ListedAgo(name) != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// Note is a line the list draws about a server rather than a box, after the
// rows: one that did not answer, one still being asked, or one whose rows
// have stopped keeping up.
type Note struct {
	Server string
	// Title is the server as a note names it (ServerTitle), and Detail what
	// is said about it.
	Title, Detail string
}

// Notes are the server notes the list draws after its rows, in order: the
// servers that did not answer, then the ones not answered yet, then the ones
// whose rows on screen are behind.
func (l *List) Notes() []Note {
	var out []Note
	for _, name := range l.Missing() {
		out = append(out, Note{Server: name, Title: ServerTitle(name), Detail: "not answering"})
	}
	for _, name := range l.WaitingOn() {
		out = append(out, Note{Server: name, Title: ServerTitle(name), Detail: "still listing"})
	}
	for _, name := range l.Behind() {
		out = append(out, Note{Server: name, Title: ServerTitle(name), Detail: l.ListedAgo(name)})
	}
	return out
}

// Origin is where a row says it came from, beside its name: the machine it
// was created on when that is not this one (Sandbox.Elsewhere). A row nested
// under its creator says nothing — the machine it would name is that
// creator, which its branch already says.
func (l *List) Origin(s Sandbox) string {
	if l.Nesting(s).Depth > 0 {
		return ""
	}
	return s.Elsewhere(l.Session)
}

// Partial is the servers a search over every server (Search) has no rows
// from yet: still being asked, then not answering. Its results are partial
// while there are any, whatever the filter.
func (l *List) Partial() []string {
	return append(slices.Clone(l.waiting), l.unreachable...)
}

// Stale is every server whose rows have stopped keeping up (ListedAgo),
// whatever the filter, named as Session.ServerName names it, in order: what a
// search over every server says about the rows it found.
func (l *List) Stale() []Note {
	var out []Note
	for name := range l.listed {
		if age := l.ListedAgo(name); age != "" {
			server := l.Session.ServerName(name)
			out = append(out, Note{Server: server, Title: server, Detail: age})
		}
	}
	slices.SortFunc(out, func(a, b Note) int { return strings.Compare(a.Server+" "+a.Detail, b.Server+" "+b.Detail) })
	return out
}

// ServerTitle is a server as a note names it. It says "server" because a
// bare name over a list of discoboxes is one more name among them.
func ServerTitle(server string) string {
	return "server " + ServerLabel(server)
}

// ServerLabel is a server's name as the list spells it, with the unnamed one —
// what a listing that arrived before the session carries — said as such.
func ServerLabel(server string) string {
	if name := strings.TrimSpace(server); name != "" {
		return name
	}
	return "this server"
}

// Sectioned reports whether the body carries any line that is not a row: the
// folder headers when it is grouped, and the server notes whenever there are
// any.
func (l *List) Sectioned() bool {
	return l.Grouped() || len(l.Missing()) > 0 || len(l.WaitingOn()) > 0 || len(l.Behind()) > 0
}

// OnPrimary reports whether the list is showing the primary's discoboxes —
// alone or among every server's — which is when the machine's resources
// belong above it: they are the primary's, and above a list narrowed to
// another server they would read as that server's.
func (l *List) OnPrimary() bool {
	return l.Filter.Server == "" || len(l.Session.Servers) == 0 || l.Filter.Server == l.Session.Servers[0]
}

// Folders are what the filter can narrow to (ADR 0111): the window's own
// folder first, whether or not anything was cut from it yet, then every other
// folder the project's discoboxes are in, newest first, then the one the
// filter is on if nothing is in it yet. The order does not follow the
// selection, so the filter's folders and the list's folder sections keep
// their places whichever folder is chosen. This machine's own key is never one
// of them: its discoboxes with no source are in every folder of this
// machine's already.
//
// A GitHub repository is one folder however many places its discoboxes are
// filed under (Sandbox.Folder), so the repository is offered once rather than
// each directory and spelling of the URL it was cut from. It is this
// machine's when any of those places is.
//
// It is derived from the listing rather than asked for separately: the
// folders worth offering are exactly the ones something is sitting in.
func (l *List) Folders() []Folder {
	at := map[string]int{"": -1, l.Session.HostKey: -1}
	out := []Folder{}
	add := func(f Folder) {
		if i, ok := at[f.Key]; ok {
			if i >= 0 {
				out[i].Local = out[i].Local || f.Local
			}
			return
		}
		at[f.Key] = len(out)
		out = append(out, f)
	}
	add(l.Session.Folder())
	// The folders on the server the filter names: the folder sits inside
	// the server, and a folder only another server has something in is a
	// choice that lists nothing.
	for _, s := range l.all {
		if l.OnServer(s) {
			add(s.Folder(l.Session))
		}
	}
	add(l.Filter.Folder)
	return out
}

// TagChoices are what the filter can narrow to by tag: every tag the
// discoboxes inside the server and folder filters carry, in order, and the
// chosen ones none of them carries any longer — the way the folders keep the
// one chosen, so a choice does not vanish from under the list it is showing.
func (l *List) TagChoices() []string {
	seen := map[string]bool{}
	var out []string
	// The folder's own discoboxes, not the ones nested there under a creator:
	// a tag only those carry would narrow the list to nothing, since the
	// creator they are drawn under does not carry it.
	for _, s := range l.all {
		if !l.OnServer(s) || !l.Filter.Folder.Holds(s, l.Session) {
			continue
		}
		if !l.Filter.Archived && s.State == StateArchived {
			continue
		}
		for _, tag := range s.Tags {
			if !seen[tag] {
				seen[tag] = true
				out = append(out, tag)
			}
		}
	}
	slices.Sort(out)
	for _, tag := range l.Filter.Tags {
		if !seen[tag] {
			out = append(out, tag)
		}
	}
	return out
}

// Sources are what a create can cut a new discobox from: every source the
// project's discoboxes were cut from, newest sandbox first.
//
// It is the folder list's counterpart and not the same list. A folder is
// where a discobox is filed — a machine and a source — so one source cut on
// two machines is two folders; a source is what can be cut from here, which
// is a repository URL as often as a path. Each carries where a discobox cut
// from it on this machine is filed. A discobox created with no source at all
// contributes nothing, because "no source" is an answer offered on its own.
func (l *List) Sources() []Source {
	seen := map[string]bool{}
	out := []Source{}
	for _, s := range l.all {
		// One source is one entry, however each discobox spelled it.
		key := SourceKey(s.Source)
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

// SourceKey tells one thing to cut from from another however it is spelled —
// a repository's https URL with or without .git, in any case on GitHub, is
// one (sourceref.Ref.CloneKey, ADR 26-10-15-664 §4). The transport is part of
// it: an ssh, http or git:// spelling of the same repository is fetched
// another way, with other credentials or none, so merging it would run a URL
// nobody chose.
func SourceKey(value string) string {
	source, err := sourceref.Parse(value)
	if err != nil {
		return value
	}
	return source.CloneKey()
}

// ArchivedCount is how many more rows listing the archived would draw, a
// creator's among them: what the list offers when they are hidden.
func (l *List) ArchivedCount() int {
	with, without := *l, *l
	with.Filter.Archived, without.Filter.Archived = true, false
	return with.Count() - without.Count()
}

// Neighbor is the row after one box's — the one before it, for the last row
// — which is where the list goes once that box leaves it. Nil when the box is
// not listed or is the only row.
func (l *List) Neighbor(box Sandbox) *Sandbox {
	rows := l.Rows()
	for i, s := range rows {
		if s.ID != box.ID || s.Server != box.Server {
			continue
		}
		switch {
		case i+1 < len(rows):
			return &rows[i+1]
		case i > 0:
			return &rows[i-1]
		}
		return nil
	}
	return nil
}

// Hit is one discobox a search found, and how well.
type Hit struct {
	Box  Sandbox
	Rank Rank
}

// Search is the discoboxes a query matches on every server the console lists
// (ADR 26-11-06-671 §5), whatever the filter: the filter is the person's view
// of the list, and a search that obeyed a forgotten one would report a box as
// absent. Archived boxes are searched only when asked for. Hits come in the
// listing's order; ranking them among other candidates is the caller's.
func (l *List) Search(q Query, archived bool) []Hit {
	var out []Hit
	for _, s := range l.all {
		if s.State == StateArchived && !archived {
			continue
		}
		primary, detail := s.SearchFields(l.Session)
		if rank, ok := MatchFields(q, primary, detail); ok {
			out = append(out, Hit{Box: s, Rank: rank})
		}
	}
	return out
}

// SearchFields are what a search matches a discobox by (ADR 26-11-06-671 §5):
// its IDs and names first, then what describes it — its source, branch, tags
// and their halves, harness, pool, server and state.
func (s Sandbox) SearchFields(session Session) (primary, detail []string) {
	primary = []string{s.ID, strings.TrimPrefix(s.ID, "sbx_"), s.Name, s.ConfigName, s.GivenName}
	detail = []string{s.Description, s.Repository, s.RepositoryKey, s.Source, s.Branch, s.Git.Branch, s.Harness, s.HarnessName, s.Pool, s.PoolID, session.ServerName(s.Server), string(s.State)}
	for _, tag := range s.Tags {
		detail = append(detail, tag)
		detail = append(detail, strings.SplitN(tag, "=", 2)...)
	}
	return primary, detail
}

// StateWord is a discobox's state as a word, for where it is spelled rather
// than drawn. What its agent reports takes the place of `running`, which
// every report implies (a box that is not running carries none); a failed
// agent is `failed`, not the box's own `error`.
func (s Sandbox) StateWord() string {
	if s.State != StateRunning || s.Agent.State == "" {
		return string(s.State)
	}
	return AgentGlyph(s.Agent.State).Word
}

// Remark is what a row says in place of its name when it is the one picked
// out (Sandbox.Remark): the text, and the command that answers it when one
// does, which a frontend offers by its own binding for it — the core names a
// command, never a key (ADR 26-11-07-598) — with what doing so does, as a
// verb: "harness changes" offers "review config" to "review" them.
type Remark struct {
	Text    string
	Command string
	Verb    string
}

// Remark is the error a box settled on, or what a repair nobody typed is
// answering; what an agent blocked on a person, or failed, said about it;
// what the box is still provisioning, or failed to; or that its harness
// changes are waiting to be reviewed. Empty when there is nothing to say and
// the name stands.
func (s Sandbox) Remark() Remark {
	switch {
	case (s.State == StateError || s.State == StateRepairing || s.State == StateRunning) && s.Message != "":
		return Remark{Text: s.Message}
	case (s.Agent.State == programstatus.Blocked || s.Agent.State == programstatus.Error) && s.Agent.Msg != "":
		return Remark{Text: s.Agent.Msg}
	case s.Provisioning != "":
		return Remark{Text: s.Provisioning}
	case s.HarnessConfigPending:
		return Remark{Text: "harness changes", Command: "review config", Verb: "review"}
	}
	return Remark{}
}

// Warned reports whether a running box carries a message: it is up, and
// something about it is still worth a mark in front of its name.
func (s Sandbox) Warned() bool {
	return s.State == StateRunning && s.Message != ""
}

// Since is how long ago something was, in one unit and two characters where
// it can manage it: a column says it to rank rows by recency, not to time
// them. Empty for a time nothing recorded.
func Since(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// Ago is how long before now t was, as a column says it: "5m ago", "3d ago",
// and "just now" for anything under a minute, which Since spells "now" and
// "now ago" would not. Empty for a time nothing recorded.
func Ago(t, now time.Time) string {
	switch age := Since(t, now); age {
	case "":
		return ""
	case "now":
		return "just now"
	default:
		return age + " ago"
	}
}
