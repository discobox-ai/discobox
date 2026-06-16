package store

import (
	"context"

	"gorm.io/gorm"

	"github.com/obot-platform/discobox/model"
)

func (s *Store) Transaction(ctx context.Context, fn func(txStore *Store, txDB *gorm.DB) error) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	var events []model.ProjectEvent
	if err := write.Transaction(func(tx *gorm.DB) error {
		txStore := s.withTx(tx, tx, &events)
		return fn(txStore, tx)
	}); err != nil {
		return err
	}
	for _, event := range events {
		s.publishProjectEvent(ctx, event)
	}
	return nil
}
