package store

import (
	"context"

	"gorm.io/gorm"
)

func (s *Store) Transaction(ctx context.Context, fn func(txStore *Store, txDB *gorm.DB) error) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Transaction(func(tx *gorm.DB) error {
		return fn(s.withTx(tx, tx), tx)
	})
}
