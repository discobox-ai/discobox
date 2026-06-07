package store

import (
	"context"

	"gorm.io/gorm"

	"github.com/obot-platform/disco2/internal/model"
)

func (s *Store) Transaction(ctx context.Context, fn func(txStore *Store, txDB *gorm.DB) error) error {
	var events []model.ProjectEvent
	if err := s.write.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		txStore := &Store{
			write:             tx,
			read:              tx,
			publisher:         s.publisher,
			afterCommitEvents: &events,
			sealer:            s.sealer,
		}
		return fn(txStore, tx)
	}); err != nil {
		return err
	}
	for _, event := range events {
		s.publishProjectEvent(ctx, event)
	}
	return nil
}
