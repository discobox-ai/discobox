// Package projectstream contains project event streaming endpoints.
package projectstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/server/internal/api"
)

const (
	messageTypeSubscribe    = "subscribe"
	messageTypeUnsubscribe  = "unsubscribe"
	messageTypeSubscribed   = "subscribed"
	messageTypeUnsubscribed = "unsubscribed"
	messageTypeEvent        = "event"
	messageTypeError        = "error"
	messageTypeComplete     = "complete"

	streamSandbox = "sandbox"

	eventConnected = "connected"
	eventListStart = "list-start"
	eventListEnd   = "list-end"
)

// ProjectStreamSubscriptionRequest is the client-to-server message used to
// manage streams multiplexed over one project websocket.
type ProjectStreamSubscriptionRequest struct {
	Type       string `json:"type"`
	Stream     string `json:"stream"`
	SandboxID  string `json:"sandboxId,omitempty"`
	Replay     bool   `json:"replay,omitempty"`
	ReplayOnly bool   `json:"replayOnly,omitempty"`
	List       *bool  `json:"list,omitempty"`
}

// ProjectStreamSocketMessage is the server-to-client message emitted by the
// project websocket.
type ProjectStreamSocketMessage struct {
	Type      string `json:"type"`
	Stream    string `json:"stream,omitempty"`
	SandboxID string `json:"sandboxId,omitempty"`
	Event     string `json:"event,omitempty"`
	Data      any    `json:"data,omitempty"`
	Seq       int64  `json:"seq,omitempty"`
	Error     string `json:"error,omitempty"`
}

type ResourceListStartEvent struct {
	ProjectID string    `json:"projectId"`
	Resources []string  `json:"resources"`
	Seq       int64     `json:"seq"`
	StartedAt time.Time `json:"startedAt"`
}

type ResourceListFinishEvent struct {
	ProjectID  string    `json:"projectId"`
	Resources  []string  `json:"resources"`
	Seq        int64     `json:"seq"`
	FinishedAt time.Time `json:"finishedAt"`
}

type ConnectedEvent struct {
	ProjectID string `json:"projectId"`
}

type CompleteEvent struct {
	Stream    string `json:"stream"`
	SandboxID string `json:"sandboxId,omitempty"`
}

type StreamErrorEvent struct {
	Stream    string `json:"stream"`
	SandboxID string `json:"sandboxId,omitempty"`
	Error     string `json:"error"`
}

type ResourceChangedEvent model.ProjectEvent

type ResourceListedEvent model.ProjectEvent

type ProjectStreamSSEInput struct {
	ProjectID  string `path:"projectId" doc:"Project ID"`
	Stream     string `query:"stream" enum:"sandbox" default:"sandbox" doc:"Stream name"`
	SandboxID  string `query:"sandboxId,omitempty" doc:"Sandbox ID to stream; defaults to all sandboxes"`
	History    bool   `query:"history" default:"true" doc:"Send full event history before live changes; defaults to true"`
	ReplayOnly bool   `query:"replayOnly,omitempty" doc:"Return after history/list instead of waiting for live events"`
	List       bool   `query:"list,omitempty" doc:"Send a current resource list before history/live changes"`
}

type subscriptionKey struct {
	stream    string
	sandboxID string
}

type subscription struct {
	cancel context.CancelFunc
}

// RegisterProjectStreamRoutes registers websocket stream routes on the router.
func RegisterProjectStreamRoutes(router chi.Router, service api.ProjectEventService) {
	router.Get("/projects/{projectId}/stream", func(w http.ResponseWriter, r *http.Request) {
		if service == nil {
			http.Error(w, "project event service is not configured", http.StatusServiceUnavailable)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		socket := NewProjectStreamSocket(ctx, cancel, conn, chi.URLParam(r, "projectId"), service)
		socket.Run()
	})
}

// RegisterProjectStreamSSEOperations registers the OpenAPI-documented static
// project stream subscription endpoint. Unlike SSE Last-Event-ID resume
// patterns, this endpoint never writes event IDs and does not accept resume IDs.
// Reconnects should request full history or opt out of history.
func RegisterProjectStreamSSEOperations(humaAPI huma.API, service api.ProjectEventService) {
	registerSSEOperation[ProjectStreamSSEInput](humaAPI, huma.Operation{
		OperationID: "subscribe-project-stream-sse",
		Method:      http.MethodGet,
		Path:        "/projects/{projectId}/stream/sse",
		Tags:        []string{"Project Streams"},
		Summary:     "Subscribe to a static project event stream using SSE",
		Description: "Subscribes to one project stream using query parameters. This endpoint does not emit SSE id fields and does not support Last-Event-ID resume semantics; reconnects should request full history or set history=false.",
		Errors:      []int{http.StatusServiceUnavailable},
	}, map[string]any{
		eventConnected:                 ConnectedEvent{},
		eventListStart:                 ResourceListStartEvent{},
		eventListEnd:                   ResourceListFinishEvent{},
		model.EventTypeResourceChanged: ResourceChangedEvent{},
		model.EventTypeResourceListed:  ResourceListedEvent{},
		messageTypeError:               StreamErrorEvent{},
		messageTypeComplete:            CompleteEvent{},
	}, func(ctx context.Context, input *ProjectStreamSSEInput, send sseSendFunc) {
		if service == nil {
			_ = send(messageTypeError, StreamErrorEvent{Stream: input.Stream, SandboxID: input.SandboxID, Error: "project event service is not configured"})
			return
		}
		runProjectStreamSSE(ctx, service, input, send)
	})
}

// ProjectStreamSocket multiplexes project-scoped subscriptions over one websocket.
type ProjectStreamSocket struct {
	service   api.ProjectEventService
	projectID string
	conn      *websocket.Conn
	ctx       context.Context
	cancel    context.CancelFunc
	outgoing  chan ProjectStreamSocketMessage

	subscriptionsMu sync.Mutex
	subscriptions   map[subscriptionKey]*subscription
}

func NewProjectStreamSocket(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, projectID string, service api.ProjectEventService) *ProjectStreamSocket {
	conn.SetReadLimit(1 << 20)
	return &ProjectStreamSocket{
		service:       service,
		projectID:     projectID,
		conn:          conn,
		ctx:           ctx,
		cancel:        cancel,
		outgoing:      make(chan ProjectStreamSocketMessage, 128),
		subscriptions: map[subscriptionKey]*subscription{},
	}
}

func (s *ProjectStreamSocket) Run() {
	defer s.cancelAllSubscriptions()
	defer s.conn.Close(websocket.StatusNormalClosure, "done")

	writerDone := make(chan struct{})
	go s.runWriter(writerDone)

	s.runReader()
	s.cancel()
	<-writerDone
}

func (s *ProjectStreamSocket) runWriter(done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-s.ctx.Done():
			return
		case message := <-s.outgoing:
			if err := wsjson.Write(s.ctx, s.conn, message); err != nil {
				s.cancel()
				return
			}
		}
	}
}

func (s *ProjectStreamSocket) runReader() {
	for {
		var req ProjectStreamSubscriptionRequest
		if err := wsjson.Read(s.ctx, s.conn, &req); err != nil {
			status := websocket.CloseStatus(err)
			if status != websocket.StatusNormalClosure && status != websocket.StatusGoingAway && status != websocket.StatusAbnormalClosure && !errors.Is(err, net.ErrClosed) && s.ctx.Err() == nil {
				log.Printf("project stream websocket read error: %v", err)
			}
			return
		}
		s.handleRequest(req)
	}
}

func (s *ProjectStreamSocket) handleRequest(req ProjectStreamSubscriptionRequest) {
	key := requestKey(req)
	switch req.Type {
	case messageTypeSubscribe:
		s.handleSubscribe(req)
	case messageTypeUnsubscribe:
		s.cancelSubscription(key)
		_ = s.writeMessage(ProjectStreamSocketMessage{Type: messageTypeUnsubscribed, Stream: req.Stream, SandboxID: req.SandboxID})
	default:
		_ = s.writeMessage(ProjectStreamSocketMessage{Type: messageTypeError, Stream: req.Stream, Error: fmt.Sprintf("unsupported message type %q", req.Type)})
	}
}

func (s *ProjectStreamSocket) handleSubscribe(req ProjectStreamSubscriptionRequest) {
	switch req.Stream {
	case streamSandbox:
		s.startSandboxSubscription(req)
	default:
		_ = s.writeMessage(ProjectStreamSocketMessage{Type: messageTypeError, Stream: req.Stream, Error: fmt.Sprintf("unsupported stream %q", req.Stream)})
	}
}

func (s *ProjectStreamSocket) startSandboxSubscription(req ProjectStreamSubscriptionRequest) {
	key := requestKey(req)
	s.cancelSubscription(key)

	streamCtx, streamCancel := context.WithCancel(s.ctx)
	ch, unsubscribe, err := s.service.SubscribeProjectEvents(streamCtx, s.projectID)
	if err != nil {
		streamCancel()
		_ = s.writeMessage(ProjectStreamSocketMessage{Type: messageTypeError, Stream: req.Stream, SandboxID: req.SandboxID, Error: err.Error()})
		return
	}
	if unsubscribe == nil {
		unsubscribe = func() {}
	}
	cursor, err := s.subscriptionCursor(streamCtx, req)
	if err != nil {
		unsubscribe()
		streamCancel()
		_ = s.writeMessage(ProjectStreamSocketMessage{Type: messageTypeError, Stream: req.Stream, SandboxID: req.SandboxID, Error: err.Error()})
		return
	}

	sub := s.trackSubscription(key, streamCancel)
	cleanupStarted := false
	cleanup := func() {
		if cleanupStarted {
			return
		}
		cleanupStarted = true
		unsubscribe()
		streamCancel()
		s.removeSubscription(key, sub)
	}
	if !s.writeMessage(ProjectStreamSocketMessage{Type: messageTypeSubscribed, Stream: req.Stream, SandboxID: req.SandboxID}) {
		cleanup()
		return
	}
	if !s.writeMessage(ProjectStreamSocketMessage{Type: messageTypeEvent, Stream: req.Stream, SandboxID: req.SandboxID, Event: eventConnected, Data: map[string]string{"projectId": s.projectID}}) {
		cleanup()
		return
	}

	if req.listEnabled() && !s.writeSandboxList(streamCtx, key, sub, req, cursor) {
		cleanup()
		return
	}
	if req.replayHistory() {
		events, err := s.service.ListProjectEventsAfterSeq(streamCtx, s.projectID, cursor, []string{"sandbox"})
		if err != nil {
			_ = s.writeMessage(ProjectStreamSocketMessage{Type: messageTypeError, Stream: req.Stream, SandboxID: req.SandboxID, Error: err.Error()})
			cleanup()
			return
		}
		for _, event := range events {
			if !s.matchesSandbox(req, event) {
				continue
			}
			if !s.writeProjectEvent(req, event) {
				cleanup()
				return
			}
			cursor = event.Seq
		}
	}

	if req.ReplayOnly {
		cleanup()
		_ = s.writeMessage(ProjectStreamSocketMessage{Type: messageTypeComplete, Stream: req.Stream, SandboxID: req.SandboxID})
		return
	}

	go func() {
		defer func() {
			cleanup()
			_ = s.writeMessage(ProjectStreamSocketMessage{Type: messageTypeComplete, Stream: req.Stream, SandboxID: req.SandboxID})
		}()
		for {
			select {
			case <-streamCtx.Done():
				return
			case event, ok := <-ch:
				if !ok {
					return
				}
				if event.Seq <= cursor || !s.matchesSandbox(req, event) {
					continue
				}
				if !s.writeProjectEvent(req, event) {
					return
				}
				cursor = event.Seq
			}
		}
	}()
}

func (s *ProjectStreamSocket) subscriptionCursor(ctx context.Context, req ProjectStreamSubscriptionRequest) (int64, error) {
	if req.Replay {
		return 0, nil
	}
	return s.service.MaxProjectEventSeq(ctx, s.projectID)
}

func (s *ProjectStreamSocket) writeSandboxList(ctx context.Context, key subscriptionKey, sub *subscription, req ProjectStreamSubscriptionRequest, seq int64) bool {
	start := ResourceListStartEvent{
		ProjectID: s.projectID,
		Resources: []string{"sandbox"},
		Seq:       seq,
		StartedAt: time.Now().UTC(),
	}
	if !s.writeMessage(ProjectStreamSocketMessage{Type: messageTypeEvent, Stream: req.Stream, SandboxID: req.SandboxID, Event: eventListStart, Data: start, Seq: seq}) {
		s.cancelTrackedSubscription(key, sub)
		return false
	}
	events, err := s.service.ListProjectResourceSnapshots(ctx, s.projectID, []string{"sandbox"}, seq)
	if err != nil {
		_ = s.writeMessage(ProjectStreamSocketMessage{Type: messageTypeError, Stream: req.Stream, SandboxID: req.SandboxID, Error: err.Error()})
		s.cancelTrackedSubscription(key, sub)
		return false
	}
	for _, event := range events {
		if !s.matchesSandbox(req, event) {
			continue
		}
		if !s.writeProjectEvent(req, event) {
			return false
		}
	}
	finish := ResourceListFinishEvent{
		ProjectID:  s.projectID,
		Resources:  []string{"sandbox"},
		Seq:        seq,
		FinishedAt: time.Now().UTC(),
	}
	if !s.writeMessage(ProjectStreamSocketMessage{Type: messageTypeEvent, Stream: req.Stream, SandboxID: req.SandboxID, Event: eventListEnd, Data: finish, Seq: seq}) {
		s.cancelTrackedSubscription(key, sub)
		return false
	}
	return true
}

func (s *ProjectStreamSocket) matchesSandbox(req ProjectStreamSubscriptionRequest, event model.ProjectEvent) bool {
	return event.ResourceType == "sandbox" && (req.SandboxID == "" || event.ResourceID == req.SandboxID)
}

func (s *ProjectStreamSocket) writeProjectEvent(req ProjectStreamSubscriptionRequest, event model.ProjectEvent) bool {
	return s.writeMessage(ProjectStreamSocketMessage{
		Type:      messageTypeEvent,
		Stream:    req.Stream,
		SandboxID: req.SandboxID,
		Event:     event.Type,
		Data:      event,
		Seq:       event.Seq,
	})
}

func (s *ProjectStreamSocket) writeMessage(message ProjectStreamSocketMessage) bool {
	select {
	case <-s.ctx.Done():
		return false
	case s.outgoing <- message:
		return true
	}
}

func (s *ProjectStreamSocket) trackSubscription(key subscriptionKey, cancel context.CancelFunc) *subscription {
	sub := &subscription{cancel: cancel}
	s.subscriptionsMu.Lock()
	defer s.subscriptionsMu.Unlock()
	s.subscriptions[key] = sub
	return sub
}

func (s *ProjectStreamSocket) removeSubscription(key subscriptionKey, sub *subscription) {
	s.subscriptionsMu.Lock()
	defer s.subscriptionsMu.Unlock()
	if current := s.subscriptions[key]; current != sub {
		return
	}
	delete(s.subscriptions, key)
}

func (s *ProjectStreamSocket) cancelSubscription(key subscriptionKey) {
	s.subscriptionsMu.Lock()
	sub, ok := s.subscriptions[key]
	if ok {
		delete(s.subscriptions, key)
	}
	s.subscriptionsMu.Unlock()
	if ok {
		sub.cancel()
	}
}

func (s *ProjectStreamSocket) cancelTrackedSubscription(key subscriptionKey, sub *subscription) {
	s.subscriptionsMu.Lock()
	if current := s.subscriptions[key]; current == sub {
		delete(s.subscriptions, key)
	}
	s.subscriptionsMu.Unlock()
	sub.cancel()
}

func (s *ProjectStreamSocket) cancelAllSubscriptions() {
	s.subscriptionsMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.subscriptions))
	for key, sub := range s.subscriptions {
		cancels = append(cancels, sub.cancel)
		delete(s.subscriptions, key)
	}
	s.subscriptionsMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func requestKey(req ProjectStreamSubscriptionRequest) subscriptionKey {
	return subscriptionKey{stream: strings.TrimSpace(req.Stream), sandboxID: req.SandboxID}
}

func (req ProjectStreamSubscriptionRequest) listEnabled() bool {
	if req.List == nil {
		return !req.Replay
	}
	return *req.List
}

func (req ProjectStreamSubscriptionRequest) replayHistory() bool {
	return req.Replay
}

type sseSendFunc func(event string, data any) error

func runProjectStreamSSE(ctx context.Context, service api.ProjectEventService, input *ProjectStreamSSEInput, send sseSendFunc) {
	stream := strings.TrimSpace(input.Stream)
	if stream == "" {
		stream = streamSandbox
	}
	list := input.List
	req := ProjectStreamSubscriptionRequest{
		Type:       messageTypeSubscribe,
		Stream:     stream,
		SandboxID:  input.SandboxID,
		Replay:     input.History,
		ReplayOnly: input.ReplayOnly,
		List:       &list,
	}
	if req.Stream != streamSandbox {
		_ = send(messageTypeError, StreamErrorEvent{Stream: req.Stream, SandboxID: req.SandboxID, Error: fmt.Sprintf("unsupported stream %q", req.Stream)})
		return
	}

	ch, unsubscribe, err := service.SubscribeProjectEvents(ctx, input.ProjectID)
	if err != nil {
		_ = send(messageTypeError, StreamErrorEvent{Stream: req.Stream, SandboxID: req.SandboxID, Error: err.Error()})
		return
	}
	if unsubscribe == nil {
		unsubscribe = func() {}
	}
	defer unsubscribe()

	cursor, err := sseSubscriptionCursor(ctx, service, input.ProjectID, req)
	if err != nil {
		_ = send(messageTypeError, StreamErrorEvent{Stream: req.Stream, SandboxID: req.SandboxID, Error: err.Error()})
		return
	}
	if err := send(eventConnected, ConnectedEvent{ProjectID: input.ProjectID}); err != nil {
		return
	}
	if req.listEnabled() {
		if !writeSSESandboxList(ctx, service, input.ProjectID, req, cursor, send) {
			return
		}
	}
	if req.Replay {
		events, err := service.ListProjectEventsAfterSeq(ctx, input.ProjectID, cursor, []string{"sandbox"})
		if err != nil {
			_ = send(messageTypeError, StreamErrorEvent{Stream: req.Stream, SandboxID: req.SandboxID, Error: err.Error()})
			return
		}
		for _, event := range events {
			if !matchesSandbox(req, event) {
				continue
			}
			if !sendSSEProjectEvent(req, event, send) {
				return
			}
			cursor = event.Seq
		}
	}
	if req.ReplayOnly {
		_ = send(messageTypeComplete, CompleteEvent{Stream: req.Stream, SandboxID: req.SandboxID})
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				_ = send(messageTypeComplete, CompleteEvent{Stream: req.Stream, SandboxID: req.SandboxID})
				return
			}
			if event.Seq <= cursor || !matchesSandbox(req, event) {
				continue
			}
			if !sendSSEProjectEvent(req, event, send) {
				return
			}
			cursor = event.Seq
		}
	}
}

func sseSubscriptionCursor(ctx context.Context, service api.ProjectEventService, projectID string, req ProjectStreamSubscriptionRequest) (int64, error) {
	if req.Replay {
		return 0, nil
	}
	return service.MaxProjectEventSeq(ctx, projectID)
}

func writeSSESandboxList(ctx context.Context, service api.ProjectEventService, projectID string, req ProjectStreamSubscriptionRequest, seq int64, send sseSendFunc) bool {
	start := ResourceListStartEvent{
		ProjectID: projectID,
		Resources: []string{"sandbox"},
		Seq:       seq,
		StartedAt: time.Now().UTC(),
	}
	if err := send(eventListStart, start); err != nil {
		return false
	}
	events, err := service.ListProjectResourceSnapshots(ctx, projectID, []string{"sandbox"}, seq)
	if err != nil {
		_ = send(messageTypeError, StreamErrorEvent{Stream: req.Stream, SandboxID: req.SandboxID, Error: err.Error()})
		return false
	}
	for _, event := range events {
		if !matchesSandbox(req, event) {
			continue
		}
		if !sendSSEProjectEvent(req, event, send) {
			return false
		}
	}
	finish := ResourceListFinishEvent{
		ProjectID:  projectID,
		Resources:  []string{"sandbox"},
		Seq:        seq,
		FinishedAt: time.Now().UTC(),
	}
	return send(eventListEnd, finish) == nil
}

func sendSSEProjectEvent(req ProjectStreamSubscriptionRequest, event model.ProjectEvent, send sseSendFunc) bool {
	switch event.Type {
	case model.EventTypeResourceListed:
		listed := ResourceListedEvent(event)
		return send(model.EventTypeResourceListed, listed) == nil
	default:
		changed := ResourceChangedEvent(event)
		return send(model.EventTypeResourceChanged, changed) == nil
	}
}

func matchesSandbox(req ProjectStreamSubscriptionRequest, event model.ProjectEvent) bool {
	return event.ResourceType == "sandbox" && (req.SandboxID == "" || event.ResourceID == req.SandboxID)
}

type sseOperationHandler[I any] func(ctx context.Context, input *I, send sseSendFunc)

func registerSSEOperation[I any](humaAPI huma.API, op huma.Operation, eventTypeMap map[string]any, handler sseOperationHandler[I]) {
	if op.Responses == nil {
		op.Responses = map[string]*huma.Response{}
	}
	if op.Responses["200"] == nil {
		op.Responses["200"] = &huma.Response{}
	}
	if op.Responses["200"].Content == nil {
		op.Responses["200"].Content = map[string]*huma.MediaType{}
	}
	eventSchemas := make([]*huma.Schema, 0, len(eventTypeMap))
	events := make([]string, 0, len(eventTypeMap))
	for event := range eventTypeMap {
		events = append(events, event)
	}
	sort.Strings(events)
	for _, event := range events {
		sample := eventTypeMap[event]
		eventSchemas = append(eventSchemas, &huma.Schema{
			Title: "Event " + event,
			Type:  huma.TypeObject,
			Properties: map[string]*huma.Schema{
				"event": {
					Type:        huma.TypeString,
					Description: "The event name.",
					Extensions:  map[string]any{"const": event},
				},
				"data": humaAPI.OpenAPI().Components.Schemas.Schema(reflect.TypeOf(sample), true, event),
			},
			Required: []string{"event", "data"},
		})
	}
	op.Responses["200"].Content["text/event-stream"] = &huma.MediaType{
		Schema: &huma.Schema{
			Title:       "Server Sent Events",
			Description: "Each oneOf object in the array represents one possible SSE message. This stream intentionally omits SSE id fields and does not support Last-Event-ID resume semantics.",
			Type:        huma.TypeArray,
			Items: &huma.Schema{
				Extensions: map[string]any{"oneOf": eventSchemas},
			},
		},
	}

	huma.Register(humaAPI, op, func(ctx context.Context, input *I) (*huma.StreamResponse, error) {
		return &huma.StreamResponse{
			Body: func(hctx huma.Context) {
				hctx.SetHeader("Content-Type", "text/event-stream")
				writer := hctx.BodyWriter()
				flusher, _ := writer.(http.Flusher)
				send := func(event string, data any) error {
					return writeSSE(writer, flusher, event, data)
				}
				handler(hctx.Context(), input, send)
			},
		}, nil
	})
}

func writeSSE(writer io.Writer, flusher http.Flusher, event string, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "event: %s\n", event); err != nil {
		return err
	}
	if _, err := writer.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err := writer.Write(payload); err != nil {
		return err
	}
	if _, err := writer.Write([]byte("\n\n")); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}
