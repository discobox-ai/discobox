package apiclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSubscribeProjectEventsReadsReplayStream(t *testing.T) {
	projectID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	eventID := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/projects/"+projectID.String()+"/events" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Fatalf("Accept = %q", got)
		}
		q := r.URL.Query()
		if got := q["resources"]; len(got) != 2 || got[0] != "sandbox" || got[1] != "project" {
			t.Fatalf("resources query = %#v", got)
		}
		if got := q.Get("afterSeq"); got != "7" {
			t.Fatalf("afterSeq = %q", got)
		}
		if got := q.Get("list"); got != "true" {
			t.Fatalf("list = %q", got)
		}
		if got := q.Get("replayOnly"); got != "true" {
			t.Fatalf("replayOnly = %q", got)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "id: 7\n")
		fmt.Fprintf(w, "event: listStart\n")
		fmt.Fprintf(w, "data: {\"projectId\":%q,\"resources\":[\"sandbox\"],\"seq\":7,\"startedAt\":\"2026-06-07T01:02:03Z\"}\n\n", projectID)
		fmt.Fprintf(w, "id: 8\n")
		fmt.Fprintf(w, "event: resourceChanged\n")
		fmt.Fprintf(w, "data: {\"id\":%q,\"seq\":8,\"projectId\":%q,\"type\":\"resource.changed\",\"resourceType\":\"sandbox\",\"resourceId\":\"sandbox-1\",\"action\":\"created\",\"data\":{\"name\":\"alpha\"},\"createdAt\":\"2026-06-07T01:02:04Z\"}\n\n", eventID, projectID)
	}))
	t.Cleanup(server.Close)

	afterSeq := int64(7)
	list := true
	replayOnly := true
	client, err := NewEventClient(server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	stream, err := client.SubscribeProjectEvents(context.Background(), projectID, ProjectEventsParams{
		Resources:  []string{"sandbox", "project"},
		AfterSeq:   &afterSeq,
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
	if msg.ID != "7" || msg.Event != ProjectEventNameListStart {
		t.Fatalf("unexpected first message: %#v", msg)
	}
	start, ok := msg.Data.(*ResourceListStartEvent)
	if !ok {
		t.Fatalf("first data type = %T", msg.Data)
	}
	if start.ProjectID != projectID || start.Seq != 7 || !start.StartedAt.Equal(time.Date(2026, 6, 7, 1, 2, 3, 0, time.UTC)) {
		t.Fatalf("unexpected list start: %#v", start)
	}

	msg, err = stream.Read()
	if err != nil {
		t.Fatalf("read resource changed: %v", err)
	}
	changed, ok := msg.Data.(*ResourceChangedEvent)
	if !ok {
		t.Fatalf("second data type = %T", msg.Data)
	}
	if changed.ID != eventID || changed.ResourceID != "sandbox-1" || changed.Action != "created" {
		t.Fatalf("unexpected changed event: %#v", changed)
	}

	_, err = stream.Read()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestReadSSEFrameCombinesMultilineData(t *testing.T) {
	msg, err := decodeProjectEventMessage(sseFrame{
		event: "unknown",
		data:  []byte("{\"a\":1}\n{\"b\":2}"),
	})
	if err != nil {
		t.Fatalf("decode unknown: %v", err)
	}
	unknown, ok := msg.Data.(*UnknownProjectEvent)
	if !ok {
		t.Fatalf("data type = %T", msg.Data)
	}
	if string(unknown.Data) != "{\"a\":1}\n{\"b\":2}" {
		t.Fatalf("data = %q", unknown.Data)
	}
}
