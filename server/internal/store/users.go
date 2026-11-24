package store

import (
	"context"

	"github.com/discobox-ai/discobox/server/internal/model"
)

func (s *Store) UpsertUser(ctx context.Context, user *model.User) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Save(user).Error
}

func (s *Store) GetUser(ctx context.Context, userID string) (*model.User, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var user model.User
	if err := read.Where("id = ?", userID).First(&user).Error; err != nil {
		return nil, mapNotFound(err)
	}
	return &user, nil
}
