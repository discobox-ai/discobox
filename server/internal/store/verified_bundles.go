package store

import (
	"context"

	"github.com/discobox-ai/discobox/server/internal/model"
	"gorm.io/gorm/clause"
)

// HasVerifiedBundle reports whether this image passed a full whiteout check.
func (s *Store) HasVerifiedBundle(ctx context.Context, digest string) (bool, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return false, err
	}
	var count int64
	err = read.Model(&model.VerifiedBundle{}).Where("digest = ?", digest).Count(&count).Error
	return count != 0, err
}

// RecordVerifiedBundle records success only; concurrent verification is harmless.
func (s *Store) RecordVerifiedBundle(ctx context.Context, digest string) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.VerifiedBundle{Digest: digest}).Error
}
