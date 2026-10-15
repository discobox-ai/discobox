package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePull answers the endpoints pull request 9 of acme/foo is read from.
type fakePull struct {
	mu    sync.Mutex
	paths []string
	// state is the pull request's, "open" unless set; lint is how the lint
	// check stands, "in_progress" unless set to a conclusion.
	state string
	lint  string
	// refuse is a path prefix answered 403, for a token that may not read it.
	refuse string
	// fork puts the head in someone else's repository.
	fork bool
	// mergeable is the pull request's mergeable_state, "blocked" unless set.
	mergeable string
	// merged is every merge asked for, as its body.
	merged []string
	// endless gives the review comments a next page, every time.
	endless bool
	// unsettled is how many reads of the pull request answer "unknown" for
	// whether it can merge, before the answer.
	unsettled int
}

func (f *fakePull) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	paths := f.paths
	f.paths = nil
	return paths
}

func (f *fakePull) set(change func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change()
}

func (f *fakePull) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.UserAgent(), "port probe") {
			// The sandbox this is developed in asks every new port HEAD /.
			return
		}
		f.mu.Lock()
		f.paths = append(f.paths, r.Method+" "+r.URL.Path)
		state, lint, refuse, fork, mergeable := f.state, f.lint, f.refuse, f.fork, f.mergeable
		f.mu.Unlock()
		if state == "" {
			state = "open"
		}
		if mergeable == "" {
			mergeable = "blocked"
		}
		if r.URL.Path == "/repos/acme/foo/pulls/9" {
			f.mu.Lock()
			if f.unsettled > 0 {
				f.unsettled--
				mergeable = "unknown"
			}
			f.mu.Unlock()
		}
		const repo = "/repos/acme/foo"
		if refuse != "" && strings.HasPrefix(r.URL.Path, repo+refuse) {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"Resource not accessible by personal access token"}`)
			return
		}
		switch r.URL.Path {
		case repo + "/issues/9":
			fmt.Fprintf(w, `{"number": 9, "title": "Reap only stopped boxes", "state": %q,
				"html_url": "https://github.com/acme/foo/pull/9", "user": {"login": "alice"}, "body": "Fixes #4.",
				"created_at": "2026-10-01T10:00:00Z", "updated_at": "2026-10-03T10:00:00Z", "comments": 1,
				"pull_request": {"merged_at": null}}`, state)
		case repo + "/issues/9/timeline":
			fmt.Fprint(w, `[
				{"event": "committed", "sha": "abc1234567890", "message": "reap only the stopped\n\nand say why",
				 "author": {"name": "Alice A", "date": "2026-10-01T09:00:00Z"}, "committer": {"name": "Alice A", "date": "2026-10-01T09:30:00Z"}},
				{"event": "review_requested", "actor": {"login": "alice"}, "review_requester": {"login": "alice"},
				 "requested_reviewer": {"login": "bob"}, "created_at": "2026-10-01T10:01:00Z"},
				{"event": "reviewed", "id": 100, "user": {"login": "bob"}, "state": "changes_requested", "body": "Nearly.",
				 "submitted_at": "2026-10-01T11:00:00Z", "html_url": "https://github.com/acme/foo/pull/9#pullrequestreview-100"},
				{"event": "reviewed", "id": 101, "user": {"login": "alice"}, "state": "commented", "body": "",
				 "submitted_at": "2026-10-01T12:00:00Z"},
				{"event": "reviewed", "id": 102, "user": {"login": "carol"}, "state": "approved", "body": "",
				 "submitted_at": "2026-10-01T13:00:00Z"},
				{"event": "reviewed", "id": 103, "user": {"login": "carol"}, "state": "commented", "body": "One more thought.",
				 "submitted_at": "2026-10-01T14:00:00Z"},
				{"event": "reviewed", "id": 104, "user": {"login": "dave"}, "state": "pending", "body": "a draft"},
				{"event": "merged", "actor": {"login": "erin"}, "commit_id": "fedcba9876543", "created_at": "2026-10-03T10:00:00Z"}
			]`)
		case repo + "/pulls/9":
			head := `"repo": {"full_name": "acme/foo"}`
			if fork {
				head = `"repo": {"full_name": "mallory/foo"}`
			}
			fmt.Fprintf(w, `{"draft": true, "merged": false, "mergeable_state": %q,`, mergeable)
			fmt.Fprintf(w, `
				"head": {"ref": "fix-reaper", "label": "mallory:fix-reaper", "sha": "abc1234567890", %s},
				"base": {"ref": "main", "label": "acme:main", "sha": "0000000", "repo": {"full_name": "acme/foo"}},
				"commits": 1, "additions": 12, "deletions": 3, "changed_files": 2, "review_comments": 3,
				"requested_reviewers": [{"login": "frank"}], "requested_teams": [{"name": "reapers", "slug": "reapers"}]}`, head)
		case repo:
			fmt.Fprint(w, `{"full_name": "acme/foo", "allow_merge_commit": false, "allow_squash_merge": true, "allow_rebase_merge": true}`)
		case repo + "/pulls/9/merge":
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.merged = append(f.merged, r.Method+" "+string(body))
			f.mu.Unlock()
			fmt.Fprint(w, `{"merged": true}`)
		case repo + "/pulls/9/comments":
			f.mu.Lock()
			endless := f.endless
			f.mu.Unlock()
			if endless {
				w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path))
			}
			fmt.Fprint(w, `[
				{"id": 1, "pull_request_review_id": 100, "path": "reaper.go", "line": 3, "original_line": 3,
				 "diff_hunk": "@@ -1,3 +1,4 @@\n-all()\n+stopped()", "user": {"login": "bob"}, "body": "What about paused ones?",
				 "created_at": "2026-10-01T11:00:00Z", "reactions": {"total_count": 1, "eyes": 1}},
				{"id": 2, "pull_request_review_id": 101, "in_reply_to_id": 1, "path": "reaper.go", "line": 3,
				 "user": {"login": "alice"}, "body": "Left alone.", "created_at": "2026-10-01T12:00:00Z"},
				{"id": 3, "pull_request_review_id": 100, "path": "old.go", "line": null, "original_line": 7,
				 "diff_hunk": "@@ -7 +7 @@", "user": {"login": "bob"}, "body": "And here.", "created_at": "2026-10-01T11:00:00Z"}
			]`)
		case repo + "/commits/abc1234567890/check-runs":
			conclusion := `"status": "in_progress", "conclusion": null`
			if lint != "" {
				conclusion = fmt.Sprintf(`"status": "completed", "conclusion": %q`, lint)
			}
			fmt.Fprintf(w, `{"total_count": 3, "check_runs": [
				{"name": "build", "status": "completed", "conclusion": "success", "html_url": "https://ci.example/build"},
				{"name": "lint", %s, "html_url": "https://ci.example/lint"},
				{"name": "e2e", "status": "completed", "conclusion": "timed_out", "html_url": "https://ci.example/e2e"}
			]}`, conclusion)
		case repo + "/commits/abc1234567890/status":
			fmt.Fprint(w, `{"state": "failure", "statuses": [
				{"context": "license/cla", "state": "error", "target_url": "https://cla.example"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A pull request is read as the issue it is — the same title, description and
// timeline — and then the rest of its page: the pull request, its review
// threads under their reviews, its checks and its diff.
func TestAPullRequestIsReadWithTheRestOfItsPage(t *testing.T) {
	t.Parallel()
	fake := &fakePull{}
	issue, err := client(fake.serve(t), "tok").Issue(t.Context(), "acme", "foo", 9, false)
	if err != nil {
		t.Fatal(err)
	}
	pull := issue.Pull
	if !issue.PullRequest || pull == nil {
		t.Fatalf("issue = %+v, want a pull request", issue)
	}
	if !pull.Draft || pull.Merged || pull.Head != "fix-reaper" || pull.Base != "main" || pull.HeadSHA != "abc1234567890" ||
		pull.Commits != 1 || pull.Additions != 12 || pull.Deletions != 3 || pull.ChangedFiles != 2 || pull.Mergeable != "blocked" ||
		!slices.Equal(pull.MergeMethods, []string{"squash", "rebase"}) {
		t.Errorf("pull = %+v", pull)
	}

	// The timeline: the push by what the commit says of itself, the request
	// by who it was of, and the reviews — less the one that is only a reply
	// in a thread, and the one still being written.
	var kinds []string
	for _, event := range issue.Timeline {
		kinds = append(kinds, event.Kind+":"+event.Actor)
	}
	want := []string{"committed:Alice A", "review_requested:alice", "reviewed:bob", "reviewed:carol", "reviewed:carol", "merged:erin"}
	if !slices.Equal(kinds, want) {
		t.Fatalf("timeline = %v, want %v", kinds, want)
	}
	if push := issue.Timeline[0]; push.Subject != "abc1234" || push.Message != "reap only the stopped" || push.At.IsZero() {
		t.Errorf("the push = %+v", push)
	}
	if asked := issue.Timeline[1]; asked.Subject != "bob" {
		t.Errorf("the review request = %+v", asked)
	}
	if merged := issue.Timeline[5]; merged.Subject != "fedcba9" {
		t.Errorf("the merge = %+v", merged)
	}

	review := issue.Timeline[2]
	if review.Review != "changes_requested" || review.Body != "Nearly." || review.At.IsZero() || len(review.Threads) != 2 {
		t.Fatalf("the review = %+v", review)
	}
	thread := review.Threads[0]
	if thread.Path != "reaper.go" || thread.Line != 3 || thread.Outdated || !strings.Contains(thread.Hunk, "+stopped()") {
		t.Errorf("the thread = %+v", thread)
	}
	if len(thread.Comments) != 2 || thread.Comments[0].Author != "bob" || thread.Comments[0].Reactions["eyes"] != 1 ||
		thread.Comments[1].Author != "alice" || thread.Comments[1].Body != "Left alone." {
		t.Errorf("the thread's comments = %+v, want the comment and the reply under it", thread.Comments)
	}
	if outdated := review.Threads[1]; !outdated.Outdated || outdated.Line != 7 {
		t.Errorf("the thread the diff moved on from = %+v, want it outdated, at the line it was on", outdated)
	}

	// Reviewers: what each last said — a later comment does not take back an
	// approval — then those still asked. The author's replies are not a review.
	wantReviewers := []Reviewer{{"bob", "changes_requested"}, {"carol", "approved"}, {"frank", "requested"}, {"reapers", "requested"}}
	if !slices.Equal(pull.Reviewers, wantReviewers) {
		t.Errorf("reviewers = %+v, want %+v", pull.Reviewers, wantReviewers)
	}

	wantChecks := []Check{
		{"build", "success", "https://ci.example/build"},
		{"lint", "pending", "https://ci.example/lint"},
		{"e2e", "failure", "https://ci.example/e2e"},
		{"license/cla", "failure", "https://cla.example"},
	}
	if !slices.Equal(pull.Checks, wantChecks) {
		t.Errorf("checks = %+v, want %+v", pull.Checks, wantChecks)
	}

	// The two commits its files changed are between.
	if pull.HeadSHA != "abc1234567890" || pull.BaseSHA != "0000000" {
		t.Errorf("head %q and base %q, want abc1234567890 and 0000000", pull.HeadSHA, pull.BaseSHA)
	}
}

// A check finishing moves nothing else, so an open pull request's checks are
// read again on a re-read that is otherwise answered from the last one. One
// that is closed has stopped running them.
func TestAnOpenPullRequestsChecksAreReadAgain(t *testing.T) {
	t.Parallel()
	fake := &fakePull{}
	c := client(fake.serve(t), "tok")
	first, err := c.Issue(t.Context(), "acme", "foo", 9, false)
	if err != nil {
		t.Fatal(err)
	}
	fake.requests()
	fake.set(func() { fake.lint, fake.mergeable = "success", "clean" })
	again, err := c.Issue(t.Context(), "acme", "foo", 9, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET /repos/acme/foo/issues/9", "GET /repos/acme/foo/commits/abc1234567890/check-runs",
		"GET /repos/acme/foo/commits/abc1234567890/status", "GET /repos/acme/foo/pulls/9",
	}
	if got := fake.requests(); !slices.Equal(got, want) {
		t.Errorf("an unchanged re-read of an open pull request asked for %v, want %v", got, want)
	}
	if again.Pull.Checks[1].State != "success" {
		t.Errorf("the re-read's lint check is %q, want the success it finished with", again.Pull.Checks[1].State)
	}
	if again.Pull.Mergeable != "clean" || first.Pull.Mergeable != "blocked" {
		t.Errorf("whether it can merge: %q after %q, want it read again and the first read left alone", again.Pull.Mergeable, first.Pull.Mergeable)
	}
	if first.Pull.Checks[1].State != "pending" {
		t.Errorf("the re-read changed the read handed out before it: lint is %q", first.Pull.Checks[1].State)
	}
	if again.Pull.BaseSHA != first.Pull.BaseSHA || len(again.Timeline) != len(first.Timeline) {
		t.Errorf("the re-read lost what was read before: base %q, %d timeline items", again.Pull.BaseSHA, len(again.Timeline))
	}

	closed := &fakePull{state: "closed"}
	c = client(closed.serve(t), "tok")
	if _, err := c.Issue(t.Context(), "acme", "foo", 9, false); err != nil {
		t.Fatal(err)
	}
	closed.requests()
	if _, err := c.Issue(t.Context(), "acme", "foo", 9, false); err != nil {
		t.Fatal(err)
	}
	if got := closed.requests(); len(got) != 1 || got[0] != "GET /repos/acme/foo/issues/9" {
		t.Errorf("an unchanged re-read of a closed pull request asked for %v, want only the issue", got)
	}
}

// The checks are not the pull request: a token that may not read them leaves
// the page without them, not unreadable.
func TestAPullRequestIsReadWithoutChecksItMayNotSee(t *testing.T) {
	t.Parallel()
	fake := &fakePull{refuse: "/commits/"}
	issue, err := client(fake.serve(t), "tok").Issue(t.Context(), "acme", "foo", 9, false)
	if err != nil {
		t.Fatalf("the pull request did not read without its checks: %v", err)
	}
	if len(issue.Pull.Checks) != 0 || issue.Pull.HeadSHA == "" {
		t.Errorf("checks %v, head %q; want no checks and the rest", issue.Pull.Checks, issue.Pull.HeadSHA)
	}
}

// A head in a fork is named with its owner, as the page names it.
func TestAForksBranchIsNamedWithItsOwner(t *testing.T) {
	t.Parallel()
	fake := &fakePull{fork: true}
	issue, err := client(fake.serve(t), "tok").Issue(t.Context(), "acme", "foo", 9, false)
	if err != nil {
		t.Fatal(err)
	}
	if issue.Pull.Head != "mallory:fix-reaper" || issue.Pull.Base != "main" {
		t.Errorf("branches = %q into %q, want the fork's named with its owner", issue.Pull.Head, issue.Pull.Base)
	}
}

func TestCheckStates(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"success": "success", "failure": "failure", "error": "failure", "timed_out": "failure",
		"action_required": "failure", gitHubCanceled: "canceled", "stale": "canceled",
		"skipped": "skipped", "neutral": "neutral", "pending": "pending", "": "pending",
	} {
		if got := checkState(in); got != want {
			t.Errorf("checkState(%q) = %q, want %q", in, got, want)
		}
	}
}

// A merge names the way and the commit that was read, and needs someone to
// merge as; the next read is a full one.
func TestAPullRequestIsMerged(t *testing.T) {
	t.Parallel()
	fake := &fakePull{}
	srv := fake.serve(t)
	c := client(srv, "tok")
	if _, err := c.Issue(t.Context(), "acme", "foo", 9, false); err != nil {
		t.Fatal(err)
	}
	if err := c.Merge(t.Context(), "acme", "foo", 9, "squash", "abc1234567890"); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	merged := slices.Clone(fake.merged)
	fake.mu.Unlock()
	if len(merged) != 1 || merged[0] != `PUT {"merge_method":"squash","sha":"abc1234567890"}` {
		t.Errorf("merged = %v", merged)
	}
	fake.requests()
	if _, err := c.Issue(t.Context(), "acme", "foo", 9, false); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.requests()); n < 5 {
		t.Errorf("the read after a merge made %d requests, want a full read", n)
	}

	anonymous := &Client{API: srv.URL, HTTP: srv.Client(), Token: func(context.Context) string { return "" }}
	if err := anonymous.Merge(t.Context(), "acme", "foo", 9, "squash", ""); !errors.Is(err, ErrNoToken) {
		t.Errorf("a merge with no token: %v, want ErrNoToken", err)
	}
}

// With no token an unchanged re-read is the one request: GitHub allows 60 an
// hour, the checks and the merge state would be three more every time, and
// whether it can merge is "unknown" to a reader it does not know anyway — so
// a full read does not wait for that answer either.
func TestAnAnonymousReReadIsOneRequest(t *testing.T) {
	t.Parallel()
	fake := &fakePull{mergeable: "clean", unsettled: 100}
	now := time.Now()
	c := client(fake.serve(t), "")
	c.now = func() time.Time { return now }
	if _, err := c.Issue(t.Context(), "acme", "foo", 9, false); err != nil {
		t.Fatal(err)
	}
	asked := 0
	for _, path := range fake.requests() {
		if path == "GET /repos/acme/foo/pulls/9" {
			asked++
		}
	}
	if asked != 1 {
		t.Errorf("an anonymous full read asked for the pull request %d times, want once", asked)
	}
	now = now.Add(AnonymousRefresh + time.Second)
	if _, err := c.Issue(t.Context(), "acme", "foo", 9, false); err != nil {
		t.Fatal(err)
	}
	if got := fake.requests(); !slices.Equal(got, []string{"GET /repos/acme/foo/issues/9"}) {
		t.Errorf("an anonymous re-read asked for %v, want the issue only", got)
	}
}

// A re-read whose checks could not be read keeps the ones it knew: a request
// that failed is not a pull request with no checks. A full read that cannot
// read them says the list is not whole.
func TestChecksThatCouldNotBeReadAreNotNoChecks(t *testing.T) {
	t.Parallel()
	fake := &fakePull{}
	c := client(fake.serve(t), "tok")
	first, err := c.Issue(t.Context(), "acme", "foo", 9, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Pull.Checks) == 0 || first.Pull.ChecksPartial {
		t.Fatalf("checks = %+v, partial %v, want all of them", first.Pull.Checks, first.Pull.ChecksPartial)
	}
	fake.set(func() { fake.refuse = "/commits/" })
	again, err := c.Issue(t.Context(), "acme", "foo", 9, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Pull.Checks) != len(first.Pull.Checks) || !again.Pull.ChecksPartial {
		t.Errorf("a failed read of the checks left %d of them (partial %v), want the %d known and said to be no longer the whole", len(again.Pull.Checks), again.Pull.ChecksPartial, len(first.Pull.Checks))
	}
	full, err := c.Issue(t.Context(), "acme", "foo", 9, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Pull.Checks) != 0 || !full.Pull.ChecksPartial {
		t.Errorf("a full read refused its checks: %d checks, partial %v, want none and said so", len(full.Pull.Checks), full.Pull.ChecksPartial)
	}
}

// GitHub answers "unknown" for whether a pull request can merge until it has
// worked it out, which asking starts: a full read asks again, a bounded number
// of times.
func TestAFullReadWaitsForGitHubToSayWhetherItCanMerge(t *testing.T) {
	t.Parallel()
	fake := &fakePull{mergeable: "clean", unsettled: 1}
	issue, err := client(fake.serve(t), "tok").Issue(t.Context(), "acme", "foo", 9, false)
	if err != nil {
		t.Fatal(err)
	}
	if issue.Pull.Mergeable != "clean" {
		t.Errorf("mergeable = %q, want the answer GitHub gave the second time", issue.Pull.Mergeable)
	}

	never := &fakePull{mergeable: "clean", unsettled: 100}
	issue, err = client(never.serve(t), "tok").Issue(t.Context(), "acme", "foo", 9, false)
	if err != nil {
		t.Fatal(err)
	}
	asked := 0
	for _, path := range never.requests() {
		if path == "GET /repos/acme/foo/pulls/9" {
			asked++
		}
	}
	if issue.Pull.Mergeable != "unknown" || asked != 1+mergeSettleTries {
		t.Errorf("mergeable = %q after %d reads of the pull request, want it left unknown after %d", issue.Pull.Mergeable, asked, 1+mergeSettleTries)
	}
}

// More review comments than are read is said, as a long timeline is: the
// threads past the last page read are not on the tab.
func TestMoreReviewCommentsThanAreReadIsSaid(t *testing.T) {
	t.Parallel()
	fake := &fakePull{endless: true}
	issue, err := client(fake.serve(t), "tok").Issue(t.Context(), "acme", "foo", 9, false)
	if err != nil {
		t.Fatal(err)
	}
	asked := 0
	for _, path := range fake.requests() {
		if path == "GET /repos/acme/foo/pulls/9/comments" {
			asked++
		}
	}
	if asked != timelinePages || !issue.Truncated {
		t.Errorf("%d pages of review comments read, truncated %v; want %d and said", asked, issue.Truncated, timelinePages)
	}

	whole, err := client((&fakePull{}).serve(t), "tok").Issue(t.Context(), "acme", "foo", 9, false)
	if err != nil {
		t.Fatal(err)
	}
	if whole.Truncated {
		t.Errorf("a pull request read whole says it was cut short")
	}
}

// A person and a team asked to review may share a name: they are two
// reviewers, not one.
func TestAUserAndATeamOfOneNameAreTwoReviewers(t *testing.T) {
	t.Parallel()
	var raw rawPull
	raw.RequestedReviewers = []rawUser{{Login: "core"}}
	raw.RequestedTeams = append(raw.RequestedTeams, struct {
		Name string `json:"name"`
	}{Name: "core"})
	timeline := []Event{{Kind: "reviewed", Actor: "core", Review: "approved"}}
	got := reviewers("alice", timeline, raw)
	if len(got) != 2 || got[0].State != "requested" || got[1].State != "requested" {
		t.Errorf("reviewers = %+v, want the person (asked again) and the team", got)
	}
}
