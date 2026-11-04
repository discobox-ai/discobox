package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/go-chi/chi/v5"

	services "github.com/discobox-ai/discobox/server/internal/services"
)

// scriptedEventStream answers each Next with the next batch, and nothing once
// they are spent.
type scriptedEventStream struct {
	mu      sync.Mutex
	batches [][]services.SandboxStreamEvent
	// cursors is where the stream stands after each batch, then after every
	// read once they are spent.
	cursors []int64
	cursor  int64
}

func (s *scriptedEventStream) Next(context.Context) ([]services.SandboxStreamEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cursors) > 0 {
		s.cursor, s.cursors = s.cursors[0], s.cursors[1:]
	}
	if len(s.batches) == 0 {
		return nil, nil
	}
	batch := s.batches[0]
	s.batches = s.batches[1:]
	return batch, nil
}

func (s *scriptedEventStream) Cursor() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor
}

type eventStreamService struct {
	routerTestServices
	mu      sync.Mutex
	project string
	cursor  *int64
	stream  services.SandboxEventStream
}

func (s *eventStreamService) OpenSandboxEvents(_ context.Context, projectID string, lastEventID *int64) (services.SandboxEventStream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.project, s.cursor = projectID, lastEventID
	return s.stream, nil
}

func eventID(id int64) *int64 { return &id }

// streamRecorder is a response that holds what the handler writes until it
// flushes, then hands it to a pipe, so the test reads only what a client would
// have been sent, without a socket.
type streamRecorder struct {
	header  http.Header
	status  int
	pending bytes.Buffer
	body    *io.PipeWriter
	// err is why the client stopped reading, returned by the next write.
	err error
}

func (r *streamRecorder) Header() http.Header    { return r.header }
func (r *streamRecorder) WriteHeader(status int) { r.status = status }

func (r *streamRecorder) Write(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	return r.pending.Write(p)
}

func (r *streamRecorder) Flush() {
	if r.err == nil {
		_, r.err = r.body.Write(r.pending.Bytes())
	}
	r.pending.Reset()
}

func TestSandboxEventsStreamsServerSentEvents(t *testing.T) {
	// The stream decides when it is idle by the clock, so the handler runs in
	// a bubble whose clock moves only when every goroutine in it is blocked:
	// on a real clock, a late poll tick on a loaded runner read as an idle
	// stream and put a ping where the replay's next batch belonged.
	synctest.Test(t, func(t *testing.T) {
		stream := &scriptedEventStream{batches: [][]services.SandboxStreamEvent{
			{
				{Type: services.SandboxEventReset, Data: json.RawMessage(`{}`)},
				{ID: eventID(7), Type: "sandbox.updated", Data: json.RawMessage(`{"sandboxId":"sbx_1"}`)},
			},
			nil,
			{{ID: eventID(9), Type: "sandbox.deleted", Data: json.RawMessage(`{"sandboxId":"sbx_2"}`)}},
		}, cursors: []int64{12, 12, 12, 15}}
		service := &eventStreamService{stream: stream}
		router := chi.NewRouter()
		router.Method(http.MethodGet, "/api/projects/{projectId}/events", sandboxEventsHandler(service, 5*time.Millisecond, 20*time.Millisecond))

		// The deadline is on the bubble's clock: a stream that never pings
		// ends at once, and the test reports what it read.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/projects/project-1/events?lastEventId=3", nil)
		req.Header.Set("Last-Event-ID", "5")
		body, bodyWriter := io.Pipe()
		resp := &streamRecorder{header: http.Header{}, body: bodyWriter}
		done := make(chan struct{})
		go func() {
			defer close(done)
			router.ServeHTTP(resp, req)
			// The handler returning ends the response, as a server would:
			// what it left unflushed is sent, then the body ends.
			resp.Flush()
			_ = bodyWriter.Close()
		}()

		var lines []string
		scanner := bufio.NewScanner(body)
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
			if strings.HasPrefix(scanner.Text(), ": ping") {
				break
			}
		}
		// The client hangs up: the handler's next write fails, and it returns.
		cancel()
		_ = body.Close()
		<-done

		if err := scanner.Err(); err != nil {
			t.Fatalf("read sandbox events: %v", err)
		}

		if resp.status != http.StatusOK {
			t.Fatalf("status %d", resp.status)
		}
		if got := resp.header.Get("Content-Type"); got != "text/event-stream" {
			t.Fatalf("content type %q", got)
		}
		want := []string{
			"retry: 2000", "",
			"event: reset", "data: {}", "",
			"id: 7", "event: sandbox.updated", `data: {"sandboxId":"sbx_1"}`, "",
			// The replay's events are older than where the stream stands.
			"id: 12", "",
			"id: 9", "event: sandbox.deleted", `data: {"sandboxId":"sbx_2"}`, "",
			"id: 12", "",
			// Quiet, the head moved: the cursor goes in the ping's place, once.
			"id: 15", "",
			": ping",
		}
		if strings.Join(lines, "\n") != strings.Join(want, "\n") {
			t.Fatalf("stream:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
		}
		service.mu.Lock()
		defer service.mu.Unlock()
		if service.project != "project-1" || service.cursor == nil || *service.cursor != 5 {
			t.Fatalf("opened %q after %v, want project-1 after the header's 5", service.project, service.cursor)
		}
	})
}

func TestSandboxEventCursor(t *testing.T) {
	for _, tc := range []struct {
		header, query string
		want          *int64
		wantErr       bool
	}{
		{},
		{query: "12", want: eventID(12)},
		{header: "4", query: "12", want: eventID(4)},
		{header: "x", wantErr: true},
		{query: "-1", wantErr: true},
	} {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/projects/p/events?lastEventId="+tc.query, nil)
		if tc.header != "" {
			req.Header.Set("Last-Event-ID", tc.header)
		}
		got, err := sandboxEventCursor(req)
		if (err != nil) != tc.wantErr {
			t.Fatalf("header %q query %q: error %v", tc.header, tc.query, err)
		}
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Fatalf("header %q query %q: cursor %v, want %v", tc.header, tc.query, got, tc.want)
		}
	}
}
