package tui

import (
	"slices"
	"strings"
	"testing"
)

// A machine no window has narrowed opens on everything: every server, every
// folder and every tag.
func TestNothingSavedOpensOnEverything(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(onServer("alpha", testSandboxes())...)
	ds.session.Servers = []string{"alpha", "beta"}
	ds.nothingSaved = true
	m := newTestModel(t, ds)

	if m.list.server != "" || m.list.folder.key != "" || len(m.list.tags) != 0 {
		t.Fatalf("view = %+v, want every server, folder and tag", m.currentView())
	}
	if got := len(m.list.rows()); got != 3 {
		t.Fatalf("rows = %d, want every unarchived discobox", got)
	}
	// Opening on the default is no choice anybody made, so nothing is saved.
	send(t, m, tickMsg{})
	if len(ds.views) != 0 {
		t.Fatalf("an untouched header wrote %v", ds.views)
	}
}

// The filters the last window was left on are where the next one opens, and a
// create goes where they say.
func TestASavedViewIsWhereTheWindowOpens(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(onServer("alpha", testSandboxes())...)
	ds.session.Servers = []string{"alpha", "beta"}
	ds.session.View = ListView{
		Server:       "beta",
		FolderKey:    testKey("/src/obot"),
		FolderLabel:  "/src/obot",
		FolderSource: "/src/obot",
		Tags:         []string{"wip"},
	}
	m := newTestModel(t, ds)

	if m.list.server != "beta" || m.list.folder.key != testKey("/src/obot") || !slices.Equal(m.list.tags, []string{"wip"}) {
		t.Fatalf("view = %+v, want the one saved", m.currentView())
	}
	if m.opts.folder != "/src/obot" {
		t.Fatalf("the run options cut from %q, want the saved folder's source", m.opts.folder)
	}
	if got := m.opts.command("fix it"); !strings.Contains(got, "--server beta") {
		t.Fatalf("command = %q, want it on the saved server", got)
	}
}

// A saved server that is no longer registered would list nothing, so the
// window opens on every server instead.
func TestASavedServerThatIsGoneIsEveryServer(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(onServer("alpha", testSandboxes())...)
	ds.session.Servers = []string{"alpha", "beta"}
	ds.session.View = ListView{Server: "gamma"}
	m := newTestModel(t, ds)
	if m.list.server != "" {
		t.Fatalf("server = %q, want every server", m.list.server)
	}
}

// Changing a filter is saved on the listing's clock, once, and on the way out.
func TestChangingAFilterIsSaved(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.nothingSaved = true
	m := newTestModel(t, ds)

	filterTo(t, m, m.session.folder().label)
	if m.list.folder.key != testKey("/src/disco2") {
		t.Fatalf("folder = %q, want the window's own", m.list.folder.label)
	}
	send(t, m, tickMsg{})
	if len(ds.views) != 1 || ds.views[0].FolderKey != testKey("/src/disco2") {
		t.Fatalf("views = %+v, want the folder saved", ds.views)
	}
	send(t, m, tickMsg{})
	if len(ds.views) != 1 {
		t.Fatalf("an unchanged header wrote again: %+v", ds.views)
	}

	// Back to every folder is a change too, and closing the window saves it.
	filterTo(t, m, allFolders)
	send(t, m, keyPress("ctrl+c"))
	if len(ds.views) != 2 || !ds.views[1].IsZero() {
		t.Fatalf("views = %+v, want the default saved on the way out", ds.views)
	}
}

// What the command line named is what the window opens narrowed to: --server
// is the primary. That is where it opens rather than a change made in it, so
// nothing is saved for it, and the folder and tags are left as they were.
func TestTheCommandLineNarrowsTheView(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(onServer("alpha", testSandboxes())...)
	ds.session.Servers = []string{"alpha", "beta"}
	ds.session.ServerChosen = true
	ds.nothingSaved = true
	m := newTestModel(t, ds)

	if m.list.server != "alpha" {
		t.Fatalf("server = %q, want the one --server named", m.list.server)
	}
	if m.list.folder.key != "" || len(m.list.tags) != 0 {
		t.Fatalf("view = %+v, want every folder and tag", m.currentView())
	}
	send(t, m, tickMsg{})
	if len(ds.views) != 0 {
		t.Fatalf("the window saved where the command line opened it: %+v", ds.views)
	}
}

// The view is one for the whole machine, so what the command line named
// outranks it: --server is saying which you mean this time. What was saved is
// not written over unless the filters move.
func TestTheCommandLineOutranksASavedView(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(onServer("alpha", testSandboxes())...)
	ds.session.Servers = []string{"alpha", "beta"}
	ds.session.ServerChosen = true
	ds.session.View = ListView{Server: "beta", Tags: []string{"wip"}}
	m := newTestModel(t, ds)
	if m.list.server != "alpha" || !slices.Equal(m.list.tags, []string{"wip"}) {
		t.Fatalf("view = %+v, want the named server with the saved tags", m.currentView())
	}
	send(t, m, tickMsg{})
	if len(ds.views) != 0 {
		t.Fatalf("the window saved where the command line opened it: %+v", ds.views)
	}
}

// The window's own folder is restored from the session rather than from the
// saved copy, whose branch and ref may since have moved on.
func TestTheOwnFolderIsRestoredFromTheSession(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.session.View = ListView{FolderKey: testKey("/src/disco2"), FolderLabel: "/src/disco2 @ old", FolderSource: "/src/disco2@old"}
	m := newTestModel(t, ds)
	own := ds.session.folder()
	if m.list.folder != own {
		t.Fatalf("folder = %+v, want the session's own %+v", m.list.folder, own)
	}
	if m.opts.folder != own.source {
		t.Fatalf("the run options cut from %q, want %q", m.opts.folder, own.source)
	}
}
