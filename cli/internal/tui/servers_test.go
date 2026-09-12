package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// The Server row is there only when there is a server to choose, and the
// primary leads it so an untouched panel creates where it always did
// (ADR 0113 §5).
func TestRunOptionsOfferAServerOnlyWhenThereIsAChoice(t *testing.T) {
	if single := newOptions(Session{Directory: "/work"}); single.server() != nil {
		t.Fatal("the run options offer a server with only the primary to create on")
	}

	opts := newOptions(Session{Directory: "/work", Servers: []string{"alpha", "beta"}})
	row := opts.server()
	if row == nil {
		t.Fatal("the run options offer no server with two to choose from")
	}
	if got := row.display(); got != "alpha (primary)" {
		t.Fatalf("Server row = %q, want the primary, saying so", got)
	}
	if req := opts.request("fix it"); req.Server != "" {
		t.Fatalf("untouched request Server = %q, want the primary", req.Server)
	}
	if strings.Contains(opts.command("fix it"), "--server") {
		t.Fatalf("command = %q, want no --server for the primary", opts.command("fix it"))
	}

	row.cycle(1)
	if req := opts.request("fix it"); req.Server != "beta" {
		t.Fatalf("request Server = %q, want beta", req.Server)
	}
	if got := opts.command("fix it"); !strings.Contains(got, "--server beta") {
		t.Fatalf("command = %q, want --server beta", got)
	}
}

// A server that stops answering is said once, when it stops, and again only
// once it has answered in between.
func TestUnreachableServersAreReportedWhenThatChanges(t *testing.T) {
	m := &Model{}
	if m.reportUnreachable([]string{"beta"}) == nil {
		t.Fatal("a server that stopped answering was not reported")
	}
	if m.reportUnreachable([]string{"beta"}) != nil {
		t.Fatal("a server still not answering was reported again")
	}
	if m.reportUnreachable(nil) != nil {
		t.Fatal("every server answering was reported")
	}
	if m.reportUnreachable([]string{"beta"}) == nil {
		t.Fatal("a server that stopped answering again was not reported")
	}
}

// The list is one section per server, in the order the session names them,
// with the primary's first (ADR 0113 §4).
func TestTheListIsOneSectionPerServer(t *testing.T) {
	l := listForTest(Session{Servers: []string{"alpha", "beta"}}, []Sandbox{
		{ID: "sbx_b1", Name: "three", Server: "beta", State: StateRunning},
		{ID: "sbx_a1", Name: "one", Server: "alpha", State: StateRunning},
		{ID: "sbx_a2", Name: "two", Server: "alpha", State: StateRunning},
	})
	l.setUnreachable([]string{"gamma"})

	out := l.view(newStyles(false), &zones{}, true)
	var order []int
	for _, text := range []string{"alpha", "one", "two", "beta", "three", "gamma"} {
		i := strings.Index(out, text)
		if i < 0 {
			t.Fatalf("view is missing %q:\n%s", text, out)
		}
		order = append(order, i)
	}
	for i := 1; i < len(order); i++ {
		if order[i] < order[i-1] {
			t.Fatalf("sections and rows are out of order:\n%s", out)
		}
	}
	// A server that did not answer is a section with no rows, so the
	// discoboxes missing from the listing say why.
	if !strings.Contains(out, "not answering") {
		t.Fatalf("view does not say gamma is not answering:\n%s", out)
	}
	// The rows themselves are grouped, which is what the cursor moves through.
	if rows := l.rows(); rows[0].Server != "alpha" || rows[1].Server != "alpha" || rows[2].Server != "beta" {
		t.Fatalf("rows = %+v, want alpha's first", rows)
	}
}

// With one server there is nothing to tell apart, and a header over every row
// would say it anyway.
func TestTheListHasNoSectionsWithOneServer(t *testing.T) {
	l := listForTest(Session{}, []Sandbox{{ID: "sbx_a1", Name: "one", State: StateRunning}})
	if l.grouped() {
		t.Fatal("the list is grouped with one server")
	}
	out := l.view(newStyles(false), &zones{}, true)
	if strings.Contains(out, "not answering") || l.drawn.rows != nil {
		t.Fatalf("the list drew sections with one server:\n%s", out)
	}
}

// A header is a label: the mouse walks past it onto the list, which is what
// the row map the draw records says (zones.go).
func TestSectionHeadersAreNotRows(t *testing.T) {
	l := listForTest(Session{Servers: []string{"alpha", "beta"}}, []Sandbox{
		{ID: "sbx_a1", Name: "one", Server: "alpha", State: StateRunning},
		{ID: "sbx_b1", Name: "two", Server: "beta", State: StateRunning},
	})
	l.view(newStyles(false), &zones{}, true)
	want := []int{-1, 0, -1, 1}
	if !slices.Equal(l.drawn.rows, want) {
		t.Fatalf("drawn rows = %v, want %v — a header is no row", l.drawn.rows, want)
	}
}

// The cursor stays on screen in a window too short for every row and its
// headers: what a row costs is known where it is drawn.
func TestTheCursorStaysOnScreenAmongSections(t *testing.T) {
	boxes := make([]Sandbox, 0, 6)
	for i, server := range []string{"alpha", "alpha", "alpha", "beta", "beta", "beta"} {
		boxes = append(boxes, Sandbox{ID: fmt.Sprintf("sbx_%d", i), Name: fmt.Sprintf("box%d", i), Server: server, State: StateRunning})
	}
	l := listForTest(Session{Servers: []string{"alpha", "beta"}}, boxes)
	l.height = 6
	l.moveTo(len(boxes) - 1)

	l.view(newStyles(false), &zones{}, true)
	if !slices.Contains(l.drawn.rows, l.cursor) {
		t.Fatalf("the cursor row %d is not among the drawn rows %v", l.cursor, l.drawn.rows)
	}
}

func listForTest(session Session, boxes []Sandbox) *sandboxList {
	l := newSandboxList(session)
	l.width, l.height = 100, 20
	l.now = func() time.Time { return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC) }
	l.setAll(boxes)
	return l
}

// The listing and the session arrive on their own schedules, so a server that
// did not answer is drawn whether or not the window knows yet how many servers
// there are — otherwise its discoboxes go missing with nothing saying why, and
// a primary with none of its own draws an entirely blank list.
func TestUnreachableSectionsDrawBeforeTheSessionArrives(t *testing.T) {
	l := listForTest(Session{}, nil)
	l.setUnreachable([]string{"beta"})
	if l.grouped() {
		t.Fatal("the list is grouped before the session says there is more than one server")
	}

	out := l.view(newStyles(false), &zones{}, true)
	if !strings.Contains(out, "beta") || !strings.Contains(out, "not answering") {
		t.Fatalf("view does not say beta is not answering:\n%s", out)
	}
	if l.drawn.rows == nil {
		t.Fatal("a body with sections in it was marked as one line per row")
	}
}

// --project names a project on the primary, and a run on another server goes to
// that server's default: the line offered for copying has to be the run Enter
// would make.
func TestRunOptionsPreviewDropsTheProjectOnAnotherServer(t *testing.T) {
	opts := newOptions(Session{
		Directory:      "/work",
		Project:        "proj_only_on_the_primary",
		DefaultProject: "default",
		Servers:        []string{"alpha", "beta"},
	})
	if got := opts.command("fix it"); !strings.Contains(got, "--project proj_only_on_the_primary") {
		t.Fatalf("command = %q, want the project named for a run on the primary", got)
	}

	opts.server().cycle(1)
	got := opts.command("fix it")
	if !strings.Contains(got, "--server beta") {
		t.Fatalf("command = %q, want --server beta", got)
	}
	if strings.Contains(got, "--project") {
		t.Fatalf("command = %q, want no --project for a run on another server", got)
	}
}
