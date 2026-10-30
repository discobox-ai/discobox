package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

type fakeEventService struct {
	start  services.ProjectEventStart
	stream *fakeEventStream
}

func (f *fakeEventService) OpenProjectEvents(_ context.Context, _ string, start services.ProjectEventStart) (services.ProjectEventStream, error) {
	f.start = start
	return f.stream, nil
}

type fakeEventStream struct {
	reset  string
	events chan model.ProjectEvent
	end    error
	closed bool
}

func (f *fakeEventStream) Reset() string { return f.reset }

func (f *fakeEventStream) Position() int64 { return 40 }

func (f *fakeEventStream) Through() (int64, bool) { return 99, true }

func (f *fakeEventStream) Next(ctx context.Context) (model.ProjectEvent, error) {
	select {
	case <-ctx.Done():
		return model.ProjectEvent{}, ctx.Err()
	case event, ok := <-f.events:
		if !ok {
			return model.ProjectEvent{}, f.end
		}
		return event, nil
	}
}

func (f *fakeEventStream) Close() { f.closed = true }

func serveEvents(t *testing.T, service services.EventService) *httptest.Server {
	t.Helper()
	router := chi.NewRouter()
	registerProjectEventRoutes(router, service)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server
}

// The stream is SSE framed as ADR 26-10-30-868 §1 says: retry first with the
// position the stream starts from, a reset with no id, an event per frame with its id, comments while idle, and the
// response ended when the stream falls behind.
func TestProjectEventsAreFramedAsServerSentEvents(t *testing.T) {
	keepalive := projectEventKeepalive
	projectEventKeepalive = 20 * time.Millisecond
	t.Cleanup(func() { projectEventKeepalive = keepalive })

	stream := &fakeEventStream{reset: services.ProjectEventResetExpired, events: make(chan model.ProjectEvent, 1), end: services.ErrProjectEventStreamBehind}
	service := &fakeEventService{stream: stream}
	server := serveEvents(t, service)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/projects/project-1/events?history=7", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Last-Event-ID", "41")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if got := response.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type = %q", got)
	}
	if service.start.LastEventID != "41" || service.start.History != 7 {
		t.Fatalf("start = %+v", service.start)
	}

	reader := bufio.NewReader(response.Body)
	frame := func() string {
		t.Helper()
		var lines []string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("read frame: %v (so far %q)", err, lines)
			}
			if line == "\n" {
				return strings.Join(lines, "")
			}
			lines = append(lines, line)
		}
	}
	if got := frame(); got != "retry: 2000\nid: 40\n" {
		t.Fatalf("first frame = %q", got)
	}
	if got := frame(); got != "event: reset\ndata: {\"reason\":\"expired\"}\n" {
		t.Fatalf("reset frame = %q", got)
	}
	if got := frame(); got != ": keepalive\nid: 99\n" {
		t.Fatalf("idle frame = %q", got)
	}
	data, err := json.Marshal(model.SandboxEvent{SandboxProjection: model.SandboxProjection{SandboxID: "sbx", Name: "alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	stream.events <- model.ProjectEvent{ID: 42, Type: model.ProjectEventSandboxUpdated, Data: data}
	for {
		got := frame()
		if got == ": keepalive\nid: 99\n" {
			continue
		}
		if want := "id: 42\nevent: sandbox.updated\ndata: " + string(data) + "\n"; got != want {
			t.Fatalf("event frame = %q, want %q", got, want)
		}
		break
	}
	close(stream.events)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if line != ": keepalive\n" && line != "id: 99\n" && line != "\n" {
			t.Fatalf("after falling behind = %q, want the response to end", line)
		}
	}
	if !stream.closed {
		t.Fatal("the stream was not closed with the response")
	}
}

func TestProjectEventsRefuseAHistoryOutOfRange(t *testing.T) {
	server := serveEvents(t, &fakeEventService{stream: &fakeEventStream{events: make(chan model.ProjectEvent)}})
	for _, history := range []string{"-1", "1001", "many"} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/projects/project-1/events?history="+history, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusBadRequest || response.Header.Get("Content-Type") != "application/problem+json" {
			t.Fatalf("history=%s: %d %s, want a 400 problem", history, response.StatusCode, response.Header.Get("Content-Type"))
		}
	}
}
