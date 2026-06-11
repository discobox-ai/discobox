package store

import (
	"context"

	"github.com/obot-platform/disco2/internal/model"
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

func (s *Store) GetProjectUserKey(ctx context.Context, projectID, userID string) (*model.ProjectUserKey, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var key model.ProjectUserKey
	if err := read.Where("project_id = ? AND user_id = ?", projectID, userID).First(&key).Error; err != nil {
		return nil, mapNotFound(err)
	}
	return &key, nil
}

func (s *Store) CreateProjectUserKeyIfMissing(ctx context.Context, key *model.ProjectUserKey) (bool, error) {
	write, err := s.getWrite(ctx)
	if err != nil {
		return false, err
	}
	result := write.
		Where("project_id = ? AND user_id = ?", key.ProjectID, key.UserID).
		FirstOrCreate(key)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}
