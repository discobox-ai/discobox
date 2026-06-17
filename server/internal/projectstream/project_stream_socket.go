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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
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
	Type      string `json:"type"`
	Stream    string `json:"stream"`
	SandboxID string `json:"sandboxId,omitempty"`
	ListOnly  bool   `json:"listOnly,omitempty"`
	History   *bool  `json:"history,omitempty"`
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
	ProjectID string `path:"projectId" doc:"Project ID"`
	Stream    string `query:"stream" enum:"sandbox" default:"sandbox" doc:"Stream name"`
	SandboxID string `query:"sandboxId,omitempty" doc:"Sandbox ID to stream; defaults to all sandboxes"`
	ListOnly  bool   `query:"listOnly,omitempty" doc:"Return after the current resource list instead of waiting for live events"`
	History   bool   `query:"history" default:"true" doc:"Send current resource data before live changes; defaults to true"`
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

// RegisterProjectStreamSSERoutes registers the OpenAPI-documented static
// project stream subscription endpoint. Reconnecting clients may request the
// current resource list before receiving live detail events.
func RegisterProjectStreamSSERoutes(router chi.Router, service api.ProjectEventService) {
	router.Get("/projects/{projectId}/stream/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		send := func(event string, data any) error {
			return writeSSE(w, flusher, event, data)
		}
		input := projectStreamSSEInputFromRequest(r)
		if service == nil {
			_ = send(messageTypeError, StreamErrorEvent{Stream: input.Stream, SandboxID: input.SandboxID, Error: "project event service is not configured"})
			return
		}
		runProjectStreamSSE(r.Context(), service, input, send)
	})
}

func projectStreamSSEInputFromRequest(r *http.Request) *ProjectStreamSSEInput {
	query := r.URL.Query()
	return &ProjectStreamSSEInput{
		ProjectID: chi.URLParam(r, "projectId"),
		Stream:    query.Get("stream"),
		SandboxID: query.Get("sandboxId"),
		ListOnly:  queryBool(query.Get("listOnly"), false),
		History:   queryBool(query.Get("history"), true),
	}
}

func queryBool(value string, fallback bool) bool {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
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
	cursor, err := s.service.MaxProjectEventSeq(streamCtx, s.projectID)
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

	if req.historyEnabled() && !s.writeSandboxList(streamCtx, key, sub, req, cursor) {
		cleanup()
		return
	}

	if req.ListOnly {
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

func (req ProjectStreamSubscriptionRequest) historyEnabled() bool {
	if req.History == nil {
		return true
	}
	return *req.History
}

type sseSendFunc func(event string, data any) error

func runProjectStreamSSE(ctx context.Context, service api.ProjectEventService, input *ProjectStreamSSEInput, send sseSendFunc) {
	stream := strings.TrimSpace(input.Stream)
	if stream == "" {
		stream = streamSandbox
	}
	history := input.History
	req := ProjectStreamSubscriptionRequest{
		Type:      messageTypeSubscribe,
		Stream:    stream,
		SandboxID: input.SandboxID,
		ListOnly:  input.ListOnly,
		History:   &history,
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

	cursor, err := service.MaxProjectEventSeq(ctx, input.ProjectID)
	if err != nil {
		_ = send(messageTypeError, StreamErrorEvent{Stream: req.Stream, SandboxID: req.SandboxID, Error: err.Error()})
		return
	}
	if err := send(eventConnected, ConnectedEvent{ProjectID: input.ProjectID}); err != nil {
		return
	}
	if req.historyEnabled() {
		if !writeSSESandboxList(ctx, service, input.ProjectID, req, cursor, send) {
			return
		}
	}
	if req.ListOnly {
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
