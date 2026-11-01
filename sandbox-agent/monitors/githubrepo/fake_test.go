package githubrepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/monitors"
	"github.com/discobox-ai/discobox/sandbox-agent/monitors/builtin"
)

// fakeGitHub answers the requests the monitor makes, the way GitHub does:
// REST bodies with ETags and a 304 for a matching If-None-Match, the GraphQL
// search paged and filtered by update, and rate limits on demand. Bodies are
// trimmed recordings of real responses.
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server

	mu   sync.Mutex
	now  time.Time
	rest map[string]string
	// nodes are every issue and PR, as the search returns them.
	nodes []activityNode
	// prs are what the watched query returns, by number.
	prs map[int64]watchedPR
	// issueStates are what the watched query returns for a named issue.
	issueStates map[int64]string
	// limitAt rate-limits the query of a kind (search, watched, reviews,
	// comments) at the call with this number, from one.
	limitAt map[string]int
	calls   map[string]int
	// reviewPages are older review pages by PR id, served by `before`.
	reviewPages map[string]reviews
	// contextPages are a head's further check pages by commit id.
	contextPages map[string]contexts
	// reviewComments are inline comments by review id.
	reviewComments map[string][]reviewComment

	// missingOK expects GETs of what is not there, answered 404.
	missingOK bool

	counted  int
	requests []string
}

func newFake(t *testing.T, now time.Time) *fakeGitHub {
	f := &fakeGitHub{
		t: t, now: now, rest: map[string]string{}, prs: map[int64]watchedPR{}, issueStates: map[int64]string{},
		limitAt: map[string]int{}, calls: map[string]int{}, reviewPages: map[string]reviews{}, contextPages: map[string]contexts{}, reviewComments: map[string][]reviewComment{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) set(path, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rest[path] = body
}

func (f *fakeGitHub) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// reset forgets what was counted, for a run whose cost a test measures.
func (f *fakeGitHub) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counted, f.requests = 0, nil
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Date", f.now.UTC().Format(http.TimeFormat))
	if r.Header.Get("Authorization") != "Bearer token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.URL.Path == "/graphql" {
		f.counted++
		f.requests = append(f.requests, "graphql")
		w.Header().Set("X-RateLimit-Resource", "graphql")
		f.graphql(w, r)
		return
	}
	path := r.URL.RequestURI()
	body, ok := f.rest[path]
	if !ok {
		if !f.missingOK {
			f.t.Errorf("unexpected GET %s", path)
		}
		w.WriteHeader(http.StatusNotFound)
		return
	}
	sum := sha256.Sum256([]byte(body))
	etag := `W/"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		f.requests = append(f.requests, "304 "+path)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	f.counted++
	f.requests = append(f.requests, "GET "+path)
	w.Header().Set("X-RateLimit-Resource", "core")
	io.WriteString(w, body)
}

var (
	updatedFrom = regexp.MustCompile(`updated:>=(\S+)`)
	prAlias     = regexp.MustCompile(`p(\d+): pullRequest\(number: \d+\)`)
	issueAlias  = regexp.MustCompile(`i(\d+): issueOrPullRequest\(number: \d+\)`)
)

func (f *fakeGitHub) graphql(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Fatal(err)
	}
	kind := ""
	switch {
	case strings.Contains(req.Query, "search("):
		kind = "search"
	case strings.Contains(req.Query, "fragment watched"):
		kind = "watched"
	case strings.Contains(req.Query, "pullRequests(states: OPEN"):
		kind = "open"
	case strings.Contains(req.Query, "reviews(last:") && strings.Contains(req.Query, "before"):
		kind = "reviews"
	case strings.Contains(req.Query, "nodes(ids:"):
		kind = "comments"
	case strings.Contains(req.Query, "... on Commit"):
		kind = "contexts"
	default:
		f.t.Errorf("unexpected query %s", req.Query)
	}
	f.calls[kind]++
	if f.calls[kind] == f.limitAt[kind] {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(f.now.Add(10*time.Minute).Unix(), 10))
		io.WriteString(w, `{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`)
		return
	}
	var data any
	switch kind {
	case "search":
		data = f.search(req.Variables)
	case "watched":
		repo := map[string]any{}
		for _, m := range prAlias.FindAllStringSubmatch(req.Query, -1) {
			n, _ := strconv.ParseInt(m[1], 10, 64)
			if pr, ok := f.prs[n]; ok {
				repo["p"+m[1]] = pr
			} else {
				repo["p"+m[1]] = nil
			}
		}
		for _, m := range issueAlias.FindAllStringSubmatch(req.Query, -1) {
			n, _ := strconv.ParseInt(m[1], 10, 64)
			repo["i"+m[1]] = map[string]string{"state": f.issueStates[n]}
		}
		data = map[string]any{"repository": repo}
	case "open":
		var nodes []map[string]any
		for _, pr := range f.prs {
			if pr.State == "OPEN" {
				nodes = append(nodes, map[string]any{"number": pr.Number, "headRefName": pr.HeadRefName, "author": pr.Author})
			}
		}
		data = map[string]any{"repository": map[string]any{"pullRequests": map[string]any{"pageInfo": pageInfo{}, "nodes": nodes}}}
	case "contexts":
		data = map[string]any{"node": map[string]any{"statusCheckRollup": map[string]any{"contexts": f.contextPages[req.Variables["id"].(string)]}}}
	case "reviews":
		data = map[string]any{"node": map[string]any{"reviews": f.reviewPages[req.Variables["id"].(string)]}}
	case "comments":
		var nodes []any
		for _, id := range req.Variables["ids"].([]any) {
			comments := f.reviewComments[id.(string)]
			nodes = append(nodes, map[string]any{"id": id, "comments": map[string]any{"totalCount": len(comments), "nodes": comments}})
		}
		data = map[string]any{"nodes": nodes}
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		f.t.Error(err)
	}
}

// search is the search's page: the nodes updated from the query's time on,
// oldest first, past the cursor, each with its timeline since `since`.
func (f *fakeGitHub) search(vars map[string]any) any {
	m := updatedFrom.FindStringSubmatch(vars["q"].(string))
	from, err := time.Parse(time.RFC3339, m[1])
	if err != nil {
		f.t.Fatal(err)
	}
	since, _ := time.Parse(time.RFC3339, vars["since"].(string))
	var matched []activityNode
	for _, n := range f.nodes {
		if !n.UpdatedAt.Before(from) {
			matched = append(matched, n)
		}
	}
	sort.SliceStable(matched, func(i, j int) bool { return matched[i].UpdatedAt.Before(matched[j].UpdatedAt) })
	start := 0
	if after, ok := vars["after"].(string); ok {
		start, _ = strconv.Atoi(after)
	}
	end := min(start+searchPage, len(matched))
	page := make([]activityNode, 0, end-start)
	for _, n := range matched[start:end] {
		var items []timelineItem
		for _, item := range n.Timeline.Nodes {
			if !item.CreatedAt.Before(since) {
				items = append(items, item)
			}
		}
		n.Timeline.Nodes = items
		page = append(page, n)
	}
	return map[string]any{"search": map[string]any{
		"pageInfo": pageInfo{HasNextPage: end < len(matched), EndCursor: strconv.Itoa(end)},
		"nodes":    page,
	}}
}

// issuesProbe is the probe's body: the issue updated last.
func (f *fakeGitHub) touchIssues() {
	f.set("/repos/o/r/issues?state=all&sort=updated&direction=desc&per_page=1", fmt.Sprintf(`[{"number":1,"updated_at":%q}]`, f.now.Format(time.RFC3339Nano)))
}

// setChecks is a head's check runs, and its commit statuses unless a test
// has set them.
func (f *fakeGitHub) setChecks(sha, checkRuns string) {
	f.set("/repos/o/r/commits/"+sha+"/check-runs?per_page=100", checkRuns)
	status := "/repos/o/r/commits/" + sha + "/status?per_page=100"
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rest[status]; !ok {
		f.rest[status] = `{"state":"pending","statuses":[]}`
	}
}

func (f *fakeGitHub) setRuns(runs ...workflowRun) {
	body, err := json.Marshal(runList{TotalCount: len(runs), Runs: runs})
	if err != nil {
		f.t.Fatal(err)
	}
	f.set("/repos/o/r/actions/runs?per_page=100", string(body))
}

// instance runs the monitor against the fake, as one runner's runs would.
type instance struct {
	t      *testing.T
	f      *fakeGitHub
	env    builtin.Env
	clock  time.Time
	events []monitors.Event
}

func newInstance(t *testing.T, f *fakeGitHub, opts map[string]string) *instance {
	in := &instance{t: t, f: f, clock: time.Now()}
	in.env = builtin.Env{
		Instance: "test",
		State:    t.TempDir(),
		Options:  opts,
		Getenv: func(name string) string {
			if name == "GH_TOKEN" {
				return "token"
			}
			return ""
		},
		Emit: func(ev monitors.Event) error {
			in.events = append(in.events, ev)
			return nil
		},
		Stderr: io.Discard,
	}
	return in
}

// poll is one run, returning the events it printed.
func (in *instance) poll() []monitors.Event {
	in.t.Helper()
	before := len(in.events)
	if err := run(context.Background(), in.env, in.f.srv.URL, in.f.srv.Client(), func() time.Time { return in.clock }); err != nil {
		in.t.Fatalf("run: %v", err)
	}
	return in.events[before:]
}

func (in *instance) state() state {
	in.t.Helper()
	var st state
	if _, err := in.env.ReadState(stateFile, &st); err != nil {
		in.t.Fatal(err)
	}
	return st
}

func kinds(events []monitors.Event) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Kind)
	}
	return out
}
