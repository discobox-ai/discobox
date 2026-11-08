package console

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// testTab is a renderer's pane at its smallest: a tab, and whether its view
// was closed.
type testTab struct {
	Tab
	closed bool
}

func (t *testTab) TabState() *Tab { return &t.Tab }
func (t *testTab) Close()         { t.closed = true }

func at(s int) time.Time { return time.Date(2026, 11, 8, 0, 0, s, 0, time.UTC) }

func tabIDs(tabs []*testTab) []string {
	out := make([]string, 0, len(tabs))
	for _, t := range tabs {
		out = append(out, t.ExecID)
	}
	return out
}

func execIDs(execs []Exec) []string {
	out := make([]string, 0, len(execs))
	for _, e := range execs {
		out = append(out, e.ID)
	}
	return out
}

func TestExecSide(t *testing.T) {
	for _, tc := range []struct {
		exec Exec
		want Side
	}{
		{Exec{ID: ExecPrimary, Primary: true}, SideTerminals},
		{Exec{ID: "t", Harness: "claude"}, SideTerminals},
		{Exec{ID: "s", Service: "web"}, SideTerminals},
		{Exec{ID: "sh"}, SideShells},
		{Exec{ID: "d", Tool: "diff"}, SideShells},
		// A tool is a tool whatever launched it.
		{Exec{ID: "d", Tool: "diff", Harness: "claude"}, SideShells},
	} {
		if got := tc.exec.Side(); got != tc.want {
			t.Errorf("%+v: side %v, want %v", tc.exec, got, tc.want)
		}
	}
}

// Services lead in declaration order, tools trail, and the rest go oldest
// first with the id breaking ties.
func TestSessionsOrderAndSelection(t *testing.T) {
	got := Sessions([]Exec{
		{ID: "tool", Tool: "diff", Live: true, Tty: true, CreatedAt: at(0)},
		{ID: "b", Live: true, Tty: true, CreatedAt: at(2)},
		{ID: "a", Live: true, Tty: true, CreatedAt: at(2)},
		{ID: "old", Harness: "claude", Live: true, Tty: true, CreatedAt: at(1)},
		{ID: "p", Primary: true, Live: true, Tty: true},
		{ID: "svc", Service: "web", Live: true, Tty: true},
		{ID: "files", FilesChanged: true, Live: true, Tty: true},
		{ID: "dead", Tty: true},
		{ID: "pipe", Live: true},
		{Live: true, Tty: true},
	})
	if want := []string{"old", "a", "b", "tool"}; !reflect.DeepEqual(execIDs(got), want) {
		t.Fatalf("sessions %v, want %v", execIDs(got), want)
	}

	services := []Exec{{ID: "2", Service: "b", ServiceOrder: 1, CreatedAt: at(0)}, {ID: "1", Service: "a", ServiceOrder: 0, CreatedAt: at(9)}, {ID: "t", Harness: "x"}}
	if !services[1].Before(services[0]) || !services[0].Before(services[2]) {
		t.Fatal("services must lead in declaration order, whenever they started")
	}
}

func TestExecTitle(t *testing.T) {
	for _, tc := range []struct {
		exec Exec
		want string
	}{
		{Exec{ID: "x", Service: "web", ServiceName: " Web server "}, "Web server"},
		{Exec{ID: "x", Service: "web", Command: []string{"/bin/sh"}}, "web"},
		{Exec{ID: "x", Harness: "claude", Command: []string{"/usr/bin/claude", "--x"}}, "claude"},
		{Exec{ID: "x", Harness: "codex", Command: []string{"/"}}, "codex"},
		{Exec{ID: "exec_abcdefgh"}, "cdefgh"},
	} {
		if got := tc.exec.Title(); got != tc.want {
			t.Errorf("%+v: title %q, want %q", tc.exec, got, tc.want)
		}
	}
}

func TestSessionTab(t *testing.T) {
	for _, tc := range []struct {
		name   string
		exec   Exec
		asked  Interaction
		action Interaction
		side   Side
		title  string
	}{
		{"primary", Exec{ID: ExecPrimary, Primary: true}, "", InteractAttach, SideTerminals, ""},
		{"harness", Exec{ID: "t", Harness: "claude"}, "", InteractTerminal, SideTerminals, "claude"},
		{"shell", Exec{ID: "s", Command: []string{"zsh"}}, "", InteractShell, SideShells, "zsh"},
		{"asked terminal", Exec{ID: "t", Command: []string{"zsh"}}, InteractTerminal, InteractTerminal, SideTerminals, "zsh"},
		{"failed create", Exec{}, InteractTerminal, InteractTerminal, SideTerminals, ""},
		{"service", Exec{ID: "v", Service: "web"}, "", InteractService, SideTerminals, "web"},
		{"tool", Exec{ID: "d", Tool: "diff"}, "", "diff", SideShells, "diff"},
	} {
		tab := SessionTab(tc.exec, tc.asked)
		if tab.Action != tc.action || tab.Side() != tc.side || tab.Title != tc.title {
			t.Errorf("%s: %+v (side %v), want action %q side %v title %q", tc.name, tab, tab.Side(), tc.action, tc.side, tc.title)
		}
	}
	if tab := SessionTab(Exec{ID: "s"}, ""); tab.Name("vim") != "vim" || tab.Name(" ") != "s" {
		t.Errorf("a tab is called what its program says, else its title: %q", tab.Name(" "))
	}
}

func TestTabEnd(t *testing.T) {
	broke := errors.New("stream lost")
	for _, tc := range []struct {
		name string
		tab  Tab
		err  error
		want TabEnd
	}{
		{"primary", Tab{Primary: true}, nil, EndsWorkspace},
		{"primary broke", Tab{Primary: true}, broke, EndsWorkspace},
		{"tool quit", Tab{Tool: "diff"}, nil, EndsTab},
		{"shell broke", Tab{ExecID: "s"}, broke, EndsTab},
		{"shell finished", Tab{ExecID: "s"}, nil, EndsHeld},
	} {
		tab := tc.tab
		if got := tab.End(tc.err); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
		if held := tc.want == EndsHeld; tab.Exited != held || (held && tab.Status != "finished") {
			t.Errorf("%s: exited %v status %q", tc.name, tab.Exited, tab.Status)
		}
	}
}

// An arrival takes its place by order and leaves the keys on the tab being
// worked in; numbers count terminals and shells across the screen and
// services within their column.
func TestColumnInsertAndNumber(t *testing.T) {
	var c Column[*testTab]
	shell := &testTab{Tab: Tab{ExecID: "late", Created: at(5)}}
	c.Insert(shell, true)
	early := &testTab{Tab: Tab{ExecID: "early", Created: at(1)}}
	if got := c.Insert(early, true); got != 0 {
		t.Fatalf("early landed at %d", got)
	}
	if c.Visible() != shell {
		t.Fatal("an arrival moved the keys off the tab being worked in")
	}
	svc := &testTab{Tab: Tab{ExecID: ServiceTabID("web"), Service: "web"}}
	c.Insert(svc, false)
	if !reflect.DeepEqual(tabIDs(c.Tabs), []string{"service:web", "early", "late"}) {
		t.Fatalf("order %v", tabIDs(c.Tabs))
	}
	for i, want := range []struct {
		n       int
		service bool
	}{{1, true}, {3, false}, {4, false}} {
		if n, service := c.Number(i, 3); n != want.n || service != want.service {
			t.Errorf("tab %d: %d %v, want %d %v", i, n, service, want.n, want.service)
		}
	}
	c.Close(0)
	if !svc.closed || c.Len() != 2 || c.ByExec("service:web") != nil {
		t.Fatal("close must close the view and take the tab out")
	}
}

func newWorkspace(tabs ...*testTab) *Workspace[*testTab] {
	w := &Workspace[*testTab]{}
	w.Open(Sandbox{ID: "sbx"}, 7)
	for _, t := range tabs {
		w.Column(t.Side()).Insert(t, false)
	}
	return w
}

func TestReconcile(t *testing.T) {
	never := func(Exec) bool { return false }
	live := func(id string, more Exec) Exec {
		more.ID, more.Live, more.Tty = id, true, true
		return more
	}
	listing := []Exec{
		live("held", Exec{CreatedAt: at(1)}),
		live("ending", Exec{CreatedAt: at(2)}),
		live("opening", Exec{CreatedAt: at(3)}),
		live("term", Exec{Harness: "claude", CreatedAt: at(5)}),
		live("sh", Exec{CreatedAt: at(4)}),
		live("diff", Exec{Tool: "diff"}),
		live("review", Exec{Tool: "review"}),
	}

	w := newWorkspace(&testTab{Tab: Tab{ExecID: "held"}}, &testTab{Tab: Tab{ExecID: "d2", Tool: "review"}})
	w.Ending["ending"] = true
	o := w.Reconcile(listing, func(e Exec) bool { return e.ID == "opening" })
	if !o.Primary {
		t.Error("a workspace with no primary opens one")
	}
	if want := []string{"sh", "term"}; !reflect.DeepEqual(execIDs(o.Sessions), want) {
		t.Errorf("sessions %v, want %v", execIDs(o.Sessions), want)
	}
	// review is already held under another session: a tool is one window.
	if want := []string{"diff"}; !reflect.DeepEqual(execIDs(o.Tools), want) {
		t.Errorf("tools %v, want %v", execIDs(o.Tools), want)
	}
	if o.NewShells != 2 {
		t.Errorf("new shells %d, want the shell and the tool", o.NewShells)
	}

	w.Terminals.Insert(&testTab{Tab: Tab{ExecID: ExecPrimary, Primary: true}}, false)
	if o := w.Reconcile(nil, never); o.Primary || len(o.Sessions) != 0 {
		t.Errorf("a failed listing opens nothing beside a held primary: %+v", o)
	}
	if o := (newWorkspace()).Reconcile(nil, func(e Exec) bool { return e.ID == ExecPrimary }); o.Primary {
		t.Error("a primary already opening is not opened twice")
	}
}

func TestReconcileServices(t *testing.T) {
	running := func(id, exec string) Service {
		return Service{ID: id, Status: "running", ExecID: exec}
	}
	web := &testTab{Tab: ServiceTab(running("web", "run1"), 0)}
	db := &testTab{Tab: Tab{ExecID: ServiceTabID("db"), Service: "db", ServiceRun: "x"}}
	gone := &testTab{Tab: Tab{ExecID: ServiceTabID("gone"), Service: "gone"}}
	w := newWorkspace(web, db, gone)

	stopped := Service{ID: "db", Status: "stopped"}
	services := []Service{running("web", "run1"), stopped, running("api", "e2"), running("busy", "e3")}
	open, closing := w.ReconcileServices(services, func(id string) bool { return id == ServiceTabID("busy") })

	var opened []string
	for _, o := range open {
		opened = append(opened, o.Service.ID)
	}
	if !reflect.DeepEqual(opened, []string{"api"}) || open[0].Order != 2 {
		t.Errorf("opened %v, want api at its place in the listing", open)
	}
	if want := []string{"service:db", "service:gone"}; !reflect.DeepEqual(tabIDs(closing), want) {
		t.Errorf("closing %v, want %v", tabIDs(closing), want)
	}
	moved := []Service{running("web", "run2")}
	if open, closing := w.ReconcileServices(moved, func(string) bool { return false }); len(open) != 1 || len(closing) == 0 || closing[0] != web {
		t.Errorf("a service whose run moved on is reopened: open %v closing %v", open, tabIDs(closing))
	}
}

func TestPushedHolds(t *testing.T) {
	w := newWorkspace()
	refused := errors.New("lease moved")
	moved, failed := w.Pushed([]SourcePush{
		{Slug: "a", Commit: "c1", Err: refused},
		{Slug: "b", Commit: "c2", Pushed: true, Branch: "main"},
	})
	if len(moved) != 1 || len(failed) != 1 || w.PushHeld["a"] != "c1" {
		t.Fatalf("moved %v failed %v held %v", moved, failed, w.PushHeld)
	}
	if got := PushedText(moved); got != "pushed c2 to origin/main in the discobox" {
		t.Errorf("pushed text %q", got)
	}
	if got := PushFailure(failed); got != "lease moved" {
		t.Errorf("failure %q", got)
	}
	w.Pushed([]SourcePush{{Slug: "a", UpToDate: true}})
	if _, held := w.PushHeld["a"]; held {
		t.Error("a source already in the origin lets its hold go")
	}
}

type testForward struct{ closed bool }

func (f *testForward) Bindings() []Binding     { return nil }
func (f *testForward) Events() <-chan struct{} { return nil }
func (f *testForward) Close() error            { f.closed = true; return nil }

func TestParkAndRelease(t *testing.T) {
	w := newWorkspace(&testTab{Tab: Tab{ExecID: "sh"}})
	if w.Parkable() {
		t.Fatal("a workspace without its primary is closed, not parked")
	}
	primary := &testTab{Tab: Tab{ExecID: ExecPrimary, Primary: true}}
	w.Terminals.Insert(primary, false)
	f := &testForward{}
	w.Forward = f
	if !w.Parkable() {
		t.Fatal("a workspace with its primary parks")
	}
	w.Park()
	if !f.closed || w.Forward != nil || primary.closed {
		t.Fatal("parking closes the forward and keeps the tabs attached")
	}
	w.Release()
	if !primary.closed || len(w.Attached()) != 0 {
		t.Fatal("release closes every tab's view")
	}
}

func TestTabBaseSkipsServices(t *testing.T) {
	w := newWorkspace(
		&testTab{Tab: Tab{ExecID: ServiceTabID("web"), Service: "web"}},
		&testTab{Tab: Tab{ExecID: ExecPrimary, Primary: true}},
		&testTab{Tab: Tab{ExecID: "t", Action: InteractTerminal, Created: at(1)}},
		&testTab{Tab: Tab{ExecID: "sh", Created: at(2)}},
	)
	if got := w.TabBase(SideShells); got != 2 {
		t.Errorf("shells start at %d, want 2", got)
	}
	if want := []string{ExecPrimary, "t", "sh"}; !reflect.DeepEqual(tabIDs(w.Numbered()), want) {
		t.Errorf("numbered %v, want %v", tabIDs(w.Numbered()), want)
	}
	if want := []string{"service:web"}; !reflect.DeepEqual(tabIDs(w.ServiceTabs()), want) {
		t.Errorf("services %v", tabIDs(w.ServiceTabs()))
	}
}

// A session whose record names a harness is a terminal, whatever it was asked
// for as.
func TestSessionTabHarnessIsATerminal(t *testing.T) {
	tab := SessionTab(Exec{ID: "s", Harness: "claude"}, InteractShell)
	if tab.Action != InteractShell || tab.Side() != SideTerminals {
		t.Fatalf("%+v on side %v, want a shell-asked harness session on the terminals", tab, tab.Side())
	}
}

type pushSource struct {
	DataSource
	held map[string]string
	err  error
}

func (p *pushSource) PushSources(_ context.Context, _ string, held map[string]string) ([]SourcePush, error) {
	p.held = held
	return []SourcePush{{Slug: "a", Pushed: true}}, p.err
}

func TestPushLook(t *testing.T) {
	w := newWorkspace()
	ds := &pushSource{}
	if w.PushLook(Sandbox{ID: "sbx"}, ds) != nil {
		t.Fatal("a box this window does not push gets no look")
	}
	w.PushHeld["a"] = "c1"
	look := w.PushLook(Sandbox{ID: "sbx", Pushable: true}, ds)
	w.PushHeld["a"] = "c2"
	if got := look(context.Background()); len(got) != 1 || ds.held["a"] != "c1" {
		t.Fatalf("got %v with holds %v, want the holds as they stood when the look was made", got, ds.held)
	}
	ds.err = errors.New("unreachable")
	if got := w.PushLook(Sandbox{ID: "sbx", Pushable: true}, ds)(context.Background()); got != nil {
		t.Fatalf("a whole-call failure answers nothing, got %v", got)
	}
}
