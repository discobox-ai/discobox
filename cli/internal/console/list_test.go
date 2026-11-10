package console

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/programstatus"
)

// listSession is a window on this machine, in /src/app, with two servers.
var listSession = Session{HostID: "host_here", OriginKey: "here:/src/app", HostKey: "here", Servers: []string{"alpha", "beta"}}

func listOf(boxes ...Sandbox) *List {
	l := NewList(listSession, 15*time.Second)
	l.Now = func() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) }
	l.SetAll(boxes)
	return &l
}

// here is a box filed in the window's own folder; elsewhere one in another
// directory of this machine's.
func here(id, name string) Sandbox {
	return Sandbox{ID: id, Name: name, Server: "alpha", State: StateRunning, OriginKey: "here:/src/app", OriginHostID: "host_here", Source: "/src/app"}
}

func elsewhere(id, name string) Sandbox {
	return Sandbox{ID: id, Name: name, Server: "alpha", State: StateRunning, OriginKey: "here:/src/other", OriginHostID: "host_here", Source: "/src/other"}
}

// createdBy is a box a box created, filed as a create from inside one is: in
// a path inside its creator.
func createdBy(id, name, by string) Sandbox {
	return Sandbox{ID: id, Name: name, Server: "alpha", State: StateRunning, CreatedBy: by, OriginKey: "box:" + by + ":/workspace/source", OriginHostID: "host_" + by, Source: "/workspace/source"}
}

func ids(rows []Sandbox) string {
	out := make([]string, len(rows))
	for i, s := range rows {
		out[i] = s.ID
	}
	return strings.Join(out, " ")
}

// On every folder the rows come in sections, the window's own folder first,
// each newest-first as the listing has them.
func TestListRowsAreSectionedByFolderWindowsOwnFirst(t *testing.T) {
	l := listOf(elsewhere("sbx_o1", "o1"), here("sbx_h1", "h1"), elsewhere("sbx_o2", "o2"), here("sbx_h2", "h2"))
	if !l.Grouped() {
		t.Fatal("a list on every folder is not grouped")
	}
	if got, want := ids(l.Rows()), "sbx_h1 sbx_h2 sbx_o1 sbx_o2"; got != want {
		t.Fatalf("rows = %s, want %s", got, want)
	}
	if got := l.SectionTitle(l.Rows()[2]); got != "/src/other" {
		t.Errorf("section title = %q, want the folder's label", got)
	}

	l.Filter.Folder = listSession.Folder()
	if l.Grouped() {
		t.Error("a list narrowed to one folder is grouped")
	}
	if got, want := ids(l.Rows()), "sbx_h1 sbx_h2"; got != want {
		t.Fatalf("rows in the window's folder = %s, want %s", got, want)
	}
}

// A box a box created is drawn under it, wherever it is filed itself, and the
// nesting says how to draw the branches.
func TestListNestsUnderTheCreator(t *testing.T) {
	l := listOf(
		createdBy("sbx_grand", "grandchild", "sbx_old"),
		here("sbx_solo", "solo"),
		createdBy("sbx_new", "newer child", "sbx_lead"),
		createdBy("sbx_old", "older child", "sbx_lead"),
		here("sbx_lead", "lead"),
	)
	if got, want := ids(l.Rows()), "sbx_solo sbx_lead sbx_new sbx_old sbx_grand"; got != want {
		t.Fatalf("rows = %s, want %s", got, want)
	}
	lead := BoxRef{Server: "alpha", ID: "sbx_lead"}
	for _, tc := range []struct {
		id   string
		want Nest
	}{
		{"sbx_lead", Nest{Under: lead}},
		{"sbx_new", Nest{Under: lead, Depth: 1, Last: false}},
		{"sbx_old", Nest{Under: lead, Depth: 1, Last: true}},
		// Its creator is the last of the lead's, so no branch carries on
		// past it.
		{"sbx_grand", Nest{Under: lead, Depth: 2, Last: true, Rules: []bool{false}}},
	} {
		got := l.Nesting(Sandbox{ID: tc.id, Server: "alpha"})
		if got.Under != tc.want.Under || got.Depth != tc.want.Depth || got.Last != tc.want.Last || !slices.Equal(got.Rules, tc.want.Rules) {
			t.Errorf("%s nests %+v, want %+v", tc.id, got, tc.want)
		}
	}
	// A nested row is in its tree's section, not its own folder's.
	if got := l.Section(Sandbox{ID: "sbx_grand", Server: "alpha", OriginKey: "box:sbx_old:/workspace/source"}); got != listSession.Folder().Key {
		t.Errorf("a nested row's section = %q, want its tree's", got)
	}

	// A creator the filter hides hands its rows up to its own creator.
	boxes := slices.Clone(l.All())
	for i := range boxes {
		if boxes[i].ID == "sbx_old" {
			boxes[i].State = StateArchived
		}
	}
	l.SetAll(boxes)
	// Its creator's rows come in the order the listing has them.
	if got, want := ids(l.Rows()), "sbx_solo sbx_lead sbx_grand sbx_new"; got != want {
		t.Fatalf("rows with the creator archived = %s, want %s", got, want)
	}
	if n := l.Nesting(Sandbox{ID: "sbx_grand", Server: "alpha"}); n.Depth != 1 || n.Last {
		t.Errorf("grandchild nests %+v under a hidden creator, want under the lead, not last", n)
	}
	if got := l.ArchivedCount(); got != 1 {
		t.Errorf("archived count = %d, want 1", got)
	}
}

// The server, the tags and the archived each narrow the list; the search
// ignores every one of them.
func TestListFiltersAndTheSearchDoesNot(t *testing.T) {
	tagged := here("sbx_tagged", "tagged")
	tagged.Tags = []string{"ticket=ENG-12", "wip"}
	beta := here("sbx_beta", "on beta")
	beta.Server = "beta"
	gone := here("sbx_gone", "archived one")
	gone.State = StateArchived
	l := listOf(tagged, beta, gone)

	if got, want := ids(l.Rows()), "sbx_tagged sbx_beta"; got != want {
		t.Fatalf("rows = %s, want %s", got, want)
	}
	l.Filter.Server = "beta"
	if got := ids(l.Rows()); got != "sbx_beta" {
		t.Fatalf("rows on beta = %s", got)
	}
	l.Filter = Filter{}
	l.Filter.AddTag("ticket=ENG-13")
	l.Filter.AddTag("ticket=ENG-12")
	if !slices.Equal(l.Filter.Tags, []string{"ticket=ENG-12"}) {
		t.Fatalf("tags = %q, want a tag to replace the other value of its key", l.Filter.Tags)
	}
	if got := ids(l.Rows()); got != "sbx_tagged" {
		t.Fatalf("rows tagged = %s", got)
	}
	if got := l.TagChoices(); !slices.Equal(got, []string{"ticket=ENG-12", "wip"}) {
		t.Errorf("tag choices = %q", got)
	}
	l.Filter.ToggleTag("ticket=ENG-12")
	l.Filter.Archived = true
	if got := ids(l.Rows()); got != "sbx_tagged sbx_beta sbx_gone" {
		t.Fatalf("rows with the archived = %s", got)
	}

	l.Filter = Filter{Server: "alpha", Tags: []string{"nothing"}}
	hits := l.Search(ParseQuery("on"), false)
	if len(hits) != 1 || hits[0].Box.ID != "sbx_beta" {
		t.Fatalf("search = %+v, want the beta box whatever the filter", hits)
	}
	if hits := l.Search(ParseQuery("archived"), false); len(hits) != 0 {
		t.Errorf("search found %+v, want the archived left out until asked for", hits)
	}
	if hits := l.Search(ParseQuery("archived"), true); len(hits) != 1 {
		t.Errorf("search with the archived = %+v", hits)
	}
	if got := l.Find(BoxRef{Server: "beta", ID: "sbx_beta"}); got == nil || got.Name != "on beta" {
		t.Errorf("find = %v", got)
	}

	l.Filter = Revealing(gone)
	if got := ids(l.Rows()); !strings.Contains(got, "sbx_gone") {
		t.Errorf("the revealing filter hides the box it reveals: %s", got)
	}
}

// What the list says about servers comes after its rows: the unreachable,
// the slow, and the ones whose rows are behind.
func TestListNotesAboutServers(t *testing.T) {
	a, b := here("sbx_a", "a"), here("sbx_b", "b")
	b.Server = "beta"
	l := listOf(a, b)
	l.SetUnreachable([]string{"gamma"})
	l.SetWaiting([]string{"delta"})
	l.SetListed(map[string]time.Time{"alpha": l.Now().Add(-40 * time.Second), "beta": l.Now()})
	want := []Note{
		{Server: "gamma", Title: "server gamma", Detail: "not answering"},
		{Server: "delta", Title: "server delta", Detail: "still listing"},
		{Server: "alpha", Title: "server alpha", Detail: "listed 40s ago"},
	}
	if got := l.Notes(); !slices.Equal(got, want) {
		t.Fatalf("notes = %+v, want %+v", got, want)
	}
	// Narrowed to one server, only that server's notes are the list's, and
	// how old its rows are is the band's to say.
	l.Filter.Server = "alpha"
	if got := l.Notes(); len(got) != 0 {
		t.Errorf("notes on alpha = %+v, want none", got)
	}
	if got := l.ListedAgo("alpha"); got != "listed 40s ago" {
		t.Errorf("listed ago = %q", got)
	}
	// A search over every server says what it is missing whatever the filter.
	if got := l.Partial(); !slices.Equal(got, []string{"delta", "gamma"}) {
		t.Errorf("partial = %q", got)
	}
	if got := l.Stale(); len(got) != 1 || got[0].Server != "alpha" || got[0].Detail != "listed 40s ago" {
		t.Errorf("stale = %+v", got)
	}
}

// An archive is listed from the press on, until a listing sent after its
// answer lands; a failed one puts the box back.
func TestListArchivesFromThePress(t *testing.T) {
	box := here("sbx_a", "a")
	l := listOf(box)
	l.MarkArchiving([]Sandbox{box})
	l.SetAll([]Sandbox{box})
	if got := l.Count(); got != 0 {
		t.Fatalf("count = %d while the archive is out, want 0", got)
	}
	l.AnsweredArchives([]Sandbox{box}, 3)
	l.SettleArchives(3)
	if len(l.Archiving()) != 1 {
		t.Fatal("a listing sent before the answer settled the archive")
	}
	l.SettleArchives(4)
	if len(l.Archiving()) != 0 {
		t.Fatal("a listing sent after the answer did not settle the archive")
	}

	l.SetAll([]Sandbox{box})
	l.MarkArchiving([]Sandbox{box})
	l.UnmarkArchiving([]Sandbox{box})
	if got := l.All()[0].State; got != StateRunning {
		t.Errorf("a failed archive left the box %s", got)
	}
}

// Under the cursor a row says what its state is about, in this order.
func TestARowsRemark(t *testing.T) {
	for _, tc := range []struct {
		box  Sandbox
		want Remark
	}{
		{Sandbox{State: StateRunning}, Remark{}},
		{Sandbox{State: StateError, Message: "disk full", Provisioning: "cloning"}, Remark{Text: "disk full"}},
		{Sandbox{State: StateStopped, Message: "stale"}, Remark{}},
		{Sandbox{State: StateRunning, Agent: programstatus.Status{State: programstatus.Blocked, Msg: "needs a token"}}, Remark{Text: "needs a token"}},
		{Sandbox{State: StateStarting, Provisioning: "cloning"}, Remark{Text: "cloning"}},
		// The command that answers it, by ID: the key is each frontend's.
		{Sandbox{State: StateRunning, HarnessConfigPending: true}, Remark{Text: "harness changes", Command: "review config", Verb: "review"}},
	} {
		if got := tc.box.Remark(); got != tc.want {
			t.Errorf("%+v remarks %+v, want %+v", tc.box, got, tc.want)
		}
	}
	if got := (Sandbox{State: StateRunning, Agent: programstatus.Status{State: programstatus.Error}}).StateWord(); got != "failed" {
		t.Errorf("a failed agent's state word = %q", got)
	}
}

// A saved view opens on the filter it stands for, and the filter saves as
// that view again.
func TestASavedViewRestoresItsFilter(t *testing.T) {
	session := listSession
	view, f := session.Restore(ListView{Server: "beta", FolderKey: "here:/src/other", FolderLabel: "/src/other", Tags: []string{"wip"}})
	if f.Server != "beta" || f.Folder.Key != "here:/src/other" || !slices.Equal(f.Tags, []string{"wip"}) {
		t.Fatalf("filter = %+v", f)
	}
	if got := f.View(); !got.Equal(view) {
		t.Errorf("view = %+v, want %+v", got, view)
	}
	// A server no longer listed is every server; --server outranks the view.
	if _, f := session.Restore(ListView{Server: "gone"}); f.Server != "" || f.Folder.Label != AllFolders {
		t.Errorf("filter on a gone server = %+v", f)
	}
	session.ServerChosen = true
	if view, f := session.Restore(ListView{Server: "beta"}); view.Server != "alpha" || f.Server != "alpha" {
		t.Errorf("--server: view %+v, filter %+v", view, f)
	}
	// The window's own folder is the session's, by its origin key too.
	if _, f := listSession.Restore(ListView{FolderKey: listSession.OriginKey, FolderLabel: "stale label"}); f.Folder != listSession.Folder() {
		t.Errorf("own folder = %+v", f.Folder)
	}
}

// The filter's choices are counted against the rest of the filter, say the
// filter marking each makes, and let a marked tag go: every frontend's card
// is drawn from them.
func TestFilterChoices(t *testing.T) {
	a := here("a", "a")
	a.Tags = []string{"wip"}
	b := elsewhere("b", "b")
	b.Server = "beta"
	l := listOf(a, b, here("c", "c"))
	l.SetUnreachable([]string{"beta"})
	l.Filter.Folder = EveryFolder

	byLabel := map[string]FilterChoice{}
	var labels []string
	for _, c := range l.FilterChoices() {
		byLabel[c.Label] = c
		labels = append(labels, string(c.Group)+":"+c.Label)
	}
	own := l.Session.Folder()
	if want := []string{"server:" + AllServers, "server:alpha", "server:beta", "folder:" + own.Label, "folder:/src/other", "folder:" + AllFolders, "tag:" + AllTags, "tag:#wip"}; !slices.Equal(labels, want) {
		t.Fatalf("choices %q, want %q", labels, want)
	}
	if c := byLabel[AllServers]; !c.Marked || c.Detail != "3 boxes · created on alpha" {
		t.Errorf("every server: %+v", c)
	}
	if c := byLabel["alpha"]; c.Detail != "2 boxes · the primary" || c.Mark.Server != "alpha" {
		t.Errorf("the primary: %+v", c)
	}
	if c := byLabel["beta"]; c.Detail != "not answering" {
		t.Errorf("a server not answering: %+v", c)
	}
	if c := byLabel[own.Label]; c.Mark.Folder.Key != own.Key || c.Detail != "2 boxes · where this window is running" {
		t.Errorf("the window's folder: %+v", c)
	}
	wip := byLabel["#wip"]
	if wip.Marked || !wip.Several || !slices.Equal(wip.Mark.Tags, []string{"wip"}) || wip.Detail != "1 box" {
		t.Fatalf("#wip unmarked: %+v", wip)
	}

	l.Filter = wip.Mark
	if got := l.FilterLabel(); got != "#wip" {
		t.Errorf("label %q, want #wip", got)
	}
	for _, c := range l.FilterChoices() {
		if c.Label == "#wip" && (!c.Marked || len(c.Unmark.Tags) != 0) {
			t.Errorf("#wip marked: %+v, want it marked and letting go to no tags", c)
		}
		if c.Label == "alpha" && c.Detail != "1 box · the primary" {
			t.Errorf("alpha counted against #wip: %q", c.Detail)
		}
	}
	l.Filter.Server = "alpha"
	if got := l.FilterLabel(); got != "server alpha · #wip" {
		t.Errorf("label %q", got)
	}
}
