package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// errNotLoggedIn is what the GitHub client answers a change with no token.
var errNotLoggedIn = errors.New("not logged in to GitHub: run gh auth login, or set GH_TOKEN")

// testIssue is issue 4 of acme/foo, with something of everything its page
// shows.
func testIssue() Issue {
	opened := time.Now().Add(-72 * time.Hour)
	return Issue{
		Repository:     "acme/foo",
		Number:         4,
		Title:          "The reaper eats the wrong boxes",
		State:          "open",
		Author:         "alice",
		Body:           "It reaps **running** boxes.\n\n- one\n- two",
		CreatedAt:      opened,
		Labels:         []IssueLabel{{Name: "bug", Color: "d73a4a"}},
		Assignees:      []string{"bob"},
		Milestone:      "v1",
		Comments:       1,
		Reactions:      IssueReactions{"+1": 2},
		SubIssues:      []IssueRef{{Repository: "acme/foo", Number: 5, Title: "Write a failing test", State: "closed"}},
		SubIssuesTotal: 1,
		SubIssuesDone:  1,
		Timeline: []IssueEvent{
			{Kind: "labeled", Actor: "carol", At: opened.Add(time.Hour), Label: &IssueLabel{Name: "bug"}},
			{Kind: "commented", Actor: "bob", At: opened.Add(2 * time.Hour), Body: "I can reproduce it."},
			{Kind: "cross-referenced", Actor: "dave", At: opened.Add(3 * time.Hour),
				Source: &IssueRef{Repository: "acme/foo", Number: 9, Title: "Reap only stopped boxes", State: "open", PullRequest: true}},
		},
	}
}

func issueSource() *fakeSource {
	ds := newFakeSource(githubTaggedSandboxes()...)
	ds.issues = map[string]Issue{"https://github.com/acme/foo#4": testIssue()}
	return ds
}

// issueTabOf is the open issue tab, or nil.
func issueTabOf(m *Model) *pane {
	for _, p := range m.shells.all() {
		if p.issue != nil {
			return p
		}
	}
	return nil
}

// openIssuePane opens the workspace on the tagged discobox and the issue tab
// beside it, from the keyboard.
func openIssuePane(t *testing.T, ds *fakeSource) (*driver, *Model) {
	t.Helper()
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key(m.leader())
	d.key(issueKey)
	d.wait("the issue", func() bool { return strings.Contains(plainFrame(m), "The reaper eats the wrong boxes") })
	return d, m
}

// A plain click on the header's issue opens it as a tab on the right rather
// than in a browser, and the tab says what GitHub's page does. The pull
// request is still the page.
func TestClickingTheHeaderIssueOpensItAsATab(t *testing.T) {
	t.Parallel()
	ds := issueSource()
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the header", func() bool { return strings.Contains(plainFrame(m), "issue #4 · PR #9") })
	opened := make(chan string, 4)
	m.openOS = func(url string) error { opened <- url; return nil }

	x, y := at(t, m, "issue #4")
	tap(t, m, x, y)
	d.wait("the issue", func() bool { return strings.Contains(plainFrame(m), "The reaper eats the wrong boxes") })
	select {
	case got := <-opened:
		t.Fatalf("clicking issue #4 opened %q, want the issue tab", got)
	default:
	}
	p := issueTabOf(m)
	if p == nil || m.focusedPane() != p || !m.onShells || !m.split() {
		t.Fatalf("the issue is not the focused tab of the right column: tab %v, focused %v, onShells %v, split %v",
			p != nil, m.focusedPane() == p, m.onShells, m.split())
	}
	term, shells := m.columns(m.shells.len())
	if m.paneWidthOf(m.primary()) != term || m.paneWidthOf(p) != shells {
		t.Errorf("the issue is not drawn as a column of the split")
	}

	frame := plainFrame(m)
	for _, want := range []string{
		"issue #4", "● Open", "alice", "opened this issue 3d ago",
		"Assignees", "bob", "Labels", "[bug]", "Milestone", "v1",
		"Sub-issues", "1 of 1 done", "#5 Write a failing test",
		"It reaps", "• one", "👍 2",
		"carol added the bug label",
		"bob commented", "I can reproduce it.",
		"dave mentioned this in", "#9 Reap only",
	} {
		if !strings.Contains(frame, want) {
			t.Errorf("the issue tab does not say %q:\n%s", want, frame)
		}
	}

	// Clicking it again goes to the tab rather than opening another.
	m.focusPane(m.primary())
	x, y = at(t, m, "issue #4")
	slowClock(m)
	tap(t, m, x, y)
	if issueTabs := len(m.shells.all()); issueTabs != 1 || m.focusedPane() != p {
		t.Errorf("a second click: %d tabs, focused on the issue %v; want the one tab, focused", issueTabs, m.focusedPane() == p)
	}

	x, y = at(t, m, "PR #9")
	tap(t, m, x, y)
	if got := <-opened; got != "https://github.com/acme/foo/pull/9" {
		t.Errorf("clicking PR #9 opened %q, want the pull request", got)
	}
}

// Ctrl-click on the header's issue is the terminal's: it follows the link, and
// the window opens nothing of its own.
func TestCtrlClickingTheHeaderIssueLeavesItToTheTerminal(t *testing.T) {
	t.Parallel()
	ds := issueSource()
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the header", func() bool { return strings.Contains(plainFrame(m), "issue #4") })
	x, y := at(t, m, "issue #4")
	send(t, m,
		tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft, Mod: tea.ModCtrl},
		tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft, Mod: tea.ModCtrl},
	)
	if issueTabOf(m) != nil {
		t.Errorf("Ctrl-click opened the issue tab, want the terminal to follow the link")
	}
}

// The issue tab is a tab like any other: the leader's arrows move off it and
// back, its keys are its own and never the terminal's, and q, like the [x],
// closes it and gives the terminal the window back.
func TestTheIssueIsATabLikeAnyOther(t *testing.T) {
	t.Parallel()
	ds := issueSource()
	d, m := openIssuePane(t, ds)
	p := issueTabOf(m)
	primary := ds.execTerm(ExecPrimary)

	if !strings.Contains(hintLine(m.hints()), "comment") {
		t.Errorf("the keys line does not offer a comment: %q", hintLine(m.hints()))
	}
	d.key("j")
	d.key("k")
	primary.mu.Lock()
	sent := string(primary.input)
	primary.mu.Unlock()
	if strings.Contains(sent, "j") || strings.Contains(sent, "k") {
		t.Errorf("keys typed at the issue reached the terminal: %q", sent)
	}
	d.key(m.leader())
	d.key("left")
	if m.focusedPane() != m.primary() {
		t.Fatalf("leader ← from the issue did not reach the terminal")
	}
	d.key(m.leader())
	d.key(issueKey)
	if m.focusedPane() != p {
		t.Fatalf("leader %s from the terminal did not go to the open issue", issueKey)
	}
	d.key(m.leader())
	d.key(paneEndKey)
	if issueTabOf(m) != nil {
		t.Fatalf("leader %s left the issue tab open", paneEndKey)
	}
	if got := m.paneWidthOf(m.primary()); got != m.width {
		t.Errorf("the terminal is %d wide after the issue closed, want the window's %d", got, m.width)
	}
	d.key(m.leader())
	d.key(issueKey)
	d.wait("the issue again", func() bool { return issueTabOf(m) != nil })
	d.key("q")
	if issueTabOf(m) != nil {
		t.Fatalf("q left the issue tab open")
	}
}

// The workspace's keys behind the leader work from the issue tab, as from any.
func TestTheWorkspaceKeysWorkFromTheIssue(t *testing.T) {
	t.Parallel()
	ds := issueSource()
	d, m := openIssuePane(t, ds)
	d.key(m.leader())
	d.key(auditKey)
	if m.audit == nil {
		t.Fatalf("leader %s from the issue did not open the audit screen", auditKey)
	}
}

// A discobox with no issue says so rather than opening an empty tab.
func TestTheLeaderSaysWhenThereIsNoIssue(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key(m.leader())
	d.key(issueKey)
	if issueTabOf(m) != nil {
		t.Fatalf("an issue tab opened on a discobox with no issue")
	}
	if !strings.Contains(m.status, "no issue to show") {
		t.Errorf("status = %q, want it to say there is no issue", m.status)
	}
}

// c writes a comment, Enter is a newline in it, and Ctrl-S posts it; the field
// empties and the issue is read again to show it.
func TestACommentIsWrittenAndPosted(t *testing.T) {
	t.Parallel()
	ds := issueSource()
	d, m := openIssuePane(t, ds)
	v := issueTabOf(m).issue
	ds.mu.Lock()
	reads := ds.issueReads
	ds.mu.Unlock()

	d.key("c")
	if !v.composing {
		t.Fatalf("c did not open the comment field")
	}
	d.dispatch(tea.PasteMsg{Content: "Fixed in"})
	d.key("enter")
	for _, r := range "#9" {
		d.key(string(r))
	}
	if !strings.Contains(plainFrame(m), "Comment on #4") {
		t.Errorf("the comment field is not drawn:\n%s", plainFrame(m))
	}
	d.key(issuePostKey)
	d.wait("the comment", func() bool {
		ds.mu.Lock()
		defer ds.mu.Unlock()
		return len(ds.comments) == 1
	})
	if got, want := ds.comments[0], "https://github.com/acme/foo#4: Fixed in\n#9"; got != want {
		t.Errorf("posted %q, want %q", got, want)
	}
	d.wait("the issue read again", func() bool {
		ds.mu.Lock()
		defer ds.mu.Unlock()
		return ds.issueReads > reads && !v.loading
	})
	if v.composing || v.composer.Value() != "" {
		t.Errorf("after posting: composing %v, field %q; want it put away and empty", v.composing, v.composer.Value())
	}
}

// A comment GitHub refuses is kept, every word, with the reason.
func TestARefusedCommentIsKept(t *testing.T) {
	t.Parallel()
	ds := issueSource()
	ds.commentErr = errNotLoggedIn
	d, m := openIssuePane(t, ds)
	v := issueTabOf(m).issue
	d.key("c")
	d.dispatch(tea.PasteMsg{Content: "Looks good"})
	d.key(issuePostKey)
	d.wait("the refusal", func() bool { return strings.Contains(m.status, "not logged in to GitHub") })
	if got := v.composer.Value(); got != "Looks good" {
		t.Errorf("the field holds %q after the refusal, want the comment kept", got)
	}
	// Esc steps out of the field and keeps what was written.
	d.key("esc")
	if v.composing || v.composer.Value() != "Looks good" {
		t.Errorf("Esc: composing %v, field %q; want the field put away and kept", v.composing, v.composer.Value())
	}
}

// x closes an open issue, asking which way, and reopens a closed one.
func TestTheIssueIsClosedAndReopened(t *testing.T) {
	t.Parallel()
	ds := issueSource()
	d, m := openIssuePane(t, ds)
	if !strings.Contains(hintLine(m.hints()), "close issue") {
		t.Errorf("the keys line does not offer closing the issue: %q", hintLine(m.hints()))
	}
	d.key("x")
	if m.dialog == nil || !strings.Contains(plainFrame(m), "close as not planned") {
		t.Fatalf("x did not ask how to close it:\n%s", plainFrame(m))
	}
	d.key("n")
	d.wait("the close", func() bool { return strings.Contains(plainFrame(m), "Closed as not planned") })
	ds.mu.Lock()
	states := append([]string(nil), ds.states...)
	ds.mu.Unlock()
	if len(states) != 1 || states[0] != "https://github.com/acme/foo#4: closed/not_planned" {
		t.Fatalf("states = %v, want it closed as not planned", states)
	}
	if !strings.Contains(hintLine(m.hints()), "reopen issue") {
		t.Errorf("a closed issue's keys line does not offer reopening it: %q", hintLine(m.hints()))
	}

	d.key("x")
	if m.dialog == nil || !strings.Contains(plainFrame(m), "Reopen #4?") {
		t.Fatalf("x on a closed issue did not ask to reopen it:\n%s", plainFrame(m))
	}
	d.key("y")
	d.wait("the reopen", func() bool { return strings.Contains(plainFrame(m), "● Open") })
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if len(ds.states) != 2 || ds.states[1] != "https://github.com/acme/foo#4: open/reopened" {
		t.Errorf("states = %v, want it reopened", ds.states)
	}
}

// A close GitHub refuses says why, and the issue is as it was.
func TestARefusedCloseSaysWhy(t *testing.T) {
	t.Parallel()
	ds := issueSource()
	ds.stateErr = errNotLoggedIn
	d, m := openIssuePane(t, ds)
	d.key("x")
	d.key("c")
	d.wait("the refusal", func() bool { return strings.Contains(m.status, "#4 was not closed") })
	if !strings.Contains(plainFrame(m), "● Open") {
		t.Errorf("the issue does not still read as open:\n%s", plainFrame(m))
	}
}

// An issue that cannot be read says why, in the tab.
func TestAnUnreadableIssueSaysWhy(t *testing.T) {
	t.Parallel()
	ds := issueSource()
	ds.issueErr = errors.New("GitHub answered 404: Not Found")
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key(m.leader())
	d.key(issueKey)
	d.wait("the failure", func() bool { return strings.Contains(plainFrame(m), "could not read the issue") })
	if !strings.Contains(plainFrame(m), "404") {
		t.Errorf("the tab does not say why:\n%s", plainFrame(m))
	}
}

// The wheel over the issue scrolls it, and the keys do too.
func TestTheIssueScrolls(t *testing.T) {
	t.Parallel()
	ds := issueSource()
	issue := testIssue()
	issue.Body = strings.Repeat("A paragraph of the description.\n\n", 60)
	ds.issues = map[string]Issue{"https://github.com/acme/foo#4": issue}
	d, m := openIssuePane(t, ds)
	v := issueTabOf(m).issue
	if v.offset != 0 {
		t.Fatalf("the issue opened scrolled to %d", v.offset)
	}
	x, y := at(t, m, "The reaper eats")
	send(t, m, tea.MouseWheelMsg{X: x, Y: y + 2, Button: tea.MouseWheelDown})
	if v.offset == 0 {
		t.Errorf("the wheel over the issue did not scroll it")
	}
	d.key("G")
	end := v.offset
	d.key("j")
	if v.offset != end {
		t.Errorf("j past the end moved to %d from %d", v.offset, end)
	}
	d.key("g")
	if v.offset != 0 {
		t.Errorf("g left the issue at %d, want the top", v.offset)
	}
}

// Leaving the workspace closes the issue tab with the rest.
func TestDetachingClosesTheIssue(t *testing.T) {
	t.Parallel()
	ds := issueSource()
	d, m := openIssuePane(t, ds)
	d.key(m.leader())
	d.key(paneDetachAlt)
	d.wait("the list", func() bool { return m.focus != focusPane })
	if issueTabOf(m) != nil {
		t.Errorf("the issue tab outlived the workspace")
	}
}

// The words an event is drawn with, for the kinds the page has words for.
func TestEventsReadAsThePageWritesThem(t *testing.T) {
	t.Parallel()
	st := newStyles(false)
	for _, tc := range []struct {
		event IssueEvent
		want  string
	}{
		{IssueEvent{Kind: "assigned", Actor: "bob", Subject: "bob"}, "self-assigned this"},
		{IssueEvent{Kind: "assigned", Actor: "bob", Subject: "carol"}, "assigned carol"},
		{IssueEvent{Kind: "closed", StateReason: "not_planned"}, "closed this as not planned"},
		{IssueEvent{Kind: "closed", Subject: "abc1234"}, "closed this as completed in abc1234"},
		{IssueEvent{Kind: "renamed", From: "a", To: "b"}, "changed the title from “a” to “b”"},
		{IssueEvent{Kind: "cross-referenced", Source: &IssueRef{Repository: "acme/bar", Number: 3, Title: "x"}}, "mentioned this in ○ acme/bar#3 x"},
		{IssueEvent{Kind: "some_new_event"}, "some new event"},
	} {
		if got := eventText(st, tc.event, "acme/foo"); got != tc.want {
			t.Errorf("eventText(%s) = %q, want %q", tc.event.Kind, got, tc.want)
		}
	}
}

// Labels wrap between chips, never inside one.
func TestLabelsWrapBetweenChips(t *testing.T) {
	t.Parallel()
	ds := issueSource()
	issue := testIssue()
	issue.Labels = []IssueLabel{{Name: "bug"}, {Name: "area/cli"}, {Name: "area/pool-agent"}, {Name: "area/sandbox-agent"}, {Name: "priority/high"}}
	ds.issues = map[string]Issue{"https://github.com/acme/foo#4": issue}
	_, m := openIssuePane(t, ds)
	frame := plainFrame(m)
	for _, label := range issue.Labels {
		if !strings.Contains(frame, "["+label.Name+"]") {
			t.Errorf("[%s] is not drawn whole:\n%s", label.Name, frame)
		}
	}
}

// A link is found where it is drawn, in cells, whatever styling is round it.
func TestLinksAreMeasuredInCells(t *testing.T) {
	t.Parallel()
	line := "\x1b[1mé\x1b[0m " + hyperlink("https://a.example/x", "\x1b[4mdocs\x1b[0m") + " and " + hyperlink("https://b.example", "b")
	var got []string
	for _, link := range lineLinks(line) {
		got = append(got, fmt.Sprintf("%s@%d+%d=%s", link.url, link.x, link.width, ansi.Strip(line[link.from:link.to])))
	}
	want := []string{"https://a.example/x@2+4=docs", "https://b.example@11+1=b"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("lineLinks = %v, want %v", got, want)
	}
}

// A link in a comment is a control: a plain click on it opens it.
func TestALinkInACommentOpensOnAClick(t *testing.T) {
	t.Parallel()
	ds := issueSource()
	issue := testIssue()
	issue.Body = "See [the design notes](https://example.com/notes) first."
	ds.issues = map[string]Issue{"https://github.com/acme/foo#4": issue}
	_, m := openIssuePane(t, ds)
	opened := make(chan string, 2)
	m.openOS = func(url string) error { opened <- url; return nil }
	if !strings.Contains(rawFrame(m), "https://example.com/notes") {
		t.Fatalf("the comment's link is not drawn as one:\n%q", rawFrame(m))
	}
	x, y := at(t, m, "the design notes")
	tap(t, m, x+2, y)
	select {
	case got := <-opened:
		if got != "https://example.com/notes" {
			t.Errorf("the click opened %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Errorf("a click on the link opened nothing")
	}
}

// The question about closing the issue goes with the tab it is about, and
// with the workspace: answering it after either has gone would change the
// issue from a screen that no longer shows it.
func TestTheCloseQuestionGoesWithItsTab(t *testing.T) {
	t.Parallel()
	ds := issueSource()
	d, m := openIssuePane(t, ds)
	d.key("x")
	if m.dialog == nil {
		t.Fatal("x asked nothing")
	}
	m.closeTab(issueTabOf(m))
	if m.dialog != nil {
		t.Errorf("the close question outlived its tab")
	}
	d.key(m.leader())
	d.key(issueKey)
	d.wait("the issue again", func() bool { return issueTabOf(m) != nil && issueTabOf(m).issue.issue != nil })
	d.key("x")
	m.closeWorkspace()
	if m.dialog != nil {
		t.Errorf("the close question outlived the workspace")
	}
}

// The editor is not opened on a comment that is still posting: what came back
// from it would be cleared by the post landing.
func TestTheEditorWaitsForAPost(t *testing.T) {
	t.Parallel()
	ds := issueSource()
	d, m := openIssuePane(t, ds)
	v := issueTabOf(m).issue
	d.key("c")
	v.posting = true
	if cmd := m.composeKey(issueTabOf(m), keyPress("alt+e")); cmd == nil {
		t.Fatal("alt+e while posting said nothing")
	} else if msg := cmd(); !strings.Contains(fmt.Sprint(msg), "still posting") {
		t.Errorf("alt+e while posting = %v, want it to say the comment is still posting", msg)
	}
}

// Only the latest read is taken: an older one landing after it would put
// back what the newer one replaced.
func TestAnOlderReadLandingLateIsDropped(t *testing.T) {
	t.Parallel()
	ds := issueSource()
	_, m := openIssuePane(t, ds)
	p := issueTabOf(m)
	stale := p.issue.reads
	m.loadIssue(p, false) // a newer read, in flight
	old := testIssue()
	old.Title = "what it was called before"
	m.issueLoaded(issueLoadedMsg{id: p.id, read: stale, issue: old})
	if p.issue.issue.Title == old.Title {
		t.Errorf("an older read replaced the issue")
	}
	if !p.issue.loading {
		t.Errorf("an older read ended the newer one's wait")
	}
}

// Only a web link is opened by a click: a stranger writes these, and this
// machine's URL handler opens a file:// or a custom scheme as readily as a page.
func TestOnlyAWebLinkInACommentOpens(t *testing.T) {
	t.Parallel()
	ds := issueSource()
	issue := testIssue()
	issue.Body = "Open [the notes](file:///etc/passwd) or [an editor](vscode://x/y)."
	ds.issues = map[string]Issue{"https://github.com/acme/foo#4": issue}
	_, m := openIssuePane(t, ds)
	opened := make(chan string, 2)
	m.openOS = func(url string) error { opened <- url; return nil }
	for _, text := range []string{"the notes", "an editor"} {
		x, y := at(t, m, text)
		slowClock(m)
		tap(t, m, x+1, y)
	}
	select {
	case got := <-opened:
		t.Errorf("a click opened %q", got)
	case <-time.After(200 * time.Millisecond):
	}
	// Nor is either drawn as a link, which the terminal's own Ctrl-click
	// would follow.
	if frame := rawFrame(m); strings.Contains(frame, "\x1b]8;;file://") || strings.Contains(frame, "\x1b]8;;vscode://") {
		t.Errorf("a non-web link is still drawn as an OSC 8:\n%q", frame)
	}
}

// A link the pointer rests on is lit, from its bare text, inside the
// sequences that make it a link.
func TestALinkUnderThePointerIsLit(t *testing.T) {
	t.Parallel()
	st := newStyles(true)
	line := "see " + hyperlink("https://a.example", st.info.Render("docs")) + " now"
	links := lineLinks(line)
	if len(links) != 1 {
		t.Fatalf("lineLinks = %v", links)
	}
	lit := litLink(st, line, links[0])
	if !strings.Contains(lit, st.hover.Render("docs")) || !strings.Contains(lit, "https://a.example") || ansi.Strip(lit) != "see docs now" {
		t.Errorf("litLink = %q", lit)
	}
}

// A comment is posted as written: an indented first line is a code block.
func TestACommentIsPostedAsWritten(t *testing.T) {
	t.Parallel()
	ds := issueSource()
	d, m := openIssuePane(t, ds)
	d.key("c")
	d.dispatch(tea.PasteMsg{Content: "    go test ./...\n\nfails here"})
	d.key(issuePostKey)
	d.wait("the comment", func() bool {
		ds.mu.Lock()
		defer ds.mu.Unlock()
		return len(ds.comments) == 1
	})
	if got, want := ds.comments[0], "https://github.com/acme/foo#4:     go test ./...\n\nfails here"; got != want {
		t.Errorf("posted %q, want %q", got, want)
	}
	_ = m
}

// The sub-issue count is GitHub's own: a list cut short says how much of it
// is shown rather than claiming more done than there are.
func TestASubIssueListCutShortKeepsGitHubsCount(t *testing.T) {
	t.Parallel()
	ds := issueSource()
	issue := testIssue()
	issue.SubIssuesTotal, issue.SubIssuesDone = 150, 120
	ds.issues = map[string]Issue{"https://github.com/acme/foo#4": issue}
	_, m := openIssuePane(t, ds)
	if frame := plainFrame(m); !strings.Contains(frame, "120 of 150 done · 1 shown") {
		t.Errorf("the sub-issue summary does not say GitHub's count:\n%s", frame)
	}
}

// Nor does a list that could not be read at all hide that there are any.
func TestSubIssuesThatCouldNotBeReadAreStillCounted(t *testing.T) {
	t.Parallel()
	ds := issueSource()
	issue := testIssue()
	issue.SubIssues, issue.SubIssuesTotal, issue.SubIssuesDone = nil, 150, 120
	ds.issues = map[string]Issue{"https://github.com/acme/foo#4": issue}
	_, m := openIssuePane(t, ds)
	if frame := plainFrame(m); !strings.Contains(frame, "120 of 150 done · 0 shown") {
		t.Errorf("the sub-issue summary is not drawn:\n%s", frame)
	}
}
