package events

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/obot-platform/discobox/id"
	"github.com/obot-platform/discobox/server/internal/apperrors"
	eventbroker "github.com/obot-platform/discobox/server/internal/events"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/store"
)

type Service struct {
	store  *store.Store
	broker *eventbroker.Broker
}

func NewService(store *store.Store, broker *eventbroker.Broker) *Service {
	return &Service{store: store, broker: broker}
}

func (s *Service) MaxProjectEventSeq(ctx context.Context, projectID string) (int64, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return 0, apiError(err, "project not found")
	}
	return s.store.MaxProjectEventSeq(ctx, projectID)
}

func apiError(err error, notFoundMessage string) error {
	if errors.Is(err, store.ErrNotFound) {
		return apperrors.NewStatusError(http.StatusNotFound, notFoundMessage)
	}
	return err
}

func (s *Service) ListProjectEventsAfterSeq(ctx context.Context, projectID string, afterSeq int64, resourceTypes []string) ([]model.ProjectEvent, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apiError(err, "project not found")
	}
	return s.store.ListProjectEventsAfterSeq(ctx, projectID, afterSeq, resourceTypes)
}

func (s *Service) ListProjectResourceSnapshots(ctx context.Context, projectID string, resourceTypes []string, seq int64) ([]model.ProjectEvent, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apiError(err, "project not found")
	}

	var result []model.ProjectEvent
	for _, resourceType := range resourceTypes {
		switch resourceType {
		case "sandbox":
			sandboxes, err := s.store.ListSandboxSnapshots(ctx, projectID)
			if err != nil {
				return nil, err
			}
			for _, sandbox := range sandboxes {
				event, err := snapshotEvent(projectID, "sandbox", sandbox.ID, seq, sandbox)
				if err != nil {
					return nil, err
				}
				result = append(result, event)
			}
		case "agentConfig":
			configs, err := s.store.ListAgentConfigSnapshots(ctx, projectID)
			if err != nil {
				return nil, err
			}
			for _, config := range configs {
				event, err := snapshotEvent(projectID, "agentConfig", config.ID, seq, config)
				if err != nil {
					return nil, err
				}
				result = append(result, event)
			}
		}
	}
	return result, nil
}

func (s *Service) SubscribeProjectEvents(ctx context.Context, projectID string) (<-chan model.ProjectEvent, func(), error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, nil, apiError(err, "project not found")
	}
	if s.broker == nil {
		return nil, nil, nil
	}
	ch, unsubscribe := s.broker.Subscribe(ctx, projectID)
	return ch, unsubscribe, nil
}

func snapshotEvent(projectID, resourceType, resourceID string, seq int64, resource any) (model.ProjectEvent, error) {
	payload, err := json.Marshal(resource)
	if err != nil {
		return model.ProjectEvent{}, err
	}
	return model.ProjectEvent{
		ID:           id.NewString(),
		Seq:          seq,
		ProjectID:    projectID,
		Type:         model.EventTypeResourceListed,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Action:       model.EventActionListed,
		Data:         payload,
		CreatedAt:    time.Now().UTC(),
	}, nil
}
