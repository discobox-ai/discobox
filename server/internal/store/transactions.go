package store

import (
	"context"

	"gorm.io/gorm"
)

// Transaction runs fn in one write transaction. The outermost transaction
// records the sandbox events of what fn changed just before it commits
// (sandboxEventBatch); one begun inside it is a savepoint, and its changes are
// recorded with the outermost's.
func (s *Store) Transaction(ctx context.Context, fn func(txStore *Store, txDB *gorm.DB) error) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Transaction(func(tx *gorm.DB) error {
		txStore := s.withTx(tx, tx)
		if s.sandboxEvents != nil {
			txStore.sandboxEvents = s.sandboxEvents
			return fn(txStore, tx)
		}
		txStore.sandboxEvents = &sandboxEventBatch{}
		if err := fn(txStore, tx); err != nil {
			return err
		}
		return txStore.sandboxEvents.record(tx)
	})
}
