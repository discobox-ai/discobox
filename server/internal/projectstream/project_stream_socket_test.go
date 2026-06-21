package projectstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"github.com/obot-platform/discobox/server/internal/model"
	services "github.com/obot-platform/discobox/server/internal/services"
)

type fakeProjectEventService struct {
	maxSeq    int64
	snapshots []model.ProjectEvent
	live      chan model.ProjectEvent
}

func (f *fakeProjectEventService) MaxProjectEventSeq(context.Context, string) (int64, error) {
	return f.maxSeq, nil
}

func (f *fakeProjectEventService) ListProjectEventsAfterSeq(context.Context, string, int64, []string) ([]model.ProjectEvent, error) {
	return nil, nil
}

func (f *fakeProjectEventService) ListProjectResourceSnapshots(_ context.Context, _ string, resourceTypes []string, _ int64) ([]model.ProjectEvent, error) {
	return filterEvents(f.snapshots, -1, resourceTypes), nil
}

func (f *fakeProjectEventService) SubscribeProjectEvents(context.Context, string) (<-chan model.ProjectEvent, func(), error) {
	if f.live == nil {
		f.live = make(chan model.ProjectEvent)
	}
	return f.live, func() {}, nil
}

func filterEvents(events []model.ProjectEvent, afterSeq int64, resourceTypes []string) []model.ProjectEvent {
	var result []model.ProjectEvent
	for _, event := range events {
		if event.Seq <= afterSeq || event.ResourceType != "sandbox" {
			continue
		}
		result = append(result, event)
	}
	return result
}

func TestSandboxSubscriptionSendsListAndLiveEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	live := make(chan model.ProjectEvent, 1)
	service := &fakeProjectEventService{
		maxSeq: 10,
		snapshots: []model.ProjectEvent{
			testProjectEvent("snapshot-1", 10, "sandbox-1", model.EventActionListed),
			testProjectEvent("snapshot-2", 10, "sandbox-2", model.EventActionListed),
		},
		live: live,
	}
	socket := testSocket(ctx, cancel, service)

	socket.startSandboxSubscription(ProjectStreamSubscriptionRequest{Type: messageTypeSubscribe, Stream: streamSandbox})

	assertMessage(t, socket.outgoing, messageTypeSubscribed, streamSandbox, "", "")
	assertMessage(t, socket.outgoing, messageTypeEvent, streamSandbox, eventConnected, "")
	assertMessage(t, socket.outgoing, messageTypeEvent, streamSandbox, eventListStart, "")
	assertMessage(t, socket.outgoing, messageTypeEvent, streamSandbox, model.EventTypeResourceListed, "snapshot-1")
	assertMessage(t, socket.outgoing, messageTypeEvent, streamSandbox, model.EventTypeResourceListed, "snapshot-2")
	assertMessage(t, socket.outgoing, messageTypeEvent, streamSandbox, eventListEnd, "")

	live <- testProjectEvent("live-1", 11, "sandbox-1", model.EventActionUpdated)
	assertMessage(t, socket.outgoing, messageTypeEvent, streamSandbox, model.EventTypeResourceChanged, "live-1")
}

func TestSandboxSubscriptionFiltersLiveEventsAfterConnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	live := make(chan model.ProjectEvent, 3)
	service := &fakeProjectEventService{
		maxSeq: 99,
		live:   live,
	}
	socket := testSocket(ctx, cancel, service)
	history := false

	socket.startSandboxSubscription(ProjectStreamSubscriptionRequest{Type: messageTypeSubscribe, Stream: streamSandbox, SandboxID: "sandbox-1", History: &history})

	assertMessage(t, socket.outgoing, messageTypeSubscribed, streamSandbox, "", "")
	assertMessage(t, socket.outgoing, messageTypeEvent, streamSandbox, eventConnected, "")
	live <- testProjectEvent("old", 99, "sandbox-1", model.EventActionUpdated)
	live <- testProjectEvent("other", 100, "sandbox-2", model.EventActionUpdated)
	live <- testProjectEvent("match", 101, "sandbox-1", model.EventActionUpdated)
	assertMessage(t, socket.outgoing, messageTypeEvent, streamSandbox, model.EventTypeResourceChanged, "match")
	assertNoMessage(t, socket.outgoing)
}

func TestSandboxSubscriptionListOnlyCompletesWithoutLiveSubscription(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	service := &fakeProjectEventService{
		maxSeq: 1,
		snapshots: []model.ProjectEvent{
			testProjectEvent("snapshot", 1, "sandbox-1", model.EventActionListed),
		},
		live: make(chan model.ProjectEvent),
	}
	socket := testSocket(ctx, cancel, service)

	socket.startSandboxSubscription(ProjectStreamSubscriptionRequest{Type: messageTypeSubscribe, Stream: streamSandbox, ListOnly: true})

	assertMessage(t, socket.outgoing, messageTypeSubscribed, streamSandbox, "", "")
	assertMessage(t, socket.outgoing, messageTypeEvent, streamSandbox, eventConnected, "")
	assertMessage(t, socket.outgoing, messageTypeEvent, streamSandbox, eventListStart, "")
	assertMessage(t, socket.outgoing, messageTypeEvent, streamSandbox, model.EventTypeResourceListed, "snapshot")
	assertMessage(t, socket.outgoing, messageTypeEvent, streamSandbox, eventListEnd, "")
	assertMessage(t, socket.outgoing, messageTypeComplete, streamSandbox, "", "")
}

func TestResubscribeOldCleanupDoesNotRemoveNewSubscription(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	service := newResubscribeProjectEventService()
	socket := testSocket(ctx, cancel, service)
	history := false
	req := ProjectStreamSubscriptionRequest{Type: messageTypeSubscribe, Stream: streamSandbox, History: &history}

	socket.startSandboxSubscription(req)
	assertMessage(t, socket.outgoing, messageTypeSubscribed, streamSandbox, "", "")
	assertMessage(t, socket.outgoing, messageTypeEvent, streamSandbox, eventConnected, "")

	socket.startSandboxSubscription(req)
	assertMessage(t, socket.outgoing, messageTypeSubscribed, streamSandbox, "", "")
	assertMessage(t, socket.outgoing, messageTypeEvent, streamSandbox, eventConnected, "")

	select {
	case <-service.firstUnsubscribeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for old subscription cleanup")
	}
	close(service.releaseFirstUnsubscribe)

	select {
	case <-service.firstUnsubscribeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for old subscription cleanup to finish")
	}

	socket.cancelSubscription(requestKey(req))

	select {
	case <-service.secondUnsubscribed:
	case <-time.After(2 * time.Second):
		t.Fatal("new subscription was not canceled after old cleanup completed")
	}
}

func TestUnsupportedStreamWritesError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	socket := testSocket(ctx, cancel, &fakeProjectEventService{})
	socket.handleSubscribe(ProjectStreamSubscriptionRequest{Type: messageTypeSubscribe, Stream: "worker"})

	msg := assertMessage(t, socket.outgoing, messageTypeError, "worker", "", "")
	if msg.Error == "" {
		t.Fatal("expected error message")
	}
}

func TestProjectStreamRouteRejectsCrossOriginWebSocket(t *testing.T) {
	server := testProjectStreamServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, resp, err := websocket.Dial(ctx, websocketURL(server.URL), &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"https://evil.example"}},
	})
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected cross-origin websocket dial to fail")
	}
}

func TestProjectStreamRouteAcceptsSameOriginWebSocket(t *testing.T) {
	server := testProjectStreamServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, websocketURL(server.URL), &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{server.URL}},
	})
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("expected same-origin websocket dial to succeed: %v", err)
	}
	if err := conn.CloseNow(); err != nil {
		t.Fatalf("close websocket: %v", err)
	}
}

func TestProjectStreamSSESendsListWithoutSSEIDs(t *testing.T) {
	server := testProjectStreamSSEServer(t, &fakeProjectEventService{
		maxSeq: 2,
		snapshots: []model.ProjectEvent{
			testProjectEvent("first", 2, "sandbox-1", model.EventActionListed),
			testProjectEvent("second", 2, "sandbox-2", model.EventActionListed),
		},
		live: make(chan model.ProjectEvent),
	})

	req, err := http.NewRequest(http.MethodGet, server.URL+"/projects/project-1/stream/sse?listOnly=true", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get sse stream: %v", err)
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read sse stream: %v", err)
	}
	body := string(bodyBytes)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sse status = %d, body = %s", resp.StatusCode, body)
	}
	for _, want := range []string{
		"event: connected\n",
		"event: list-start\n",
		"event: " + model.EventTypeResourceListed + "\n",
		"event: list-end\n",
		`"id":"first"`,
		`"id":"second"`,
		"event: complete\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("sse body missing %q:\n%s", want, body)
		}
	}
	if strings.HasPrefix(body, "id:") || strings.Contains(body, "\nid:") {
		t.Fatalf("sse body unexpectedly included event transport id field:\n%s", body)
	}
}

func TestProjectStreamSSECanOptOutOfHistory(t *testing.T) {
	server := testProjectStreamSSEServer(t, &fakeProjectEventService{
		maxSeq: 1,
		snapshots: []model.ProjectEvent{
			testProjectEvent("first", 1, "sandbox-1", model.EventActionUpdated),
		},
		live: make(chan model.ProjectEvent),
	})

	resp, err := http.Get(server.URL + "/projects/project-1/stream/sse?history=false&listOnly=true")
	if err != nil {
		t.Fatalf("get sse stream: %v", err)
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read sse stream: %v", err)
	}
	body := string(bodyBytes)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sse status = %d, body = %s", resp.StatusCode, body)
	}
	if strings.Contains(body, "resourceListed") || strings.Contains(body, `"id":"first"`) {
		t.Fatalf("sse body included resource data despite history=false:\n%s", body)
	}
	if !strings.Contains(body, "event: connected\n") || !strings.Contains(body, "event: complete\n") {
		t.Fatalf("sse body missing lifecycle events:\n%s", body)
	}
}

func testProjectStreamServer(t *testing.T) *httptest.Server {
	t.Helper()
	router := chi.NewRouter()
	RegisterProjectStreamRoutes(router, &fakeProjectEventService{live: make(chan model.ProjectEvent)})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server
}

func testProjectStreamSSEServer(t *testing.T, service services.ProjectEventService) *httptest.Server {
	t.Helper()
	router := chi.NewRouter()
	RegisterProjectStreamSSERoutes(router, service)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server
}

func websocketURL(serverURL string) string {
	return "ws" + strings.TrimPrefix(serverURL, "http") + "/projects/project-1/stream"
}

func testSocket(ctx context.Context, cancel context.CancelFunc, service services.ProjectEventService) *ProjectStreamSocket {
	return &ProjectStreamSocket{
		service:       service,
		projectID:     "project-1",
		ctx:           ctx,
		cancel:        cancel,
		outgoing:      make(chan ProjectStreamSocketMessage, 32),
		subscriptions: map[subscriptionKey]*subscription{},
	}
}

type resubscribeProjectEventService struct {
	mu                      sync.Mutex
	calls                   int
	firstUnsubscribeStarted chan struct{}
	firstUnsubscribeDone    chan struct{}
	releaseFirstUnsubscribe chan struct{}
	secondUnsubscribed      chan struct{}
	secondUnsubscribedOnce  sync.Once
}

func newResubscribeProjectEventService() *resubscribeProjectEventService {
	return &resubscribeProjectEventService{
		firstUnsubscribeStarted: make(chan struct{}),
		firstUnsubscribeDone:    make(chan struct{}),
		releaseFirstUnsubscribe: make(chan struct{}),
		secondUnsubscribed:      make(chan struct{}),
	}
}

func (s *resubscribeProjectEventService) MaxProjectEventSeq(context.Context, string) (int64, error) {
	return 0, nil
}

func (s *resubscribeProjectEventService) ListProjectEventsAfterSeq(context.Context, string, int64, []string) ([]model.ProjectEvent, error) {
	return nil, nil
}

func (s *resubscribeProjectEventService) ListProjectResourceSnapshots(context.Context, string, []string, int64) ([]model.ProjectEvent, error) {
	return nil, nil
}

func (s *resubscribeProjectEventService) SubscribeProjectEvents(context.Context, string) (<-chan model.ProjectEvent, func(), error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()

	events := make(chan model.ProjectEvent)
	switch call {
	case 1:
		return events, func() {
			close(s.firstUnsubscribeStarted)
			<-s.releaseFirstUnsubscribe
			close(s.firstUnsubscribeDone)
		}, nil
	default:
		return events, func() {
			s.secondUnsubscribedOnce.Do(func() {
				close(s.secondUnsubscribed)
			})
		}, nil
	}
}

func testProjectEvent(id string, seq int64, sandboxID, action string) model.ProjectEvent {
	payload, _ := json.Marshal(map[string]string{"id": sandboxID})
	return model.ProjectEvent{
		ID:           id,
		Seq:          seq,
		ProjectID:    "project-1",
		Type:         eventTypeForAction(action),
		ResourceType: "sandbox",
		ResourceID:   sandboxID,
		Action:       action,
		Data:         payload,
		CreatedAt:    time.Now().UTC(),
	}
}

func eventTypeForAction(action string) string {
	if action == model.EventActionListed {
		return model.EventTypeResourceListed
	}
	return model.EventTypeResourceChanged
}

func assertMessage(t *testing.T, messages <-chan ProjectStreamSocketMessage, wantType, wantStream, wantEvent, wantID string) ProjectStreamSocketMessage {
	t.Helper()
	select {
	case msg := <-messages:
		if msg.Type != wantType || msg.Stream != wantStream || (wantEvent != "" && msg.Event != wantEvent) || (wantID != "" && messageDataID(msg) != wantID) {
			t.Fatalf("message = %#v, want type=%q stream=%q event=%q id=%q", msg, wantType, wantStream, wantEvent, wantID)
		}
		return msg
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for message type=%q stream=%q event=%q", wantType, wantStream, wantEvent)
		return ProjectStreamSocketMessage{}
	}
}

func messageDataID(msg ProjectStreamSocketMessage) string {
	event, ok := msg.Data.(model.ProjectEvent)
	if !ok {
		return ""
	}
	return event.ID
}

func assertNoMessage(t *testing.T, messages <-chan ProjectStreamSocketMessage) {
	t.Helper()
	select {
	case msg := <-messages:
		t.Fatalf("unexpected message: %#v", msg)
	case <-time.After(50 * time.Millisecond):
	}
}
