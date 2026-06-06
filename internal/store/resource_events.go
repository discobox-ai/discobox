package store

import (
	"context"
	"encoding/json"

	"gorm.io/gorm"

	"github.com/obot-platform/disco2/internal/model"
)

type eventResource interface {
	EventProjectID() string
	EventResourceType() string
	EventResourceID() string
}

func withResourceEvent[T eventResource](ctx context.Context, s *Store, action string, mutate func(tx *gorm.DB) (T, error)) (T, error) {
	var zero T
	var result T
	var event model.ProjectEvent
	if err := s.write.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		result, err = mutate(tx)
		if err != nil {
			return err
		}
		event, err = createProjectEvent(ctx, tx, action, result)
		return err
	}); err != nil {
		return zero, err
	}
	s.publishProjectEvent(ctx, event)
	return result, nil
}

func createProjectEvent[T eventResource](ctx context.Context, tx *gorm.DB, action string, resource T) (model.ProjectEvent, error) {
	payload, err := json.Marshal(resource)
	if err != nil {
		return model.ProjectEvent{}, err
	}
	event := model.ProjectEvent{
		ProjectID:    resource.EventProjectID(),
		Type:         model.EventTypeResourceChanged,
		ResourceType: resource.EventResourceType(),
		ResourceID:   resource.EventResourceID(),
		Action:       action,
		Data:         payload,
	}
	if err := tx.WithContext(ctx).Create(&event).Error; err != nil {
		return model.ProjectEvent{}, err
	}
	return event, nil
}
