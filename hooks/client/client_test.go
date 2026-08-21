package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/hooks/api/model"
	"github.com/discobox-ai/discobox/internal/shorttmp"
)

func writeTestJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func TestUnixTransportAndRequestShapes(t *testing.T) {
	sock := filepath.Join(shorttmp.Dir(t), "daemon.sock")
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	seen := make(chan string, 4)
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Method + " " + r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/ping":
			writeTestJSON(t, w, PingResponse{OK: true, SessionID: "s1", Version: 123})
		case "/events":
			if r.URL.Query().Get("hook_id") != "hook one" || r.URL.Query().Get("limit") != "5" {
				t.Fatalf("unexpected events query: %s", r.URL.RawQuery)
			}
			writeTestJSON(t, w, EventsResponse{Events: []Event{{ID: "01test", Type: "hook.run.finished", HookID: "hook one"}}})
		case "/runs":
			if r.URL.Query().Get("hook_id") != "hook one" || !isExpectedLimit(r, "5", "0") {
				t.Fatalf("unexpected runs query: %s", r.URL.RawQuery)
			}
			writeTestJSON(t, w, RunsResponse{Runs: []Run{{ID: "run-1", HookID: "hook one", Status: "success"}}})
		case "/changes":
			if !isExpectedLimit(r, "5", "0") {
				t.Fatalf("unexpected changes query: %s", r.URL.RawQuery)
			}
			writeTestJSON(t, w, ChangesResponse{Changes: []ObservedFileChange{{ID: "change-1", Path: "main.go", Kind: "modified"}}})
		case "/snapshots":
			if !isExpectedLimit(r, "5", "0") {
				t.Fatalf("unexpected snapshots query: %s", r.URL.RawQuery)
			}
			writeTestJSON(t, w, SnapshotsResponse{Snapshots: []WorkspaceSnapshot{{ID: "snapshot-1", PatchBytes: 42}}})
		case "/snapshots/snapshot-1":
			writeTestJSON(t, w, WorkspaceSnapshot{ID: "snapshot-1", Patch: "diff", PatchBytes: 4})
		case "/queue":
			if !isExpectedLimit(r, "5", "0") {
				t.Fatalf("unexpected queue query: %s", r.URL.RawQuery)
			}
			writeTestJSON(t, w, QueueResponse{Queue: []QueuedHook{{HookID: "hook one", Position: 1}}})
		case "/events/stream":
			if r.URL.Query().Get("hook_id") != "hook one" || r.URL.Query().Get("limit") != "5" || r.Header.Get("Last-Event-ID") != "01test" {
				t.Fatalf("unexpected stream request query=%s last=%q", r.URL.RawQuery, r.Header.Get("Last-Event-ID"))
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "id: 02test\nevent: hook.run.finished\ndata: {\"id\":\"02test\",\"type\":\"hook.run.finished\",\"hook_id\":\"hook one\"}\n\n")
		case "/execution":
			var body model.ExecutionPatchRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !body.Paused {
				t.Fatalf("unexpected execution body: %#v err=%v", body, err)
			}
			writeTestJSON(t, w, model.ExecutionResponse{Paused: true})
		case "/hooks/hook%20one/run":
			var body model.RunRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !body.Force {
				t.Fatalf("unexpected run body: %#v err=%v", body, err)
			}
			writeTestJSON(t, w, RunResponse{HookID: "hook one", Enqueued: true})
		case "/hooks/hook%20one/output":
			writeTestJSON(t, w, struct {
				HookID string `json:"hook_id"`
				Output string `json:"output"`
			}{HookID: "hook one"})
		default:
			http.NotFound(w, r)
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Shutdown(context.Background())

	c := New(sock)
	info, err := c.PingInfo(context.Background())
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	if !info.OK || info.SessionID != "s1" || info.Version != 123 {
		t.Fatalf("unexpected ping info: %#v", info)
	}
	if got := <-seen; got != "GET /ping" {
		t.Fatalf("ping request = %q", got)
	}
	events, err := c.ListEvents(context.Background(), EventOptions{HookID: "hook one", Limit: 5})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(events) != 1 || events[0].Type != "hook.run.finished" || events[0].HookID != "hook one" {
		t.Fatalf("unexpected events: %#v", events)
	}
	if got := <-seen; got != "GET /events" {
		t.Fatalf("events request = %q", got)
	}
	runs, err := c.ListRuns(context.Background(), RunListOptions{HookID: "hook one", Limit: 5})
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != "run-1" || runs[0].HookID != "hook one" {
		t.Fatalf("unexpected runs: %#v", runs)
	}
	if got := <-seen; got != "GET /runs" {
		t.Fatalf("runs request = %q", got)
	}
	changes, err := c.ListObservedChanges(context.Background(), ListOptions{Limit: 5})
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "main.go" {
		t.Fatalf("unexpected changes: %#v", changes)
	}
	if got := <-seen; got != "GET /changes" {
		t.Fatalf("changes request = %q", got)
	}
	snapshots, err := c.ListWorkspaceSnapshots(context.Background(), ListOptions{Limit: 5})
	if err != nil {
		t.Fatalf("snapshots: %v", err)
	}
	if len(snapshots) != 1 || snapshots[0].PatchBytes != 42 {
		t.Fatalf("unexpected snapshots: %#v", snapshots)
	}
	if got := <-seen; got != "GET /snapshots" {
		t.Fatalf("snapshots request = %q", got)
	}
	snapshot, err := c.GetWorkspaceSnapshot(context.Background(), "snapshot-1")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.ID != "snapshot-1" || snapshot.Patch != "diff" {
		t.Fatalf("unexpected snapshot: %#v", snapshot)
	}
	if got := <-seen; got != "GET /snapshots/snapshot-1" {
		t.Fatalf("snapshot request = %q", got)
	}
	queue, err := c.ListQueue(context.Background(), ListOptions{Limit: 5})
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	if len(queue) != 1 || queue[0].HookID != "hook one" {
		t.Fatalf("unexpected queue: %#v", queue)
	}
	if got := <-seen; got != "GET /queue" {
		t.Fatalf("queue request = %q", got)
	}
	if _, err := c.ListRuns(context.Background(), RunListOptions{HookID: "hook one", Limit: 0, LimitSet: true}); err != nil {
		t.Fatalf("runs limit zero: %v", err)
	}
	if got := <-seen; got != "GET /runs" {
		t.Fatalf("runs limit zero request = %q", got)
	}
	if _, err := c.ListObservedChanges(context.Background(), ListOptions{Limit: 0, LimitSet: true}); err != nil {
		t.Fatalf("changes limit zero: %v", err)
	}
	if got := <-seen; got != "GET /changes" {
		t.Fatalf("changes limit zero request = %q", got)
	}
	if _, err := c.ListWorkspaceSnapshots(context.Background(), ListOptions{Limit: 0, LimitSet: true}); err != nil {
		t.Fatalf("snapshots limit zero: %v", err)
	}
	if got := <-seen; got != "GET /snapshots" {
		t.Fatalf("snapshots limit zero request = %q", got)
	}
	if _, err := c.ListQueue(context.Background(), ListOptions{Limit: 0, LimitSet: true}); err != nil {
		t.Fatalf("queue limit zero: %v", err)
	}
	if got := <-seen; got != "GET /queue" {
		t.Fatalf("queue limit zero request = %q", got)
	}
	var streamed []Event
	// FollowEvents reconnects when the stream ends, so stop it from the callback.
	streamCtx, stopStream := context.WithCancel(context.Background())
	defer stopStream()
	if err := c.FollowEvents(streamCtx, EventOptions{HookID: "hook one", Limit: 5, LastEventID: "01test"}, func(event Event) error {
		streamed = append(streamed, event)
		stopStream()
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("follow events: %v", err)
	}
	if len(streamed) != 1 || streamed[0].ID != "02test" || streamed[0].HookID != "hook one" {
		t.Fatalf("unexpected streamed events: %#v", streamed)
	}
	if got := <-seen; got != "GET /events/stream" {
		t.Fatalf("stream request = %q", got)
	}
	if err := c.PauseAll(context.Background()); err != nil {
		t.Fatalf("pause all: %v", err)
	}
	if got := <-seen; got != "PATCH /execution" {
		t.Fatalf("pause request = %q", got)
	}
	if resp, err := c.RunHook(context.Background(), "hook one", RunOptions{Force: true}); err != nil || !resp.Enqueued {
		t.Fatalf("run: resp=%#v err=%v", resp, err)
	}
	if got := <-seen; got != "POST /hooks/hook%20one/run" {
		t.Fatalf("run request = %q", got)
	}
	out, err := c.Output(context.Background(), "hook one")
	if err != nil {
		t.Fatalf("output: %v", err)
	}
	if string(out) != "" {
		t.Fatalf("output = %q", out)
	}
}

// TestFollowEventsReconnects covers a daemon restart: the stream drops after the
// shutdown event and the client reconnects, resuming after the last event seen.
func TestFollowEventsReconnects(t *testing.T) {
	sock := filepath.Join(shorttmp.Dir(t), "daemon.sock")
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	lastEventIDs := make(chan string, 4)
	attempts := 0
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/events/stream" {
			http.NotFound(w, r)
			return
		}
		lastEventIDs <- r.Header.Get("Last-Event-ID")
		attempts++
		w.Header().Set("Content-Type", "text/event-stream")
		if attempts == 1 {
			fmt.Fprint(w, "data: {\"id\":\"01test\",\"type\":\"daemon.shutdown.requested\"}\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"id\":\"02test\",\"type\":\"hook.run.finished\"}\n\n")
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Shutdown(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	disconnects := 0
	var streamed []string
	err = New(sock).FollowEvents(ctx, EventOptions{
		OnDisconnect: func(_ error, _ int) { disconnects++ },
	}, func(event Event) error {
		streamed = append(streamed, event.ID)
		if event.ID == "02test" {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("follow events after reconnect: %v", err)
	}
	if len(streamed) != 2 || streamed[0] != "01test" || streamed[1] != "02test" {
		t.Fatalf("streamed events = %v", streamed)
	}
	if disconnects != 1 {
		t.Fatalf("disconnect notices = %d, want 1", disconnects)
	}
	if got := <-lastEventIDs; got != "" {
		t.Fatalf("first Last-Event-ID = %q, want empty", got)
	}
	if got := <-lastEventIDs; got != "01test" {
		t.Fatalf("reconnect Last-Event-ID = %q, want 01test", got)
	}
}

// TestFollowEventsCallbackErrorIsTerminal ensures a callback failure ends the
// follow instead of being retried as a stream failure.
func TestFollowEventsCallbackErrorIsTerminal(t *testing.T) {
	sock := filepath.Join(shorttmp.Dir(t), "daemon.sock")
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	requests := 0
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"01test\",\"type\":\"hook.run.finished\"}\n\n")
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Shutdown(context.Background())

	boom := errors.New("boom")
	err = New(sock).FollowEvents(context.Background(), EventOptions{}, func(Event) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("follow events error = %v, want boom", err)
	}
	if requests != 1 {
		t.Fatalf("stream requests = %d, want 1", requests)
	}
}

func isExpectedLimit(r *http.Request, expected ...string) bool {
	values, ok := r.URL.Query()["limit"]
	if !ok || len(values) != 1 {
		return false
	}
	for _, value := range expected {
		if values[0] == value {
			return true
		}
	}
	return false
}

func TestReadSSEEventsHandlesLargeDataLine(t *testing.T) {
	largeMessage := strings.Repeat("x", 128*1024)
	stream := `data: {"id":"large","type":"file.change.observed","message":"` + largeMessage + `"}` + "\n\n"

	var events []Event
	if err := readSSEEvents(strings.NewReader(stream), func(event Event) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatalf("read SSE events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].ID != "large" || events[0].Type != "file.change.observed" || events[0].Message != largeMessage {
		t.Fatalf("unexpected event: id=%q type=%q message len=%d", events[0].ID, events[0].Type, len(events[0].Message))
	}
}

func TestMissingSocketIsNotRunning(t *testing.T) {
	c := New(filepath.Join(shorttmp.Dir(t), "missing.sock"))
	err := c.Ping(context.Background())
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("expected ErrNotRunning, got %v", err)
	}
}

func TestShutdownSuccessIgnoresRemovedSocket(t *testing.T) {
	sock := filepath.Join(shorttmp.Dir(t), "daemon.sock")
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.EscapedPath() != "/shutdown" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = os.Remove(sock)
		writeTestJSON(t, w, model.ShutdownResponse{OK: true})
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Shutdown(context.Background())

	c := New(sock)
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown should ignore removed socket after successful response: %v", err)
	}
}

func TestResponseErrorUsesJSONMessage(t *testing.T) {
	err := responseError(http.StatusBadRequest, []byte(`{"message":"bad hook"}`))
	if err == nil || err.Error() != "daemon returned 400: bad hook" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
