package apigen

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type fakeProjectEventService struct {
	maxSeq    int64
	snapshots []ProjectEvent
	live      chan ProjectEvent
}

func (f *fakeProjectEventService) ListProjectResourceSnapshots(_ context.Context, _ string, resourceTypes []string, _ int64) []ProjectEvent {
	return filterProjectEvents(f.snapshots, -1, resourceTypes)
}

func TestSubscribeProjectEventsReadsProjectStream(t *testing.T) {
	service := &fakeProjectEventService{
		maxSeq: 7,
		snapshots: []ProjectEvent{
			testProjectEvent("snapshot", 7, "sandbox-1", EventActionListed),
		},
		live: make(chan ProjectEvent),
	}
	server := newProjectStreamTestServer(t, service)

	history := true
	listOnly := true
	client, err := NewClient(server.URL, WithClient(server.Client()))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	stream, err := client.SubscribeProjectEvents(context.Background(), "project-1", ProjectEventsParams{
		History:  &history,
		ListOnly: &listOnly,
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer stream.Close()

	msg, err := stream.Read()
	if err != nil {
		t.Fatalf("read list start: %v", err)
	}
	if msg.Event != ProjectEventNameListStart {
		t.Fatalf("first event = %q, want %q", msg.Event, ProjectEventNameListStart)
	}
	start, ok := msg.Data.(*ResourceListStartEvent)
	if !ok || start.ProjectID != "project-1" || start.Seq != 7 {
		t.Fatalf("list start = %#v", msg.Data)
	}

	msg, err = stream.Read()
	if err != nil {
		t.Fatalf("read resource listed: %v", err)
	}
	listed, ok := msg.Data.(*ResourceListedEvent)
	if !ok || listed.ID != "snapshot" || listed.ResourceID != "sandbox-1" {
		t.Fatalf("listed event = %#v", msg.Data)
	}

	msg, err = stream.Read()
	if err != nil {
		t.Fatalf("read list finish: %v", err)
	}
	if msg.Event != ProjectEventNameListFinish {
		t.Fatalf("third event = %q, want %q", msg.Event, ProjectEventNameListFinish)
	}

	_, err = stream.Read()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF after list-only stream, got %v", err)
	}
}

func newProjectStreamTestServer(t *testing.T, service *fakeProjectEventService) *httptest.Server {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/projects/") || !strings.HasSuffix(r.URL.Path, "/stream") {
			http.NotFound(w, r)
			return
		}
		projectID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/projects/"), "/stream")
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")

		var req projectStreamSubscriptionRequest
		if err := wsjson.Read(r.Context(), conn, &req); err != nil {
			return
		}
		if err := wsjson.Write(r.Context(), conn, projectStreamSocketMessage{Type: "subscribed", Stream: req.Stream, SandboxID: req.SandboxID}); err != nil {
			return
		}
		if req.History != nil && *req.History {
			snapshots := service.ListProjectResourceSnapshots(r.Context(), projectID, nil, service.maxSeq)
			if !writeProjectStreamMessage(r.Context(), conn, ProjectEventNameListStart, ResourceListStartEvent{ProjectID: projectID, Resources: []string{"sandbox"}, Seq: service.maxSeq, StartedAt: time.Now()}) {
				return
			}
			for _, event := range snapshots {
				if !writeProjectStreamEvent(r.Context(), conn, event) {
					return
				}
			}
			if !writeProjectStreamMessage(r.Context(), conn, ProjectEventNameListFinish, ResourceListFinishEvent{ProjectID: projectID, Resources: []string{"sandbox"}, Seq: service.maxSeq, FinishedAt: time.Now()}) {
				return
			}
		}
		if req.ListOnly {
			_ = wsjson.Write(r.Context(), conn, projectStreamSocketMessage{Type: "complete"})
		}
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func writeProjectStreamEvent(ctx context.Context, conn *websocket.Conn, event ProjectEvent) bool {
	return writeProjectStreamMessage(ctx, conn, event.Type, event)
}

func writeProjectStreamMessage(ctx context.Context, conn *websocket.Conn, eventName string, data any) bool {
	payload, err := json.Marshal(data)
	if err != nil {
		return false
	}
	return wsjson.Write(ctx, conn, projectStreamSocketMessage{Type: "event", Stream: "sandbox", Event: eventName, Data: payload}) == nil
}

func filterProjectEvents(events []ProjectEvent, afterSeq int64, resourceTypes []string) []ProjectEvent {
	var result []ProjectEvent
	for _, event := range events {
		if event.Seq <= afterSeq || event.ResourceType != "sandbox" {
			continue
		}
		result = append(result, event)
	}
	return result
}

func testProjectEvent(id string, seq int64, sandboxID, action string) ProjectEvent {
	eventType := EventTypeResourceChanged
	if action == EventActionListed {
		eventType = EventTypeResourceListed
	}
	return ProjectEvent{
		ID:           id,
		Seq:          seq,
		ProjectID:    "project-1",
		Type:         eventType,
		ResourceType: "sandbox",
		ResourceID:   sandboxID,
		Action:       action,
		Data:         []byte(`{"id":"` + sandboxID + `"}`),
		CreatedAt:    time.Date(2026, 6, 7, 1, 2, 3, 0, time.UTC),
	}
}
