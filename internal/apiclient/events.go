package apiclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	ProjectEventNameResourceChanged = "resourceChanged"
	ProjectEventNameResourceListed  = "resourceListed"
	ProjectEventNameListStart       = "listStart"
	ProjectEventNameListFinish      = "listFinish"
)

// EventClient handles API operations that ogen cannot generate, such as SSE.
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
	Resources  []string
	AfterSeq   *int64
	List       *bool
	ReplayOnly *bool
}

func (c *EventClient) SubscribeProjectEvents(ctx context.Context, projectID uuid.UUID, params ProjectEventsParams) (*ProjectEventStream, error) {
	u := c.serverURL.JoinPath("projects", projectID.String(), "events")
	q := u.Query()
	for _, resource := range params.Resources {
		q.Add("resources", resource)
	}
	if params.AfterSeq != nil {
		q.Set("afterSeq", strconv.FormatInt(*params.AfterSeq, 10))
	}
	if params.List != nil {
		q.Set("list", strconv.FormatBool(*params.List))
	}
	if params.ReplayOnly != nil {
		q.Set("replayOnly", strconv.FormatBool(*params.ReplayOnly))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("subscribe project events: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return &ProjectEventStream{
		resp:   resp,
		reader: bufio.NewReader(resp.Body),
	}, nil
}

type ProjectEventStream struct {
	resp   *http.Response
	reader *bufio.Reader
}

func (s *ProjectEventStream) Close() error {
	if s == nil || s.resp == nil || s.resp.Body == nil {
		return nil
	}
	return s.resp.Body.Close()
}

func (s *ProjectEventStream) Read() (*ProjectEventMessage, error) {
	for {
		frame, err := readSSEFrame(s.reader)
		if err != nil {
			return nil, err
		}
		if frame == nil {
			continue
		}
		return decodeProjectEventMessage(*frame)
	}
}

type ProjectEventMessage struct {
	ID    string
	Event string
	Retry *time.Duration
	Data  ProjectEventData
}

type ProjectEventData interface {
	projectEventData()
}

type ResourceChangedEvent struct {
	ID           uuid.UUID       `json:"id"`
	Seq          int64           `json:"seq"`
	ProjectID    uuid.UUID       `json:"projectId"`
	Type         string          `json:"type"`
	ResourceType string          `json:"resourceType"`
	ResourceID   string          `json:"resourceId"`
	Action       string          `json:"action"`
	Data         json.RawMessage `json:"data"`
	CreatedAt    time.Time       `json:"createdAt"`
}

func (*ResourceChangedEvent) projectEventData() {}

type ResourceListedEvent ResourceChangedEvent

func (*ResourceListedEvent) projectEventData() {}

type ResourceListStartEvent struct {
	ProjectID uuid.UUID `json:"projectId"`
	Resources []string  `json:"resources"`
	Seq       int64     `json:"seq"`
	StartedAt time.Time `json:"startedAt"`
}

func (*ResourceListStartEvent) projectEventData() {}

type ResourceListFinishEvent struct {
	ProjectID  uuid.UUID `json:"projectId"`
	Resources  []string  `json:"resources"`
	Seq        int64     `json:"seq"`
	FinishedAt time.Time `json:"finishedAt"`
}

func (*ResourceListFinishEvent) projectEventData() {}

type UnknownProjectEvent struct {
	Data json.RawMessage
}

func (*UnknownProjectEvent) projectEventData() {}

type sseFrame struct {
	id    string
	event string
	data  []byte
	retry *time.Duration
}

func readSSEFrame(reader *bufio.Reader) (*sseFrame, error) {
	var frame sseFrame
	hasFields := false

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && hasFields {
				return &frame, nil
			}
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if hasFields {
				if len(frame.data) > 0 {
					frame.data = frame.data[:len(frame.data)-1]
				}
				return &frame, nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		hasFields = true

		name, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")

		switch name {
		case "id":
			frame.id = value
		case "event":
			frame.event = value
		case "data":
			frame.data = append(frame.data, value...)
			frame.data = append(frame.data, '\n')
		case "retry":
			ms, err := strconv.Atoi(value)
			if err == nil {
				retry := time.Duration(ms) * time.Millisecond
				frame.retry = &retry
			}
		}
	}
}

func decodeProjectEventMessage(frame sseFrame) (*ProjectEventMessage, error) {
	msg := &ProjectEventMessage{
		ID:    frame.id,
		Event: frame.event,
		Retry: frame.retry,
	}

	switch frame.event {
	case ProjectEventNameResourceChanged:
		var data ResourceChangedEvent
		if err := json.Unmarshal(frame.data, &data); err != nil {
			return nil, fmt.Errorf("decode %s event: %w", frame.event, err)
		}
		msg.Data = &data
	case ProjectEventNameResourceListed:
		var data ResourceListedEvent
		if err := json.Unmarshal(frame.data, &data); err != nil {
			return nil, fmt.Errorf("decode %s event: %w", frame.event, err)
		}
		msg.Data = &data
	case ProjectEventNameListStart:
		var data ResourceListStartEvent
		if err := json.Unmarshal(frame.data, &data); err != nil {
			return nil, fmt.Errorf("decode %s event: %w", frame.event, err)
		}
		msg.Data = &data
	case ProjectEventNameListFinish:
		var data ResourceListFinishEvent
		if err := json.Unmarshal(frame.data, &data); err != nil {
			return nil, fmt.Errorf("decode %s event: %w", frame.event, err)
		}
		msg.Data = &data
	default:
		msg.Data = &UnknownProjectEvent{Data: append(json.RawMessage(nil), frame.data...)}
	}

	return msg, nil
}

func trimTrailingSlashes(u *url.URL) {
	u.Path = strings.TrimRight(u.Path, "/")
}
