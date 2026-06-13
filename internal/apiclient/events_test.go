package apiclient

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/obot-platform/discobox/internal/model"
	"github.com/obot-platform/discobox/internal/realtime"
)

type fakeProjectEventService struct {
	maxSeq    int64
	history   []model.ProjectEvent
	snapshots []model.ProjectEvent
	live      chan model.ProjectEvent
}

func (f *fakeProjectEventService) MaxProjectEventSeq(context.Context, string) (int64, error) {
	return f.maxSeq, nil
}

func (f *fakeProjectEventService) ListProjectEventsAfterSeq(_ context.Context, _ string, afterSeq int64, resourceTypes []string) ([]model.ProjectEvent, error) {
	return filterProjectEvents(f.history, afterSeq, resourceTypes), nil
}

func (f *fakeProjectEventService) ListProjectResourceSnapshots(_ context.Context, _ string, resourceTypes []string, _ int64) ([]model.ProjectEvent, error) {
	return filterProjectEvents(f.snapshots, -1, resourceTypes), nil
}

func (f *fakeProjectEventService) SubscribeProjectEvents(context.Context, string) (<-chan model.ProjectEvent, func(), error) {
	if f.live == nil {
		f.live = make(chan model.ProjectEvent)
	}
	return f.live, func() {}, nil
}

func TestSubscribeProjectEventsReadsProjectStream(t *testing.T) {
	service := &fakeProjectEventService{
		maxSeq: 7,
		snapshots: []model.ProjectEvent{
			testProjectEvent("snapshot", 7, "sandbox-1", model.EventActionListed),
		},
		live: make(chan model.ProjectEvent),
	}
	server := newProjectStreamTestServer(t, service)

	list := true
	replayOnly := true
	client, err := NewEventClient(server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	stream, err := client.SubscribeProjectEvents(context.Background(), "project-1", ProjectEventsParams{
		List:       &list,
		ReplayOnly: &replayOnly,
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
		t.Fatalf("expected EOF after replay-only stream, got %v", err)
	}
}

func TestSubscribeProjectEventsReplaysHistoryAndFiltersSandbox(t *testing.T) {
	service := &fakeProjectEventService{
		maxSeq: 4,
		history: []model.ProjectEvent{
			testProjectEvent("old", 2, "sandbox-1", model.EventActionUpdated),
			testProjectEvent("match", 3, "sandbox-1", model.EventActionUpdated),
			testProjectEvent("other", 4, "sandbox-2", model.EventActionUpdated),
		},
		live: make(chan model.ProjectEvent),
	}
	server := newProjectStreamTestServer(t, service)

	replayOnly := true
	client, err := NewEventClient(server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	stream, err := client.SubscribeProjectEvents(context.Background(), "project-1", ProjectEventsParams{
		Replay:     true,
		ReplayOnly: &replayOnly,
		SandboxID:  "sandbox-1",
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer stream.Close()

	msg, err := stream.Read()
	if err != nil {
		t.Fatalf("read replay: %v", err)
	}
	changed, ok := msg.Data.(*ResourceChangedEvent)
	if !ok || changed.ID != "old" || changed.ResourceID != "sandbox-1" {
		t.Fatalf("changed event = %#v", msg.Data)
	}
	msg, err = stream.Read()
	if err != nil {
		t.Fatalf("read replay: %v", err)
	}
	changed, ok = msg.Data.(*ResourceChangedEvent)
	if !ok || changed.ID != "match" || changed.ResourceID != "sandbox-1" {
		t.Fatalf("changed event = %#v", msg.Data)
	}
	_, err = stream.Read()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF after replay-only stream, got %v", err)
	}
}

func newProjectStreamTestServer(t *testing.T, service *fakeProjectEventService) *httptest.Server {
	t.Helper()
	router := chi.NewRouter()
	realtime.RegisterProjectStreamRoutes(router, service)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server
}

func filterProjectEvents(events []model.ProjectEvent, afterSeq int64, resourceTypes []string) []model.ProjectEvent {
	var result []model.ProjectEvent
	for _, event := range events {
		if event.Seq <= afterSeq || event.ResourceType != "sandbox" {
			continue
		}
		result = append(result, event)
	}
	return result
}

func testProjectEvent(id string, seq int64, sandboxID, action string) model.ProjectEvent {
	eventType := model.EventTypeResourceChanged
	if action == model.EventActionListed {
		eventType = model.EventTypeResourceListed
	}
	return model.ProjectEvent{
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
