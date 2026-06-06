package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/sse"

	"github.com/obot-platform/disco2/internal/model"
)

type ProjectEventsInput struct {
	ProjectID  string   `path:"projectId" doc:"Project ID" format:"uuid"`
	Resources  []string `query:"resources" doc:"Resource types to watch. Repeat the parameter or use comma-separated values. Defaults to sandbox."`
	AfterSeq   int64    `query:"afterSeq" default:"-1" doc:"Replay changes after this sequence. Omit for list-watch baseline behavior. A value of 0 starts at the current max sequence." minimum:"-1"`
	List       bool     `query:"list" doc:"When afterSeq is omitted, send a full list of requested resources before live changes"`
	ReplayOnly bool     `query:"replayOnly" doc:"Return after replay/list instead of waiting for live events"`
}

type ResourceChangedEvent model.ProjectEvent

type ResourceListedEvent model.ProjectEvent

type ResourceListStartEvent struct {
	ProjectID string    `json:"projectId" doc:"Project ID" format:"uuid"`
	Resources []string  `json:"resources" doc:"Resource types included in the list"`
	Seq       int64     `json:"seq" doc:"Sequence used as the list baseline" minimum:"0"`
	StartedAt time.Time `json:"startedAt" doc:"List start timestamp" format:"date-time"`
}

type ResourceListFinishEvent struct {
	ProjectID  string    `json:"projectId" doc:"Project ID" format:"uuid"`
	Resources  []string  `json:"resources" doc:"Resource types included in the list"`
	Seq        int64     `json:"seq" doc:"Sequence used as the list baseline" minimum:"0"`
	FinishedAt time.Time `json:"finishedAt" doc:"List finish timestamp" format:"date-time"`
}

// RegisterProjectEventOperations registers project-scoped event subscriptions.
func RegisterProjectEventOperations(api huma.API, service ProjectEventService) {
	sse.Register(api, huma.Operation{
		OperationID: "subscribe-project-events",
		Method:      http.MethodGet,
		Path:        "/projects/{projectId}/events",
		Summary:     "Subscribe to project events",
		Tags:        []string{"Project Events"},
	}, map[string]any{
		"resourceChanged": ResourceChangedEvent{},
		"resourceListed":  ResourceListedEvent{},
		"listStart":       ResourceListStartEvent{},
		"listFinish":      ResourceListFinishEvent{},
	}, func(ctx context.Context, input *ProjectEventsInput, send sse.Sender) {
		if service == nil {
			return
		}

		resourceTypes := normalizeResourceTypes(input.Resources)

		var ch <-chan model.ProjectEvent
		var unsubscribe func()
		if !input.ReplayOnly {
			var err error
			ch, unsubscribe, err = service.SubscribeProjectEvents(ctx, input.ProjectID)
			if err != nil {
				return
			}
			if unsubscribe != nil {
				defer unsubscribe()
			}
		}

		cursor, err := subscriptionCursor(ctx, service, input.ProjectID, input.AfterSeq)
		if err != nil {
			return
		}
		if input.AfterSeq < 0 && input.List {
			if err := sendList(ctx, service, send, input.ProjectID, resourceTypes, cursor); err != nil {
				return
			}
		}
		if input.AfterSeq > 0 {
			events, err := service.ListProjectEventsAfterSeq(ctx, input.ProjectID, input.AfterSeq, resourceTypes)
			if err != nil {
				return
			}
			for _, event := range events {
				if err := sendProjectEvent(send, event); err != nil {
					return
				}
				cursor = event.Seq
			}
		}
		if input.ReplayOnly {
			return
		}
		if ch == nil {
			<-ctx.Done()
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-ch:
				if !ok {
					return
				}
				if event.Seq <= cursor || !watchResource(resourceTypes, event.ResourceType) {
					continue
				}
				if err := sendProjectEvent(send, event); err != nil {
					return
				}
				cursor = event.Seq
			}
		}
	})
}

func subscriptionCursor(ctx context.Context, service ProjectEventService, projectID string, afterSeq int64) (int64, error) {
	if afterSeq > 0 {
		return afterSeq, nil
	}
	return service.MaxProjectEventSeq(ctx, projectID)
}

func sendList(ctx context.Context, service ProjectEventService, send sse.Sender, projectID string, resourceTypes []string, seq int64) error {
	start := ResourceListStartEvent{
		ProjectID: projectID,
		Resources: resourceTypes,
		Seq:       seq,
		StartedAt: time.Now().UTC(),
	}
	if err := send(sse.Message{ID: int(seq), Data: start}); err != nil {
		return err
	}

	events, err := service.ListProjectResourceSnapshots(ctx, projectID, resourceTypes, seq)
	if err != nil {
		return err
	}
	for _, event := range events {
		if err := send(sse.Message{ID: int(seq), Data: ResourceListedEvent(event)}); err != nil {
			return err
		}
	}

	finish := ResourceListFinishEvent{
		ProjectID:  projectID,
		Resources:  resourceTypes,
		Seq:        seq,
		FinishedAt: time.Now().UTC(),
	}
	return send(sse.Message{ID: int(seq), Data: finish})
}

func sendProjectEvent(send sse.Sender, event model.ProjectEvent) error {
	return send(sse.Message{ID: int(event.Seq), Data: ResourceChangedEvent(event)})
}

func normalizeResourceTypes(values []string) []string {
	var result []string
	seen := map[string]bool{}
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part == "" || seen[part] {
				continue
			}
			seen[part] = true
			result = append(result, part)
		}
	}
	if len(result) == 0 {
		return []string{"sandbox"}
	}
	return result
}

func watchResource(resourceTypes []string, resourceType string) bool {
	for _, candidate := range resourceTypes {
		if candidate == resourceType {
			return true
		}
	}
	return false
}
