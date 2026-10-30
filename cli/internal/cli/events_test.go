package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testSandboxEvent = `{"sandboxId":"sbx_1","projectId":"proj_1","name":"alpha","desiredState":"present","state":"ready","runtimeState":"running","agent":{"state":"blocked","kind":"permission"},"at":"2026-10-30T12:00:00Z"}`

// eventServer answers each connection with the next script, and records the
// Last-Event-ID and query each one carried.
type eventServer struct {
	mu       sync.Mutex
	scripts  []string
	lastIDs  []string
	queries  []string
	finished chan struct{}
}

func (s *eventServer) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	n := len(s.lastIDs)
	s.lastIDs = append(s.lastIDs, r.Header.Get("Last-Event-ID"))
	s.queries = append(s.queries, r.URL.RawQuery)
	s.mu.Unlock()
	if n >= len(s.scripts) {
		close(s.finished)
		<-r.Context().Done()
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, s.scripts[n])
}

// A dropped stream is resumed from the last event ID it read, the one the
// server's opening id-only frame set included, and printed one line an event.
func TestStreamEventsResumesFromTheLastEventID(t *testing.T) {
	events := &eventServer{
		finished: make(chan struct{}),
		scripts: []string{
			"retry: 10\nid: 40\n\n" +
				": keepalive\n\n" +
				"id: 41\nevent: sandbox.updated\ndata: " + testSandboxEvent + "\n\n" +
				"id: 42\nevent: pool.updated\ndata: {}\n\n",
			// Dropped before any event: its own opening frame is where the
			// next resumes.
			"retry: 10\nid: 42\n\n",
			"retry: 10\nid: 42\n\nevent: reset\ndata: {\"reason\":\"expired\"}\n\n",
		},
	}
	server := httptest.NewServer(ignoringPortProbe(events.handler))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-events.finished:
		case <-time.After(10 * time.Second):
		}
		cancel()
	}()
	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
	var stdout, stderr bytes.Buffer
	if err := app.streamEvents(ctx, "proj_1", 5, &stdout, &stderr); err != nil {
		t.Fatalf("stream events: %v", err)
	}

	events.mu.Lock()
	defer events.mu.Unlock()
	if want := []string{"", "42", "42", "42"}; strings.Join(events.lastIDs, ",") != strings.Join(want, ",") {
		t.Fatalf("Last-Event-IDs = %q, want %q", events.lastIDs, want)
	}
	if events.queries[0] != "history=5" {
		t.Fatalf("query = %q, want history=5", events.queries[0])
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout = %q, want two events", stdout.String())
	}
	for _, want := range []string{"sandbox.updated", "alpha", "state=ready", "power=running", "agent=blocked(permission)"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("line %q is missing %q", lines[0], want)
		}
	}
	if !strings.HasSuffix(lines[1], "  pool.updated") {
		t.Fatalf("an unknown kind = %q, want it printed by type", lines[1])
	}
	if !strings.Contains(stderr.String(), "no longer kept; showing the last 5") {
		t.Fatalf("stderr = %q, want the reset explained", stderr.String())
	}
}

// With -o json each event is printed as the server sent it, a line each.
func TestStreamEventsPrintsJSONLines(t *testing.T) {
	events := &eventServer{
		finished: make(chan struct{}),
		scripts:  []string{"retry: 10\nid: 40\n\nid: 41\nevent: sandbox.created\ndata: " + testSandboxEvent + "\n\n"},
	}
	server := httptest.NewServer(ignoringPortProbe(events.handler))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-events.finished; cancel() }()

	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse, output: "json"}
	var stdout bytes.Buffer
	if err := app.streamEvents(ctx, "proj_1", 0, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatalf("stream events: %v", err)
	}
	if want := `{"data":` + testSandboxEvent + `,"event":"sandbox.created","id":"41"}` + "\n"; stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

// A stream the server refuses is an error at once, with the server's reason,
// not a reconnect loop.
func TestStreamEventsFailsOnARefusal(t *testing.T) {
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"status":404,"title":"Not Found","detail":"project not found"}`))
	}))
	defer server.Close()

	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
	err := app.streamEvents(context.Background(), "nope", 0, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "project not found") {
		t.Fatalf("stream events = %v, want the server's refusal", err)
	}
}

// Fields follow the HTML standard: multi-line data joins with newlines, a
// leading space is dropped once, comments and unknown fields are ignored.
func TestReadServerSentEventsParsesTheStandard(t *testing.T) {
	body := "retry: 1500\n:comment\nid: 7\nevent: x\ndata: one\ndata:  two\nunknown: y\n\n" +
		"data: no type\r\n\r\n"
	state := &eventStreamState{}
	var got []serverSentEvent
	_ = readServerSentEvents(strings.NewReader(body), state, func(event serverSentEvent) error {
		got = append(got, event)
		return nil
	})
	if len(got) != 2 {
		t.Fatalf("events = %+v", got)
	}
	if got[0] != (serverSentEvent{id: "7", event: "x", data: "one\n two"}) {
		t.Fatalf("first = %+v", got[0])
	}
	if got[1] != (serverSentEvent{id: "7", event: "", data: "no type"}) {
		t.Fatalf("second = %+v (the id carries over)", got[1])
	}
	if state.retry != 1500*time.Millisecond || state.lastID != "7" {
		t.Fatalf("state = %+v", state)
	}
}

// A name is whatever a discobox's creator sent, and a box may create one: an
// escape sequence in it is shown, never obeyed, as a line or as JSON
// (cli/REVIEW.md).
func TestEventsEscapeWhatADiscoboxNamed(t *testing.T) {
	data := `{"sandboxId":"sbx_1","name":"evil\u001b]52;c;aGk=\u0007\u009b2J\u202e","desiredState":"present","state":"ready","agent":null,"at":"2026-10-30T12:00:00Z"}`
	for _, asJSON := range []bool{false, true} {
		var out bytes.Buffer
		printer := &eventPrinter{json: asJSON, out: &out, errOut: &bytes.Buffer{}}
		if err := printer.print(serverSentEvent{id: "1", event: "sandbox.created", data: data}); err != nil {
			t.Fatal(err)
		}
		for _, raw := range []string{"\x1b", "\a", "\u009b", "\u202e"} {
			if strings.Contains(out.String(), raw) {
				t.Fatalf("json=%v: output %q carries %q raw", asJSON, out.String(), raw)
			}
		}
	}
}
