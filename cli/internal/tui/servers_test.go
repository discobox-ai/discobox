package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// The Server row is there only when there is a server to choose, and the
// primary leads it so an untouched panel creates where it always did
// (ADR 0116 §5).
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
	if req := opts.request("fix it", opts.opts[optSource].selected()); req.Server != "" {
		t.Fatalf("untouched request Server = %q, want the primary", req.Server)
	}
	if strings.Contains(opts.command("fix it"), "--server") {
		t.Fatalf("command = %q, want no --server for the primary", opts.command("fix it"))
	}

	row.cycle(1)
	if req := opts.request("fix it", opts.opts[optSource].selected()); req.Server != "beta" {
		t.Fatalf("request Server = %q, want beta", req.Server)
	}
	if got := opts.command("fix it"); !strings.Contains(got, "--server beta") {
		t.Fatalf("command = %q, want --server beta", got)
	}
}

// The window asks for one listing at a time — a tick that comes round while
// the last one is still out asks for nothing — and a refresh asked for while
// one was out is asked for again as soon as that one lands, so the read an
// action wanted is not the one that gets dropped.
func TestTheWindowAsksForOneListingAtATime(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	if m.refresh() == nil {
		t.Fatal("the first refresh did not go out")
	}
	if m.refresh() != nil {
		t.Fatal("a second refresh went out while the first was still out")
	}

	m.update(listLoadedMsg{})
	if m.listPoll.since.IsZero() {
		t.Fatal("the refresh asked for while one was out was dropped")
	}

	// And with nothing waiting behind it, a listing that lands leaves none out.
	m.update(listLoadedMsg{})
	if !m.listPoll.since.IsZero() {
		t.Fatal("a listing landed with nothing waiting and left a refresh out")
	}
}

// The inbox follows the same rule, and for the same reason: an approval
// re-reads it to take the request it just answered out, and that read must not
// be the one dropped because the beat's was in flight.
func TestAnInboxReadAskedForWhileOneIsOutIsNotLost(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	if m.loadCredentialRequests() == nil {
		t.Fatal("the first inbox read did not go out")
	}
	if m.loadCredentialRequests() != nil {
		t.Fatal("a second inbox read went out while the first was still out")
	}
	if m.update(credentialsLoadedMsg{}) == nil {
		t.Fatal("the inbox read asked for while one was out was dropped")
	}
}

// A primary that is not answering is an error that stays on screen, not a note
// said once: everything else the window does is the primary's, and each of
// those failures is about to be reported on its own.
func TestADeadPrimaryIsReportedAsAnError(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	m.session.Servers = []string{"alpha", "beta"}

	m.update(m.reportUnreachable([]string{"beta"})())
	if m.statusE {
		t.Fatalf("a registered server that is down was reported as an error: %q", m.status)
	}

	m.update(m.reportUnreachable([]string{"alpha", "beta"})())
	if !m.statusE {
		t.Fatalf("a primary that is down was reported as a note: %q", m.status)
	}
}

// A window opened against a primary that is already down hears about it before
// the session has said which server the primary is — a refused connection
// comes back while the session is still asking git what branch this is. The
// level is revisited when that changes, or the one report that matters would
// be the one made before anything knew.
func TestADeadPrimaryBecomesAnErrorOnceTheSessionNamesIt(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	m.update(m.reportUnreachable([]string{"alpha"})())
	if m.statusE {
		t.Fatalf("a server was called the primary before the session named one: %q", m.status)
	}

	m.session.Servers = []string{"alpha", "beta"}
	cmd := m.reportUnreachable([]string{"alpha"})
	if cmd == nil {
		t.Fatal("the same servers at a new level were not reported again")
	}
	m.update(cmd())
	if !m.statusE {
		t.Fatalf("a primary that was down all along stayed a note: %q", m.status)
	}

	// And still said once: nothing has changed now.
	if m.reportUnreachable([]string{"alpha"}) != nil {
		t.Fatal("the same report at the same level was made twice")
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

// Servers are not sections: every server's discoboxes are one list, newest
// first as the listing gave them, and which server one is on is the header's
// filter to answer. A server that did not answer is still said.
func TestServersAreNotSections(t *testing.T) {
	l := listForTest(Session{Servers: []string{"alpha", "beta"}}, []Sandbox{
		{ID: "sbx_b1", Name: "three", Server: "beta", State: StateRunning, OriginKey: testKey("/src/disco2")},
		{ID: "sbx_a1", Name: "one", Server: "alpha", State: StateRunning, OriginKey: testKey("/src/disco2")},
		{ID: "sbx_a2", Name: "two", Server: "alpha", State: StateRunning, OriginKey: testKey("/src/disco2")},
	})
	l.folder = folder{key: testKey("/src/disco2"), label: "/src/disco2"}
	if l.grouped() {
		t.Fatal("one folder on two servers is drawn in sections")
	}
	out := l.view(newStyles(false), &zones{}, true)
	if strings.Contains(out, "server alpha") || strings.Contains(out, "server beta") {
		t.Fatalf("the list names a server over its rows:\n%s", out)
	}
	if rows := l.rows(); rows[0].Name != "three" || rows[1].Name != "one" || rows[2].Name != "two" {
		t.Fatalf("rows = %+v, want the listing's own order", rows)
	}

	// A server that did not answer is a section with no rows, so the
	// discoboxes missing from the listing say why.
	l.setUnreachable([]string{"gamma"})
	if out = l.view(newStyles(false), &zones{}, true); !strings.Contains(out, "server gamma") || !strings.Contains(out, "not answering") {
		t.Fatalf("view does not say gamma is not answering:\n%s", out)
	}
}

// With one server there is nothing to tell apart, and a header over every row
// would say it anyway. The list is on one folder, so no other dimension
// sections it either.
func TestTheListHasNoSectionsWithOneServer(t *testing.T) {
	l := listForTest(Session{}, []Sandbox{{ID: "sbx_a1", Name: "one", State: StateRunning, OriginKey: testKey("/src/disco2")}})
	l.folder = folder{key: testKey("/src/disco2"), label: "/src/disco2"}
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
	l := listForTest(Session{}, []Sandbox{
		{ID: "sbx_a1", Name: "one", State: StateRunning, OriginKey: testKey("/src/disco2"), Source: "/src/disco2"},
		{ID: "sbx_b1", Name: "two", State: StateRunning, OriginKey: testKey("/src/obot"), Source: "/src/obot"},
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
	for i, dir := range []string{"/src/a", "/src/a", "/src/a", "/src/b", "/src/b", "/src/b"} {
		boxes = append(boxes, Sandbox{ID: fmt.Sprintf("sbx_%d", i), Name: fmt.Sprintf("box%d", i), OriginKey: testKey(dir), Source: dir, State: StateRunning})
	}
	l := listForTest(Session{}, boxes)
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
	out := l.view(newStyles(false), &zones{}, true)
	if !strings.Contains(out, "beta") || !strings.Contains(out, "not answering") {
		t.Fatalf("view does not say beta is not answering:\n%s", out)
	}
	if l.drawn.rows == nil {
		t.Fatal("a body with sections in it was marked as one line per row")
	}
}

// A server that has not answered yet is slow, not missing, and the list says
// that where its rows would go — while every other server's rows are drawn.
func TestAServerStillBeingAskedSaysSoWhereItsRowsGo(t *testing.T) {
	l := listForTest(Session{Servers: []string{"alpha", "beta"}}, []Sandbox{
		{ID: "sbx_a1", Name: "one", Server: "alpha", State: StateRunning},
	})
	l.setWaiting([]string{"beta"})

	out := l.view(newStyles(false), &zones{}, true)
	if !strings.Contains(out, "one") {
		t.Fatalf("the server that answered is not listed:\n%s", out)
	}
	if !strings.Contains(out, "beta") || !strings.Contains(out, "still listing") {
		t.Fatalf("view does not say beta is still being listed:\n%s", out)
	}
	if strings.Contains(out, "not answering") {
		t.Fatalf("a server that is still being asked was called unreachable:\n%s", out)
	}
	if l.drawn.rows == nil {
		t.Fatal("a body with sections in it was marked as one line per row")
	}
}

// The band says a listing is slow only while one is: an indicator that is
// always on is one nobody reads.
func TestTheBandSaysAListingIsSlowOnlyWhileItIs(t *testing.T) {
	l := listForTest(Session{}, []Sandbox{{ID: "sbx_a1", Name: "one", State: StateRunning}})
	if out := l.view(newStyles(false), &zones{}, true); strings.Contains(out, "still listing") {
		t.Fatalf("the band says a listing is slow when none is:\n%s", out)
	}
	l.slow = true
	out := l.view(newStyles(false), &zones{}, true)
	if !strings.Contains(out, "still listing") {
		t.Fatalf("the band does not say the listing is slow:\n%s", out)
	}

	// A refresh being out is not what decides the invitation, though: a
	// project that has answered "none" says so whether or not the next
	// listing is late, or the one screen a new user reads would blink at them
	// every time a poll ran long.
	empty := listForTest(Session{}, nil)
	empty.slow = true
	if out := empty.view(newStyles(false), &zones{}, true); !strings.Contains(out, "no discoboxes here yet") {
		t.Fatalf("a project known to be empty stopped saying so while a refresh was out:\n%s", out)
	}
}

// Until a listing lands there is no answer about the project, so the list does
// not offer to create the first discobox — that offer is an answer.
func TestTheInvitationWaitsForTheFirstListing(t *testing.T) {
	fresh := newSandboxList(Session{})
	fresh.width, fresh.height = 100, 20
	fresh.now = func() time.Time { return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC) }
	if out := fresh.view(newStyles(false), &zones{}, true); strings.Contains(out, "no discoboxes here yet") {
		t.Fatalf("a list with no listing yet was reported as an empty project:\n%s", out)
	}

	fresh.setAll(nil)
	if out := fresh.view(newStyles(false), &zones{}, true); !strings.Contains(out, "no discoboxes here yet") {
		t.Fatalf("a listing that came back empty does not offer the first discobox:\n%s", out)
	}
}

// The window says a refresh is slow only once it has been out longer than the
// wait, and stops the moment a listing lands. What decides is how long the
// refresh that is out has been out: nothing ties a timer to the refresh that
// armed it, so one left over from a refresh that landed in milliseconds must
// not pass judgement on the one that is out when it goes off.
func TestTheWindowSaysARefreshIsSlowAfterTheWait(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	m.update(listingSlowMsg{})
	if m.list.slow {
		t.Fatal("the window called a listing slow with no refresh out")
	}

	// A refresh that went out a moment ago, with a stale timer going off over
	// it: the refresh key pressed just before the beat's timer comes due.
	m.refresh()
	m.update(listingSlowMsg{})
	if m.list.slow {
		t.Fatalf("a refresh out for no time was called late by a stale timer")
	}

	// And the same refresh, once it really has been out that long.
	m.listPoll.since = m.now().Add(-listingSlowAfter)
	m.update(listingSlowMsg{})
	if !m.list.slow {
		t.Fatal("a refresh still out after the wait was not reported")
	}

	m.update(listLoadedMsg{})
	if m.list.slow || !m.listPoll.since.IsZero() {
		t.Fatal("a listing that landed left the window saying it was still listing")
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

// A server that did not answer is said so however long the list is. Its
// section is reserved out of the window before the rows are drawn, because
// nothing scrolls to it — offset and cursor walk rows — so a list longer than
// the window, which is the list the launcher is for, would otherwise never say
// a server was missing at all.
func TestTheNotAnsweringSectionSurvivesAFullList(t *testing.T) {
	boxes := make([]Sandbox, 40)
	for i := range boxes {
		boxes[i] = Sandbox{
			ID:    fmt.Sprintf("sbx_%02d", i),
			Name:  fmt.Sprintf("box-%02d", i),
			State: StateRunning,
		}
	}
	l := listForTest(Session{}, boxes)
	l.setUnreachable([]string{"beta"})

	out := l.view(newStyles(false), &zones{}, true)
	if !strings.Contains(out, "not answering") {
		t.Fatalf("a list that fills the window dropped the section saying beta is missing:\n%s", out)
	}

	// And at the bottom, where the rows have had every chance to take the room.
	l.cursor = len(boxes) - 1
	l.clamp()
	out = l.view(newStyles(false), &zones{}, true)
	if !strings.Contains(out, "not answering") {
		t.Fatalf("scrolled to the end of the list, the section is gone:\n%s", out)
	}
}

// The window opens on every server, which is the listing ADR 0116 §4 describes:
// seeing them side by side is the point of registering them. The filter is
// there to narrow that down, not to hide it until asked.
func TestTheWindowOpensOnEveryServer(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(
		Sandbox{ID: "sbx_a1", Name: "alpha-box", Server: "alpha", State: StateRunning, OriginKey: testKey("/src/disco2")},
		Sandbox{ID: "sbx_b1", Name: "beta-box", Server: "beta", State: StateRunning, OriginKey: testKey("/src/disco2")},
	)
	ds.session.Servers = []string{"alpha", "beta"}
	m := newTestModel(t, ds)

	if m.list.server != "" {
		t.Fatalf("the window opened on %q, want every server", m.list.server)
	}
	if rows := m.list.rows(); len(rows) != 2 {
		t.Fatalf("rows = %+v, want both servers'", rows)
	}
	out := plainFrame(m)
	if strings.Contains(headerLine(m), "server ") {
		t.Fatalf("the header names a server while it is showing every one: %q", headerLine(m))
	}
	// Both servers' rows are one list: which server a row is on is the
	// filter's to answer, not a band's.
	if m.list.grouped() || strings.Contains(out, "server alpha") {
		t.Fatalf("the list is sectioned by server:\n%s", out)
	}

	// And narrowing to one is a choice on the card.
	filterTo(t, m, "alpha")
	if m.list.server != "alpha" {
		t.Fatalf("the list is showing %q, want alpha", m.list.server)
	}
	if m.list.grouped() {
		t.Fatal("the list is sectioned by server while it is showing one")
	}
	out = plainFrame(m)
	if !strings.Contains(out, "server alpha") {
		t.Fatalf("the header does not name the server:\n%s", out)
	}
	if !strings.Contains(out, "alpha-box") || strings.Contains(out, "beta-box") {
		t.Fatalf("the list is not alpha's discoboxes alone:\n%s", out)
	}
}

// The server on screen is the server the prompt creates on: the header and the
// run options' Server row are one control, so what Enter would do is what the
// window says.
func TestTheHeaderServerIsWhereThePromptRuns(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(onServer("alpha", testSandboxes())...)
	ds.session.Servers = []string{"alpha", "beta"}
	m := newTestModel(t, ds)

	// Up out of the list reaches the filter; its card offers "all servers"
	// and then the servers themselves, in the order the session names them.
	send(t, m, keyPress("tab"), keyPress("up"))
	if m.focus != focusFilter {
		t.Fatalf("focus = %v, want the filter", m.focus)
	}
	send(t, m, keyPress("enter"))
	var servers []string
	for _, row := range m.dialog.filter.rows() {
		if row.group == "Server" {
			servers = append(servers, row.label)
		}
	}
	if !slices.Equal(servers, []string{allServers, "alpha", "beta"}) {
		t.Fatalf("servers = %q, want every server, then alpha and beta", servers)
	}
	markFilter(t, m, "beta")
	send(t, m, keyPress("enter"))
	if m.list.server != "beta" {
		t.Fatalf("the list is showing %q, want beta", m.list.server)
	}
	if got := m.opts.command("fix it"); !strings.Contains(got, "--server beta") {
		t.Fatalf("command = %q, want --server beta", got)
	}

	send(t, m, keyPress("esc"))
	send(t, m, typeString("fix the reaper")...)
	send(t, m, keyPress("enter"), keyPress("enter"))
	if len(ds.runs) != 1 || ds.runs[0].Server != "beta" {
		t.Fatalf("runs = %+v, want one on beta", ds.runs)
	}
}

// Every server at once is no answer to which server a create belongs on, so it
// falls back to the primary — where `discobox run` with no --server goes
// (ADR 0116 §5) — however the window arrived back there.
func TestEveryServerStillCreatesOnThePrimary(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(onServer("alpha", testSandboxes())...)
	ds.session.Servers = []string{"alpha", "beta"}
	m := newTestModel(t, ds)

	// Out to beta and back, so this is the choice being made rather than the
	// window never having been touched.
	filterTo(t, m, "beta")
	filterTo(t, m, allServers)
	if m.list.server != "" {
		t.Fatalf("the list is showing %q, want every server", m.list.server)
	}
	if got := m.opts.command("fix it"); strings.Contains(got, "--server") {
		t.Fatalf("command = %q, want no --server, which is the primary", got)
	}
}

// And the same control from the other side: the Server row in the run options
// moves the list, the way the Source row moves the folder.
func TestTheRunOptionsServerRowMovesTheList(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(onServer("alpha", testSandboxes())...)
	ds.session.Servers = []string{"alpha", "beta"}
	m := newTestModel(t, ds)

	m.optionsOpen = true
	m.opts.moveTo(optServer)
	send(t, m, keyPress("right"))
	if m.list.server != "beta" {
		t.Fatalf("the list is showing %q, want the server the row moved to", m.list.server)
	}
	send(t, m, keyPress("left"))
	if m.list.server != "alpha" {
		t.Fatalf("the list is showing %q, want alpha", m.list.server)
	}
}

// A server that did not answer is said where its discoboxes would have been:
// showing every server, or showing that one. Showing another server it is
// somewhere else's news, and the section would be a row about a machine whose
// discoboxes were never going to be on screen.
func TestTheNotAnsweringSectionFollowsTheFilter(t *testing.T) {
	l := listForTest(Session{Servers: []string{"alpha", "beta"}}, []Sandbox{
		{ID: "sbx_a1", Name: "one", Server: "alpha", State: StateRunning},
	})
	l.setUnreachable([]string{"beta"})

	l.server = "alpha"
	if out := l.view(newStyles(false), &zones{}, true); strings.Contains(out, "not answering") {
		t.Fatalf("showing alpha, the list says beta is missing:\n%s", out)
	}
	l.server = "beta"
	out := l.view(newStyles(false), &zones{}, true)
	if !strings.Contains(out, "not answering") {
		t.Fatalf("showing beta, the list does not say it is not answering:\n%s", out)
	}
	if strings.Contains(out, "one") {
		t.Fatalf("showing beta, the list has alpha's discoboxes in it:\n%s", out)
	}
}

// The filter is one stop however many servers there are: Tab goes round the
// prompt, the discoboxes and the filter, and Up from the list reaches it and
// stays there.
func TestManyServersAreStillOneFilter(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(onServer("alpha", testSandboxes())...)
	ds.session.Servers = []string{"alpha", "beta"}
	m := newTestModel(t, ds)

	for _, want := range []focusArea{focusList, focusFilter, focusPrompt} {
		send(t, m, keyPress("tab"))
		if m.focus != want {
			t.Fatalf("Tab reached %v, want %v", m.focus, want)
		}
	}
	send(t, m, keyPress("tab"))
	m.list.moveTo(0)
	for _, want := range []focusArea{focusFilter, focusFilter} {
		send(t, m, keyPress("up"))
		if m.focus != want {
			t.Fatalf("Up reached %v, want %v", m.focus, want)
		}
	}
	send(t, m, keyPress("down"))
	if m.focus != focusList {
		t.Fatalf("Down reached %v, want the list", m.focus)
	}
}

// With one server there is nothing to pick, so the card offers no servers and
// the header names none.
func TestOneServerOffersNoServers(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	for _, row := range m.filterDialog().filter.rows() {
		if row.group == "Server" {
			t.Fatalf("the card offers server %q with one server", row.label)
		}
	}
	if strings.Contains(plainFrame(m), allServers) {
		t.Fatalf("the window offers a server filter with one server:\n%s", plainFrame(m))
	}
}

// onServer puts a listing's discoboxes on one server, the way a window that
// lists more than one stamps every row with the server it came from.
func onServer(server string, boxes []Sandbox) []Sandbox {
	for i := range boxes {
		boxes[i].Server = server
	}
	return boxes
}

// A server that did not answer is said where the window shows it: on the
// filter's card, beside its name, and as its own section when the list is narrowed
// to it — while narrowed to another server nothing says it at all.
func TestANarrowedServerThatIsNotAnsweringSaysSo(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(onServer("alpha", testSandboxes())...)
	ds.session.Servers = []string{"alpha", "beta"}
	ds.unreachable = []string{"beta"}
	m := newTestModel(t, ds)

	send(t, m, keyPress("tab"), keyPress("up"), keyPress("enter"))
	if got := dialogText(m); !strings.Contains(got, "not answering") {
		t.Fatalf("the filter card does not say beta is not answering:\n%s", got)
	}
	send(t, m, keyPress("esc"))

	// all servers → alpha: alpha's rows, and not a word about beta.
	filterTo(t, m, "alpha")
	if out := plainFrame(m); strings.Contains(out, "not answering") {
		t.Fatalf("narrowed to alpha, the list says beta is not answering:\n%s", out)
	}
	// alpha → beta: no rows, and the section saying why.
	filterTo(t, m, "beta")
	if out := plainFrame(m); !strings.Contains(out, "not answering") {
		t.Fatalf("narrowed to beta, the list does not say it is not answering:\n%s", out)
	}
}

// The harnesses and secrets screens are the header's server's, and the header
// says so over them (ADR 0131 §2): narrowed to beta, F3 and F4 read and name
// beta, and the control is one a pointer can reach there too.
func TestTheConfigScreensAreTheHeadersServer(t *testing.T) {
	t.Parallel()
	for _, key := range []string{harnessesKey, secretsKey} {
		ds := newFakeSource(onServer("alpha", testSandboxes())...)
		ds.session.Servers = []string{"alpha", "beta"}
		m := newTestModel(t, ds)
		filterTo(t, m, "beta")
		if m.list.server != "beta" {
			t.Fatalf("the list is showing %q, want beta", m.list.server)
		}
		send(t, m, keyPress(key))

		if header := ansi.Strip(windowLines(m)[0]); !strings.Contains(header, "server beta") {
			t.Fatalf("%s: the screen does not say it is beta's: %q", key, header)
		}
		if _, ok := m.zones.find(hitFilter); !ok {
			t.Fatalf("%s: the server the screen names is not marked", key)
		}
		want := "Harnesses@beta"
		if key == secretsKey {
			want = "Secrets@beta"
		}
		if !slices.Contains(ds.calls(), want) {
			t.Fatalf("%s: calls = %v, want %s", key, ds.calls(), want)
		}
	}
}

// From every server, the screens are the primary's and say so by name — every
// server at once is not a server a secret can be added to — and leaving them
// leaves the list showing every server, as it was.
func TestTheConfigScreensNameThePrimaryFromEveryServer(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(onServer("alpha", testSandboxes())...)
	ds.session.Servers = []string{"alpha", "beta"}
	m := newTestModel(t, ds)
	send(t, m, keyPress(secretsKey))

	header := ansi.Strip(windowLines(m)[0])
	if !strings.Contains(header, "server alpha") || strings.Contains(header, allServers) {
		t.Fatalf("header = %q, want it to name the primary", header)
	}
	if choices := m.serverChoices(); slices.Contains(choices, "") {
		t.Fatalf("choices = %q, want no every-server choice over the secrets screen", choices)
	}
	if !slices.Contains(ds.calls(), "Secrets@alpha") {
		t.Fatalf("calls = %v, want the primary's secrets", ds.calls())
	}
	send(t, m, keyPress("esc"))
	if m.list.server != "" {
		t.Fatalf("the list is showing %q after the secrets screen, want every server", m.list.server)
	}
}

// The arrows move the one server there is over the screens, and it is the
// list's: beta's secrets replace alpha's rather than sitting under beta's name
// while they are read, and the list is on beta after Esc.
func TestTheArrowsMoveTheConfigScreensServer(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(onServer("alpha", testSandboxes())...)
	ds.session.Servers = []string{"alpha", "beta"}
	ds.secretsOn = map[string][]Secret{
		"alpha": {{ID: "sec_a", Name: "alpha token"}},
		"beta":  {{ID: "sec_b", Name: "beta token"}},
	}
	m := newTestModel(t, ds)
	send(t, m, keyPress(secretsKey))
	if out := plainFrame(m); !strings.Contains(out, "alpha token") {
		t.Fatalf("the secrets screen does not list alpha's:\n%s", out)
	}

	send(t, m, keyPress("right"))
	out := plainFrame(m)
	if !strings.Contains(out, "beta token") || strings.Contains(out, "alpha token") {
		t.Fatalf("after → the screen does not list beta's alone:\n%s", out)
	}
	if header := ansi.Strip(windowLines(m)[0]); !strings.Contains(header, "server beta") {
		t.Fatalf("header = %q, want beta named", header)
	}
	// Round, without stopping at every server.
	send(t, m, keyPress("right"))
	if m.configServer() != "alpha" {
		t.Fatalf("→ from beta reached %q, want alpha", m.configServer())
	}
	send(t, m, keyPress("left"), keyPress("esc"))
	if m.list.server != "beta" {
		t.Fatalf("the list is showing %q after beta's secrets, want beta", m.list.server)
	}
}

// A secrets listing that lands after the header moved on is not drawn under
// the name of the server it was not read from.
func TestAStaleSecretsListingIsDropped(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(onServer("alpha", testSandboxes())...)
	ds.session.Servers = []string{"alpha", "beta"}
	m := newTestModel(t, ds)
	send(t, m, keyPress(secretsKey), keyPress("right"))

	send(t, m, secretsLoadedListMsg{server: "alpha", secrets: []Secret{{ID: "sec_a", Name: "alpha token"}}})
	if out := plainFrame(m); strings.Contains(out, "alpha token") {
		t.Fatalf("alpha's secrets are drawn under beta:\n%s", out)
	}
}

// A request on a registered server's discobox marks its row, and everything
// that answers it goes to that server: the secrets offered, and the approval
// (ADR 0131 §1). The secrets screen only offers it on that server.
func TestARequestOnAnotherServerIsAnsweredThere(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(onServer("beta", testSandboxes())...)
	ds.session.Servers = []string{"alpha", "beta"}
	req := waitingRequest()
	req.Server = "beta"
	ds.requests = []CredentialRequest{req}
	ds.secretsOn = map[string][]Secret{"beta": {{ID: "sec_gh", Name: "GitHub token", Type: "bearer", Routes: []secretroute.Route{{Hosts: []string{"api.github.com"}}}}}}
	m := newTestModel(t, ds)

	if pending := m.pendingFor("sbx_one"); len(pending) != 1 {
		t.Fatalf("pending on beta's discobox = %v, want its request", pending)
	}
	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	if !onRequestCard(m) {
		t.Fatalf("dialog = %s, want the request card", describe(m.dialog))
	}
	if body := dialogText(m); !strings.Contains(body, "beta") {
		t.Fatalf("card = %q, want it to name the server", body)
	}
	drain(t, m, m.dialog.action("secret:sec_gh"), 0)
	grantFor(t, m, time.Hour)
	calls := ds.calls()
	for _, want := range []string{"Secrets@beta", "ApproveCredentialRequest@beta"} {
		if !slices.Contains(calls, want) {
			t.Fatalf("calls = %v, want %s", calls, want)
		}
	}
}

// The secrets screen's inbox is its server's: alpha's secrets cannot answer
// beta's request, so alpha's screen does not offer it.
func TestTheSecretsScreenListsItsServersRequests(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(onServer("beta", testSandboxes())...)
	ds.session.Servers = []string{"alpha", "beta"}
	req := waitingRequest()
	req.Server = "beta"
	ds.requests = []CredentialRequest{req}
	m := newTestModel(t, ds)

	send(t, m, keyPress(secretsKey))
	if n := len(m.requestRows.all); n != 0 {
		t.Fatalf("alpha's secrets screen lists %d requests, want beta's left out", n)
	}
	send(t, m, keyPress("right"))
	if n := len(m.requestRows.all); n != 1 {
		t.Fatalf("beta's secrets screen lists %d requests, want its one", n)
	}
}

// The archived offer counts what A would show: a discobox archived on another
// server is not one pressing A brings back into a list narrowed to this one.
func TestTheArchivedOfferCountsTheServerOnScreen(t *testing.T) {
	l := listForTest(Session{Servers: []string{"alpha", "beta"}}, []Sandbox{
		{ID: "sbx_a1", Name: "one", Server: "alpha", State: StateRunning},
		{ID: "sbx_b1", Name: "gone", Server: "beta", State: StateArchived},
	})
	if n := l.archivedCount(); n != 1 {
		t.Fatalf("every server: archived = %d, want 1", n)
	}
	l.server = "alpha"
	if n := l.archivedCount(); n != 0 {
		t.Fatalf("narrowed to alpha: archived = %d, want beta's left out", n)
	}
}

// The folders sit inside the server: the card offers and counts the folders
// on the server marked, as it is marked, so a folder only another server has
// something in is not a choice that then lists nothing.
func TestTheCardsFoldersAreTheMarkedServers(t *testing.T) {
	t.Parallel()
	boxes := testSandboxes()
	for i := range boxes {
		boxes[i].Server = "alpha"
		// /src/obot's discoboxes are all on beta.
		if boxes[i].OriginKey == testKey("/src/obot") {
			boxes[i].Server = "beta"
		}
	}
	ds := newFakeSource(boxes...)
	ds.session.Servers = []string{"alpha", "beta"}
	m := newTestModel(t, ds)

	send(t, m, keyPress("tab"), keyPress("up"), keyPress("enter"))
	folder := func(label string) (filterRow, bool) {
		for _, row := range m.dialog.filter.rows() {
			if row.group == "Folder" && row.label == label {
				return row, true
			}
		}
		return filterRow{}, false
	}
	if _, ok := folder("/src/obot"); !ok {
		t.Fatal("every server: the folders do not include beta's /src/obot")
	}
	markFilter(t, m, "alpha")
	if _, ok := folder("/src/obot"); ok {
		t.Fatal("alpha marked: the folders offer /src/obot, which only beta has")
	}
	if m.list.server != "" {
		t.Fatalf("the list moved to %q before Enter", m.list.server)
	}
	onAlpha := 0
	for _, s := range boxes {
		if s.Server == "alpha" && s.State != StateArchived {
			onAlpha++
		}
	}
	if row, _ := folder(allFolders); row.detail != plural(onAlpha, "box", "boxes") {
		t.Fatalf("all folders detail = %q, want %d boxes, counted on alpha", row.detail, onAlpha)
	}
	send(t, m, keyPress("enter"))
	if m.list.server != "alpha" {
		t.Fatalf("the list is showing %q, want alpha", m.list.server)
	}
}

// The machine line is the primary's figures, unnamed. Above a list narrowed to
// another server it would read as that server's capacity, so it is not there.
func TestTheMachineLineIsOnlyOverThePrimary(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(onServer("alpha", testSandboxes())...)
	ds.session.Servers = []string{"alpha", "beta"}
	ds.setResources(Resources{Known: true, CPUVCPUs: 4.2, CPUCapacity: 24})
	m := newTestModel(t, ds)

	for _, step := range []struct {
		choice string
		server string
		shown  bool
	}{
		{"", "", true},
		{"alpha", "alpha", true},
		{"beta", "beta", false},
	} {
		if step.choice != "" {
			filterTo(t, m, step.choice)
		}
		if m.list.server != step.server {
			t.Fatalf("the list is showing %q, want %q", m.list.server, step.server)
		}
		if got := strings.Contains(plainFrame(m), "cpu 4.2/24"); got != step.shown {
			t.Fatalf("showing %q: machine line drawn = %v, want %v:\n%s", step.server, got, step.shown, plainFrame(m))
		}
	}
}

// On every server and every folder, a section is a folder, with every
// server's discoboxes in it under one band that names no server.
func TestAFolderIsOneSectionAcrossServers(t *testing.T) {
	session := Session{Servers: []string{"alpha", "beta"}, OriginKey: testKey("/src/disco2"), Directory: "/src/disco2"}
	l := listForTest(session, []Sandbox{
		{ID: "sbx_1", Name: "one", Server: "beta", State: StateRunning, OriginKey: testKey("/src/obot"), Source: "/src/obot"},
		{ID: "sbx_2", Name: "two", Server: "alpha", State: StateRunning, OriginKey: testKey("/src/obot"), Source: "/src/obot"},
		{ID: "sbx_3", Name: "three", Server: "alpha", State: StateRunning, OriginKey: testKey("/src/disco2"), Source: "/src/disco2"},
		{ID: "sbx_4", Name: "four", Server: "beta", State: StateRunning, OriginKey: testKey("/src/disco2"), Source: "/src/disco2"},
	})
	out := l.view(newStyles(false), &zones{}, true)
	// The window's own folder leads, the way the filter offers it, and each
	// folder's rows keep the listing's order whichever server they are on.
	order := []string{"/src/disco2", "three", "four", "/src/obot", "one", "two"}
	last := -1
	for _, text := range order {
		i := strings.Index(out, text)
		if i <= last {
			t.Fatalf("%q is missing or out of order:\n%s", text, out)
		}
		last = i
	}
	if strings.Contains(out, " on alpha") || strings.Contains(out, " on beta") {
		t.Fatalf("a section names a server:\n%s", out)
	}
	if want := []int{-1, 0, 1, -1, 2, 3}; !slices.Equal(l.drawn.rows, want) {
		t.Fatalf("drawn rows = %v, want %v", l.drawn.rows, want)
	}

	// Narrowed to one server, the folders are still what is told apart.
	l.server = "alpha"
	l.view(newStyles(false), &zones{}, true)
	if want := []int{-1, 0, -1, 1}; !slices.Equal(l.drawn.rows, want) {
		t.Fatalf("drawn rows = %v, want %v", l.drawn.rows, want)
	}

	// Narrowed to one folder there is one section, and so none, on one server
	// or on every one.
	l.folder = session.folder()
	if l.grouped() {
		t.Fatal("the list is sectioned while it shows one folder")
	}
	l.server = ""
	if out = l.view(newStyles(false), &zones{}, true); l.grouped() || strings.Contains(out, "server alpha") {
		t.Fatalf("one folder's list is sectioned by server:\n%s", out)
	}
}

// Folders the filter does not offer — this machine's sourceless discoboxes,
// and rows with no key at all — are each one section, however their rows
// arrived interleaved.
func TestUnofferedFoldersAreOneSectionEach(t *testing.T) {
	session := Session{OriginKey: testKey("/src/disco2"), HostKey: testHostKey, HostID: testHostID}
	l := listForTest(session, []Sandbox{
		{ID: "sbx_1", Name: "one", State: StateRunning, OriginKey: testHostKey, OriginHostID: testHostID},
		{ID: "sbx_2", Name: "two", State: StateRunning},
		{ID: "sbx_3", Name: "three", State: StateRunning, OriginKey: testHostKey, OriginHostID: testHostID},
		{ID: "sbx_4", Name: "four", State: StateRunning},
	})
	l.view(newStyles(false), &zones{}, true)
	headers := 0
	for _, r := range l.drawn.rows {
		if r < 0 {
			headers++
		}
	}
	if headers != 2 {
		t.Fatalf("drawn rows = %v, want two sections of two", l.drawn.rows)
	}
}

// Discoboxes whose source resolved to one GitHub repository — a clone in a
// directory, the URL with and without `.git`, whatever case it was typed in
// (the data source keys them alike), whichever server they are on — are one
// section named for the repository,
// and one choice on the filter's card. A source that resolved to none keeps
// the folder it is filed under.
func TestAGitHubRepositoryIsOneFolder(t *testing.T) {
	const repository, key = "https://github.com/discobox-ai/discobox", "discobox-ai/discobox"
	session := Session{
		Servers:   []string{"alpha", "beta"},
		OriginKey: testKey("/src/disco2"), Directory: "/src/disco2", RepositoryKey: key,
		HostID: testHostID, HostKey: testHostKey,
	}
	l := listForTest(session, []Sandbox{
		{ID: "sbx_1", Name: "one", Server: "alpha", State: StateRunning, OriginKey: testKey("/src/obot"), Source: "/src/obot", OriginHostID: testHostID},
		{ID: "sbx_2", Name: "two", Server: "beta", State: StateRunning, OriginKey: testKey(repository + ".git"), Source: repository + ".git", SourceRemote: true, Repository: repository, RepositoryKey: key, OriginHostID: testHostID},
		{ID: "sbx_3", Name: "three", Server: "alpha", State: StateRunning, OriginKey: testKey("/src/other"), Source: "/src/other", OriginHostID: testHostID},
		{ID: "sbx_4", Name: "four", Server: "alpha", State: StateRunning, OriginKey: testKey("/src/disco2"), Source: "/src/disco2", Repository: repository, RepositoryKey: key, OriginHostID: testHostID},
		{ID: "sbx_5", Name: "five", Server: "beta", State: StateRunning, OriginKey: testKey(repository), Source: repository, SourceRemote: true, Repository: "https://github.com/Discobox-AI/Discobox", RepositoryKey: key, OriginHostID: farHostID},
		// Cut from the window's directory before its upstream was recorded:
		// filed under the window's key, so in the window's folder.
		{ID: "sbx_6", Name: "six", Server: "alpha", State: StateRunning, OriginKey: testKey("/src/disco2"), Source: "/src/disco2", OriginHostID: testHostID},
	})
	out := l.view(newStyles(false), &zones{}, true)
	// The repository is the window's own folder, so it leads, and its rows
	// keep the order the listing gave them.
	order := []string{"discobox-ai/discobox on github", "4 boxes", "two", "four", "five", "six", "/src/obot", "one", "/src/other", "three"}
	last := -1
	for _, text := range order {
		i := strings.Index(out, text)
		if i <= last {
			t.Fatalf("%q is missing or out of order:\n%s", text, out)
		}
		last = i
	}
	if want := []int{-1, 0, 1, 2, 3, -1, 4, -1, 5}; !slices.Equal(l.drawn.rows, want) {
		t.Fatalf("drawn rows = %v, want %v", l.drawn.rows, want)
	}

	// The filter offers the repository once, and none of the places its
	// discoboxes are filed under.
	var labels []string
	for _, f := range l.folders() {
		labels = append(labels, f.label)
	}
	if want := []string{"discobox-ai/discobox on github", "/src/obot", "/src/other"}; !slices.Equal(labels, want) {
		t.Fatalf("folders = %q, want %q", labels, want)
	}
	// It cuts from the window's own source, and narrowed to it the list is
	// every one of the repository's discoboxes.
	own := l.folders()[0]
	if own.source != "/src/disco2" || !own.local {
		t.Fatalf("the repository's folder = %+v, want this machine's, cutting from /src/disco2", own)
	}
	l.folder = own
	if got := len(l.rows()); got != 4 {
		t.Fatalf("the repository's folder lists %d discoboxes, want 4", got)
	}
}

// A repository the window is not running in cuts from what its newest
// discobox was cut from, when that can be cut from here, and from the
// repository itself when it was a directory on another machine.
func TestARepositoryFolderCutsFromWhatCanBeCutFromHere(t *testing.T) {
	const repository, key = "https://github.com/discobox-ai/discobox", "discobox-ai/discobox"
	session := Session{OriginKey: testKey("/src/obot"), Directory: "/src/obot", HostID: testHostID, HostKey: testHostKey}
	far := Sandbox{ID: "sbx_1", Name: "one", State: StateRunning, OriginKey: testKey("far"), Source: "/home/wilma/disco", Repository: repository, RepositoryKey: key, OriginHostID: farHostID}
	near := Sandbox{ID: "sbx_2", Name: "two", State: StateRunning, OriginKey: testKey("/src/disco2"), Source: "/src/disco2", Repository: repository, RepositoryKey: key, OriginHostID: testHostID}

	if f := listForTest(session, []Sandbox{far}).folders()[1]; f.source != repository || f.local {
		t.Fatalf("another machine's clone: folder = %+v, want one cutting from %s", f, repository)
	}
	if f := listForTest(session, []Sandbox{near, far}).folders()[1]; f.source != "/src/disco2" || !f.local {
		t.Fatalf("this machine's clone: folder = %+v, want one cutting from /src/disco2", f)
	}
	// This machine's wherever in the listing its clone is.
	if f := listForTest(session, []Sandbox{far, near}).folders()[1]; !f.local {
		t.Fatalf("folder = %+v, want this machine's", f)
	}
}

// A view saved while the window's folder was its origin key opens on the
// repository that folder now is, rather than on a folder the card no longer
// offers.
func TestAViewSavedOnTheWindowsOriginKeyOpensOnItsRepository(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	if ds.session.OriginKey == "" {
		t.Fatal("the test session has no origin key to have saved a view on")
	}
	ds.session.RepositoryKey = "discobox-ai/discobox"
	ds.session.View = ListView{FolderKey: ds.session.OriginKey, FolderLabel: "/src/disco2", FolderSource: "/src/disco2", FolderLocal: true}
	m := newTestModel(t, ds)

	if got := m.list.folder; got.key != "discobox-ai/discobox" || got.label != "discobox-ai/discobox on github" {
		t.Fatalf("the window opened on %+v, want the repository's folder", got)
	}
	if !strings.Contains(headerLine(m), "discobox-ai/discobox on github") {
		t.Fatalf("the header does not name the repository: %q", headerLine(m))
	}
	// Its own discoboxes, filed under the key and resolved to nothing, are
	// still in it.
	if len(m.list.rows()) == 0 {
		t.Fatal("the window's folder lists none of the discoboxes filed under its key")
	}
}

// The window's folder is the repository its checkout resolves to now, and a
// discobox is in the repository its source resolved to when it was cut. In a
// fork, a discobox cut from this directory while its branch tracked the
// upstream is in the upstream's folder, not the window's: it is filed under
// the window's key, and its work goes back to the other repository.
func TestADiscoboxFromTheWindowsDirectoryIsInTheRepositoryItWasCutFrom(t *testing.T) {
	session := Session{OriginKey: testKey("/src/disco2"), Directory: "/src/disco2", RepositoryKey: "me/discobox"}
	l := listForTest(session, []Sandbox{
		{ID: "sbx_1", Name: "fork", State: StateRunning, OriginKey: testKey("/src/disco2"), Source: "/src/disco2", Repository: "https://github.com/me/discobox", RepositoryKey: "me/discobox"},
		{ID: "sbx_2", Name: "upstream", State: StateRunning, OriginKey: testKey("/src/disco2"), Source: "/src/disco2", Repository: "https://github.com/discobox-ai/discobox", RepositoryKey: "discobox-ai/discobox"},
	})
	var labels []string
	for _, f := range l.folders() {
		labels = append(labels, f.label)
	}
	if want := []string{"me/discobox on github", "discobox-ai/discobox on github"}; !slices.Equal(labels, want) {
		t.Fatalf("folders = %q, want %q", labels, want)
	}
	l.folder = session.folder()
	if rows := l.rows(); len(rows) != 1 || rows[0].Name != "fork" {
		t.Fatalf("the window's folder lists %+v, want the fork's discobox alone", rows)
	}
}

// Rows kept on screen while their server is slow to answer again say how old
// they are once they have missed a couple of refreshes — and not before: an
// ordinary poll leaves them a refresh old, and saying so every cycle is the
// always-on indicator nobody reads.
func TestRowsThatStopKeepingUpSayHowOld(t *testing.T) {
	l := listForTest(Session{}, []Sandbox{{ID: "sbx_a1", Name: "one", State: StateRunning}})
	now := l.now()

	l.setListed(map[string]time.Time{"": now.Add(-refreshEvery)})
	if out := l.view(newStyles(false), &zones{}, true); strings.Contains(out, "listed") {
		t.Fatalf("rows one refresh old say how old they are:\n%s", out)
	}

	l.setListed(map[string]time.Time{"": now.Add(-40 * time.Second)})
	l.slow = true
	out := l.view(newStyles(false), &zones{}, true)
	if !strings.Contains(out, "listed 40s ago") {
		t.Fatalf("the band does not say the rows are 40s old:\n%s", out)
	}
	// The age is the same news as "still listing" with the size of it.
	if strings.Contains(out, "still listing") {
		t.Fatalf("the band says both how old the rows are and that it is still listing:\n%s", out)
	}

	l.setListed(map[string]time.Time{"": now.Add(-3 * time.Minute)})
	if out := l.view(newStyles(false), &zones{}, true); !strings.Contains(out, "listed 3m ago") {
		t.Fatalf("the band does not say the rows are 3m old:\n%s", out)
	}
}

// With several servers on screen their rows share folders, so it is a note of
// the server that has fallen behind that says so, after the rows the way a
// server that did not answer is said — and only that server, and not the band.
// Filtered to that one server, the band says it instead.
func TestOnlyTheServerThatFellBehindSaysHowOldItsRowsAre(t *testing.T) {
	l := listForTest(Session{Servers: []string{"alpha", "beta"}}, []Sandbox{
		{ID: "sbx_a1", Name: "one", Server: "alpha", State: StateRunning},
		{ID: "sbx_b1", Name: "two", Server: "beta", State: StateRunning},
	})
	now := l.now()
	// gamma is behind too, but has nothing on screen to be old: a note about
	// rows nobody can see is a note about somewhere else.
	l.setListed(map[string]time.Time{"alpha": now, "beta": now.Add(-time.Minute), "gamma": now.Add(-time.Minute)})

	out := l.view(newStyles(false), &zones{}, true)
	if strings.Contains(out, "server gamma") {
		t.Fatalf("a server with no rows on screen got a note saying they are old:\n%s", out)
	}
	var alpha, beta, band string
	for line := range strings.SplitSeq(out, "\n") {
		switch {
		case strings.Contains(line, "server alpha"):
			alpha = line
		case strings.Contains(line, "server beta"):
			beta = line
		case strings.Contains(line, "Discoboxes"):
			band = line
		}
	}
	if !strings.Contains(beta, "listed 1m ago") {
		t.Fatalf("there is no note saying beta's rows are a minute old: %q\n%s", beta, out)
	}
	if alpha != "" {
		t.Fatalf("alpha, which is current, got a note: %q", alpha)
	}
	if strings.Contains(band, "listed") {
		t.Fatalf("the band, which counts both servers' rows, says how old they are: %q", band)
	}
	if !slices.Contains(l.drawn.rows, -1) {
		t.Fatalf("no line was marked as a note rather than a row: %v", l.drawn.rows)
	}

	l.server = "beta"
	out = l.view(newStyles(false), &zones{}, true)
	if strings.Contains(out, "server beta") {
		t.Fatalf("filtered to beta, the note is drawn as well as the band:\n%s", out)
	}
	if !strings.Contains(out, "listed 1m ago") {
		t.Fatalf("filtered to beta, the band does not say its rows are a minute old:\n%s", out)
	}
}

// The workspace header is read off the listing, so it says the same thing the
// list does when that listing has stopped keeping up.
func TestTheWorkspaceHeaderSaysHowOldTheListingIs(t *testing.T) {
	t.Parallel()
	ds := newFakeSource()
	m := newTestModel(t, ds)
	m.paneBox = Sandbox{ID: "sbx_one", Name: "one"}

	if got := m.paneHeaderFields().text(); strings.Contains(got, "listed") {
		t.Fatalf("a header with nothing stale says how old it is: %q", got)
	}
	// Through a listing landing, as the data source reports it.
	ds.mu.Lock()
	ds.listed = map[string]time.Time{"": m.list.now().Add(-20 * time.Second)}
	ds.mu.Unlock()
	drain(t, m, m.refresh(), 0)
	if got := m.paneHeaderFields().text(); !strings.Contains(got, "listed 20s ago") {
		t.Fatalf("the header does not say the listing is 20s old: %q", got)
	}
}

// The workspace header says a pull request's status through the git word: a
// blocked one replaces it in red whatever git says, a ready one turns the
// work's own `ready` green, and a ready one over work in any other state
// leaves that state's word alone. A credential request waiting is blocked.
func TestTheWorkspaceHeaderSaysThePullRequestStatus(t *testing.T) {
	t.Parallel()
	ahead := GitState{Known: true, Branch: "main", Commit: "2222222"}
	dirty := GitState{Known: true, Branch: "main", Commit: "2222222", Dirty: true}
	st := newStyles(true)
	for _, tc := range []struct {
		name string
		git  GitState
		tags []string
		// pending is how many credential requests are waiting on the box.
		pending int
		want    string
		not     string
	}{
		{"ready work, ready pr", ahead, []string{"ready"}, 0, st.readyText.Render("ready"), ""},
		{"ready work, no tag", ahead, nil, 0, st.name.Render("ready"), st.readyText.Render("ready")},
		{"dirty work, ready pr", dirty, []string{"ready"}, 0, st.statusWA.Render("dirty"), "ready"},
		{"dirty work, blocked pr", dirty, []string{"blocked"}, 0, st.blockedText.Render("blocked"), "dirty"},
		{"unknown work, blocked pr", GitState{}, []string{"blocked", "ready"}, 0, st.blockedText.Render("blocked"), "ready"},
		{"ready work, ready pr, a request waiting", ahead, []string{"ready"}, 1, st.blockedText.Render("blocked"), st.readyText.Render("ready")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newColorModel(t, newFakeSource())
			m.paneBox = Sandbox{ID: "sbx_one", Name: "one", Branch: "main", Commit: "1111111", Git: tc.git, Tags: tc.tags, PendingRequests: tc.pending}
			got := m.paneHeaderFields().text()
			if !strings.Contains(got, tc.want) {
				t.Fatalf("header = %q, want %q in it", got, tc.want)
			}
			if tc.not != "" && strings.Contains(got, tc.not) {
				t.Fatalf("header = %q, want no %q in it", got, tc.not)
			}
		})
	}
}

// A narrowing header keeps a box's blocked longer than its git summary: the
// git word goes early because its mark is on the position, and no mark there
// says blocked.
func TestANarrowWorkspaceHeaderKeepsBlocked(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource())
	m.paneBox = Sandbox{
		ID: "sbx_one", Name: "one", Branch: "main", Commit: "1111111", Tags: []string{"blocked"},
		Git: GitState{Known: true, Branch: "main", Commit: "2222222", Dirty: true},
	}
	fields := m.paneHeaderFields()
	full := fields.text()
	if !strings.Contains(full, "blocked") || strings.Contains(full, "dirty") {
		t.Fatalf("header = %q, want blocked in place of the git word", full)
	}
	// Narrowed until the git position is gone.
	narrow := fields
	for room := len(full); strings.Contains(narrow.text(), "main@"); room-- {
		narrow = fields.fit(room)
	}
	if got := narrow.text(); !strings.Contains(got, "blocked") {
		t.Fatalf("narrowed to %q, want blocked kept after the git position is gone", got)
	}
}

// Ctrl-L re-reads the listing, not only the screen: whoever reaches for it is
// asking whether what is on screen is current. A read already out is not
// doubled — it is said, once it is late.
func TestCtrlLReReadsTheListing(t *testing.T) {
	t.Parallel()
	ds := newFakeSource()
	m := newTestModel(t, ds)
	ds.mu.Lock()
	ds.sandboxes = []Sandbox{{ID: "sbx_new", Name: "arrived", State: StateRunning}}
	ds.mu.Unlock()

	press(t, m, "ctrl+l")
	if len(m.list.all) != 1 || m.list.all[0].ID != "sbx_new" {
		t.Fatalf("Ctrl-L did not re-read the listing: %+v", m.list.all)
	}

	m.listPoll.since = m.now().Add(-12 * time.Second)
	_, cmd := m.Update(keyPress("ctrl+l"))
	if !m.listPoll.again {
		t.Fatal("Ctrl-L with a read out did not ask for another once it lands")
	}
	drain(t, m, cmd, 0)
	if !strings.Contains(m.status, "still listing") || !strings.Contains(m.status, "12s") {
		t.Fatalf("Ctrl-L with a read out 12s did not say so: %q", m.status)
	}
}
