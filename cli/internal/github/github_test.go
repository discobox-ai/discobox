package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub answers the endpoints an issue is read from, with a timeline in
// two pages, and records what is posted to it.
type fakeGitHub struct {
	mu       sync.Mutex
	auth     []string
	paths    []string
	comments []string
	status   int
	// updated is the issue's updated_at.
	updated string
}

// requests are the paths asked for since the last call.
func (f *fakeGitHub) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	paths := f.paths
	f.paths = nil
	return paths
}

func (f *fakeGitHub) setUpdated(at string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updated = at
}

func (f *fakeGitHub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.paths = append(f.paths, r.Method+" "+r.URL.Path)
		status, updated := f.status, f.updated
		if updated == "" {
			updated = "2026-10-03T10:00:00Z"
		}
		f.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/foo/issues/4":
			fmt.Fprintf(w, `{"updated_at": %q,`, updated)
			fmt.Fprint(w, `
				"number": 4, "title": "The reaper", "state": "closed", "state_reason": "not_planned",
				"html_url": "https://github.com/acme/foo/issues/4", "user": {"login": "alice"},
				"body": "It reaps.", "created_at": "2026-10-01T10:00:00Z", "closed_at": "2026-10-03T10:00:00Z",
				"closed_by": {"login": "bob"}, "labels": [{"name": "bug", "color": "d73a4a"}],
				"assignees": [{"login": "bob"}], "milestone": {"title": "v1"}, "type": {"name": "Bug"},
				"comments": 1, "reactions": {"url": "x", "total_count": 3, "+1": 2, "heart": 1, "eyes": 0},
				"sub_issues_summary": {"total": 1, "completed": 1}
			}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/foo/issues/4/sub_issues" && r.URL.Query().Get("page") == "":
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/acme/foo/issues/4/sub_issues?per_page=100&page=2>; rel="next"`, srv.URL))
			fmt.Fprint(w, `[{"number": 5, "title": "A test", "state": "closed", "html_url": "u"}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/foo/issues/4/sub_issues" && r.URL.Query().Get("page") == "2":
			fmt.Fprint(w, `[{"number": 6, "title": "Another", "state": "open", "html_url": "u"}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/foo/issues/4/timeline" && r.URL.Query().Get("page") == "":
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/acme/foo/issues/4/timeline?per_page=100&page=2>; rel="next", <x>; rel="last"`, srv.URL))
			fmt.Fprint(w, `[
				{"event": "labeled", "actor": {"login": "carol"}, "created_at": "2026-10-01T11:00:00Z", "label": {"name": "bug", "color": "d73a4a"}},
				{"event": "subscribed", "actor": {"login": "carol"}, "created_at": "2026-10-01T11:00:00Z"},
				{"event": "commented", "user": {"login": "bob"}, "actor": {"login": "bob"}, "created_at": "2026-10-02T10:00:00Z",
				 "body": "Same here.", "html_url": "c", "reactions": {"total_count": 1, "rocket": 1}}
			]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/foo/issues/4/timeline" && r.URL.Query().Get("page") == "2":
			fmt.Fprint(w, `[
				{"event": "cross-referenced", "actor": {"login": "dave"}, "created_at": "2026-10-02T12:00:00Z",
				 "source": {"type": "issue", "issue": {"number": 9, "title": "Fix it", "state": "closed",
				  "pull_request": {"merged_at": "2026-10-03T09:00:00Z"}, "repository": {"full_name": "acme/foo"}}}},
				{"event": "closed", "actor": {"login": "bob"}, "created_at": "2026-10-03T10:00:00Z", "state_reason": "not_planned",
				 "commit_id": "0123456789abcdef"}
			]`)
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/acme/foo/issues/4":
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.comments = append(f.comments, "PATCH "+string(body))
			f.mu.Unlock()
			fmt.Fprint(w, `{}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/foo/issues/4/comments":
			var payload struct{ Body string }
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &payload)
			f.mu.Lock()
			f.comments = append(f.comments, payload.Body)
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func client(srv *httptest.Server, token string) *Client {
	return &Client{API: srv.URL, HTTP: srv.Client(), Token: func(context.Context) string { return token }}
}

// The issue comes back as its page shows it: the issue, its sub-issues, and
// the timeline across every page, less the events about notifications.
func TestAnIssueIsReadWithItsWholeTimeline(t *testing.T) {
	t.Parallel()
	fake := &fakeGitHub{}
	srv := fake.serve(t)
	issue, err := client(srv, "tok").Issue(t.Context(), "acme", "foo", 4, false)
	if err != nil {
		t.Fatal(err)
	}
	if issue.Repository != "acme/foo" || issue.Title != "The reaper" || issue.State != "closed" ||
		issue.StateReason != "not_planned" || issue.Author != "alice" || issue.ClosedBy != "bob" ||
		issue.Milestone != "v1" || issue.Type != "Bug" || issue.Comments != 1 {
		t.Errorf("issue = %+v", issue)
	}
	if len(issue.Labels) != 1 || issue.Labels[0] != (Label{Name: "bug", Color: "d73a4a"}) {
		t.Errorf("labels = %+v", issue.Labels)
	}
	if want := (Reactions{"+1": 2, "heart": 1}); fmt.Sprint(issue.Reactions) != fmt.Sprint(want) {
		t.Errorf("reactions = %v, want %v", issue.Reactions, want)
	}
	if len(issue.SubIssues) != 2 || issue.SubIssues[0].Number != 5 || issue.SubIssues[1].Number != 6 || issue.SubIssuesDone != 1 {
		t.Errorf("sub-issues = %+v, %d done", issue.SubIssues, issue.SubIssuesDone)
	}
	var kinds []string
	for _, event := range issue.Timeline {
		kinds = append(kinds, event.Kind)
	}
	if got, want := strings.Join(kinds, " "), "labeled commented cross-referenced closed"; got != want {
		t.Fatalf("timeline = %s, want %s", got, want)
	}
	if c := issue.Timeline[1]; c.Actor != "bob" || c.Body != "Same here." || c.Reactions["rocket"] != 1 {
		t.Errorf("comment = %+v", c)
	}
	if src := issue.Timeline[2].Source; src == nil || src.Number != 9 || !src.PullRequest || !src.Merged || src.Repository != "acme/foo" {
		t.Errorf("cross-reference = %+v", src)
	}
	if closed := issue.Timeline[3]; closed.Subject != "0123456" || closed.StateReason != "not_planned" {
		t.Errorf("close = %+v", closed)
	}
	for _, auth := range fake.auth {
		if auth != "Bearer tok" {
			t.Fatalf("sent Authorization %q, want the token", auth)
		}
	}
}

// With no token a public issue is still read, anonymously; a comment is not
// posted at all.
func TestWithoutATokenReadingIsAnonymousAndCommentingIsRefused(t *testing.T) {
	t.Parallel()
	fake := &fakeGitHub{}
	srv := fake.serve(t)
	c := client(srv, "")
	if _, err := c.Issue(t.Context(), "acme", "foo", 4, false); err != nil {
		t.Fatal(err)
	}
	for _, auth := range fake.auth {
		if auth != "" {
			t.Fatalf("sent Authorization %q with no token", auth)
		}
	}
	if err := c.Comment(t.Context(), "acme", "foo", 4, "hi"); !errors.Is(err, ErrNoToken) {
		t.Errorf("Comment = %v, want ErrNoToken", err)
	}
	if len(fake.comments) != 0 {
		t.Errorf("posted %v with no token", fake.comments)
	}
}

func TestACommentIsPosted(t *testing.T) {
	t.Parallel()
	fake := &fakeGitHub{}
	srv := fake.serve(t)
	if err := client(srv, "tok").Comment(t.Context(), "acme", "foo", 4, "Fixed in #9"); err != nil {
		t.Fatal(err)
	}
	if len(fake.comments) != 1 || fake.comments[0] != "Fixed in #9" {
		t.Errorf("posted %v", fake.comments)
	}
}

// A refusal carries GitHub's own message, and an anonymous 404 says what it
// usually means.
func TestARefusalSaysWhy(t *testing.T) {
	t.Parallel()
	fake := &fakeGitHub{status: http.StatusNotFound}
	srv := fake.serve(t)
	_, err := client(srv, "").Issue(t.Context(), "acme", "foo", 4, false)
	var refused *Error
	if !errors.As(err, &refused) || refused.Status != 404 || !strings.Contains(refused.Message, "gh auth login") {
		t.Errorf("err = %v, want a 404 that says how to log in", err)
	}
}

// A token GitHub refuses is asked for again on the next request.
func TestARefusedTokenIsAskedForAgain(t *testing.T) {
	t.Parallel()
	fake := &fakeGitHub{status: http.StatusUnauthorized}
	srv := fake.serve(t)
	asked := 0
	c := &Client{API: srv.URL, HTTP: srv.Client(), Token: func(context.Context) string { asked++; return "tok" }}
	_, _ = c.Issue(t.Context(), "acme", "foo", 4, false)
	_, _ = c.Issue(t.Context(), "acme", "foo", 4, false)
	if asked != 2 {
		t.Errorf("the token was asked for %d times across two refusals, want 2", asked)
	}
}

func TestRepositoryIsReadOffTheWebAddress(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		url, owner, name string
		ok               bool
	}{
		{"https://github.com/acme/foo", "acme", "foo", true},
		{"https://github.com/acme/foo.git", "acme", "foo", true},
		{"https://github.com/acme/foo/", "acme", "foo", true},
		{"https://gitlab.com/acme/foo", "", "", false},
		{"https://github.com/acme", "", "", false},
		{"https://github.com/acme/foo/issues", "", "", false},
	} {
		owner, name, ok := Repository(tc.url)
		if owner != tc.owner || name != tc.name || ok != tc.ok {
			t.Errorf("Repository(%q) = %q, %q, %v", tc.url, owner, name, ok)
		}
	}
}

func TestTheTokenComesFromTheEnvironmentFirst(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", " from-env \n")
	if got := LocalToken(t.Context()); got != "from-env" {
		t.Errorf("LocalToken = %q, want GITHUB_TOKEN's", got)
	}
	t.Setenv("GH_TOKEN", "gh-first")
	if got := LocalToken(t.Context()); got != "gh-first" {
		t.Errorf("LocalToken = %q, want GH_TOKEN's ahead of it", got)
	}
}

// Closing sends the state and how it closed; reopening sends the state alone
// when there is no reason; neither is sent with no token.
func TestTheStateIsSet(t *testing.T) {
	t.Parallel()
	fake := &fakeGitHub{}
	srv := fake.serve(t)
	c := client(srv, "tok")
	if err := c.SetState(t.Context(), "acme", "foo", 4, "closed", "not_planned"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetState(t.Context(), "acme", "foo", 4, "open", ""); err != nil {
		t.Fatal(err)
	}
	want := []string{`PATCH {"state":"closed","state_reason":"not_planned"}`, `PATCH {"state":"open"}`}
	if fmt.Sprint(fake.comments) != fmt.Sprint(want) {
		t.Errorf("sent %v, want %v", fake.comments, want)
	}
	if err := client(srv, "").SetState(t.Context(), "acme", "foo", 4, "closed", "completed"); !errors.Is(err, ErrNoToken) {
		t.Errorf("SetState with no token = %v, want ErrNoToken", err)
	}
}

// A re-read of an issue nothing has happened to is the issue alone; one that
// was updated, or that a comment was posted on from here, is read in full.
func TestARereadReadsTheTimelineOnlyWhenTheIssueChanged(t *testing.T) {
	t.Parallel()
	fake := &fakeGitHub{}
	srv := fake.serve(t)
	c := client(srv, "tok")
	first, err := c.Issue(t.Context(), "acme", "foo", 4, false)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(fake.requests()); n < 5 {
		t.Fatalf("the first read made %d requests, want the issue, both pages of its sub-issues and both of the timeline", n)
	}
	again, err := c.Issue(t.Context(), "acme", "foo", 4, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := fake.requests(); len(got) != 1 || got[0] != "GET /repos/acme/foo/issues/4" {
		t.Errorf("an unchanged re-read asked for %v, want only the issue", got)
	}
	if len(again.Timeline) != len(first.Timeline) {
		t.Errorf("the unchanged re-read has %d timeline items, want the %d read before", len(again.Timeline), len(first.Timeline))
	}

	fake.setUpdated("2026-10-04T10:00:00Z")
	if _, err := c.Issue(t.Context(), "acme", "foo", 4, false); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.requests()); n < 4 {
		t.Errorf("a re-read after an update made %d requests, want a full read", n)
	}

	if err := c.Comment(t.Context(), "acme", "foo", 4, "hi"); err != nil {
		t.Fatal(err)
	}
	fake.requests()
	if _, err := c.Issue(t.Context(), "acme", "foo", 4, false); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.requests()); n < 4 {
		t.Errorf("a re-read after a comment from here made %d requests, want a full read", n)
	}
}

// With no token GitHub allows 60 requests an hour, so an issue is not read
// again until AnonymousRefresh has passed.
func TestWithoutATokenAnIssueIsReadAgainOnlyAfterAWhile(t *testing.T) {
	t.Parallel()
	fake := &fakeGitHub{}
	srv := fake.serve(t)
	now := time.Now()
	c := client(srv, "")
	c.now = func() time.Time { return now }
	if _, err := c.Issue(t.Context(), "acme", "foo", 4, false); err != nil {
		t.Fatal(err)
	}
	fake.requests()
	now = now.Add(AnonymousRefresh - time.Second)
	if _, err := c.Issue(t.Context(), "acme", "foo", 4, false); err != nil {
		t.Fatal(err)
	}
	if got := fake.requests(); len(got) != 0 {
		t.Errorf("an anonymous re-read inside %v asked for %v, want nothing", AnonymousRefresh, got)
	}
	now = now.Add(2 * time.Second)
	if _, err := c.Issue(t.Context(), "acme", "foo", 4, false); err != nil {
		t.Fatal(err)
	}
	if got := fake.requests(); len(got) == 0 {
		t.Errorf("an anonymous re-read after %v asked for nothing", AnonymousRefresh)
	}
}

// No token is believed for a while rather than `gh` being run per request.
func TestNoTokenIsAskedForOnlyNowAndThen(t *testing.T) {
	t.Parallel()
	fake := &fakeGitHub{}
	srv := fake.serve(t)
	now := time.Now()
	asked := 0
	c := &Client{API: srv.URL, HTTP: srv.Client(), Token: func(context.Context) string { asked++; return "" }, now: func() time.Time { return now }}
	if _, err := c.Issue(t.Context(), "acme", "foo", 4, false); err != nil {
		t.Fatal(err)
	}
	if asked != 1 {
		t.Errorf("one read of several requests asked for the token %d times, want once", asked)
	}
	now = now.Add(tokenRetry)
	_ = c.Comment(t.Context(), "acme", "foo", 4, "hi")
	if asked != 2 {
		t.Errorf("after %v the token was asked for %d times in all, want twice", tokenRetry, asked)
	}
}

// A full read is what someone asked for, and gets the timeline whatever was
// kept; a kept read is trusted on updated_at alone only for a while, since a
// reaction does not move it.
func TestAFullReadAndAnOldOneReadTheTimeline(t *testing.T) {
	t.Parallel()
	fake := &fakeGitHub{}
	srv := fake.serve(t)
	now := time.Now()
	c := client(srv, "tok")
	c.now = func() time.Time { return now }
	if _, err := c.Issue(t.Context(), "acme", "foo", 4, false); err != nil {
		t.Fatal(err)
	}
	fake.requests()
	if _, err := c.Issue(t.Context(), "acme", "foo", 4, true); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.requests()); n < 4 {
		t.Errorf("a full read made %d requests, want the whole issue", n)
	}
	now = now.Add(keptFor)
	if _, err := c.Issue(t.Context(), "acme", "foo", 4, false); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.requests()); n < 4 {
		t.Errorf("a re-read %v after the last full one made %d requests, want the whole issue", keptFor, n)
	}
	// Without a token, a full read is not held back either.
	anon := client(srv, "")
	if _, err := anon.Issue(t.Context(), "acme", "foo", 4, false); err != nil {
		t.Fatal(err)
	}
	fake.requests()
	if _, err := anon.Issue(t.Context(), "acme", "foo", 4, true); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.requests()); n == 0 {
		t.Errorf("an anonymous full read asked for nothing")
	}
}
