package console

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// glyph is a mark as the TUI draws it, for the words these tests read.
func glyph(m IssueMark) string {
	if m.Filled {
		return "●"
	}
	return "○"
}

func testPullIssue() Issue {
	return Issue{
		Repository: "acme/foo", Number: 9, Title: "Reap only what was asked", State: "open", PullRequest: true,
		Pull: &PullRequest{Head: "fix-reaper", Base: "main", HeadSHA: "abc1234567890", Mergeable: "clean", MergeMethods: []string{"squash", "rebase"}},
	}
}

// The words an event is drawn with, for the kinds the page has words for.
func TestEventsReadAsThePageWritesThem(t *testing.T) {
	t.Parallel()
	issue := Issue{Repository: "acme/foo"}
	for _, tc := range []struct {
		event IssueEvent
		want  string
	}{
		{IssueEvent{Kind: "assigned", Actor: "bob", Subject: "bob"}, "self-assigned this"},
		{IssueEvent{Kind: "assigned", Actor: "bob", Subject: "carol"}, "assigned carol"},
		{IssueEvent{Kind: "closed", StateReason: "not_planned"}, "closed this as not planned"},
		{IssueEvent{Kind: "closed", Subject: "abc1234"}, "closed this as completed in abc1234"},
		{IssueEvent{Kind: "renamed", From: "a", To: "b"}, "changed the title from “a” to “b”"},
		{IssueEvent{Kind: "cross-referenced", Source: &IssueRef{Repository: "acme/bar", Number: 3, Title: "x"}}, "mentioned this in acme/bar#3 x"},
		{IssueEvent{Kind: "cross-referenced", Source: &IssueRef{Repository: "acme/foo", Number: 4, Title: "y", State: "closed"}}, "mentioned this in #4 y"},
		{IssueEvent{Kind: "some_new_event"}, "some new event"},
	} {
		if got := issue.EventText(tc.event); got != tc.want {
			t.Errorf("EventText(%s) = %q, want %q", tc.event.Kind, got, tc.want)
		}
	}
}

// A pull request's events are in its own words, and the rest an issue's.
func TestPullRequestEventsReadAsThePageWritesThem(t *testing.T) {
	t.Parallel()
	pull := testPullIssue()
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
		if got := pull.EventText(tc.event); got != tc.want {
			t.Errorf("EventText(%s) = %q, want %q", tc.event.Kind, got, tc.want)
		}
	}
}

// Merging closes a pull request, and the timeline says the one thing.
func TestAMergesCloseIsNotSaidTwice(t *testing.T) {
	t.Parallel()
	pull := testPullIssue()
	pull.Timeline = []IssueEvent{{Kind: "merged"}, {Kind: "closed"}, {Kind: "closed"}}
	if pull.TimelineShows(1) || !pull.TimelineShows(0) || !pull.TimelineShows(2) {
		t.Errorf("the close after the merge shows %v; the merge %v, a later close %v", pull.TimelineShows(1), pull.TimelineShows(0), pull.TimelineShows(2))
	}
	issue := Issue{Timeline: pull.Timeline}
	if !issue.TimelineShows(1) {
		t.Errorf("an issue's close is dropped")
	}
}

// The badge is the page's: a draft, a merge and a close each say so.
func TestTheBadge(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		issue Issue
		want  string
	}{
		{Issue{State: "open", Pull: &PullRequest{}}, "● Open"},
		{Issue{State: "open", Pull: &PullRequest{Draft: true}}, "○ Draft"},
		{Issue{State: "closed", Pull: &PullRequest{}}, "○ Closed"},
		{Issue{State: "closed", Pull: &PullRequest{Merged: true}}, "● Merged"},
		{Issue{State: "open"}, "● Open"},
		{Issue{State: "closed", StateReason: "not_planned"}, "○ Closed as not planned"},
		{Issue{State: "closed", StateReason: "duplicate"}, "○ Closed as duplicate"},
		{Issue{State: "closed", StateReason: "completed"}, "○ Closed"},
	} {
		word, mark := tc.issue.Badge()
		if got := glyph(mark) + " " + word; got != tc.want {
			t.Errorf("Badge(%+v) = %q, want %q", tc.issue, got, tc.want)
		}
	}
}

// Each way of not being ready says which, in GitHub's terms.
func TestTheMergeBoxSaysWhyNot(t *testing.T) {
	t.Parallel()
	for state, want := range map[string]string{
		"clean":     "● Ready to merge into main",
		"has_hooks": "● Ready to merge into main",
		"dirty":     "● Not ready to merge — conflicts with main must be resolved",
		"behind":    "● Not ready to merge — out of date with main",
		"blocked":   "● Not ready to merge — a required review or check is missing",
		"unstable":  "● Not ready to merge — its checks have not passed",
		"unknown":   "○ GitHub has not said yet whether this can merge",
	} {
		pull := testPullIssue()
		pull.Pull.Mergeable = state
		m := pull.Merge()
		if got := glyph(m.Mark) + " " + m.Said("main"); got != want {
			t.Errorf("Merge(%s) = %q, want %q", state, got, want)
		}
		if m.Ready != (state == "clean" || state == "has_hooks") || m.Ready != pull.Ready() {
			t.Errorf("Merge(%s).Ready = %v, Ready() = %v", state, m.Ready, pull.Ready())
		}
		if pull.Settling() != (state == "unknown") {
			t.Errorf("Settling(%s) = %v", state, pull.Settling())
		}
	}
	draft := testPullIssue()
	draft.Pull.Draft = true
	if m := draft.Merge(); m.Ready || !strings.Contains(m.Said("main"), "a draft") {
		t.Errorf("a draft GitHub calls clean reads %q", m.Said("main"))
	}
	closed := testPullIssue()
	closed.State = "closed"
	if m := closed.Merge(); m.Shown || closed.Settling() {
		t.Errorf("a closed pull request still has a merge box: %+v", m)
	}
}

// The checks are summed up, and the ones that have not passed are listed.
func TestTheChecksAreSummedUp(t *testing.T) {
	t.Parallel()
	pull := PullRequest{ChecksPartial: true, Checks: []PullCheck{
		{Name: "build", State: "success"}, {Name: "lint", State: "failure"}, {Name: "e2e", State: "pending"},
		{Name: "flaky", State: "canceled"}, {Name: "docs", State: "skipped"},
	}}
	sum := pull.CheckSummary()
	if sum.Failing != 2 || sum.Pending != 1 || sum.Passed != 2 || !sum.Partial {
		t.Errorf("CheckSummary = %+v", sum)
	}
	var names []string
	for _, c := range sum.Unpassed {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "lint,e2e,flaky" {
		t.Errorf("unpassed = %v, want lint, e2e and flaky in order", names)
	}
	if m := CheckMark("pending"); m.Tone != IssueToneBusy || !m.Filled {
		t.Errorf("a pending check is marked %+v", m)
	}
}

// Closing an issue asks how; a pull request and a reopen are a yes or no;
// a merged one has nothing to close.
func TestClosingAndReopeningAsks(t *testing.T) {
	t.Parallel()
	issue := Issue{Number: 4, Title: "t", State: "open"}
	q, err := issue.StateQuestion()
	if err != nil || q.Title != "Close #4" || len(q.Choices) != 2 {
		t.Fatalf("closing an issue asks %+v, %v", q, err)
	}
	if state, reason, ok := issue.StateAnswer(CloseNotPlanned); state != "closed" || reason != "not_planned" || !ok {
		t.Errorf("not planned closes %q %q %v", state, reason, ok)
	}
	if _, _, ok := issue.StateAnswer(AnswerYes); ok {
		t.Errorf("a yes closes an issue that asked how")
	}
	issue.State = "closed"
	if q, _ := issue.StateQuestion(); q.Title != "Reopen #4?" || len(q.Choices) != 0 {
		t.Errorf("reopening asks %+v", q)
	}
	if state, reason, ok := issue.StateAnswer(AnswerYes); state != "open" || reason != "reopened" || !ok {
		t.Errorf("yes reopens %q %q %v", state, reason, ok)
	}
	if _, _, ok := issue.StateAnswer(AnswerNo); ok {
		t.Errorf("no reopened it")
	}
	pull := testPullIssue()
	if q, _ := pull.StateQuestion(); q.Title != "Close #9 without merging it?" || len(q.Choices) != 0 {
		t.Errorf("closing a pull request asks %+v", q)
	}
	if state, reason, ok := pull.StateAnswer(AnswerYes); state != "closed" || reason != "" || !ok {
		t.Errorf("yes closes a pull request %q %q %v", state, reason, ok)
	}
	pull.State = "closed"
	if state, reason, _ := pull.StateAnswer(AnswerYes); state != "open" || reason != "" {
		t.Errorf("a pull request reopens %q %q, want no reason", state, reason)
	}
	pull.Pull.Merged = true
	if _, err := pull.StateQuestion(); err == nil {
		t.Errorf("a merged pull request can be closed or reopened")
	}
	if got := StateChanged(4, "closed", "not_planned", nil); got != "closed #4 as not planned" {
		t.Errorf("StateChanged = %q", got)
	}
	if got := StateChanged(4, "open", "reopened", errors.New("no")); got != "#4 was not reopened: no" {
		t.Errorf("StateChanged refused = %q", got)
	}
}

// Merging asks which way, offering a way the repository refuses with why; it
// names the commit merged, and refuses what GitHub could not merge.
func TestMergingAsksWhichWay(t *testing.T) {
	t.Parallel()
	pull := testPullIssue()
	q, err := pull.MergeQuestion("abc1234567890", "")
	if err != nil || q.Title != "Merge #9 into main" {
		t.Fatalf("MergeQuestion = %+v, %v", q, err)
	}
	why := map[string]string{}
	for _, c := range q.Choices {
		why[c.Key] = c.Why
	}
	if len(why) != 3 || why["squash"] != "" || why["rebase"] != "" || why["merge"] != "not allowed in this repository" {
		t.Errorf("the ways offered: %v", why)
	}
	if !strings.Contains(q.Body, "at abc123456789") || strings.Contains(q.Body, "Not ready") {
		t.Errorf("the body of a ready merge: %q", q.Body)
	}
	if m, ok := pull.MergeMethod("squash"); m != "squash" || !ok {
		t.Errorf("squash is not taken")
	}
	if _, ok := pull.MergeMethod("merge"); ok {
		t.Errorf("a way the repository refuses is taken")
	}
	pushed, _ := pull.MergeQuestion("def4567890", "abc1234567890")
	if !strings.Contains(pushed.Body, "commits were pushed since abc123456789") {
		t.Errorf("a pushed head is not said: %q", pushed.Body)
	}
	behind := testPullIssue()
	behind.Pull.Mergeable = "behind"
	if q, _ := behind.MergeQuestion("x", ""); !strings.Contains(q.Body, "Not ready to merge — out of date with main") {
		t.Errorf("a merge that is not ready does not say why: %q", q.Body)
	}
	for name, change := range map[string]func(*Issue){
		"merged": func(i *Issue) { i.Pull.Merged = true },
		"closed": func(i *Issue) { i.State = "closed" },
		"draft":  func(i *Issue) { i.Pull.Draft = true },
		"dirty":  func(i *Issue) { i.Pull.Mergeable = "dirty" },
		"issue":  func(i *Issue) { i.Pull = nil },
	} {
		i := testPullIssue()
		change(&i)
		if _, err := i.MergeQuestion("x", ""); err == nil {
			t.Errorf("a %s pull request is asked how to merge", name)
		}
	}
	open := testPullIssue()
	open.Pull.MergeMethods = nil
	if q, _ := open.MergeQuestion("x", ""); q.Choices[1].Why != "" {
		t.Errorf("a repository that would not say what it allows refuses a way: %+v", q.Choices)
	}
}

// An empty comment is not posted, and one is posted as written.
func TestACommentIsPostedAsWritten(t *testing.T) {
	t.Parallel()
	if _, err := CommentBody(" \n\t"); !errors.Is(err, ErrEmptyComment) {
		t.Errorf("an empty comment: %v", err)
	}
	if body, err := CommentBody("    code\n"); err != nil || body != "    code\n" {
		t.Errorf("a comment is changed on its way: %q, %v", body, err)
	}
}

// A review thread is shown under the last of its hunk, at its place.
func TestAThreadIsShownUnderItsHunk(t *testing.T) {
	t.Parallel()
	thread := ReviewThread{Path: "a.go", Line: 12, Outdated: true, Hunk: "@@ -1 +1 @@\n a\n\n-b\n+c\n d\n"}
	if got := ThreadWhere(thread); got != "a.go:12 · outdated" {
		t.Errorf("ThreadWhere = %q", got)
	}
	if got := strings.Join(ThreadHunk(thread), "|"); got != "-b|+c| d" {
		t.Errorf("ThreadHunk = %q", got)
	}
}

// A reference to a character that would be obeyed rather than shown is left
// as it was typed; the rest decode.
func TestMarkdownSourceKeepsWhatWouldBeObeyed(t *testing.T) {
	t.Parallel()
	got := MarkdownSource("a &#x1b;[31m b &#x202e; c &amp; &#128512; &nosuch;\r\nd")
	want := "a &amp;#x1b;[31m b &amp;#x202e; c &amp; &#128512; &nosuch;\nd"
	if got != want {
		t.Errorf("MarkdownSource = %q, want %q", got, want)
	}
}

func TestTheWorkIsNamed(t *testing.T) {
	t.Parallel()
	if WorkLabel(9, true) != "PR #9" || WorkLabel(4, false) != "issue #4" {
		t.Errorf("labels: %q %q", WorkLabel(9, true), WorkLabel(4, false))
	}
	if got := WorkAddress("https://github.com/acme/foo", 9, true); got != "https://github.com/acme/foo/pull/9" {
		t.Errorf("WorkAddress = %q", got)
	}
	if got := FilesAddress("https://github.com/acme/foo", 9); got != "https://github.com/acme/foo/pull/9/files" {
		t.Errorf("FilesAddress = %q", got)
	}
	if got := ReactionText(IssueReactions{"eyes": 1, "+1": 2}); got != "+1 2 · eyes 1" {
		t.Errorf("ReactionText = %q", got)
	}
}

// A files changed tab quit by the person goes; one that ended before they
// typed at it is held with what it said; one whose stream broke goes.
func TestAFilesChangedTabEnds(t *testing.T) {
	t.Parallel()
	tab := FilesTab(Exec{ID: "e1"}, 9)
	if tab.Title != "files changed #9" || tab.Side() != SideShells {
		t.Errorf("FilesTab = %+v on %v", tab, tab.Side())
	}
	held := tab
	if got := held.End(nil); got != EndsHeld || !held.Exited || held.Status != "discobox-review ended" {
		t.Errorf("an untouched files tab ends %v, %+v", got, held)
	}
	typed := tab
	typed.Typed = true
	if got := typed.End(nil); got != EndsTab {
		t.Errorf("a files tab quit ends %v", got)
	}
	broke := tab
	if got := broke.End(errors.New("gone")); got != EndsTab {
		t.Errorf("a files tab whose stream broke ends %v", got)
	}
}

// Which work a box has, and why it has none, is one answer for every console.
func TestABoxsWorkIsItsTags(t *testing.T) {
	t.Parallel()
	box := Sandbox{Repository: "https://github.com/acme/foo", Tags: []string{"issue=4"}}
	if n, err := box.Work(false); n != 4 || err != nil {
		t.Errorf("Work(issue) = %d, %v", n, err)
	}
	if _, err := box.Work(true); err == nil || !strings.Contains(err.Error(), "tag this discobox pr=N") {
		t.Errorf("Work(pr) = %v", err)
	}
	box.Repository = ""
	if _, err := box.Work(false); err == nil {
		t.Errorf("a box on no GitHub repository has an issue")
	}
}

// The head a tab showed moves only with the first read and the ones the
// person asked for; a newer head on the tab's own clock is said.
func TestTheShownHeadMovesOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	at := func(head string) Issue { return Issue{Pull: &PullRequest{HeadSHA: head}} }
	var s ShownHead
	if p := s.Read(at("a"), false); p != "" || s.Head() != "a" {
		t.Errorf("the first read: pushed %q, shown %q", p, s.Head())
	}
	if p := s.Read(at("b"), false); p != "a" || s.Head() != "a" {
		t.Errorf("a push read on the tab's clock: pushed %q, shown %q", p, s.Head())
	}
	if p := s.Read(at("b"), true); p != "" || s.Head() != "b" {
		t.Errorf("a read asked for: pushed %q, shown %q", p, s.Head())
	}
	kept := NewShownHead("a")
	if p := kept.Read(at("b"), false); p != "a" {
		t.Errorf("a tab that had shown a head lost it: %q", p)
	}
	if p := s.Read(Issue{}, false); p != "" || s.Head() != "" {
		t.Errorf("an issue has no head")
	}
}

// fakeIssues is a DataSource that records the acts sent to GitHub.
type fakeIssues struct {
	DataSource
	sent []string
	err  error
}

func (f *fakeIssues) CommentOnIssue(_ context.Context, _ string, n int, body string) error {
	f.sent = append(f.sent, fmt.Sprintf("comment %d %q", n, body))
	return f.err
}

func (f *fakeIssues) SetIssueState(_ context.Context, _ string, n int, state, reason string) error {
	f.sent = append(f.sent, fmt.Sprintf("state %d %s %s", n, state, reason))
	return f.err
}

func (f *fakeIssues) MergePullRequest(_ context.Context, _ string, n int, method, sha string) error {
	f.sent = append(f.sent, fmt.Sprintf("merge %d %s@%s", n, method, sha))
	return f.err
}

// Each act refuses what its question would, takes the answer, and words what
// came of it; an answer that changes nothing is a question declined.
func TestTheActsAreTheCores(t *testing.T) {
	t.Parallel()
	f := &fakeIssues{}
	ctx := context.Background()
	if note, err := PostComment(ctx, f, "r", 4, "hi"); note != "commented on #4" || err != nil {
		t.Errorf("PostComment = %q, %v", note, err)
	}
	if _, err := PostComment(ctx, f, "r", 4, " "); !errors.Is(err, ErrEmptyComment) {
		t.Errorf("an empty comment: %v", err)
	}
	issue := Issue{Number: 4, State: "open"}
	if note, err := ChangeIssueState(ctx, f, "r", issue, CloseCompleted); note != "closed #4 as completed" || err != nil {
		t.Errorf("ChangeIssueState = %q, %v", note, err)
	}
	if _, err := ChangeIssueState(ctx, f, "r", issue, AnswerNo); !errors.Is(err, context.Canceled) {
		t.Errorf("an answer that closes nothing: %v", err)
	}
	pull := testPullIssue()
	if note, err := MergeIssue(ctx, f, "r", pull, "squash", "abc"); note != "merged #9 (squash)" || err != nil {
		t.Errorf("MergeIssue = %q, %v", note, err)
	}
	if _, err := MergeIssue(ctx, f, "r", pull, "merge", "abc"); !errors.Is(err, context.Canceled) {
		t.Errorf("a way the repository refuses: %v", err)
	}
	want := []string{`comment 4 "hi"`, "state 4 closed completed", "merge 9 squash@abc"}
	if !slices.Equal(f.sent, want) {
		t.Errorf("sent %q, want %q", f.sent, want)
	}
	f.err = errors.New("no")
	if _, err := MergeIssue(ctx, f, "r", pull, "squash", "abc"); err == nil || err.Error() != "#9 was not merged: no" {
		t.Errorf("a refused merge: %v", err)
	}
}
