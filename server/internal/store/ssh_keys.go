package store

import (
	"context"

	"gorm.io/gorm"

	"github.com/obot-platform/discobox/server/internal/model"
)

func (s *Store) CreateSSHKey(ctx context.Context, key *model.SSHKey) error {
	created, err := withResourceEvent(ctx, s, model.EventActionCreated, func(tx *gorm.DB) (*model.SSHKey, error) {
		if err := tx.Create(key).Error; err != nil {
			return nil, err
		}
		return key, nil
	})
	if err != nil {
		return err
	}
	*key = *created
	return nil
}

func (s *Store) GetSSHKey(ctx context.Context, projectID, keyID string) (*model.SSHKey, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.SSHKey](read.Where("project_id = ?", projectID), "id", keyID)
}

// ListSSHKeys returns a project's enrolled SSH keys.
func (s *Store) ListSSHKeys(ctx context.Context, projectID string) ([]model.SSHKey, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.SSHKey
	err = read.Where("project_id = ?", projectID).Order("created_at ASC").Find(&out).Error
	return out, err
}

func (s *Store) DeleteSSHKey(ctx context.Context, projectID, keyID string) error {
	_, err := withResourceEvent(ctx, s, model.EventActionDeleted, func(tx *gorm.DB) (*model.SSHKey, error) {
		key, err := firstByID[model.SSHKey](tx.Where("project_id = ?", projectID), "id", keyID)
		if err != nil {
			return nil, err
		}
		if err := tx.Delete(key).Error; err != nil {
			return nil, err
		}
		return key, nil
	})
	return err
}
