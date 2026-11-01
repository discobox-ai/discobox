package discoboxbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/sandboxmeta"
	"github.com/discobox-ai/discobox/sandbox-agent/monitors"
	"github.com/discobox-ai/discobox/sandbox-agent/monitors/builtin"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// fakeAPI is the discobox API as a box with discoboxApi sees it: the
// project's boxes, and the secret requests of the boxes it created.
type fakeAPI struct {
	mu    sync.Mutex
	boxes []box
	// requests are served as the API writes them, not as the monitor's
	// struct would encode: a field read as the wrong type fails here too.
	requests []map[string]any
	// refuse, when set, is the status and text every call is answered with.
	refuse     int
	refuseText string
	calls      []string
	srv        *httptest.Server
}

func newFake(t *testing.T) *fakeAPI {
	f := &fakeAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.URL.RequestURI())
	if f.refuse != 0 {
		http.Error(w, f.refuseText, f.refuse)
		return
	}
	switch r.URL.Path {
	case "/projects/default/sandboxes":
		selectors, err := sandboxmeta.ParseSelectors(r.URL.Query()["tag"])
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		out := []map[string]any{}
		for _, b := range f.boxes {
			if sandboxmeta.MatchesAll(b.Meta.Tags, selectors) {
				out = append(out, apiBox(b))
			}
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"sandboxes": out}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	case "/projects/default/secret-requests":
		if err := json.NewEncoder(w).Encode(map[string]any{"secretRequests": f.requests}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	default:
		http.NotFound(w, r)
	}
}

// apiBox is a Sandbox as the API serves it, written out by hand rather than
// encoded from the monitor's struct, with fields the monitor does not read.
func apiBox(b box) map[string]any {
	out := map[string]any{
		"id": b.ID, "projectId": "default", "poolId": "pool_1", "createdAt": b.CreatedAt, "updatedAt": b.CreatedAt,
		"discoboxApi": false, "displayName": b.ID + ": on it",
		"config": map[string]any{"name": b.Config.Name, "bundleDigest": "sha256:0"},
		"meta":   map[string]any{"name": b.Meta.Name, "tags": b.Meta.Tags},
	}
	if b.Runtime != nil {
		runtime := map[string]any{"displayState": b.Runtime.DisplayState, "state": "ready", "desiredState": "present"}
		if b.Runtime.AgentStatus != nil {
			runtime["agentStatus"] = map[string]any{"sessions": b.Runtime.AgentStatus.Sessions, "git": map[string]any{}}
		}
		out["runtime"] = runtime
	}
	return out
}

// request is a SecretRequest as the API serves it.
func request(id, sandbox, status, name string, created time.Time) map[string]any {
	return map[string]any{
		"id": id, "sandboxId": sandbox, "status": status, "name": name, "purpose": "use",
		"projectId": "default", "createdAt": created, "updatedAt": created,
		"routes": []map[string]any{{"hosts": []string{"api.github.com"}}},
	}
}

func (f *fakeAPI) box(id string) *box {
	for i := range f.boxes {
		if f.boxes[i].ID == id {
			return &f.boxes[i]
		}
	}
	panic("no box " + id)
}

func (f *fakeAPI) remove(id string) {
	f.boxes = slices.DeleteFunc(f.boxes, func(b box) bool { return b.ID == id })
}

// newBox is a running box named name with tags, its primary terminal's
// program reporting program ("" for none).
func newBox(id, name string, created time.Time, program string, tags map[string]string) box {
	var b box
	b.ID, b.CreatedAt, b.Meta.Name, b.Meta.Tags = id, created, name, tags
	setState(&b, "running")
	setProgram(&b, program, "")
	return b
}

func setState(b *box, state string) {
	if b.Runtime == nil {
		b.Runtime = &struct {
			DisplayState string `json:"displayState"`
			AgentStatus  *struct {
				Sessions json.RawMessage `json:"sessions"`
			} `json:"agentStatus"`
		}{}
	}
	b.Runtime.DisplayState = state
}

func setProgram(b *box, state, kind string) {
	b.Runtime.AgentStatus = &struct {
		Sessions json.RawMessage `json:"sessions"`
	}{}
	if state == "" {
		b.Runtime.AgentStatus.Sessions = json.RawMessage(`[]`)
		return
	}
	b.Runtime.AgentStatus.Sessions = json.RawMessage(fmt.Sprintf(
		`[{"primary":true,"startedAt":"2026-11-01T11:00:00Z","programStatus":{"state":%q,"kind":%q,"msg":"on it"}}]`, state, kind))
}

type instance struct {
	t      *testing.T
	f      *fakeAPI
	env    builtin.Env
	vars   map[string]string
	clock  time.Time
	events []monitors.Event
}

func newInstance(t *testing.T, f *fakeAPI, opts map[string]string) *instance {
	in := &instance{t: t, f: f, clock: t0, vars: map[string]string{envAPIURL: f.srv.URL}}
	in.env = builtin.Env{
		Instance: "test",
		State:    t.TempDir(),
		Options:  opts,
		Getenv:   func(name string) string { return in.vars[name] },
		Emit: func(ev monitors.Event) error {
			in.events = append(in.events, ev)
			return nil
		},
		Stderr: io.Discard,
	}
	return in
}

// poll is one run, a minute after the last, returning what it printed.
func (in *instance) poll() []monitors.Event {
	in.t.Helper()
	events, err := in.try()
	if err != nil {
		in.t.Fatalf("run: %v", err)
	}
	return events
}

func (in *instance) try() ([]monitors.Event, error) {
	before := len(in.events)
	err := run(context.Background(), in.env, in.f.srv.Client(), func() time.Time { return in.clock })
	in.clock = in.clock.Add(time.Minute)
	return in.events[before:], err
}

func kinds(events []monitors.Event) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Kind)
	}
	return out
}

func titles(events []monitors.Event) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Title)
	}
	return out
}

func workers(f *fakeAPI) {
	worker := map[string]string{"role": "worker"}
	f.boxes = []box{
		newBox("sbx_a", "issue-1", t0.Add(-time.Hour), "working", worker),
		newBox("sbx_b", "issue-2", t0.Add(-time.Hour), "idle", worker),
		newBox("sbx_c", "someone-else", t0.Add(-time.Hour), "working", nil),
	}
}

func TestTheBaselineReportsNothingAndThenEachChange(t *testing.T) {
	f := newFake(t)
	workers(f)
	f.requests = []map[string]any{request("r0", "sbx_a", "pending", "github", t0.Add(-time.Minute))}
	in := newInstance(t, f, map[string]string{"tag": "role=worker"})
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("baseline reported %+v", got)
	}
	// Tags alone are the server's filter.
	if !slices.Contains(f.calls, "/projects/default/sandboxes?tag=role%3Dworker") {
		t.Fatalf("calls = %v", f.calls)
	}

	setState(f.box("sbx_a"), "stopped")
	setProgram(f.box("sbx_b"), "working", "")
	setProgram(f.box("sbx_c"), "idle", "")
	got := in.poll()
	if !slices.Equal(kinds(got), []string{"box.state", "box.busy"}) ||
		!slices.Equal(titles(got), []string{"issue-1 is stopped", "issue-2 is working"}) || got[0].Summary != "was running" {
		t.Fatalf("events = %+v", got)
	}
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("repeated %+v", got)
	}

	// The terminal's status, not its screen: blocked on a permission is
	// idle, with why.
	setProgram(f.box("sbx_b"), "blocked", "permission")
	got = in.poll()
	if !slices.Equal(kinds(got), []string{"box.idle"}) || got[0].Title != "issue-2 is waiting on a permission" ||
		got[0].Key != "box-sbx_b-terminal" || got[0].Body != "on it" {
		t.Fatalf("events = %+v", got)
	}
}

func TestASecretRequestIsReportedPendingAndAnswered(t *testing.T) {
	f := newFake(t)
	workers(f)
	f.requests = []map[string]any{request("r0", "sbx_a", "pending", "github", t0.Add(-time.Minute))}
	f.requests[0]["envName"] = "GH_TOKEN"
	in := newInstance(t, f, map[string]string{"tag": "role=worker", "watch": "requests"})
	in.poll()

	f.requests[0]["status"], f.requests[0]["decidedBy"] = "approved", "darren"
	npm := request("r1", "sbx_b", "pending", "npm", t0.Add(30*time.Second))
	npm["justification"] = "install deps\nmore"
	npm["routes"] = []map[string]any{
		{"hosts": []string{"registry.npmjs.org"}, "methods": []string{"GET"}, "paths": []string{"/**"}},
		{"hosts": []string{"registry.npmjs.org"}, "paths": []string{"/-/user/**"}, "blocked": true},
	}
	// Another box's, which this one did not create: the server lists it
	// for no one but its creator, and it is not watched here.
	f.requests = append(f.requests, npm, request("r2", "sbx_c", "pending", "other", t0.Add(30*time.Second)))
	got := in.poll()
	want := []string{"issue-1's request for github (GH_TOKEN) was approved by darren", "issue-2 asks for npm"}
	if !slices.Equal(titles(got), want) || got[0].Key != "request-r0" || got[1].Key != "request-r1" ||
		got[1].Summary != "install deps" || got[1].Body != "GET registry.npmjs.org/**\nregistry.npmjs.org/-/user/**=blocked" {
		t.Fatalf("events = %+v", got)
	}
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("repeated %+v", got)
	}
}

func TestARouteIsShownWithEverythingThatNarrowsIt(t *testing.T) {
	// A lead approves what the event says: a narrowing left out would show
	// a blocked GraphQL subscription as GraphQL refused.
	var routes []route
	if err := json.Unmarshal([]byte(`[
		{"hosts":["api.github.com"],"paths":["/graphql"]},
		{"hosts":["api.github.com"],"paths":["/graphql"],"blocked":true,"graphql":{"operations":["subscription"]}},
		{"hosts":["mcp.example.com"],"mcp":{"tools":["search","read_*"]}},
		{"hosts":["x.example.com"],"websocket":{"frames":"all"}}
	]`), &routes); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(routes))
	for _, r := range routes {
		got = append(got, r.String())
	}
	want := []string{
		"api.github.com/graphql",
		"api.github.com/graphql graphql=subscription=blocked",
		"mcp.example.com mcp=search,read_*",
		"x.example.com (narrowed by websocket, not shown)",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("routes = %q", got)
	}
}

func TestABoxCreatedSinceTheBaselineIsNews(t *testing.T) {
	f := newFake(t)
	workers(f)
	in := newInstance(t, f, map[string]string{"tag": "role=worker"})
	in.poll()

	f.boxes = append(f.boxes, newBox("sbx_d", "issue-3", t0.Add(30*time.Second), "working", map[string]string{"role": "worker"}))
	f.requests = []map[string]any{request("r1", "sbx_d", "pending", "github", t0.Add(40*time.Second))}
	got := in.poll()
	if !slices.Equal(titles(got), []string{"issue-3 was created: running", "issue-3 asks for github"}) {
		t.Fatalf("events = %+v", got)
	}
}

func TestAWorkerThatTagsItselfLateIsStillNews(t *testing.T) {
	// A worker is created, asks for a credential, and only then tags
	// itself: created after the baseline, it is news when it is first seen,
	// and so is its request.
	f := newFake(t)
	workers(f)
	in := newInstance(t, f, map[string]string{"tag": "role=worker"})
	in.poll()
	f.boxes = append(f.boxes, newBox("sbx_d", "issue-3", t0.Add(10*time.Second), "working", nil))
	f.requests = []map[string]any{request("r1", "sbx_d", "pending", "github", t0.Add(20*time.Second))}
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("an unselected box reported %+v", got)
	}
	f.box("sbx_d").Meta.Tags = map[string]string{"role": "worker"}
	if got := in.poll(); !slices.Equal(titles(got), []string{"issue-3 was created: running", "issue-3 asks for github"}) {
		t.Fatalf("events = %+v", got)
	}
}

func TestABoxSelectedLateThatWasThereAtTheBaselineIsQuiet(t *testing.T) {
	f := newFake(t)
	workers(f)
	f.requests = []map[string]any{request("r0", "sbx_c", "pending", "github", t0.Add(-time.Minute))}
	in := newInstance(t, f, map[string]string{"tag": "role=worker"})
	in.poll()
	f.box("sbx_c").Meta.Tags = map[string]string{"role": "worker"}
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("events = %+v", got)
	}
}

func TestABoxIsCalledWhatEveryClientCallsIt(t *testing.T) {
	// No name in its meta: its terminal's title, never the msg beside it;
	// with no title either, its configured name.
	f := newFake(t)
	b := newBox("sbx_a", "", t0.Add(-time.Hour), "", map[string]string{"role": "worker"})
	b.Config.Name = "optimistic_mcclintock"
	f.boxes = []box{b}
	in := newInstance(t, f, map[string]string{"tag": "role=worker"})
	in.poll()
	setState(f.box("sbx_a"), "stopped")
	if got := in.poll(); !slices.Equal(titles(got), []string{"optimistic_mcclintock is stopped"}) {
		t.Fatalf("events = %+v", got)
	}
	f.box("sbx_a").Runtime.AgentStatus.Sessions = json.RawMessage(`[{"primary":true,"startedAt":"2026-11-01T11:00:00Z","title":"Fix the build","programStatus":{"state":"working","msg":"on it"}}]`)
	setState(f.box("sbx_a"), "running")
	if got := in.poll(); !slices.Equal(titles(got), []string{"Fix the build is running", "Fix the build is working"}) {
		t.Fatalf("events = %+v", got)
	}
}

func TestABoxNamedSinceTheBaselineStartsAsItsOwnBaseline(t *testing.T) {
	// Options changed in place (enable --update): a box named later
	// reports what happens after it was named, not where it already stood.
	f := newFake(t)
	workers(f)
	f.requests = []map[string]any{request("r0", "sbx_c", "pending", "github", t0.Add(-time.Minute))}
	in := newInstance(t, f, map[string]string{"id": "sbx_a"})
	in.poll()

	in.env.Options = map[string]string{"id": "sbx_a,sbx_c"}
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("a newly named box reported its standing state: %+v", got)
	}
	setProgram(f.box("sbx_c"), "idle", "")
	f.requests[0]["status"] = "denied"
	got := in.poll()
	if !slices.Equal(kinds(got), []string{"box.idle", "box.request.answered"}) {
		t.Fatalf("events = %+v", got)
	}
	// Named by id, it need not match a tag: the list is the project's.
	if strings.Contains(f.calls[len(f.calls)-2], "tag=") {
		t.Fatalf("calls = %v", f.calls)
	}
}

func TestAnInstanceWatchingOnlyIdsEndsWhenEveryBoxIsGone(t *testing.T) {
	f := newFake(t)
	workers(f)
	in := newInstance(t, f, map[string]string{"id": "sbx_a, sbx_b"})
	in.poll()

	f.remove("sbx_a")
	got := in.poll()
	if !slices.Equal(titles(got), []string{"issue-1 is gone"}) || got[0].Final {
		t.Fatalf("events = %+v", got)
	}
	f.remove("sbx_b")
	got = in.poll()
	if !slices.Equal(kinds(got), []string{"box.gone", "discobox.finished"}) || !got[1].Final {
		t.Fatalf("events = %+v", got)
	}
}

func TestANamedBoxGoneIsKeptGoneWhenStateIsNotWatched(t *testing.T) {
	f := newFake(t)
	workers(f)
	in := newInstance(t, f, map[string]string{"id": "sbx_a", "watch": "terminal,requests"})
	in.poll()
	f.remove("sbx_a")
	if got := in.poll(); !slices.Equal(kinds(got), []string{"discobox.finished"}) {
		t.Fatalf("events = %+v", got)
	}
}

func TestABoxThatLosesItsTagIsLetGoQuietly(t *testing.T) {
	f := newFake(t)
	workers(f)
	in := newInstance(t, f, map[string]string{"tag": "role=worker"})
	in.poll()
	f.box("sbx_a").Meta.Tags = nil
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("events = %+v", got)
	}
	var st state
	if _, err := in.env.ReadState(stateFile, &st); err != nil || st.Boxes["sbx_a"] != nil {
		t.Fatalf("state = %+v, %v", st, err)
	}
}

func TestANamedBoxThatDoesNotExistSaysSo(t *testing.T) {
	f := newFake(t)
	workers(f)
	in := newInstance(t, f, map[string]string{"id": "sbx_nope"})
	if _, err := in.try(); err == nil || !strings.Contains(err.Error(), "no box sbx_nope") {
		t.Fatalf("err = %v", err)
	}
}

func TestWithoutTheDiscoboxAPITheFailureSaysWhy(t *testing.T) {
	f := newFake(t)
	in := newInstance(t, f, map[string]string{"tag": "role=worker"})
	in.vars = map[string]string{}
	if _, err := in.try(); err == nil || !strings.Contains(err.Error(), "no discobox API in this box") {
		t.Fatalf("unset: err = %v", err)
	}
	// What the server says to a box without discoboxApi.
	in.vars[envAPIURL] = f.srv.URL
	f.refuse, f.refuseText = http.StatusForbidden, "this discobox may not call the discobox API; a person allows it by setting discoboxApi on it\n"
	_, err := in.try()
	if err == nil || err.Error() != "the discobox API refused listing boxes (403): this discobox may not call the discobox API; a person allows it by setting discoboxApi on it" {
		t.Fatalf("refused: err = %v", err)
	}
}

func TestOptions(t *testing.T) {
	for _, tc := range []struct {
		opts map[string]string
		want string
	}{
		{map[string]string{}, "name the boxes to watch"},
		{map[string]string{"tag": "role=worker", "watch": "logs"}, `"logs" is not state`},
		{map[string]string{"tag": "=worker"}, "option tag"},
	} {
		if _, err := parseConfig(tc.opts); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.opts, err, tc.want)
		}
	}
	cfg, err := parseConfig(map[string]string{"id": "sbx_a,sbx_b sbx_a", "tag": "issue role=worker", "watch": "state,terminal"})
	if err != nil || !slices.Equal(cfg.IDs, []string{"sbx_a", "sbx_b"}) || !slices.Equal(cfg.Tags, []string{"issue", "role=worker"}) ||
		!cfg.Watch[watchState] || !cfg.Watch[watchTerminal] || cfg.Watch[watchRequests] {
		t.Fatalf("cfg = %+v, %v", cfg, err)
	}
}

func TestTheImageDeclaresThisMonitor(t *testing.T) {
	path, err := filepath.Abs("../../features/cli/monitors/50-discobox-box.sh")
	if err != nil {
		t.Fatal(err)
	}
	def, err := monitors.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if def.ID != Name || def.Problem != "" || def.Interval != 30*time.Second || def.MinInterval != 10*time.Second {
		t.Fatalf("declaration = %+v", def)
	}
	known := []string{"id", "tag", "watch", "interval"}
	for _, opt := range def.Options {
		if !slices.Contains(known, opt.Name) {
			t.Errorf("declares %q, which the monitor does not read", opt.Name)
		}
	}
	if len(def.Options) != len(known) {
		t.Errorf("declares %d options, the monitor reads %d", len(def.Options), len(known))
	}
}
