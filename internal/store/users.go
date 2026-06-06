package store

import (
	"context"

	"github.com/obot-platform/disco2/internal/model"
)

func (s *Store) UpsertUser(ctx context.Context, user *model.User) error {
	return s.write.WithContext(ctx).Save(user).Error
}
