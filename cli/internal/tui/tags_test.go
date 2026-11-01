package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func taggedSandboxes() []Sandbox {
	boxes := testSandboxes()
	boxes[0].Tags = []string{"ticket=ENG-12", "wip"}
	boxes[1].Tags = []string{"wip"}
	return boxes
}

func headerLine(m *Model) string { return ansi.Strip(m.viewHeaderLeft()) }

// tagRows are the tags the filter card offers, as it reads them.
func tagRows(m *Model) []string {
	var out []string
	for _, row := range m.filterDialog().filter.rows() {
		if row.group == "Tag" {
			out = append(out, row.label)
		}
	}
	return out
}

// A project nobody tags offers no tags: a group that can only say "all tags"
// is a group spent saying nothing.
func TestThereAreNoTagsToPickUntilSomethingIsTagged(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	if got := tagRows(m); len(got) != 0 {
		t.Fatalf("the card offers tags %q with nothing tagged", got)
	}
}

// Once a discobox is tagged the card offers every tag after the folders, each
// with how many boxes carry it, and marking one narrows the list to the boxes
// carrying it.
func TestATagNarrowsTheList(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(taggedSandboxes()...))
	filterTo(t, m, "Folder: "+m.session.folder().label)
	if got := tagRows(m); len(got) != 3 || got[0] != allTags || got[1] != "#ticket=ENG-12" || got[2] != "#wip" {
		t.Fatalf("tags = %q, want every box then each tag in order", got)
	}
	view := m.filterDialog().view(m.st, &m.zones, 120, 40)
	for _, want := range []string{allTags, "#ticket=ENG-12", "#wip", "2 boxes", "ticket set to this value"} {
		if !strings.Contains(view, want) {
			t.Errorf("the card is missing %q:\n%s", want, view)
		}
	}
	rows := func() []string {
		var ids []string
		for _, s := range m.list.rows() {
			ids = append(ids, s.ID)
		}
		return ids
	}
	if got := rows(); len(got) != 2 {
		t.Fatalf("rows = %v, want both of this folder's boxes", got)
	}

	filterTo(t, m, "#wip")
	if got := rows(); !slices.Equal(m.list.tags, []string{"wip"}) || len(got) != 2 {
		t.Fatalf("tags %q list %v, want both boxes tagged wip", m.list.tags, got)
	}
	filterTo(t, m, "#ticket=ENG-12")
	if !slices.Equal(m.list.tags, []string{"ticket=ENG-12", "wip"}) || !strings.Contains(headerLine(m), "#ticket=ENG-12 #wip") {
		t.Fatalf("tags = %q, header = %q, want both tags", m.list.tags, headerLine(m))
	}
	if got := rows(); len(got) != 1 || got[0] != "sbx_one" {
		t.Fatalf("rows = %v, want only the box carrying both tags", got)
	}
	filterTo(t, m, allTags)
	if len(m.list.tags) != 0 || strings.Contains(headerLine(m), "#") {
		t.Fatalf("tags = %q, header = %q, want every box and no tag named", m.list.tags, headerLine(m))
	}
}

// A tag that goes while the filter is on it stays the filter's choice rather
// than vanishing from under it, so what the header says is still what is shown.
func TestAChosenTagOutlivesItsLastBox(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(taggedSandboxes()...))
	m.list.tags = []string{"wip"}
	untagged := testSandboxes()
	m.list.setAll(untagged)
	if got := tagRows(m); !slices.Contains(got, "#wip") || !strings.Contains(headerLine(m), "#wip") {
		t.Fatalf("header = %q, tags = %q, want the filter still naming #wip", headerLine(m), got)
	}
	if len(m.list.rows()) != 0 {
		t.Fatalf("rows = %d, want none: nothing carries the tag now", len(m.list.rows()))
	}
}

// Space turns a tag on and off again, so several can be marked; Enter only
// ever turns the row it is on on, so the key that applies the card never drops
// the tag under the cursor.
func TestSpaceTogglesTagsAndEnterOnlyMarks(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(taggedSandboxes()...))
	m.dialog = m.filterDialog()
	p := m.dialog.filter
	marked := func() []string { return p.list.tags }

	markFilter(t, m, "#wip")
	markFilter(t, m, "#ticket=ENG-12")
	if got := marked(); !slices.Equal(got, []string{"ticket=ENG-12", "wip"}) {
		t.Fatalf("marked %q, want both tags", got)
	}
	if got := dialogText(m); !strings.Contains(got, "■ #wip") || !strings.Contains(got, "□ "+allTags) {
		t.Fatalf("the tags are not drawn as boxes, marked:\n%s", got)
	}
	markFilter(t, m, "#wip")
	if got := marked(); !slices.Equal(got, []string{"ticket=ENG-12"}) {
		t.Fatalf("marked %q after Space on a marked tag, want it let go", got)
	}
	markFilter(t, m, "#ticket=ENG-12")
	if got := marked(); len(got) != 0 {
		t.Fatalf("marked %q, want none", got)
	}

	markFilter(t, m, "#wip")
	send(t, m, keyPress("enter"))
	if !slices.Equal(m.list.tags, []string{"wip"}) {
		t.Fatalf("tags = %q after Enter on a marked tag, want it kept", m.list.tags)
	}

	m.dialog = m.filterDialog()
	markFilter(t, m, "#ticket=ENG-12")
	markFilter(t, m, allTags)
	if got := m.dialog.filter.list.tags; len(got) != 0 {
		t.Fatalf("marked %q after all tags, want none", got)
	}
}

// A discobox has one value for a key, so marking another value of a key
// already marked takes its place rather than narrowing to what nothing carries.
func TestATagReplacesAnotherValueOfItsKey(t *testing.T) {
	t.Parallel()
	var l sandboxList
	l.addTag("wip")
	l.addTag("ticket=ENG-12")
	l.addTag("ticket=ENG-13")
	if want := []string{"ticket=ENG-13", "wip"}; !slices.Equal(l.tags, want) {
		t.Fatalf("tags = %q, want %q", l.tags, want)
	}
	l.addTag("ticket")
	if want := []string{"ticket", "wip"}; !slices.Equal(l.tags, want) {
		t.Fatalf("tags = %q, want a bare key in place of its value: %q", l.tags, want)
	}
}

// An `issue=` or `pr=` tag on a discobox cut from a GitHub repository is the
// number of an issue or pull request there; any other tag, or one of those on
// a discobox with no GitHub repository to number it in, points nowhere.
func TestATagThatNumbersGitHubWorkLinksToIt(t *testing.T) {
	t.Parallel()
	box := Sandbox{Repository: "https://github.com/acme/foo"}
	for _, tc := range []struct{ tag, want string }{
		{"issue=4", "https://github.com/acme/foo/issues/4"},
		{"pr=123", "https://github.com/acme/foo/pull/123"},
		{"issue", ""},
		{"issue=", ""},
		{"issue=04", ""},
		{"issue=ENG-12", ""},
		{"pr=-1", ""},
		{"ticket=4", ""},
		{"wip", ""},
	} {
		if got := box.tagURL(tc.tag); got != tc.want {
			t.Errorf("tagURL(%q) = %q, want %q", tc.tag, got, tc.want)
		}
	}
	if got := (Sandbox{}).tagURL("issue=4"); got != "" {
		t.Errorf("tagURL on a discobox with no GitHub repository = %q, want nothing", got)
	}
}

// githubTaggedSandboxes is the listing with the first discobox, cut from a
// GitHub repository, working on an issue and a pull request in it.
func githubTaggedSandboxes() []Sandbox {
	boxes := testSandboxes()
	boxes[0].Name = "reaper"
	boxes[0].Repository = "https://github.com/acme/foo"
	boxes[0].Tags = []string{"issue=4", "pr=9", "wip"}
	return boxes
}

// On the list, the tag is the link: a plain click on `#issue=4` opens the
// issue rather than only pointing at the row, and it is drawn as the OSC 8 a
// terminal's own Ctrl-click follows.
func TestClickingAnIssueTagOnTheListOpensTheIssue(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(githubTaggedSandboxes()...)
	d, m := openList(t, ds)
	// Wide enough for the tags beside the name: they give way before it does.
	d.dispatch(tea.WindowSizeMsg{Width: 180, Height: 40})
	d.wait("the tags", func() bool { return strings.Contains(plainFrame(m), "#issue=4 #pr=9 #wip") })

	opened := make(chan string, 4)
	m.openOS = func(url string) error { opened <- url; return nil }

	if frame := rawFrame(m); !strings.Contains(frame, "https://github.com/acme/foo/issues/4") ||
		!strings.Contains(frame, "https://github.com/acme/foo/pull/9") {
		t.Fatalf("the issue and pull request tags are not links:\n%q", frame)
	}

	x, y := at(t, m, "#issue=4")
	tap(t, m, x, y)
	if got := <-opened; got != "https://github.com/acme/foo/issues/4" {
		t.Errorf("clicking #issue=4 opened %q, want the issue", got)
	}
	x, y = at(t, m, "#pr=9")
	slowClock(m)
	tap(t, m, x, y)
	if got := <-opened; got != "https://github.com/acme/foo/pull/9" {
		t.Errorf("clicking #pr=9 opened %q, want the pull request", got)
	}

	// A tag that numbers nothing is the row, as it was.
	x, y = at(t, m, "#wip")
	tap(t, m, x, y)
	select {
	case got := <-opened:
		t.Errorf("clicking #wip opened %q, want nothing opened", got)
	default:
	}
	// Nor has the wheel: over a linked tag it scrolls the list, as it does
	// anywhere else on the row, and opens nothing.
	m.list.cursor = 0
	x, y = at(t, m, "#issue=4")
	send(t, m, tea.MouseWheelMsg{X: x, Y: y, Button: tea.MouseWheelDown})
	if m.list.cursor == 0 {
		t.Errorf("the wheel over #issue=4 did not move the list on")
	}
	select {
	case got := <-opened:
		t.Errorf("the wheel over #issue=4 opened %q, want nothing opened", got)
	default:
	}

	// The right button has nothing to do with a link, so it is the row's menu
	// there as anywhere else on the row.
	x, y = at(t, m, "#issue=4")
	send(t, m,
		tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseRight},
		tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseRight},
	)
	if m.dialog == nil {
		t.Errorf("right-clicking #issue=4 opened no menu, want the row's actions")
	}
}

// The workspace says which issue and pull request it is working on at the top,
// each a link to it. A plain click on either opens it beside the workspace
// instead (issue_test.go, pull_test.go); the link is still what Ctrl-click
// follows.
func TestTheWorkspaceHeaderLinksTheIssueAndPullRequest(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(githubTaggedSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the header", func() bool { return strings.Contains(plainFrame(m), "issue #4 · PR #9") })

	if _, y := at(t, m, "issue #4"); y != 0 {
		t.Fatalf("issue #4 is drawn on row %d, want the header", y)
	}
	if frame := rawFrame(m); !strings.Contains(frame, "https://github.com/acme/foo/issues/4") {
		t.Errorf("issue #4 is not a link:\n%q", frame)
	}
	if _, y := at(t, m, "PR #9"); y != 0 {
		t.Fatalf("PR #9 is drawn on row %d, want the header", y)
	}
	if frame := rawFrame(m); !strings.Contains(frame, "https://github.com/acme/foo/pull/9") {
		t.Errorf("PR #9 is not a link:\n%q", frame)
	}
}

// Without a GitHub repository, `issue=4` is only a tag: nothing in the
// workspace header claims to know whose issue it is.
func TestTheWorkspaceHeaderLeavesAnUnnumberedIssueOut(t *testing.T) {
	t.Parallel()
	boxes := githubTaggedSandboxes()
	boxes[0].Repository = ""
	if field := workField(newStyles(false), boxes[0]); !field.empty() {
		t.Fatalf("workField = %q, want nothing without a repository", field.text())
	}
}

// treeTagBoxes are three discoboxes for the tree's tag lines: one with more
// tags than its line holds, one with none, and one a pull request is blocked
// on.
func treeTagBoxes() []Sandbox {
	return []Sandbox{
		{ID: "sbx_a", Name: "fix the reaper", State: StateRunning, Repository: "https://github.com/acme/disco",
			Tags: []string{"issue=342", "ready", "area=tui", "size=s", "kind=bug"}},
		{ID: "sbx_b", Name: "plain", State: StateRunning},
		{ID: "sbx_c", Name: "consolidate", State: StateRunning, Repository: "https://github.com/acme/disco",
			Tags: []string{"pr=12", "blocked"}},
	}
}

// In the workspace tree a tagged row has its tags on a line of its own under
// it, drawn as the row draws them beside the name — the pull request status
// first and in its color, an issue or pull request a link, the rest dim — with
// how many did not fit, and an untagged row has no such line. The name gets
// the row's width rather than sharing it.
func TestTheTreeDrawsATaggedRowsTagsUnderIt(t *testing.T) {
	st := newStyles(true)
	l := listForTest(Session{}, treeTagBoxes())
	l.width, l.tagLines = treeWidth-2, true
	z := &zones{}
	out := l.view(st, z, true)
	lines := strings.Split(ansi.Strip(out), "\n")

	var body []string
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) != "" && !strings.Contains(line, "boxes") {
			body = append(body, strings.TrimRight(line, " "))
		}
	}
	want := []string{
		"❯ ●   fix the reaper",
		"      #ready #issue=342 +3",
		"  ●   plain",
		"  ●   consolidate",
		"      #blocked #pr=12",
	}
	if strings.Join(body, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rows:\n%s\nwant:\n%s", strings.Join(body, "\n"), strings.Join(want, "\n"))
	}
	for _, span := range []string{
		st.readyText.Render("#ready"),
		st.blockedText.Render("#blocked"),
		st.info.Render("#issue=342"),
		st.info.Render("#pr=12"),
	} {
		if !strings.Contains(out, span) {
			t.Errorf("the tag lines have no %q:\n%s", span, out)
		}
	}

	// A press on a tag line points at its row, and one on a link opens it.
	if hit, ok := z.at(30, 6); !ok || hit.what.kind != hitRow || hit.what.idx != 2 {
		t.Fatalf("a press on the last row's tag line hits %+v, want row 2", hit)
	}
	if hit, ok := z.at(14, 3); !ok || hit.what.kind != hitURL || hit.what.url != "https://github.com/acme/disco/issues/342" {
		t.Fatalf("a press on #issue=342 hits %+v, want its issue", hit)
	}

	// The list itself, wide, keeps its tags beside the name on one line.
	l.width, l.tagLines = 160, false
	wide := ansi.Strip(l.view(st, &zones{}, true))
	if !strings.Contains(wide, "fix the reaper #ready #issue=342") || strings.Contains(wide, "      #ready") {
		t.Fatalf("the wide list does not keep its tags beside the name:\n%s", wide)
	}
}

// A tree too short for every row keeps the cursor's row on screen with its tag
// line, counting the tag lines above it.
func TestTheTreeScrollsByTheLinesItsTagsTake(t *testing.T) {
	var boxes []Sandbox
	for i := range 6 {
		boxes = append(boxes, Sandbox{ID: fmt.Sprintf("sbx_%d", i), Name: fmt.Sprintf("box %d", i), State: StateRunning, Tags: []string{"t"}})
	}
	l := listForTest(Session{}, boxes)
	l.width, l.height, l.tagLines = treeWidth-2, 7, true
	l.moveTo(5)
	lines := strings.Split(ansi.Strip(l.view(newStyles(true), &zones{}, true)), "\n")
	text := strings.Join(lines, "\n")
	if !strings.Contains(text, "❯ ●   box 5") || strings.Count(text, "#t") < 3 {
		t.Fatalf("the cursor's row, or the tag lines, are off the tree:\n%s", text)
	}
	if strings.Contains(text, "box 2") {
		t.Fatalf("the tree draws more rows than it has lines for:\n%s", text)
	}
}

// The workspace header carries the box's tags it does not say otherwise, dim:
// not the issue and pull request it links, nor a pull request status, which
// it says through the git word.
func TestTheWorkspaceHeaderCarriesTheBoxsOtherTags(t *testing.T) {
	t.Parallel()
	st := newStyles(true)
	m := newColorModel(t, newFakeSource())
	m.paneBox = Sandbox{ID: "sbx_one", Name: "one", Repository: "https://github.com/acme/disco",
		Tags: []string{"issue=4", "ready", "area=tui", "kind=bug"}}
	got := m.paneHeaderFields().text()
	if !strings.Contains(got, st.dimText.Render("#area=tui #kind=bug")) {
		t.Fatalf("header = %q, want the box's other tags dim in it", got)
	}
	if strings.Contains(got, "#issue") || strings.Contains(got, "#ready") {
		t.Fatalf("header = %q, want the issue and the status said once, their own way", got)
	}
	m.paneBox.Tags = []string{"issue=4"}
	if got := m.paneHeaderFields(); len(got) != 2 {
		t.Fatalf("header = %q, want the id and the issue and no tags field for a box with none left to say", got.text())
	}
}
