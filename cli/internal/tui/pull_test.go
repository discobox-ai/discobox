package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// testPull is pull request 9 of acme/foo, with something of everything its
// page shows beyond an issue's.
func testPull() Issue {
	opened := time.Now().Add(-48 * time.Hour)
	return Issue{
		Repository:  "acme/foo",
		Number:      9,
		Title:       "Reap only stopped boxes",
		State:       "open",
		Author:      "alice",
		Body:        "Fixes #4.",
		CreatedAt:   opened,
		Comments:    1,
		PullRequest: true,
		Pull: &PullRequest{
			Head: "fix-reaper", Base: "main", HeadSHA: "abc1234567890", BaseSHA: "0001112223334",
			MergeMethods: []string{"squash", "rebase"},
			Commits:      2, Additions: 12, Deletions: 3, ChangedFiles: 3,
			Mergeable: "blocked",
			Reviewers: []PullReviewer{{Name: "bob", State: "changes_requested"}, {Name: "erin", State: "requested"}},
			Checks: []PullCheck{
				{Name: "build", State: "success", URL: "https://ci.example/build"},
				{Name: "lint", State: "failure", URL: "https://ci.example/lint"},
				{Name: "e2e", State: "pending", URL: "file:///etc/passwd"},
			},
		},
		Timeline: []IssueEvent{
			{Kind: "committed", Actor: "Alice A", At: opened, Subject: "abc1234", Message: "reap only the stopped"},
			{Kind: "committed", Actor: "Alice A", At: opened, Subject: "def5678", Message: "test it"},
			{Kind: "review_requested", Actor: "alice", At: opened.Add(time.Hour), Subject: "bob"},
			{Kind: "reviewed", Actor: "bob", At: opened.Add(2 * time.Hour), Review: "changes_requested", Body: "Nearly.",
				Threads: []ReviewThread{{
					Path: "reaper/reaper.go", Line: 3, Hunk: "@@ -1,3 +1,4 @@\n func reap() {\n-\tall()\n+\tstopped()",
					Comments: []ReviewComment{
						{Author: "bob", At: opened.Add(2 * time.Hour), Body: "What about paused ones?"},
						{Author: "alice", At: opened.Add(3 * time.Hour), Body: "Left alone."},
					},
				}}},
			{Kind: "commented", Actor: "carol", At: opened.Add(4 * time.Hour), Body: "Thanks for this."},
			{Kind: "head_ref_force_pushed", Actor: "alice", At: opened.Add(5 * time.Hour)},
		},
	}
}

// testPullDiff is pull request 9's files changed at its head.
const testPullDiff = "diff --git a/reaper/reaper.go b/reaper/reaper.go\n--- a/reaper/reaper.go\n+++ b/reaper/reaper.go\n" +
	"@@ -1,3 +1,3 @@\n func reap() {\n-\tall()\n+\tstopped()\n }\n" +
	"diff --git a/reaper/reaper_test.go b/reaper/reaper_test.go\nnew file mode 100644\n--- /dev/null\n+++ b/reaper/reaper_test.go\n" +
	"@@ -0,0 +1,2 @@\n+package reaper\n+// tested\n"

func pullSource() *fakeSource {
	ds := issueSource()
	ds.issues["https://github.com/acme/foo#9"] = testPull()
	ds.pullDiffs = map[string]string{"abc1234567890": testPullDiff}
	return ds
}

// openPullPane opens the workspace on the tagged discobox and the pull request
// tab beside it, from the keyboard.
func openPullPane(t *testing.T, ds *fakeSource) (*driver, *Model) {
	t.Helper()
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key(m.leader())
	d.key(pullKey)
	d.wait("the pull request", func() bool { return strings.Contains(plainFrame(m), "Reap only stopped boxes") })
	return d, m
}

// A plain click on the header's pull request opens it as a tab on the right,
// like the issue beside it, and the tab says what GitHub's page does.
func TestClickingTheHeaderPullRequestOpensItAsATab(t *testing.T) {
	t.Parallel()
	ds := pullSource()
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the header", func() bool { return strings.Contains(plainFrame(m), "issue #4 · PR #9") })
	opened := make(chan string, 4)
	m.openOS = func(url string) error { opened <- url; return nil }

	x, y := at(t, m, "PR #9")
	tap(t, m, x, y)
	d.wait("the pull request", func() bool { return strings.Contains(plainFrame(m), "Reap only stopped boxes") })
	select {
	case got := <-opened:
		t.Fatalf("clicking PR #9 opened %q, want the pull request tab", got)
	default:
	}
	p := issueTabOf(m)
	if p == nil || m.focusedPane() != p || p.name() != "PR #9" {
		t.Fatalf("the pull request is not the focused tab, named for it: %v", p)
	}
	if got := p.issue.url; got != "https://github.com/acme/foo/pull/9" {
		t.Errorf("the tab's page is %q, want the pull request's", got)
	}

	frame := tightFrame(m)
	for _, want := range []string{
		"● Open", "alice wants to merge 2 commits into main from", "fix-reaper · 2d ago · 1 comment",
		"Reviewers", "● bob requested changes", "○ erin review requested",
		"Changes", "3 files · 2 commits · +12 −3",
		"Checks", "1 failing · 1 pending · 1 passed", "● lint", "● e2e",
		"● Not ready to merge — a required review or check is",
		"Fixes #4.",
		"Alice A abc1234 reap only the stopped", "Alice A def5678 test it",
		"alice requested a review from bob",
	} {
		if !strings.Contains(frame, want) {
			t.Errorf("the pull request tab does not say %q:\n%s", want, frame)
		}
	}
	if strings.Contains(frame, "build") {
		t.Errorf("a check that passed is listed with the ones to go and read:\n%s", frame)
	}

	// The review and its thread are further down than the window is tall.
	d.key("G")
	frame = tightFrame(m)
	for _, want := range []string{
		"bob requested changes", "Nearly.",
		"reaper/reaper.go:3", "+ stopped()", "What about paused ones?", "Left alone.",
		"carol commented", "alice force-pushed the fix-reaper branch",
	} {
		if !strings.Contains(frame, want) {
			t.Errorf("the foot of the pull request does not say %q:\n%s", want, frame)
		}
	}
}

// The leader's p is the pull request, as its i is the issue, and both can be
// open at once: they are different tabs.
func TestTheLeaderOpensThePullRequest(t *testing.T) {
	t.Parallel()
	ds := pullSource()
	d, m := openPullPane(t, ds)
	pull := issueTabOf(m)
	if !strings.Contains(hintLine(m.hints()), "files changed") {
		t.Errorf("the keys line does not offer the files changed: %q", hintLine(m.hints()))
	}
	d.key(m.leader())
	d.key(issueKey)
	d.wait("the issue", func() bool { return strings.Contains(plainFrame(m), "The reaper eats the wrong boxes") })
	if got := len(m.shells.all()); got != 2 {
		t.Fatalf("%d tabs with the issue and the pull request open, want 2", got)
	}
	d.key(m.leader())
	d.key(pullKey)
	if m.focusedPane() != pull || len(m.shells.all()) != 2 {
		t.Errorf("leader %s with the pull request open did not go back to its tab", pullKey)
	}
}

// A discobox with no pull request says so rather than opening an empty tab.
func TestTheLeaderSaysWhenThereIsNoPullRequest(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key(m.leader())
	d.key(pullKey)
	if issueTabOf(m) != nil {
		t.Fatalf("a tab opened on a discobox with no pull request")
	}
	if !strings.Contains(m.status, "no pull request to show") {
		t.Errorf("status = %q, want it to say there is no pull request", m.status)
	}
}

// f turns the tab to the files changed and back, each page keeping its place.
// The files changed are discobox-review's window over the pull request
// between the two commits the tab read. It is a reader: nothing in it
// comments, approves or checks out, and the tab's way back is its own key.
func TestThePullRequestShowsItsFilesChanged(t *testing.T) {
	t.Parallel()
	ds := pullSource()
	d, m := openPullPane(t, ds)
	v := issueTabOf(m).issue
	d.key("j")
	read := v.conversation.offset

	d.key("f")
	d.wait("the files changed", func() bool { return strings.Contains(plainFrame(m), "reaper/reaper_test.go") })
	// The window opens with its files folded, and Space unfolds the one
	// under its cursor, as in any discobox's diff.
	d.key(" ")
	frame := plainFrame(m)
	for _, want := range []string{"reaper/reaper.go", "reaper/reaper_test.go", "all()", "stopped()"} {
		if !strings.Contains(frame, want) {
			t.Errorf("the files changed do not show %q:\n%s", want, frame)
		}
	}
	if want := []string{"https://github.com/acme/foo#9: 0001112223334...abc1234567890"}; !slices.Equal(ds.pullSources, want) {
		t.Errorf("the files changed were read as %q, want %q", ds.pullSources, want)
	}
	hints := hintLine(m.hints())
	if !strings.Contains(hints, "f conversation") || strings.Contains(hints, "close PR") || strings.Contains(hints, "comment") {
		t.Errorf("the keys line is %q, want the way back and none of the conversation's", hints)
	}
	if got := v.pageURL(); got != "https://github.com/acme/foo/pull/9/files" {
		t.Errorf("o would open %q, want the files changed", got)
	}

	// The window's keys are the window's: c is a comment in discobox-review,
	// and over a pull request there is nowhere to put one.
	d.key("c")
	if v.composing {
		t.Errorf("c on the files changed opened the tab's comment field")
	}

	d.key("f")
	if v.files || v.conversation.offset != read {
		t.Errorf("f again: files %v, the conversation at %d; want it back where it was read to, %d", v.files, v.conversation.offset, read)
	}
	// Back again is the same window, not another read of GitHub.
	d.key("f")
	if !v.files || len(ds.pullSources) != 1 {
		t.Errorf("f a second time: files %v, %d sources opened; want the window that was open", v.files, len(ds.pullSources))
	}
	// q is the window's close, which is the way back too.
	d.key("q")
	if v.files || issueTabOf(m) == nil {
		t.Errorf("q in the window: files %v, tab open %v; want the conversation", v.files, issueTabOf(m) != nil)
	}
}

// A pull request whose files changed cannot be opened says why, and the tab
// stays on the conversation.
func TestFilesChangedThatCannotBeOpenedSayWhy(t *testing.T) {
	t.Parallel()
	ds := pullSource()
	ds.pullSourceErr = errors.New("acme/foo is not a GitHub repository")
	d, m := openPullPane(t, ds)
	v := issueTabOf(m).issue
	d.key("f")
	if v.files || v.review != nil {
		t.Errorf("a source that failed left the tab on files %v, window %v", v.files, v.review != nil)
	}
	if frame := plainFrame(m); !strings.Contains(frame, "cannot show the files changed: acme/foo is not") {
		t.Errorf("the failure is not said:\n%s", frame)
	}
}

// A pull request's diff is a stranger's text, and the files changed draw it
// as it came from GitHub: the window, not the source, is what keeps an escape
// sequence in it from reaching the terminal.
func TestTheFilesChangedDrawAStrangersEscapesAsText(t *testing.T) {
	t.Parallel()
	ds := pullSource()
	evil := "\x1b]8;;https://evil.example\x07click\x1b]8;;\x07 \x1b]52;c;cm0=\x07 \x1b[2J"
	ds.pullDiffs["abc1234567890"] = "diff --git a/f.go b/f.go\n--- a/f.go\n+++ b/f.go\n@@ -1 +1 @@\n-old\n+new " + evil + "\n"
	d, m := openPullPane(t, ds)
	d.key("f")
	d.wait("the files changed", func() bool { return strings.Contains(plainFrame(m), "f.go") })
	d.key(" ")
	d.wait("the diff", func() bool { return strings.Contains(plainFrame(m), "new") })
	raw := rawFrame(m)
	for _, sequence := range []string{"\x1b]8;;https://evil", "\x1b]52", "\x1b[2J"} {
		if strings.Contains(raw, sequence) {
			t.Errorf("the diff's %q reached the terminal:\n%s", sequence, plainFrame(m))
		}
	}
}

// x closes an open pull request without merging it — one way, so one question
// — and reopens a closed one; a merged one is past both.
func TestThePullRequestIsClosedAndReopened(t *testing.T) {
	t.Parallel()
	ds := pullSource()
	d, m := openPullPane(t, ds)
	if !strings.Contains(hintLine(m.hints()), "close PR") {
		t.Errorf("the keys line does not offer closing the pull request: %q", hintLine(m.hints()))
	}
	d.key("x")
	if m.dialog == nil || !strings.Contains(plainFrame(m), "Close #9 without merging it?") {
		t.Fatalf("x did not ask before closing it:\n%s", plainFrame(m))
	}
	d.key("y")
	// The tab goes to its foot to show what was done, so the keys line is
	// what says how it stands.
	d.wait("the close", func() bool { return strings.Contains(hintLine(m.hints()), "reopen PR") })
	d.key("g")
	if !strings.Contains(plainFrame(m), "○ Closed") {
		t.Errorf("the closed pull request's badge does not say so:\n%s", plainFrame(m))
	}
	d.key("x")
	if m.dialog == nil || !strings.Contains(plainFrame(m), "Reopen #9?") {
		t.Fatalf("x on a closed pull request did not ask to reopen it:\n%s", plainFrame(m))
	}
	d.key("y")
	d.wait("the reopen", func() bool { return strings.Contains(hintLine(m.hints()), "close PR") })
	ds.mu.Lock()
	states := append([]string(nil), ds.states...)
	ds.mu.Unlock()
	if len(states) != 2 || states[0] != "https://github.com/acme/foo#9: closed/" || states[1] != "https://github.com/acme/foo#9: open/" {
		t.Errorf("states = %v, want it closed and reopened, with no reason: a pull request has none", states)
	}
}

func TestAMergedPullRequestIsNotClosed(t *testing.T) {
	t.Parallel()
	ds := pullSource()
	pull := testPull()
	pull.State, pull.Pull.Merged, pull.Pull.MergedBy, pull.Pull.MergedAt = "closed", true, "dave", time.Now().Add(-time.Hour)
	pull.Timeline = append(pull.Timeline,
		IssueEvent{Kind: "merged", Actor: "dave", At: pull.Pull.MergedAt, Subject: "fedcba9"},
		IssueEvent{Kind: "closed", Actor: "dave", At: pull.Pull.MergedAt},
	)
	ds.issues["https://github.com/acme/foo#9"] = pull
	d, m := openPullPane(t, ds)
	frame := tightFrame(m)
	for _, want := range []string{"● Merged", "dave merged 2 commits into main from"} {
		if !strings.Contains(frame, want) {
			t.Errorf("the merged pull request does not say %q:\n%s", want, frame)
		}
	}
	if strings.Contains(frame, "to merge") {
		t.Errorf("a merged pull request still says whether it can merge:\n%s", frame)
	}
	if hints := hintLine(m.hints()); strings.Contains(hints, "close PR") || strings.Contains(hints, "reopen PR") {
		t.Errorf("a merged pull request's keys line offers %q", hints)
	}
	d.key("x")
	if m.dialog != nil || !strings.Contains(m.status, "is merged") {
		t.Errorf("x on a merged pull request: dialog %v, status %q; want it to say it is merged", m.dialog != nil, m.status)
	}
	d.key("G")
	frame = tightFrame(m)
	if !strings.Contains(frame, "dave merged commit fedcba9 into main") || strings.Contains(frame, "dave closed this") {
		t.Errorf("the merge is not the one thing said of it:\n%s", frame)
	}
}

// A comment on a pull request is posted the way one on an issue is, and the
// tab comes back to the conversation to show it.
func TestACommentIsPostedOnAPullRequest(t *testing.T) {
	t.Parallel()
	ds := pullSource()
	d, m := openPullPane(t, ds)
	v := issueTabOf(m).issue
	d.key("c")
	if !strings.Contains(plainFrame(m), "Comment on #9") {
		t.Fatalf("c did not open the comment field:\n%s", plainFrame(m))
	}
	for _, r := range "LGTM" {
		d.key(string(r))
	}
	d.key(issuePostKey)
	d.wait("the comment", func() bool {
		ds.mu.Lock()
		defer ds.mu.Unlock()
		return len(ds.comments) == 1
	})
	if got, want := ds.comments[0], "https://github.com/acme/foo#9: LGTM"; got != want {
		t.Errorf("posted %q, want %q", got, want)
	}
	d.wait("the field put away", func() bool { return !v.composing })
}

// What the tag says a number is gives way to what GitHub says: `issue=9` on a
// pull request is a pull request, tab and page.
func TestAnIssueTagOnAPullRequestIsAPullRequest(t *testing.T) {
	t.Parallel()
	boxes := githubTaggedSandboxes()
	boxes[0].Tags = []string{"issue=9"}
	ds := newFakeSource(boxes...)
	ds.issues = map[string]Issue{"https://github.com/acme/foo#9": testPull()}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key(m.leader())
	d.key(issueKey)
	d.wait("the pull request", func() bool { return strings.Contains(plainFrame(m), "Reap only stopped boxes") })
	p := issueTabOf(m)
	if p.name() != "PR #9" || p.issue.url != "https://github.com/acme/foo/pull/9" {
		t.Errorf("the tab is %q onto %q, want the pull request", p.name(), p.issue.url)
	}
	if !strings.Contains(hintLine(m.hints()), "close PR") {
		t.Errorf("the keys line is an issue's: %q", hintLine(m.hints()))
	}
}

// A check is a link to its own page — when that page is on the web. A check's
// URL is whatever its app reported, and a click hands it to this machine.
func TestAFailingCheckIsALinkToItsPage(t *testing.T) {
	t.Parallel()
	ds := pullSource()
	_, m := openPullPane(t, ds)
	opened := make(chan string, 4)
	m.openOS = func(url string) error { opened <- url; return nil }
	if frame := rawFrame(m); !strings.Contains(frame, "https://ci.example/lint") || strings.Contains(frame, "file:///etc/passwd") {
		t.Fatalf("the checks' links are wrong: want lint's and not e2e's:\n%q", frame)
	}
	x, y := at(t, m, "● lint")
	slowClock(m)
	tap(t, m, x+2, y)
	select {
	case got := <-opened:
		if got != "https://ci.example/lint" {
			t.Errorf("clicking the check opened %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Errorf("clicking the failing check opened nothing")
	}
}

// The words a pull request's events are drawn with.
func TestPullRequestEventsReadAsThePageWritesThem(t *testing.T) {
	t.Parallel()
	st := newStyles(false)
	pull := testPull()
	for _, tc := range []struct {
		event IssueEvent
		want  string
	}{
		{IssueEvent{Kind: "closed"}, "closed this"},
		{IssueEvent{Kind: "merged", Subject: "abc1234"}, "merged commit abc1234 into main"},
		{IssueEvent{Kind: "review_requested", Actor: "bob", Subject: "bob"}, "self-requested a review"},
		{IssueEvent{Kind: "review_requested", Actor: "alice", Subject: "bob"}, "requested a review from bob"},
		{IssueEvent{Kind: "ready_for_review"}, "marked this ready for review"},
		{IssueEvent{Kind: "head_ref_deleted"}, "deleted the fix-reaper branch"},
		{IssueEvent{Kind: "labeled", Label: &IssueLabel{Name: "bug"}}, "added the bug label"},
		{IssueEvent{Kind: "copilot_work_started"}, "started a Copilot review"},
		{IssueEvent{Kind: "some_new_event"}, "some new event"},
	} {
		if got := pullEventText(st, tc.event, pull); got != tc.want {
			t.Errorf("pullEventText(%s) = %q, want %q", tc.event.Kind, got, tc.want)
		}
	}
}

// The badge is the page's: a draft, a merge and a close each say so.
func TestThePullRequestBadge(t *testing.T) {
	t.Parallel()
	st := newStyles(false)
	for _, tc := range []struct {
		state         string
		draft, merged bool
		want          string
	}{
		{"open", false, false, "● Open"},
		{"open", true, false, "○ Draft"},
		{"closed", false, false, "○ Closed"},
		{"closed", false, true, "● Merged"},
	} {
		issue := Issue{State: tc.state, Pull: &PullRequest{Draft: tc.draft, Merged: tc.merged}}
		if got := pullState(st, issue); got != tc.want {
			t.Errorf("pullState(%s, draft %v, merged %v) = %q, want %q", tc.state, tc.draft, tc.merged, got, tc.want)
		}
	}
}

// readyPull is the pull request with nothing left in its way.
func readyPull() Issue {
	pull := testPull()
	pull.Pull.Mergeable = "clean"
	return pull
}

// A pull request that can merge says so where it cannot be missed: a bar
// under the title, and a mark on the tab that shows from any pane.
func TestAPullRequestReadyToMergeSaysSo(t *testing.T) {
	t.Parallel()
	ds := pullSource()
	ds.issues["https://github.com/acme/foo#9"] = readyPull()
	d, m := openPullPane(t, ds)
	frame := plainFrame(m)
	if !strings.Contains(frame, "● Ready to merge into main") {
		t.Errorf("the ready pull request does not say so:\n%s", frame)
	}
	if got := issueTabOf(m).name(); got != "PR #9 ●" {
		t.Errorf("the tab is named %q, want the mark on it", got)
	}
	if !strings.Contains(hintLine(m.hints()), "m merge") {
		t.Errorf("the keys line does not offer the merge: %q", hintLine(m.hints()))
	}
	d.key(m.leader())
	d.key("left")
	if !strings.Contains(plainFrame(m), "PR #9 ●") {
		t.Errorf("the mark does not show from the terminal:\n%s", plainFrame(m))
	}
}

// Each way of not being ready says which, in GitHub's terms.
func TestTheMergeBannerSaysWhyNot(t *testing.T) {
	t.Parallel()
	st := newStyles(false)
	for state, want := range map[string]string{
		"dirty":    "● Not ready to merge — conflicts with main must be resolved",
		"behind":   "● Not ready to merge — out of date with main",
		"blocked":  "● Not ready to merge — a required review or check is missing",
		"unstable": "● Not ready to merge — its checks have not passed",
		"unknown":  "○ GitHub has not said yet whether this can merge — r asks again",
	} {
		pull := testPull()
		pull.Pull.Mergeable = state
		if got := ansi.Strip(mergeBanner(st, pull, 100)); got != want {
			t.Errorf("mergeBanner(%s) = %q, want %q", state, got, want)
		}
	}
	draft := readyPull()
	draft.Pull.Draft = true
	if got := ansi.Strip(mergeBanner(st, draft, 100)); !strings.Contains(got, "Not ready to merge — a draft") {
		t.Errorf("a draft GitHub calls clean reads %q", got)
	}
	closed := readyPull()
	closed.State = "closed"
	if got := mergeBanner(st, closed, 100); got != "" {
		t.Errorf("a closed pull request still has a merge bar: %q", got)
	}
	if got := ansi.StringWidth(mergeBanner(st, readyPull(), 30)); got != 30 {
		t.Errorf("the ready bar is %d wide in a tab of 30", got)
	}
}

// m merges, asking which way, of the ways the repository allows; what is
// merged is the commit that was read.
func TestThePullRequestIsMerged(t *testing.T) {
	t.Parallel()
	ds := pullSource()
	ds.issues["https://github.com/acme/foo#9"] = readyPull()
	d, m := openPullPane(t, ds)
	d.key(pullMergeKey)
	frame := plainFrame(m)
	if m.dialog == nil || !strings.Contains(frame, "Merge #9 into main") || !strings.Contains(frame, "squash and merge") {
		t.Fatalf("m did not ask how to merge it:\n%s", frame)
	}
	if !strings.Contains(frame, "not allowed in this repository") {
		t.Errorf("the merge commit this repository does not allow is offered as the rest are:\n%s", frame)
	}
	// The way the repository does not allow is not taken.
	d.key("c")
	ds.mu.Lock()
	early := len(ds.merges)
	ds.mu.Unlock()
	if early != 0 || m.dialog == nil {
		t.Fatalf("a method the repository does not allow merged it: %d merges, dialog %v", early, m.dialog != nil)
	}
	d.key("s")
	d.wait("the merge", func() bool { return strings.Contains(m.status, "merged #9 (squash)") })
	ds.mu.Lock()
	merges := append([]string(nil), ds.merges...)
	ds.mu.Unlock()
	if len(merges) != 1 || merges[0] != "https://github.com/acme/foo#9: squash@abc1234567890" {
		t.Fatalf("merges = %v, want one squash of the commit that was read", merges)
	}
	d.wait("the badge", func() bool {
		d.key("g")
		return strings.Contains(plainFrame(m), "● Merged")
	})
	if got := issueTabOf(m).name(); got != "PR #9" {
		t.Errorf("the merged tab is still named %q", got)
	}
	if strings.Contains(hintLine(m.hints()), "m merge") {
		t.Errorf("a merged pull request still offers the merge: %q", hintLine(m.hints()))
	}
}

// A pull request that is not ready may still be merged by someone allowed to:
// the question says how it stands, and GitHub's refusal is reported as it is.
func TestARefusedMergeSaysWhy(t *testing.T) {
	t.Parallel()
	ds := pullSource()
	ds.mergeErr = errors.New("GitHub answered 405: Required status check \"lint\" is failing")
	d, m := openPullPane(t, ds)
	d.key(pullMergeKey)
	if frame := plainFrame(m); m.dialog == nil || !strings.Contains(frame, "Not ready to merge") {
		t.Fatalf("the question does not say it is not ready:\n%s", frame)
	}
	d.key("s")
	d.wait("the refusal", func() bool {
		return strings.Contains(m.status, "#9 was not merged") && strings.Contains(m.status, "lint")
	})
	if v := issueTabOf(m).issue; v.changing || v.issue.Pull.Merged {
		t.Errorf("after the refusal: changing %v, merged %v", v.changing, v.issue.Pull.Merged)
	}
}

// What GitHub could not merge either is not asked about.
func TestADraftOrConflictedPullRequestIsNotMerged(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(*PullRequest){
		"is a draft":     func(p *PullRequest) { p.Draft = true },
		"conflicts with": func(p *PullRequest) { p.Mergeable = "dirty" },
	} {
		ds := pullSource()
		pull := testPull()
		change(pull.Pull)
		ds.issues["https://github.com/acme/foo#9"] = pull
		d, m := openPullPane(t, ds)
		d.key(pullMergeKey)
		if m.dialog != nil || !strings.Contains(m.status, name) {
			t.Errorf("%s: dialog %v, status %q", name, m.dialog != nil, m.status)
		}
	}
}

// GitHub works out whether a pull request can merge when first asked, so one
// it has yet to answer for is read again in a moment rather than a minute.
func TestAPullRequestGitHubHasNotSettledIsReadAgain(t *testing.T) {
	t.Parallel()
	ds := pullSource()
	pull := testPull()
	pull.Pull.Mergeable = "unknown"
	ds.issues["https://github.com/acme/foo#9"] = pull
	d, m := openPullPane(t, ds)
	p := issueTabOf(m)
	if p.issue.settles != 1 {
		t.Fatalf("settles = %d after the first read, want one early read asked for", p.issue.settles)
	}
	ds.mu.Lock()
	ds.issues["https://github.com/acme/foo#9"] = readyPull()
	ds.mu.Unlock()
	d.dispatch(pullSettleMsg{id: p.id})
	d.wait("the answer", func() bool { return strings.Contains(plainFrame(m), "Ready to merge") })
}

// The key that asks how to merge is not one of the answers: a second press of
// it, or its repeat, merges nothing.
func TestTheMergeKeyTwiceDoesNotMerge(t *testing.T) {
	t.Parallel()
	ds := pullSource()
	pull := readyPull()
	pull.Pull.MergeMethods = nil
	ds.issues["https://github.com/acme/foo#9"] = pull
	d, m := openPullPane(t, ds)
	d.key(pullMergeKey)
	d.key(pullMergeKey)
	ds.mu.Lock()
	merges := len(ds.merges)
	ds.mu.Unlock()
	if merges != 0 || m.dialog == nil {
		t.Fatalf("%s twice: %d merges, question up %v; want none and the question still asked", pullMergeKey, merges, m.dialog != nil)
	}
	for _, method := range mergeMethods {
		if method.press == pullMergeKey {
			t.Errorf("%q is picked with the key that asks", method.label)
		}
	}
}

// The band's key is lit under the pointer, from the cells it is pressed on.
func TestTheReadyBandsKeyIsLitUnderThePointer(t *testing.T) {
	t.Parallel()
	ds := pullSource()
	ds.issues["https://github.com/acme/foo#9"] = readyPull()
	d, m := openPullPane(t, ds)
	m.st = newStyles(true)
	lit := m.st.hover.Render(pullMergeKey + mergeBandHint)
	if strings.Contains(rawFrame(m), lit) {
		t.Fatalf("the band's key is lit with the pointer elsewhere")
	}
	x, y := at(t, m, "merges it")
	d.dispatch(tea.MouseMotionMsg{X: x, Y: y})
	if !strings.Contains(rawFrame(m), lit) {
		t.Errorf("the band's key is not lit under the pointer:\n%q", rawFrame(m))
	}
}

// A band too narrow for its key draws none, and says so to whoever marks it.
func TestANarrowReadyBandHasNoKey(t *testing.T) {
	t.Parallel()
	st := newStyles(false)
	issue := readyPull()
	for width := 1; width <= 40; width++ {
		drawn := strings.Contains(mergeBanner(st, issue, width), pullMergeKey+strings.TrimRight(mergeBandHint, " "))
		if drawn != mergeBandKeyFits(width) {
			t.Errorf("at %d wide the key is drawn %v, and said to fit %v", width, drawn, mergeBandKeyFits(width))
		}
	}
}

// The ready band's key is a control: a click on it asks, as the key does.
func TestTheReadyBandsKeyIsPressed(t *testing.T) {
	t.Parallel()
	ds := pullSource()
	ds.issues["https://github.com/acme/foo#9"] = readyPull()
	_, m := openPullPane(t, ds)
	x, y := at(t, m, "merges it")
	tap(t, m, x, y)
	if m.dialog == nil || !strings.Contains(plainFrame(m), "Merge #9 into main") {
		t.Errorf("a click on the band's key did not ask how to merge:\n%s", plainFrame(m))
	}
}

// A push while the tab is open is said, on the tab and in the merge question,
// which names the commit it would merge: what was read here before is not it.
// The cursor follows its file, and a read the person asks for is what they
// have now seen.
func TestAPushWhileTheTabIsOpenIsSaid(t *testing.T) {
	t.Parallel()
	ds := pullSource()
	ds.issues["https://github.com/acme/foo#9"] = readyPull()
	d, m := openPullPane(t, ds)
	p := issueTabOf(m)
	v := p.issue
	d.key("f")
	d.wait("the files changed", func() bool { return strings.Contains(plainFrame(m), "reaper/reaper_test.go") })
	opened := v.review

	pushed := readyPull()
	pushed.Pull.HeadSHA = "fff9999000111"
	ds.mu.Lock()
	ds.issues["https://github.com/acme/foo#9"] = pushed
	ds.pullDiffs["fff9999000111"] = "diff --git a/a/first.go b/a/first.go\nnew file mode 100644\n--- /dev/null\n+++ b/a/first.go\n@@ -0,0 +1 @@\n+package a\n"
	ds.mu.Unlock()
	d.dispatch(issueTickMsg{id: p.id})
	d.wait("the push", func() bool { return strings.Contains(plainFrame(m), "commits were pushed since") })
	// A push nobody asked to see leaves the files being read where they are.
	if v.review != opened || !strings.Contains(plainFrame(m), "reaper/reaper_test.go") {
		t.Errorf("the push moved the files changed under the person reading them:\n%s", plainFrame(m))
	}
	// The merge is asked from the conversation.
	d.key("f")
	d.key(pullMergeKey)
	frame := plainFrame(m)
	if m.dialog == nil || !strings.Contains(m.dialog.body, "at fff999900011 — commits were pushed since abc123456789 was read here: the head is now fff999900011") {
		t.Errorf("the merge question does not name the commit and the push:\n%s", frame)
	}
	d.key("esc")
	// A read asked for that fails has shown them nothing: the notice stays.
	ds.mu.Lock()
	ds.issueErr = errors.New("boom")
	ds.mu.Unlock()
	d.key("r")
	d.wait("the failed read", func() bool { return v.err != "" && !v.loading })
	if v.pushed == "" || !strings.Contains(plainFrame(m), "commits were pushed") {
		t.Errorf("a read that failed took the notice down (pushed %q):\n%s", v.pushed, plainFrame(m))
	}
	ds.mu.Lock()
	ds.issueErr = nil
	ds.mu.Unlock()
	// A read the person asks for is what they have seen, a head newer still
	// included: the notice goes and is not raised again by that read.
	again := readyPull()
	again.Pull.HeadSHA = "eee8888000222"
	ds.mu.Lock()
	ds.issues["https://github.com/acme/foo#9"] = again
	ds.mu.Unlock()
	d.key("r")
	d.wait("the read", func() bool { return v.issue.Pull.HeadSHA == "eee8888000222" && !v.loading })
	if v.pushed != "" || strings.Contains(plainFrame(m), "commits were pushed") {
		t.Errorf("a read that was asked for left the notice up (pushed %q):\n%s", v.pushed, plainFrame(m))
	}

	// r is the tab's on the files changed too, and what it reads is what the
	// person has now seen: the window opens over the new head.
	d.key("f")
	if v.review == opened {
		t.Errorf("f after a read of a new head opened the old head's files")
	}
	ds.mu.Lock()
	ds.issues["https://github.com/acme/foo#9"] = pushed
	ds.mu.Unlock()
	d.key("r")
	d.wait("the new head's files", func() bool { return v.review != nil && v.review.head == "fff9999000111" })
	d.wait("the new head's files drawn", func() bool { return strings.Contains(plainFrame(m), "a/first.go") })
	if v.pushed != "" || !v.files {
		t.Errorf("r on the files changed: files %v, pushed %q", v.files, v.pushed)
	}
	// The new head is still ready to merge, and the tab still says so.
	if !strings.HasSuffix(p.title, " ●") {
		t.Errorf("r on the files changed left the tab titled %q, want it marked ready", p.title)
	}
	// The window over the old head was replaced, and its reads end with it.
	ds.mu.Lock()
	replaced := ds.pullContexts[len(ds.pullContexts)-2]
	ds.mu.Unlock()
	if replaced.Err() == nil {
		t.Errorf("the old head's window was replaced and its reads go on")
	}
}

// Whatever takes the tab away ends the window's reads with it: closing the
// tab, and leaving the workspace the tab is in.
func TestTheFilesChangedEndWithTheirTab(t *testing.T) {
	t.Parallel()
	ds := pullSource()
	d, m := openPullPane(t, ds)
	d.key("f")
	d.wait("the files changed", func() bool { return strings.Contains(plainFrame(m), "reaper/reaper_test.go") })
	d.key(m.leader())
	d.key("d")
	d.wait("the workspace to close", func() bool { return !m.inPanes() })
	ds.mu.Lock()
	opened := slices.Clone(ds.pullContexts)
	ds.mu.Unlock()
	if len(opened) != 1 || opened[0].Err() == nil {
		t.Errorf("leaving the workspace left the files changed reading GitHub (%d opened)", len(opened))
	}
}

// A drag in the files changed is the window's to the end: it is held as a
// terminal's is, and what was selected elsewhere goes.
func TestTheFilesChangedHoldTheirDrag(t *testing.T) {
	t.Parallel()
	ds := pullSource()
	d, m := openPullPane(t, ds)
	p := issueTabOf(m)
	d.key("f")
	d.wait("the files changed", func() bool { return strings.Contains(plainFrame(m), "reaper/reaper_test.go") })
	x, y := at(t, m, "reaper/reaper_test.go")
	send(t, m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	if m.mouseCapture != p.id {
		t.Errorf("a press in the files changed is held by pane %d, want the tab's %d", m.mouseCapture, p.id)
	}
	send(t, m, tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	if m.mouseCapture != 0 {
		t.Errorf("the release left the drag held by %d", m.mouseCapture)
	}
}

// While a comment is being written the keys are the field's: the ready band's
// key is not a control then, so a click on it types nothing.
func TestTheReadyBandsKeyIsNotPressedWhileWriting(t *testing.T) {
	t.Parallel()
	ds := pullSource()
	ds.issues["https://github.com/acme/foo#9"] = readyPull()
	d, m := openPullPane(t, ds)
	v := issueTabOf(m).issue
	d.key("c")
	if !v.composing {
		t.Fatal("c did not open the comment field")
	}
	x, y := at(t, m, "merges it")
	tap(t, m, x, y)
	if m.dialog != nil || v.composer.Value() != "" {
		t.Errorf("a click on the band's key while writing: question up %v, field %q", m.dialog != nil, v.composer.Value())
	}
}

// Checks that could not all be read say so, so a row of passes is not taken
// for the whole.
func TestChecksNotAllReadSaySo(t *testing.T) {
	t.Parallel()
	st := newStyles(false)
	if got := checkRows(st, []PullCheck{{Name: "build", State: "success"}}, true, 80); len(got) == 0 || !strings.Contains(got[0], "1 passed · not all could be read") {
		t.Errorf("partial checks = %q", got)
	}
	if got := checkRows(st, nil, true, 80); len(got) != 1 || !strings.Contains(got[0], "could not be read") {
		t.Errorf("unread checks = %q", got)
	}
	if got := checkRows(st, nil, false, 80); got != nil {
		t.Errorf("no checks = %q, want no row", got)
	}
}
