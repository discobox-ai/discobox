package store

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/obot-platform/discobox/server/internal/model"
)

// UpsertHarnessConfigSecretBinding creates or replaces the binding for a harness
// config's environment variable.
func (s *Store) UpsertHarnessConfigSecretBinding(ctx context.Context, binding *model.HarnessConfigSecretBinding) error {
	_, err := withResourceEvent(ctx, s, model.EventActionUpdated, func(tx *gorm.DB) (*model.HarnessConfigSecretBinding, error) {
		if binding.ID == "" {
			if err := binding.BeforeCreate(nil); err != nil {
				return nil, err
			}
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "harness_config_id"}, {Name: "env_name"}},
			DoUpdates: clause.AssignmentColumns([]string{"secret_id", "updated_at", "deleted_at"}),
		}).Create(binding).Error; err != nil {
			return nil, err
		}
		return binding, nil
	})
	return err
}

// ListHarnessConfigSecretBindings returns a harness config's secret bindings.
func (s *Store) ListHarnessConfigSecretBindings(ctx context.Context, projectID, harnessConfigID string) ([]model.HarnessConfigSecretBinding, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.HarnessConfigSecretBinding
	err = read.Where("project_id = ? AND harness_config_id = ?", projectID, harnessConfigID).
		Order("env_name ASC").Find(&out).Error
	return out, err
}

// DeleteHarnessConfigSecretBinding removes the binding for a harness config's
// environment variable.
func (s *Store) DeleteHarnessConfigSecretBinding(ctx context.Context, projectID, harnessConfigID, envName string) error {
	_, err := withResourceEvent(ctx, s, model.EventActionDeleted, func(tx *gorm.DB) (*model.HarnessConfigSecretBinding, error) {
		var binding model.HarnessConfigSecretBinding
		if err := tx.Where("project_id = ? AND harness_config_id = ? AND env_name = ?", projectID, harnessConfigID, envName).
			First(&binding).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, ErrNotFound
			}
			return nil, err
		}
		if err := tx.Delete(&binding).Error; err != nil {
			return nil, err
		}
		return &binding, nil
	})
	return err
}

// deleteHarnessConfigSecretBindingsBySecret removes every binding that references a
// secret. Called when the secret is deleted so no binding dangles.
func (s *Store) deleteHarnessConfigSecretBindingsBySecret(tx *gorm.DB, secretID string) error {
	return tx.Where("secret_id = ?", secretID).Delete(&model.HarnessConfigSecretBinding{}).Error
}

// deleteHarnessConfigSecretBindingsByHarnessConfig removes every binding for a harness
// config. Called when the harness config is deleted.
func (s *Store) deleteHarnessConfigSecretBindingsByHarnessConfig(tx *gorm.DB, harnessConfigID string) error {
	return tx.Where("harness_config_id = ?", harnessConfigID).Delete(&model.HarnessConfigSecretBinding{}).Error
}
