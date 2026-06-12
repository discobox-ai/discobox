package store

import (
	"context"

	"github.com/obot-platform/discobox/internal/model"
)

func (s *Store) MaxProjectEventSeq(ctx context.Context, projectID string) (int64, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return 0, err
	}
	var seq *int64
	err = read.
		Model(&model.ProjectEvent{}).
		Where("project_id = ?", projectID).
		Select("MAX(seq)").
		Scan(&seq).Error
	if err != nil || seq == nil {
		return 0, err
	}
	return *seq, nil
}

func (s *Store) ListProjectEventsAfterSeq(ctx context.Context, projectID string, afterSeq int64, resourceTypes []string) ([]model.ProjectEvent, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var events []model.ProjectEvent
	query := read.
		Where("project_id = ? AND seq > ?", projectID, afterSeq).
		Order("seq ASC")
	if len(resourceTypes) > 0 {
		query = query.Where("resource_type IN ?", resourceTypes)
	}
	err = query.Find(&events).Error
	return events, err
}

func (s *Store) publishProjectEvent(ctx context.Context, event model.ProjectEvent) {
	if s.afterCommitEvents != nil {
		*s.afterCommitEvents = append(*s.afterCommitEvents, event)
		return
	}
	if s.publisher != nil {
		s.publisher.PublishProjectEvent(ctx, event)
	}
}
