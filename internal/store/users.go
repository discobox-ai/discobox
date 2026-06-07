package store

import (
	"context"

	"github.com/obot-platform/disco2/internal/model"
	"gorm.io/gorm/clause"
)

func (s *Store) UpsertUser(ctx context.Context, user *model.User) error {
	return s.write.WithContext(ctx).Save(user).Error
}

func (s *Store) GetUserByID(ctx context.Context, userID string) (*model.User, error) {
	var user model.User
	if err := s.read.WithContext(ctx).Where("id = ?", userID).First(&user).Error; err != nil {
		return nil, mapNotFound(err)
	}
	return &user, nil
}

func (s *Store) GetProjectUserKey(ctx context.Context, projectID, userID string) (*model.ProjectUserKey, error) {
	var key model.ProjectUserKey
	if err := s.read.WithContext(ctx).Where("project_id = ? AND user_id = ?", projectID, userID).First(&key).Error; err != nil {
		return nil, mapNotFound(err)
	}
	return &key, nil
}

func (s *Store) CreateProjectUserKeyIfMissing(ctx context.Context, key *model.ProjectUserKey) (bool, error) {
	result := s.write.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(key)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}
