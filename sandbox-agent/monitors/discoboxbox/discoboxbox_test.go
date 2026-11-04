package discoboxbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/monitors"
	"github.com/discobox-ai/discobox/sandbox-agent/monitors/builtin"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// hold is how long a test's run follows the stream.
const hold = 150 * time.Millisecond

// fakeAPI is the discobox API as a box with discoboxApi sees it: the
// project's sandbox event stream (ADR 26-10-29-316 §4), and the secret
// requests of the boxes it created.
type fakeAPI struct {
	mu sync.Mutex
	// boxes are each live box's data, as the server writes it.
	boxes map[string]map[string]any
	// events is the log, oldest first; events[i] has ID i+1.
	events []fakeEvent
	// pruned is the highest ID retention has removed: a cursor before it is
	// reset.
	pruned int
	// now is when a change made now is recorded.
	now time.Time
	// requests are served as the API writes them, not as the monitor's
	// struct would encode: a field read as the wrong type fails here too.
	requests []map[string]any
	// refuse, when set, is the status and text every call is answered with.
	refuse     int
	refuseText string
	// cut ends each stream once it has sent what there is, rather than
	// holding it open until the client leaves.
	cut bool
	// cutReplay ends a replay before the ID-only event that ends it, as a
	// connection dropped part way through one would.
	cutReplay bool
	// raw, when set, is sent as one event's data in place of the log's.
	raw     string
	calls   []string
	cursors []string
	srv     *httptest.Server
}

type fakeEvent struct {
	typ  string
	data map[string]any
}

func newFake(t *testing.T) *fakeAPI {
	f := &fakeAPI{boxes: map[string]map[string]any{}, now: t0.Add(-time.Hour)}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.URL.RequestURI())
	if f.refuse != 0 {
		f.mu.Unlock()
		http.Error(w, f.refuseText, f.refuse)
		return
	}
	switch r.URL.Path {
	case "/api/projects/default/events":
		cursor := r.Header.Get("Last-Event-ID")
		f.cursors = append(f.cursors, cursor)
		frames := f.framesAfter(cursor)
		cut := f.cut || f.cutReplay
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "retry: 2000\n\n"+frames)
		w.(http.Flusher).Flush()
		if !cut {
			<-r.Context().Done()
		}
	case "/projects/default/secret-requests":
		defer f.mu.Unlock()
		if err := json.NewEncoder(w).Encode(map[string]any{"secretRequests": f.requests}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	default:
		f.mu.Unlock()
		http.NotFound(w, r)
	}
}

// framesAfter is what the stream sends a client at cursor: every event after
// it, or with no cursor the latest event of each live box and then where the
// stream stands; a cursor retention has passed is a reset and then the same.
func (f *fakeAPI) framesAfter(cursor string) string {
	var b strings.Builder
	write := func(id int) {
		ev := f.events[id-1]
		data, err := json.Marshal(ev.data)
		if err != nil {
			panic(err)
		}
		if f.raw != "" {
			data = []byte(f.raw)
		}
		fmt.Fprintf(&b, "id: %d\nevent: %s\ndata: %s\n\n", id, ev.typ, data)
	}
	from, _ := strconv.Atoi(cursor)
	if cursor != "" && from >= f.pruned {
		for id := from + 1; id <= len(f.events); id++ {
			write(id)
		}
		return b.String()
	}
	if cursor != "" {
		b.WriteString("event: reset\ndata: {}\n\n")
	}
	latest := map[string]int{}
	for i, ev := range f.events {
		latest[ev.data["sandboxId"].(string)] = i + 1
	}
	var ids []int
	for sandboxID, id := range latest {
		if _, live := f.boxes[sandboxID]; live {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		write(id)
	}
	// A replay always ends with where the stream stands, even where that is
	// its last event's ID.
	if !f.cutReplay {
		fmt.Fprintf(&b, "id: %d\n\n", len(f.events))
	}
	return b.String()
}

// record appends an event with the box's state now.
func (f *fakeAPI) record(typ, id string) {
	data := map[string]any{}
	for k, v := range f.boxes[id] {
		data[k] = v
	}
	data["at"] = f.now
	f.events = append(f.events, fakeEvent{typ: typ, data: data})
}

// create adds a running box, its primary terminal's program reporting
// program ("" for none).
func (f *fakeAPI) create(id, name, program string, tags map[string]string, createdBy string) {
	box := map[string]any{
		"projectId": "proj_1", "sandboxId": id, "name": name,
		"desiredState": "present", "state": "ready", "runtimeState": "running", "displayState": "running",
	}
	if program != "" {
		box["program"] = map[string]any{"state": program, "app": "claude-code"}
	}
	if len(tags) > 0 {
		box["tags"] = tags
	}
	if createdBy != "" {
		box["parentSandboxId"] = createdBy
	}
	f.boxes[id] = box
	f.record("sandbox.created", id)
}

func (f *fakeAPI) update(id string, change func(box map[string]any)) {
	change(f.boxes[id])
	f.record("sandbox.updated", id)
}

func (f *fakeAPI) power(id, state string) {
	f.update(id, func(box map[string]any) {
		box["runtimeState"], box["displayState"] = state, state
		if state != "running" {
			delete(box, "program")
		}
	})
}

func (f *fakeAPI) program(id, state, kind string) {
	f.update(id, func(box map[string]any) {
		program := map[string]any{"state": state, "app": "claude-code"}
		if kind != "" {
			program["kind"] = kind
		}
		box["program"] = program
	})
}

func (f *fakeAPI) tag(id string, tags map[string]string) {
	f.update(id, func(box map[string]any) {
		if len(tags) == 0 {
			delete(box, "tags")
			return
		}
		box["tags"] = tags
	})
}

func (f *fakeAPI) remove(id string) {
	f.record("sandbox.deleted", id)
	delete(f.boxes, id)
}

// request is a SecretRequest as the API serves it.
func request(id, sandbox, status, name string, created time.Time) map[string]any {
	return map[string]any{
		"id": id, "sandboxId": sandbox, "status": status, "name": name, "purpose": "use",
		"projectId": "default", "createdAt": created, "updatedAt": created,
		"routes": []map[string]any{{"hosts": []string{"api.github.com"}}},
	}
}

type instance struct {
	t      *testing.T
	f      *fakeAPI
	env    builtin.Env
	vars   map[string]string
	self   string
	events []monitors.Event
}

func newInstance(t *testing.T, f *fakeAPI, opts map[string]string) *instance {
	in := &instance{t: t, f: f, vars: map[string]string{envAPIURL: f.srv.URL}, self: "sbx_lead"}
	in.env = builtin.Env{
		Instance: "test",
		State:    t.TempDir(),
		Options:  opts,
		Interval: hold,
		Getenv:   func(name string) string { return in.vars[name] },
		Emit: func(ev monitors.Event) error {
			in.events = append(in.events, ev)
			return nil
		},
		Stderr: io.Discard,
	}
	return in
}

// poll is one run at t0, or a minute after the change that preceded it,
// returning what it printed.
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
	in.f.mu.Lock()
	clock := in.f.now.Add(time.Minute)
	if clock.Before(t0) {
		clock = t0
	}
	in.f.mu.Unlock()
	err := run(context.Background(), in.env, deps{
		http: in.f.srv.Client(),
		now:  func() time.Time { return clock },
		self: func() (string, error) {
			if in.self == "" {
				return "", errors.New("no sandbox config")
			}
			return in.self, nil
		},
	})
	return in.events[before:], err
}

// at sets when the fake's next changes are recorded.
func (f *fakeAPI) at(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t0.Add(d)
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

var worker = map[string]string{"role": "worker"}

func workers(f *fakeAPI) {
	f.create("sbx_a", "issue-1", "working", worker, "sbx_lead")
	f.create("sbx_b", "issue-2", "idle", worker, "sbx_lead")
	f.create("sbx_c", "someone-else", "working", nil, "")
}

func TestTheBaselineReportsNothingAndThenEachChange(t *testing.T) {
	f := newFake(t)
	workers(f)
	in := newInstance(t, f, map[string]string{"tag": "role=worker"})
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("baseline reported %+v", got)
	}

	f.at(30 * time.Second)
	f.power("sbx_a", "stopped")
	f.program("sbx_b", "working", "")
	f.program("sbx_c", "idle", "")
	got := in.poll()
	if !slices.Equal(kinds(got), []string{"box.state", "box.busy"}) ||
		!slices.Equal(titles(got), []string{"issue-1 is stopped", "issue-2 is working"}) || got[0].Summary != "was running" {
		t.Fatalf("events = %+v", got)
	}
	// Each run goes on from where the last left off.
	if !slices.Equal(f.cursors, []string{"", "3"}) {
		t.Fatalf("cursors = %q, want none and then where the replay stood", f.cursors)
	}
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("repeated %+v", got)
	}

	// The terminal's status, not its screen: blocked on a permission is
	// idle, with why.
	f.at(2 * time.Minute)
	f.program("sbx_b", "blocked", "permission")
	got = in.poll()
	if !slices.Equal(kinds(got), []string{"box.idle"}) || got[0].Title != "issue-2 is waiting on a permission" ||
		got[0].Key != "box-sbx_b-terminal" {
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

	f.at(30 * time.Second)
	f.create("sbx_d", "issue-3", "working", worker, "sbx_lead")
	f.requests = []map[string]any{request("r1", "sbx_d", "pending", "github", t0.Add(40*time.Second))}
	got := in.poll()
	if !slices.Equal(titles(got), []string{"issue-3 was created: running", "issue-3 asks for github"}) {
		t.Fatalf("events = %+v", got)
	}
}

func TestAWorkerThatTagsItselfLateIsStillNews(t *testing.T) {
	// A worker is created, asks for a credential, and only then tags
	// itself: created after the baseline, it is news when it is first
	// watched, and so is its request.
	f := newFake(t)
	workers(f)
	in := newInstance(t, f, map[string]string{"tag": "role=worker"})
	in.poll()
	f.at(10 * time.Second)
	f.create("sbx_d", "issue-3", "working", nil, "sbx_lead")
	f.requests = []map[string]any{request("r1", "sbx_d", "pending", "github", t0.Add(20*time.Second))}
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("an unselected box reported %+v", got)
	}
	f.at(2 * time.Minute)
	f.tag("sbx_d", worker)
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
	f.at(30 * time.Second)
	f.tag("sbx_c", worker)
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("events = %+v", got)
	}
	// Watched now, its changes are news.
	f.at(2 * time.Minute)
	f.power("sbx_c", "stopped")
	if got := in.poll(); !slices.Equal(titles(got), []string{"someone-else is stopped"}) {
		t.Fatalf("events = %+v", got)
	}
}

func TestCreatedBySelectsTheBoxesABoxCreated(t *testing.T) {
	f := newFake(t)
	workers(f)
	in := newInstance(t, f, map[string]string{"created-by": "me", "watch": "terminal"})
	in.poll()
	f.at(30 * time.Second)
	f.program("sbx_a", "idle", "")
	f.program("sbx_c", "idle", "")
	if got := in.poll(); !slices.Equal(titles(got), []string{"issue-1 is idle"}) {
		t.Fatalf("events = %+v", got)
	}
	// With a tag too, both must match.
	in.env.Options = map[string]string{"created-by": "sbx_lead", "tag": "role=lead"}
	f.at(2 * time.Minute)
	f.program("sbx_a", "working", "")
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("events = %+v", got)
	}
	// This box's own ID unknown, created-by=me cannot be resolved.
	in.env.Options, in.self = map[string]string{"created-by": "me"}, ""
	if _, err := in.try(); err == nil || !strings.Contains(err.Error(), "this box's own ID is unknown") {
		t.Fatalf("err = %v", err)
	}
}

func TestABoxIsCalledByItsConfiguredName(t *testing.T) {
	f := newFake(t)
	f.create("sbx_a", "optimistic_mcclintock", "", worker, "")
	f.create("sbx_b", "", "", worker, "")
	in := newInstance(t, f, map[string]string{"tag": "role=worker"})
	in.poll()
	f.at(30 * time.Second)
	f.power("sbx_a", "stopped")
	f.power("sbx_b", "stopped")
	if got := in.poll(); !slices.Equal(titles(got), []string{"optimistic_mcclintock is stopped", "sbx_b is stopped"}) {
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
	f.at(2 * time.Minute)
	f.program("sbx_c", "idle", "")
	f.requests[0]["status"] = "denied"
	got := in.poll()
	if !slices.Equal(kinds(got), []string{"box.idle", "box.request.answered"}) {
		t.Fatalf("events = %+v", got)
	}
}

func TestAnInstanceWatchingOnlyIdsEndsWhenEveryBoxIsGone(t *testing.T) {
	f := newFake(t)
	workers(f)
	in := newInstance(t, f, map[string]string{"id": "sbx_a, sbx_b"})
	in.poll()

	f.at(30 * time.Second)
	f.remove("sbx_a")
	got := in.poll()
	if !slices.Equal(titles(got), []string{"issue-1 is gone"}) || got[0].Final {
		t.Fatalf("events = %+v", got)
	}
	f.at(2 * time.Minute)
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
	f.at(30 * time.Second)
	f.remove("sbx_a")
	if got := in.poll(); !slices.Equal(kinds(got), []string{"discobox.finished"}) {
		t.Fatalf("events = %+v", got)
	}
}

func TestABoxSelectedByTagThatIsDeletedIsGone(t *testing.T) {
	// The stream says a box was deleted, so a box watched by its tag is
	// reported gone as a named one is; the instance goes on.
	f := newFake(t)
	workers(f)
	in := newInstance(t, f, map[string]string{"tag": "role=worker"})
	in.poll()
	f.at(30 * time.Second)
	f.remove("sbx_a")
	f.remove("sbx_c")
	got := in.poll()
	if !slices.Equal(titles(got), []string{"issue-1 is gone"}) || got[0].Final {
		t.Fatalf("events = %+v", got)
	}
}

func TestABoxThatLosesItsTagIsLetGoQuietly(t *testing.T) {
	f := newFake(t)
	workers(f)
	in := newInstance(t, f, map[string]string{"tag": "role=worker"})
	in.poll()
	f.at(30 * time.Second)
	f.tag("sbx_a", nil)
	f.power("sbx_a", "stopped")
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("events = %+v", got)
	}
	var st state
	if _, err := in.env.ReadState(stateFile, &st); err != nil || st.Boxes["sbx_a"] == nil || st.Boxes["sbx_a"].Watched {
		t.Fatalf("state = %+v, %v; want sbx_a held and not watched", st, err)
	}
}

func TestAResetReportsWhatChangedAndWhatWentWhileEventsWereMissed(t *testing.T) {
	f := newFake(t)
	workers(f)
	in := newInstance(t, f, map[string]string{"tag": "role=worker"})
	in.poll()

	// Events are missed: retention passes the cursor while sbx_a stops and
	// sbx_b is deleted.
	f.at(30 * time.Second)
	f.power("sbx_a", "stopped")
	f.remove("sbx_b")
	// A worker made in the gap, whose latest event is an update: it is new
	// all the same, since every box the project had is held.
	f.create("sbx_e", "issue-5", "working", worker, "sbx_lead")
	f.program("sbx_e", "idle", "")
	f.pruned = len(f.events)

	// A replay cut off before the ID-only event that ends it concludes
	// nothing, and keeps the cursor the reset found, so the next run is
	// reset again.
	f.cutReplay = true
	got := in.poll()
	if !slices.Equal(titles(got), []string{"issue-1 is stopped", "issue-5 was created: running"}) {
		t.Fatalf("cut short: events = %+v", got)
	}
	f.cutReplay = false
	got = in.poll()
	if !slices.Equal(titles(got), []string{"issue-2 is gone"}) {
		t.Fatalf("after the whole replay: events = %+v", got)
	}
	if n := len(f.cursors); f.cursors[n-1] != f.cursors[n-2] || f.cursors[n-1] != "3" {
		t.Fatalf("cursors = %q, want the pre-reset cursor twice", f.cursors)
	}
	// Then it goes on from where the replay stood.
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("repeated %+v", got)
	}
	if last := f.cursors[len(f.cursors)-1]; last != strconv.Itoa(len(f.events)) {
		t.Fatalf("cursor = %q, want the head %d", last, len(f.events))
	}
}

func TestRequestsAreStillListedWhileTheStreamFails(t *testing.T) {
	f := newFake(t)
	workers(f)
	in := newInstance(t, f, map[string]string{"tag": "role=worker"})
	in.poll()
	// The stream breaks for a reason of its own; a request waiting on
	// someone is reported all the same, and the run still fails.
	f.at(30 * time.Second)
	f.power("sbx_a", "stopped")
	f.raw = `{"sandboxId":"sbx_a","tags":["not","a","map"]}`
	f.requests = []map[string]any{request("r1", "sbx_b", "pending", "github", t0.Add(40*time.Second))}
	got, err := in.try()
	if err == nil || !strings.Contains(err.Error(), "event 4") {
		t.Fatalf("err = %v, want the stream's", err)
	}
	if !slices.Equal(titles(got), []string{"issue-2 asks for github"}) {
		t.Fatalf("events = %+v", got)
	}
}

func TestAFirstReplayCutShortSavesNoCursor(t *testing.T) {
	// The first run's replay is cut off before the event that ends it: the
	// next run starts from the current state again, rather than from part
	// way through, and holds every box.
	f := newFake(t)
	workers(f)
	f.cutReplay = true
	in := newInstance(t, f, map[string]string{"id": "sbx_a,sbx_c"})
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("cut short: events = %+v", got)
	}
	f.cutReplay = false
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("the second run reported %+v", got)
	}
	if !slices.Equal(f.cursors, []string{"", ""}) {
		t.Fatalf("cursors = %q, want none twice", f.cursors)
	}
	var st state
	if _, err := in.env.ReadState(stateFile, &st); err != nil || st.Cursor != "3" || len(st.Boxes) != 3 {
		t.Fatalf("state = %+v, %v; want every box held, at cursor 3", st, err)
	}
}

func TestABoxDeletedAfterACutFirstReplayIsGone(t *testing.T) {
	// The first replay is cut off before its end, holding the boxes it got;
	// one of them is deleted before the next run, whose replay from scratch
	// leaves it out. It is gone, and the instance watching only it ends;
	// the boxes the cut replay held and the next one shows are not new.
	f := newFake(t)
	workers(f)
	f.cutReplay = true
	in := newInstance(t, f, map[string]string{"id": "sbx_a"})
	in.poll()
	f.cutReplay = false
	f.at(30 * time.Second)
	f.remove("sbx_a")
	got := in.poll()
	if !slices.Equal(kinds(got), []string{"box.gone", "discobox.finished"}) {
		t.Fatalf("events = %+v", got)
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
	if err == nil || err.Error() != "the discobox API refused reading events (403): this discobox may not call the discobox API; a person allows it by setting discoboxApi on it" {
		t.Fatalf("refused: err = %v", err)
	}
}

func TestAnEventItCannotReadFailsTheRun(t *testing.T) {
	f := newFake(t)
	workers(f)
	f.raw = `{"sandboxId":"sbx_a","tags":["not","a","map"]}`
	in := newInstance(t, f, map[string]string{"tag": "role=worker"})
	if _, err := in.try(); err == nil || !strings.Contains(err.Error(), "event 1") {
		t.Fatalf("err = %v", err)
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
		{map[string]string{"created-by": "sbx_a,sbx_b"}, "option created-by"},
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
	if cfg, err := parseConfig(map[string]string{"created-by": " me "}); err != nil || cfg.CreatedBy != "me" || cfg.onlyIDs() {
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
	if def.ID != Name || def.Problem != "" || def.Interval != defaultHold || def.MinInterval != 10*time.Second {
		t.Fatalf("declaration = %+v", def)
	}
	known := []string{"id", "tag", "created-by", "watch", "interval"}
	for _, opt := range def.Options {
		if !slices.Contains(known, opt.Name) {
			t.Errorf("declares %q, which the monitor does not read", opt.Name)
		}
	}
	if len(def.Options) != len(known) {
		t.Errorf("declares %d options, the monitor reads %d", len(def.Options), len(known))
	}
}

func TestAdoptionAndReleaseReportSelectionChangesOnce(t *testing.T) {
	f := newFake(t)
	workers(f)
	in := newInstance(t, f, map[string]string{"created-by": "me", "watch": "terminal"})
	in.poll()
	f.at(time.Minute)
	f.boxes["sbx_c"]["parentSandboxId"] = "sbx_lead"
	f.record(eventUpdated, "sbx_c")
	got := in.poll()
	if !slices.Equal(kinds(got), []string{"box.adopted"}) {
		t.Fatalf("adoption: %+v", got)
	}
	f.at(2 * time.Minute)
	f.boxes["sbx_c"]["parentSandboxId"] = "another-lead"
	f.record(eventUpdated, "sbx_c")
	got = in.poll()
	if !slices.Equal(kinds(got), []string{"box.gone"}) {
		t.Fatalf("adopted away: %+v", got)
	}
	if got = in.poll(); len(got) != 0 {
		t.Fatalf("repeated gone: %+v", got)
	}
}

func TestAdoptionConsumesNewBoxBeforeTagToggle(t *testing.T) {
	f := newFake(t)
	workers(f)
	in := newInstance(t, f, map[string]string{"created-by": "me", "tag": "role=worker"})
	in.poll()
	f.at(time.Minute)
	f.create("sbx_new", "new child", "", map[string]string{"role": "worker"}, "other-lead")
	in.poll()
	f.at(2 * time.Minute)
	f.update("sbx_new", func(box map[string]any) { box["parentSandboxId"] = "sbx_lead" })
	if got := in.poll(); !slices.Equal(kinds(got), []string{"box.adopted"}) {
		t.Fatalf("adoption: %+v", got)
	}
	f.at(3 * time.Minute)
	f.update("sbx_new", func(box map[string]any) { delete(box, "tags") })
	in.poll()
	f.at(4 * time.Minute)
	f.update("sbx_new", func(box map[string]any) { box["tags"] = map[string]string{"role": "worker"} })
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("tag toggle reported adoption again: %+v", got)
	}
}
