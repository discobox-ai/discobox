package bridge

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/pkg/execstream/frame"
)

const testRepo = "https://github.com/acme/foo"

// workBox is a box on acme/foo tagged with issue 4 and pull request 9.
func workBox(id string) console.Sandbox {
	b := running(id)
	b.Repository = testRepo
	b.Tags = []string{"issue=4", "pr=9"}
	return b
}

func testPull() console.Issue {
	return console.Issue{
		Repository: "acme/foo", Number: 9, Title: "Reap only what was asked", State: "open", PullRequest: true, Author: "alice",
		Body: "fixes it &#x1b;[31m", Reactions: console.IssueReactions{"+1": 2},
		Pull: &console.PullRequest{
			Head: "fix-reaper", Base: "main", HeadSHA: "abc1234567890", BaseSHA: "base000000000", Mergeable: "clean",
			MergeMethods: []string{"squash"}, Reviewers: []console.PullReviewer{{Name: "bob", State: "approved"}},
			Checks: []console.PullCheck{{Name: "build", State: "success"}, {Name: "lint", State: "failure", URL: "https://ci.example/lint"}},
		},
		Timeline: []console.IssueEvent{
			{Kind: "commented", Actor: "bob", Body: "looks **good**"},
			{Kind: "reviewed", Actor: "bob", Review: "approved", Threads: []console.ReviewThread{{Path: "a.go", Line: 3, Hunk: "@@ -1 +1 @@\n-a\n+b", Comments: []console.ReviewComment{{Author: "bob", Body: "nit"}}}}},
			{Kind: "committed", Actor: "alice", Subject: "abc1234", Message: "reap"},
			{Kind: "merged", Actor: "alice", Subject: "abc1234"},
			{Kind: "closed", Actor: "alice"},
			{Kind: "labeled", Actor: "carol", Label: &console.IssueLabel{Name: "bug", Color: "d73a4a"}},
		},
	}
}

// The issue topic is the box's tagged issue as its tab draws it: every rule's
// answer the core's, every body made safe, read in full when asked, and a
// head newer than the one first shown said.
func TestTheIssueTopicIsTheCores(t *testing.T) {
	core := newFakeCore("home", workBox("b1"), running("bare"))
	core.setIssue(testRepo, 9, testPull())
	a := dial(t, core)
	a.hello()
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicIssue, Box: "b1", Issue: &IssueQuery{Pull: true, Full: true}}})
	i := a.expect(TypeSnapshot).Snapshot.Issue
	if i == nil || i.Label != "PR #9" || i.URL != testRepo+"/pull/9" || i.Badge != "Open" || i.Mark.Tone != console.IssueToneOpen || !i.Mark.Filled {
		t.Fatalf("issue %+v", i)
	}
	if i.Body != "fixes it &amp;#x1b;[31m" || i.Reactions != "+1 2" {
		t.Fatalf("the body is not made safe: %q, reactions %q", i.Body, i.Reactions)
	}
	p := i.PullRequest
	if p == nil || !p.Merge.Ready || p.Merge.Said != "Ready to merge into main" || p.Checks.Failing != 1 || p.Checks.Passed != 1 || len(p.Checks.Unpassed) != 1 || p.Checks.Unpassed[0].URL != "https://ci.example/lint" {
		t.Fatalf("pull %+v", p)
	}
	if len(p.Reviewers) != 1 || p.Reviewers[0].Said != "approved" {
		t.Fatalf("reviewers %+v", p.Reviewers)
	}
	var kinds []string
	for _, item := range i.Timeline {
		kinds = append(kinds, string(item.Kind)+":"+item.Text)
	}
	want := []string{"comment:", "review:approved these changes", "commit:reap", "event:merged commit abc1234 into main", "event:added the bug label"}
	if !slices.Equal(kinds, want) {
		t.Fatalf("timeline %q, want %q", kinds, want)
	}
	if th := i.Timeline[1].Threads; len(th) != 1 || th[0].Where != "a.go:3" || !slices.Equal(th[0].Hunk, []string{"@@ -1 +1 @@", "-a", "+b"}) || th[0].Comments[0].Body != "nit" {
		t.Fatalf("threads %+v", th)
	}
	if !slices.Contains(core.called(), "issue "+testRepo+"#9 full=true") {
		t.Fatalf("calls %q, want a full read", core.called())
	}

	// A command here that changes it reads it again, in full, and a head
	// newer than the one first shown is said.
	pushed := testPull()
	pushed.Pull.HeadSHA = "def4567890123"
	core.setIssue(testRepo, 9, pushed)
	if r := a.result(a.command(Command{Command: "pr", Targets: []string{"b1"}, Args: Args{Act: ActComment, Comment: "thanks"}})); r.Status != StatusDone || r.Note != "commented on #9" {
		t.Fatalf("comment %+v", r)
	}
	i = a.expect(TypeSnapshot).Snapshot.Issue
	if i.Pushed != "abc1234567890" || i.Notice != console.PushedNotice("abc1234567890", "def4567890123") {
		t.Fatalf("the push is not said: %q %q", i.Pushed, i.Notice)
	}

	// A tab back on screen says a push made while it was away from the head
	// it had shown; a read asked for shows the head it brings.
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 3, Topic: TopicIssue, Box: "b1", Issue: &IssueQuery{Pull: true, Shown: "abc1234567890"}}})
	if i := a.expect(TypeSnapshot).Snapshot.Issue; i.Pushed != "abc1234567890" {
		t.Fatalf("a parked tab lost the head it showed: %q", i.Pushed)
	}
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 4, Topic: TopicIssue, Box: "b1", Issue: &IssueQuery{Pull: true, Full: true, Shown: "abc1234567890"}}})
	if i := a.expect(TypeSnapshot).Snapshot.Issue; i.Pushed != "" {
		t.Fatalf("a read asked for still says the push: %q", i.Pushed)
	}

	// A merged pull request has no merge box, and what is sent of it is the
	// wire's (every message is held to its schema): the empty box once went
	// out with an empty tone no app could read, and the tab never drew.
	merged := testPull()
	merged.State, merged.Pull.Merged, merged.Pull.MergedBy = "closed", true, "alice"
	core.setIssue(testRepo, 9, merged)
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 5, Topic: TopicIssue, Box: "b1", Issue: &IssueQuery{Pull: true, Full: true}}})
	if i := a.expect(TypeSnapshot).Snapshot.Issue; i == nil || i.Badge != "Merged" || i.PullRequest.Merge != nil {
		t.Fatalf("a merged pull request: %+v", i)
	}

	// A box with no tag, or on no GitHub repository, says why.
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 2, Topic: TopicIssue, Box: "bare", Issue: &IssueQuery{}}})
	_, none := running("bare").Work(false)
	if s := a.expect(TypeSnapshot).Snapshot; s.Error != none.Error() {
		t.Fatalf("no tag: %+v", s)
	}
}

// The issue and pr commands hand the app the tab, named; their acts are the
// core's: a comment as written, a close or reopen asked, a merge asked of the
// ways the repository allows, of the head the app showed.
func TestTheIssueCommandsActAsTheCoreAsks(t *testing.T) {
	core := newFakeCore("home", workBox("b1"), running("bare"))
	core.setIssue(testRepo, 4, console.Issue{Repository: "acme/foo", Number: 4, Title: "Reaper", State: "open"})
	core.setIssue(testRepo, 9, testPull())
	a := dial(t, core)
	a.hello()
	run := func(id string, args Args) uint64 {
		return a.command(Command{Command: id, Targets: []string{"b1"}, Args: args})
	}
	if r := a.result(run("issue", Args{})); r.Status != StatusEnter || r.Box != "b1" || r.Work == nil || r.Work.Label != "issue #4" || r.Work.URL != testRepo+"/issues/4" {
		t.Fatalf("issue %+v", r)
	}
	if r := a.result(a.command(Command{Command: "pr", Targets: []string{"bare"}})); r.Status != StatusFailed || !strings.Contains(r.Why, "tag this discobox pr=N") {
		t.Fatalf("a box with no tag: %+v", r)
	}
	if r := a.result(run("issue", Args{Act: ActComment, Comment: "  "})); r.Status != StatusFailed || r.Why != console.ErrEmptyComment.Error() {
		t.Fatalf("an empty comment: %+v", r)
	}

	// Closing an issue asks how.
	id := run("issue", Args{Act: ActState})
	q := a.expect(TypeQuestion).Question
	if q.Title != "Close #4" || len(q.Choices) != 2 {
		t.Fatalf("question %+v", q)
	}
	a.send(Message{Type: TypeAnswer, Answer: &Answer{Question: q.ID, Key: console.CloseNotPlanned}})
	if r := a.result(id); r.Status != StatusDone || r.Note != "closed #4 as not planned" {
		t.Fatalf("close %+v", r)
	}
	if !slices.Contains(core.called(), "state "+testRepo+"#4 closed not_planned") {
		t.Fatalf("calls %q", core.called())
	}
	// Declined, nothing is done.
	id = run("pr", Args{Act: ActState})
	q = a.expect(TypeQuestion).Question
	a.send(Message{Type: TypeAnswer, Answer: &Answer{Question: q.ID, Key: console.AnswerNo}})
	if r := a.result(id); r.Status != StatusCanceled {
		t.Fatalf("a close declined: %+v", r)
	}

	// Merging asks which way, offering one the repository refuses with why,
	// and merges the head the app showed.
	id = run("pr", Args{Act: ActMerge, Head: "abc1234567890"})
	q = a.expect(TypeQuestion).Question
	if q.Title != "Merge #9 into main" || len(q.Choices) != 3 || q.Choices[1].Why != "not allowed in this repository" || !strings.Contains(q.Body, "at abc123456789") {
		t.Fatalf("merge question %+v", q)
	}
	a.send(Message{Type: TypeAnswer, Answer: &Answer{Question: q.ID, Key: "merge"}})
	if r := a.result(id); r.Status != StatusCanceled {
		t.Fatalf("a way the repository refuses: %+v", r)
	}
	id = run("pr", Args{Act: ActMerge, Head: "abc1234567890"})
	q = a.expect(TypeQuestion).Question
	a.send(Message{Type: TypeAnswer, Answer: &Answer{Question: q.ID, Key: "squash"}})
	if r := a.result(id); r.Status != StatusDone || r.Note != "merged #9 (squash)" {
		t.Fatalf("merge %+v", r)
	}
	if !slices.Contains(core.called(), "merge "+testRepo+"#9 squash@abc1234567890") {
		t.Fatalf("calls %q", core.called())
	}
	// One GitHub could not merge is refused before it is asked.
	conflicted := testPull()
	conflicted.Pull.Mergeable = "dirty"
	core.setIssue(testRepo, 9, conflicted)
	if r := a.result(run("pr", Args{Act: ActMerge})); r.Status != StatusFailed || !strings.Contains(r.Why, "conflicts with main") {
		t.Fatalf("a conflicted merge: %+v", r)
	}
}

// The files command is discobox-review over the head and base the app
// showed, its session on the command's stream as its tab; quitting it takes
// the tab, one that ends untouched is held, and either way the session is
// ended in the box — as it is when its tab is closed, or the window goes.
func TestFilesChangedIsTheTabsOwnSession(t *testing.T) {
	core := newFakeCore("home", workBox("b1"))
	core.setIssue(testRepo, 9, testPull())
	a := dial(t, core)
	a.hello()
	files := func(stream uint32, args Args) Result {
		args.Stream, args.Cols, args.Rows = stream, 100, 30
		id := a.command(Command{Command: "files", Targets: []string{"b1"}, Args: args})
		if st := a.expect(TypeStream).Stream; st.Stream != stream || st.State != StreamAttached {
			t.Fatalf("stream %+v", st)
		}
		return a.result(id)
	}
	ended := func(what string) {
		t.Helper()
		eventually(t, what, func() bool {
			return slices.Contains(core.called(), "end b1 review1")
		})
		core.mu.Lock()
		core.calls = nil
		core.mu.Unlock()
	}

	r := files(3, Args{Head: "fed0000000000", Base: "bad0000000000"})
	if r.Status != StatusDone || r.Session == nil || r.Session.Kind != TabFiles || r.Session.Files != 9 || r.Session.Title != "files changed #9" {
		t.Fatalf("files %+v", r)
	}
	if !slices.Contains(core.called(), "review b1 bad0000000000..fed0000000000 100x30") {
		t.Fatalf("calls %q, want the head and base the app showed", core.called())
	}
	// Typed at, then quit: the tab goes.
	a.frame(frame.Input, 3, []byte("q"))
	term := core.opening("review1")
	eventually(t, "the key reached it", func() bool { return slices.Contains(term.log(), "input q") })
	close(term.out)
	if st := a.expect(TypeStream).Stream; st.State != StreamEnded || st.End != EndTab {
		t.Fatalf("a files tab quit: %+v", st)
	}
	ended("the session ended once quit")

	// Untouched, it ended on its own: held, with what it said.
	files(4, Args{})
	if !slices.Contains(core.called(), "review b1 base000000000..abc1234567890 100x30") {
		t.Fatalf("calls %q, want the head read now", core.called())
	}
	close(core.opening("review1").out)
	if st := a.expect(TypeStream).Stream; st.State != StreamEnded || st.End != EndHeld || st.Status != "discobox-review ended" {
		t.Fatalf("a files tab ended on its own: %+v", st)
	}
	ended("the session ended on its own")

	// Its tab closed: the session is ended in the box.
	files(5, Args{})
	a.send(Message{Type: TypeClose, Close: &Close{Stream: 5}})
	ended("the session ended with its tab")

	// The window gone: so is the session, before the bridge returns.
	files(6, Args{})
	_ = a.out.Close()
	select {
	case <-a.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the bridge did not return")
	}
	if !slices.Contains(core.called(), "end b1 review1") {
		t.Fatalf("the bridge returned with the files session running: %q", core.called())
	}
}

// A files changed session whose start is under way when the window goes is
// waited for, and ended in its box once it lands: nothing would list it.
func TestAFilesChangedStartingAsTheWindowGoesIsEnded(t *testing.T) {
	core := newFakeCore("home", workBox("b1"))
	core.setIssue(testRepo, 9, testPull())
	started := make(chan struct{})
	land := make(chan struct{})
	core.review = func() {
		close(started)
		<-land
	}
	a := dial(t, core)
	a.hello()
	a.command(Command{Command: "files", Targets: []string{"b1"}, Args: Args{Stream: 3, Cols: 80, Rows: 24}})
	<-started
	_ = a.out.Close()
	select {
	case <-a.done:
		t.Fatal("the bridge returned while a files changed session was still starting")
	case <-time.After(100 * time.Millisecond):
	}
	close(land)
	select {
	case <-a.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the bridge did not return")
	}
	if !slices.Contains(core.called(), "end b1 review1") {
		t.Fatalf("a session that landed as the window went is still running: %q", core.called())
	}
}
