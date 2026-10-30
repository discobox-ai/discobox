package store

import (
	"context"

	"gorm.io/gorm"
)

// Transaction runs fn in one write transaction. Called on a store already
// bound to one, it runs fn in a savepoint of it.
//
// The project events fn's writes queue are appended when the outermost
// transaction's fn returns, as its last statements before commit
// (project_events.go); a savepoint that rolls back takes its own events with
// it.
func (s *Store) Transaction(ctx context.Context, fn func(txStore *Store, txDB *gorm.DB) error) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	if s.events != nil {
		queue := s.events
		return write.Transaction(func(tx *gorm.DB) error {
			queued := len(queue.events)
			if err := fn(s.withTx(tx, tx, queue), tx); err != nil {
				queue.events = queue.events[:queued]
				return err
			}
			return nil
		})
	}
	queue := &eventQueue{}
	return write.Transaction(func(tx *gorm.DB) error {
		if err := fn(s.withTx(tx, tx, queue), tx); err != nil {
			return err
		}
		return appendProjectEventsTx(ctx, tx, queue.events)
	})
}
