package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// nestSession is a window on this machine, in /src/app.
var nestSession = Session{HostID: "host_here", OriginKey: "here:/src/app", HostKey: "here"}

// nestBoxes is a lead a person created here, two discoboxes it created — one
// of which created one of its own — and an unrelated one. Each created one
// records what a create from inside a discobox records: that discobox as the
// machine, and a path inside it.
func nestBoxes() []Sandbox {
	created := func(id, name, by string) Sandbox {
		return Sandbox{
			ID: id, Name: name, State: StateRunning, CreatedBy: by,
			OriginKey: "box:" + by + ":/workspace/source", OriginHostID: "host_" + by, OriginHost: by,
			Source: "/workspace/source",
		}
	}
	return []Sandbox{
		created("sbx_grand", "grandchild", "sbx_old"),
		{ID: "sbx_solo", Name: "solo", State: StateRunning, OriginKey: "here:/src/app", OriginHostID: "host_here", Source: "/src/app"},
		created("sbx_new", "newer child", "sbx_lead"),
		created("sbx_old", "older child", "sbx_lead"),
		{ID: "sbx_lead", Name: "lead", State: StateRunning, OriginKey: "here:/src/app", OriginHostID: "host_here", Source: "/src/app"},
	}
}

func rowNames(l *sandboxList) []string {
	var out []string
	for _, s := range l.rows() {
		out = append(out, s.tree+s.Name)
	}
	return out
}

// A discobox a discobox created is drawn under it, in the folder its creator
// is in, though the folder it is filed in itself is a path inside the creator.
func TestADiscoboxIsNestedUnderTheOneThatCreatedIt(t *testing.T) {
	l := listForTest(nestSession, nestBoxes())
	l.folder = nestSession.folder()

	want := []string{"solo", "lead", "├ newer child", "└ older child", "  └ grandchild"}
	if got := rowNames(l); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("rows = %q, want %q", got, want)
	}

	out := ansi.Strip(l.view(newStyles(true), &zones{}, true))
	// The state dot, then the agent's cell, blank for a box whose agent
	// reports nothing, then the branch and the name.
	for _, line := range []string{"●   lead", "●   ├ newer child", "●   └ older child", "●     └ grandchild"} {
		if !strings.Contains(out, line) {
			t.Errorf("view has no %q:\n%s", line, out)
		}
	}
	if strings.Contains(out, "from ") {
		t.Errorf("a nested row says which machine it is from, which is the discobox it is under:\n%s", out)
	}
	if !strings.Contains(out, "5 boxes") {
		t.Errorf("the band does not count the five discoboxes drawn:\n%s", out)
	}
}

// A discobox filed in another folder than its creator is drawn once on every
// folder at once: under its creator, and not a second time in its own folder.
// Only a folder that does not draw its creator lists it as a top.
func TestADiscoboxFiledApartFromItsCreatorIsDrawnOnceUnderIt(t *testing.T) {
	l := listForTest(nestSession, nestBoxes())
	l.folder = everyFolder

	want := []string{"solo", "lead", "├ newer child", "└ older child", "  └ grandchild"}
	if got := rowNames(l); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("rows = %q, want %q", got, want)
	}
	// A nested row is in the section of the row its tree hangs from.
	for _, s := range l.rows() {
		if got := l.sectionOf(s); got != "here:/src/app" {
			t.Fatalf("%s is in section %q, want its creator's", s.Name, got)
		}
	}

	out := l.view(newStyles(false), &zones{}, true)
	if !strings.Contains(out, "5 boxes") {
		t.Errorf("the band does not count the five discoboxes drawn:\n%s", out)
	}
	if strings.Contains(out, "from sbx_lead") {
		t.Errorf("the folder the created discoboxes are filed in has a section of its own:\n%s", out)
	}

	// Its own folder lists it as the server filed it when its creator is not
	// drawn there.
	l.folder = folder{key: "box:sbx_lead:/workspace/source"}
	if got := rowNames(l); strings.Join(got, "|") != "newer child|older child|└ grandchild" {
		t.Fatalf("rows in the created discoboxes' folder = %q", got)
	}
}

// A discobox filed in its creator's own folder is drawn once, under it.
func TestADiscoboxFiledWithItsCreatorIsDrawnOnce(t *testing.T) {
	boxes := []Sandbox{
		{ID: "sbx_child", Name: "child", CreatedBy: "sbx_lead", OriginKey: "here"},
		{ID: "sbx_lead", Name: "lead", OriginKey: "here"},
	}
	for _, f := range []folder{everyFolder, {key: "here"}} {
		l := listForTest(nestSession, boxes)
		l.folder = f
		if got := rowNames(l); strings.Join(got, "|") != "lead|└ child" {
			t.Fatalf("rows in %q = %q", f.key, got)
		}
	}
}

// What the list says there is to show counts what showing it would draw: an
// archived discobox nested under a creator in this folder is one of them.
func TestTheArchivedOfferCountsTheNestedOnes(t *testing.T) {
	boxes := nestBoxes()
	for i := range boxes {
		if boxes[i].ID == "sbx_new" {
			boxes[i].State = StateArchived
		}
	}
	l := listForTest(nestSession, boxes)
	l.folder = nestSession.folder()
	if got := l.archivedCount(); got != 1 {
		t.Fatalf("archivedCount = %d, want 1", got)
	}
	if got := l.count(); got != 4 {
		t.Fatalf("count = %d, want 4", got)
	}
	// A tag only a nested row carries is not offered: its creator lacks it,
	// so choosing it would draw nothing.
	boxes[3].Tags = []string{"ready"}
	l.setAll(boxes)
	if got := l.tagChoices(); len(got) != 0 {
		t.Fatalf("tagChoices = %q", got)
	}
}

// A refreshed listing leaves the cursor on the discobox it was on, nested or
// not.
func TestTheCursorStaysOnANestedDiscobox(t *testing.T) {
	l := listForTest(nestSession, nestBoxes())
	l.folder = everyFolder
	l.moveTo(4) // "grandchild", two levels down
	l.setAll(append([]Sandbox{{ID: "sbx_fresh", Name: "fresh", OriginKey: "here:/src/app", OriginHostID: "host_here"}}, nestBoxes()...))
	if s := l.current(); s == nil || s.ID != "sbx_grand" {
		t.Fatalf("cursor on %v after a refresh, want sbx_grand", s)
	}
}

// A row whose creator is not on screen moves up to the nearest one that is,
// and is drawn in its own folder alone when none is.
func TestARowNestsUnderTheNearestCreatorOnScreen(t *testing.T) {
	boxes := nestBoxes()
	for i := range boxes {
		if boxes[i].ID == "sbx_old" {
			boxes[i].State = StateArchived
		}
	}
	l := listForTest(nestSession, boxes)
	l.folder = nestSession.folder()
	want := []string{"solo", "lead", "├ grandchild", "└ newer child"}
	if got := rowNames(l); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("rows with a creator archived = %q, want %q", got, want)
	}

	// A row nested under nothing says where it is from, creator listed or not.
	l = listForTest(nestSession, boxes[:1])
	l.folder = everyFolder
	l.width = 140
	if got := rowNames(l); len(got) != 1 || got[0] != "grandchild" {
		t.Fatalf("rows with the creator gone = %q", got)
	}
	if out := ansi.Strip(l.view(newStyles(false), &zones{}, true)); !strings.Contains(out, "grandchild from sbx_old") {
		t.Errorf("a row nested under nothing does not say where it is from:\n%s", out)
	}
}

// An ID is one server's: a discobox is not nested under another server's
// discobox that happens to share its creator's ID.
func TestNestingStaysInsideOneServer(t *testing.T) {
	l := listForTest(Session{Servers: []string{"alpha", "beta"}}, []Sandbox{
		{ID: "sbx_child", Name: "child", Server: "beta", CreatedBy: "sbx_lead"},
		{ID: "sbx_lead", Name: "lead", Server: "alpha"},
	})
	// Neither is nested, so both keep the listing's order.
	if got := rowNames(l); strings.Join(got, "|") != "child|lead" {
		t.Fatalf("rows = %q", got)
	}
}

// A listing that holds a loop still lists every row.
func TestALoopOfCreatorsStillListsEveryRow(t *testing.T) {
	l := listForTest(Session{}, []Sandbox{
		{ID: "sbx_a", Name: "a", CreatedBy: "sbx_b"},
		{ID: "sbx_b", Name: "b", CreatedBy: "sbx_a"},
	})
	if got := l.rows(); len(got) != 2 {
		t.Fatalf("rows = %d, want 2", len(got))
	}
}
