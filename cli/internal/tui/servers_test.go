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

// The list is one section per server, in the order the session names them,
// with the primary's first (ADR 0116 §4).
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
	if !strings.Contains(out, allServers) {
		t.Fatalf("the header does not say it is showing every server:\n%s", out)
	}
	// Every server on screen is more than one to tell apart, so the rows are
	// sectioned under the server each is on.
	if !m.list.grouped() {
		t.Fatalf("the list is not sectioned by server:\n%s", out)
	}

	// And narrowing to one is a press: the section bands go, because the
	// header now says the name a band over every row would repeat.
	send(t, m, keyPress("tab"), keyPress("up"), keyPress("up"), keyPress("right"))
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

	// Up out of the list reaches the folder, and Up again the server; right
	// walks off "all servers" onto the servers themselves, in the order the
	// session names them.
	send(t, m, keyPress("tab"), keyPress("up"), keyPress("up"))
	if m.focus != focusServer {
		t.Fatalf("focus = %v, want the server filter", m.focus)
	}
	send(t, m, keyPress("right"), keyPress("right"))
	if m.list.server != "beta" {
		t.Fatalf("the list is showing %q, want beta", m.list.server)
	}
	if got := m.opts.command("fix it"); !strings.Contains(got, "--server beta") {
		t.Fatalf("command = %q, want --server beta", got)
	}

	send(t, m, keyPress("esc"))
	send(t, m, typeString("fix the reaper")...)
	send(t, m, keyPress("enter"))
	if len(ds.runs) != 1 || ds.runs[0].Server != "beta" {
		t.Fatalf("runs = %+v, want one on beta", ds.runs)
	}
}

// Every server at once is no answer to which server a create belongs on, so it
// falls back to the primary — where `discobox new` with no --server goes
// (ADR 0116 §5) — however the window arrived back there.
func TestEveryServerStillCreatesOnThePrimary(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(onServer("alpha", testSandboxes())...)
	ds.session.Servers = []string{"alpha", "beta"}
	m := newTestModel(t, ds)

	// Out to beta and back, so this is the choice being made rather than the
	// window never having been touched.
	send(t, m, keyPress("tab"), keyPress("up"), keyPress("up"), keyPress("left"), keyPress("right"))
	if m.list.server != "" {
		t.Fatalf("the list is showing %q, want every server", m.list.server)
	}
	if !m.list.grouped() {
		t.Fatal("the list showing every server is not sectioned by server")
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

// Tab goes round the window in the order Up climbs it: the prompt, the
// discoboxes, the folder they were cut from, the server they are on, and back
// to the prompt.
func TestTabWalksBothHeaderFilters(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(onServer("alpha", testSandboxes())...)
	ds.session.Servers = []string{"alpha", "beta"}
	m := newTestModel(t, ds)

	for _, want := range []focusArea{focusList, focusFolder, focusServer, focusPrompt} {
		send(t, m, keyPress("tab"))
		if m.focus != want {
			t.Fatalf("Tab reached %v, want %v", m.focus, want)
		}
	}
}

// Up climbs the same ladder Tab walks — discoboxes, folder, server — and stops
// at the top; Down comes back down it one rung at a time, to the prompt.
func TestUpAndDownClimbBothHeaderFilters(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(onServer("alpha", testSandboxes())...)
	ds.session.Servers = []string{"alpha", "beta"}
	m := newTestModel(t, ds)

	send(t, m, keyPress("tab"))
	m.list.moveTo(0)
	for _, want := range []focusArea{focusFolder, focusServer, focusServer} {
		send(t, m, keyPress("up"))
		if m.focus != want {
			t.Fatalf("Up reached %v, want %v", m.focus, want)
		}
	}
	// Down past the last row is the prompt, so the walk down ends there.
	for _, want := range []focusArea{focusFolder, focusList} {
		send(t, m, keyPress("down"))
		if m.focus != want {
			t.Fatalf("Down reached %v, want %v", m.focus, want)
		}
	}
	m.list.moveTo(len(m.list.rows()) - 1)
	send(t, m, keyPress("down"))
	if m.focus != focusPrompt {
		t.Fatalf("Down off the last row reached %v, want the prompt", m.focus)
	}
}

// With one server there is nothing to pick, so the header has no filter to
// reach and Tab goes round the window it always went round.
func TestOneServerLeavesTheHeaderAsItWas(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(testSandboxes()...))

	for _, want := range []focusArea{focusList, focusFolder, focusPrompt} {
		send(t, m, keyPress("tab"))
		if m.focus != want {
			t.Fatalf("Tab reached %v, want %v", m.focus, want)
		}
	}
	// And the folder is the top of the ladder, with no server above it.
	send(t, m, keyPress("tab"), keyPress("up"), keyPress("up"))
	if m.focus != focusFolder {
		t.Fatalf("Up past the folder reached %v, want it to stay on the folder", m.focus)
	}
	if strings.Contains(plainFrame(m), allServers) {
		t.Fatalf("the header offers a server filter with one server:\n%s", plainFrame(m))
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

// A server that did not answer is said where the window shows it: in the
// dropdown, beside its name, and as its own section when the list is narrowed
// to it — while narrowed to another server nothing says it at all.
func TestANarrowedServerThatIsNotAnsweringSaysSo(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(onServer("alpha", testSandboxes())...)
	ds.session.Servers = []string{"alpha", "beta"}
	ds.unreachable = []string{"beta"}
	m := newTestModel(t, ds)

	send(t, m, keyPress("tab"), keyPress("up"), keyPress("up"), keyPress("enter"))
	if got := dialogText(m); !strings.Contains(got, "not answering") {
		t.Fatalf("the server dropdown does not say beta is not answering:\n%s", got)
	}
	send(t, m, keyPress("esc"))

	// all servers → alpha: alpha's rows, and not a word about beta.
	send(t, m, keyPress("right"))
	if out := plainFrame(m); strings.Contains(out, "not answering") {
		t.Fatalf("narrowed to alpha, the list says beta is not answering:\n%s", out)
	}
	// alpha → beta: no rows, and the section saying why.
	send(t, m, keyPress("right"))
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
		send(t, m, keyPress("tab"), keyPress("up"), keyPress("up"), keyPress("left"))
		if m.list.server != "beta" {
			t.Fatalf("the list is showing %q, want beta", m.list.server)
		}
		send(t, m, keyPress("esc"), keyPress(key))

		if header := frame(m)[1]; !strings.Contains(header, "server beta") {
			t.Fatalf("%s: the screen does not say it is beta's: %q", key, header)
		}
		if _, ok := m.zones.find(hitServer); !ok {
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

	header := frame(m)[1]
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
	if !strings.Contains(frame(m)[1], "server beta") {
		t.Fatalf("header = %q, want beta named", frame(m)[1])
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
	ds.secretsOn = map[string][]Secret{"beta": {{ID: "sec_gh", Name: "GitHub token", Type: "bearer", Host: "api.github.com"}}}
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

// The folder filter sits inside the server one: its choices and its counts are
// the server's, so a folder only another server has something in is not a
// choice that then lists nothing.
func TestTheFolderDropdownIsTheServersFolders(t *testing.T) {
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

	has := func() bool {
		for _, f := range m.folderChoices() {
			if f.key == testKey("/src/obot") {
				return true
			}
		}
		return false
	}
	if !has() {
		t.Fatal("every server: the folders do not include beta's /src/obot")
	}
	send(t, m, keyPress("tab"), keyPress("up"), keyPress("up"), keyPress("right"))
	if m.list.server != "alpha" {
		t.Fatalf("the list is showing %q, want alpha", m.list.server)
	}
	if has() {
		t.Fatal("narrowed to alpha: the folders offer /src/obot, which only beta has")
	}
	onAlpha := 0
	for _, s := range boxes {
		if s.Server == "alpha" {
			onAlpha++
		}
	}
	if got, want := m.folderDetail(everyFolder), plural(onAlpha, "box", "boxes")+" on alpha"; got != want {
		t.Fatalf("all folders detail = %q, want %q", got, want)
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

	send(t, m, keyPress("tab"), keyPress("up"), keyPress("up"))
	for _, step := range []struct {
		key    string
		server string
		shown  bool
	}{
		{"", "", true},
		{"right", "alpha", true},
		{"right", "beta", false},
	} {
		if step.key != "" {
			send(t, m, keyPress(step.key))
		}
		if m.list.server != step.server {
			t.Fatalf("the list is showing %q, want %q", m.list.server, step.server)
		}
		if got := strings.Contains(plainFrame(m), "cpu 4.2/24"); got != step.shown {
			t.Fatalf("showing %q: machine line drawn = %v, want %v:\n%s", step.server, got, step.shown, plainFrame(m))
		}
	}
}
