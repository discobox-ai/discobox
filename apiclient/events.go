package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/obot-platform/discobox/model"
)

const (
	ProjectEventNameResourceChanged = model.EventTypeResourceChanged
	ProjectEventNameResourceListed  = model.EventTypeResourceListed
	ProjectEventNameListStart       = "list-start"
	ProjectEventNameListFinish      = "list-end"
)

// EventClient handles API operations that ogen cannot generate, such as WebSockets.
type EventClient struct {
	serverURL *url.URL
	client    *http.Client
}

type EventClientOption func(*EventClient)

func WithHTTPClient(client *http.Client) EventClientOption {
	return func(c *EventClient) {
		if client != nil {
			c.client = client
		}
	}
}

func NewEventClient(serverURL string, opts ...EventClientOption) (*EventClient, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, err
	}
	trimTrailingSlashes(u)

	c := &EventClient{
		serverURL: u,
		client:    http.DefaultClient,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

type ProjectEventsParams struct {
	History   *bool
	ListOnly  *bool
	SandboxID string
}

func (c *EventClient) SubscribeProjectEvents(ctx context.Context, projectID string, params ProjectEventsParams) (*ProjectEventStream, error) {
	u := c.serverURL.JoinPath("projects", projectID, "stream")
	u.Scheme = websocketScheme(u.Scheme)

	conn, resp, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPClient: c.client})
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("connect project event stream: %w", err)
	}

	stream := &ProjectEventStream{ctx: ctx, conn: conn}
	req := projectStreamSubscriptionRequest{
		Type:      "subscribe",
		Stream:    "sandbox",
		SandboxID: params.SandboxID,
		ListOnly:  boolValue(params.ListOnly),
		History:   params.History,
	}
	if err := wsjson.Write(ctx, conn, req); err != nil {
		if closeErr := conn.CloseNow(); closeErr != nil {
			return nil, fmt.Errorf("close project event stream after subscribe failure: %w", closeErr)
		}
		return nil, fmt.Errorf("subscribe project event stream: %w", err)
	}
	return stream, nil
}

type ProjectEventStream struct {
	ctx  context.Context
	conn *websocket.Conn
}

func (s *ProjectEventStream) Close() error {
	if s == nil || s.conn == nil {
		return nil
	}
	return s.conn.Close(websocket.StatusNormalClosure, "done")
}

func (s *ProjectEventStream) Read() (*ProjectEventMessage, error) {
	for {
		var message projectStreamSocketMessage
		if err := wsjson.Read(s.ctx, s.conn, &message); err != nil {
			if closeStatus := websocket.CloseStatus(err); closeStatus == websocket.StatusNormalClosure || closeStatus == websocket.StatusGoingAway {
				return nil, io.EOF
			}
			return nil, err
		}
		switch message.Type {
		case "subscribed", "unsubscribed":
			continue
		case "complete":
			return nil, io.EOF
		case "error":
			return nil, errors.New(message.Error)
		case "event":
			if message.Event == "connected" {
				continue
			}
			return decodeProjectEventMessage(message)
		}
	}
}

type ProjectEventMessage struct {
	Event string           `json:"event,omitempty"`
	Data  ProjectEventData `json:"data,omitempty"`
}

type ProjectEventData interface {
	projectEventData()
}

type ResourceChangedEvent model.ProjectEvent

func (*ResourceChangedEvent) projectEventData() {}

type ResourceListedEvent model.ProjectEvent

func (*ResourceListedEvent) projectEventData() {}

type ResourceListStartEvent struct {
	ProjectID string    `json:"projectId"`
	Resources []string  `json:"resources"`
	Seq       int64     `json:"seq"`
	StartedAt time.Time `json:"startedAt"`
}

func (*ResourceListStartEvent) projectEventData() {}

type ResourceListFinishEvent struct {
	ProjectID  string    `json:"projectId"`
	Resources  []string  `json:"resources"`
	Seq        int64     `json:"seq"`
	FinishedAt time.Time `json:"finishedAt"`
}

func (*ResourceListFinishEvent) projectEventData() {}

type UnknownProjectEvent struct {
	Data json.RawMessage
}

func (*UnknownProjectEvent) projectEventData() {}

type projectStreamSubscriptionRequest struct {
	Type      string `json:"type"`
	Stream    string `json:"stream"`
	SandboxID string `json:"sandboxId,omitempty"`
	ListOnly  bool   `json:"listOnly,omitempty"`
	History   *bool  `json:"history,omitempty"`
}

type projectStreamSocketMessage struct {
	Type      string          `json:"type"`
	Stream    string          `json:"stream,omitempty"`
	SandboxID string          `json:"sandboxId,omitempty"`
	Event     string          `json:"event,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Seq       int64           `json:"seq,omitempty"`
	Error     string          `json:"error,omitempty"`
}

func decodeProjectEventMessage(message projectStreamSocketMessage) (*ProjectEventMessage, error) {
	msg := &ProjectEventMessage{Event: message.Event}
	switch message.Event {
	case ProjectEventNameResourceChanged:
		var data ResourceChangedEvent
		if err := json.Unmarshal(message.Data, &data); err != nil {
			return nil, fmt.Errorf("decode %s event: %w", message.Event, err)
		}
		msg.Data = &data
	case ProjectEventNameResourceListed:
		var data ResourceListedEvent
		if err := json.Unmarshal(message.Data, &data); err != nil {
			return nil, fmt.Errorf("decode %s event: %w", message.Event, err)
		}
		msg.Data = &data
	case ProjectEventNameListStart:
		var data ResourceListStartEvent
		if err := json.Unmarshal(message.Data, &data); err != nil {
			return nil, fmt.Errorf("decode %s event: %w", message.Event, err)
		}
		msg.Data = &data
	case ProjectEventNameListFinish:
		var data ResourceListFinishEvent
		if err := json.Unmarshal(message.Data, &data); err != nil {
			return nil, fmt.Errorf("decode %s event: %w", message.Event, err)
		}
		msg.Data = &data
	default:
		msg.Data = &UnknownProjectEvent{Data: append(json.RawMessage(nil), message.Data...)}
	}
	return msg, nil
}

func websocketScheme(scheme string) string {
	switch scheme {
	case "https", "wss":
		return "wss"
	default:
		return "ws"
	}
}

func boolValue(value *bool) bool {
	return value != nil && *value
}

func trimTrailingSlashes(u *url.URL) {
	u.Path = strings.TrimRight(u.Path, "/")
}
