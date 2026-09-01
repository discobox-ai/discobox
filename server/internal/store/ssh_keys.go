package store

import (
	"context"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/model"
)

func (s *Store) CreateSSHKey(ctx context.Context, key *model.SSHKey) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Create(key).Error
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

// DeleteSSHKey removes an enrolled key. The lookup and the delete share a
// transaction because keyID may be an ID prefix (firstByID), so the row the
// delete removes has to be the row the lookup resolved.
func (s *Store) DeleteSSHKey(ctx context.Context, projectID, keyID string) error {
	return s.Transaction(ctx, func(_ *Store, tx *gorm.DB) error {
		key, err := firstByID[model.SSHKey](tx.Where("project_id = ?", projectID), "id", keyID)
		if err != nil {
			return err
		}
		return tx.Delete(key).Error
	})
}
