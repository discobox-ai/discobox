package store

import (
	"context"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// UpsertHarnessConfigSecretBinding creates or replaces the binding for a harness
// config's environment variable.
func (s *Store) UpsertHarnessConfigSecretBinding(ctx context.Context, binding *model.HarnessConfigSecretBinding) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	if binding.ID == "" {
		if err := binding.BeforeCreate(nil); err != nil {
			return err
		}
	}
	return write.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "harness_config_id"}, {Name: "env_name"}},
		DoUpdates: clause.AssignmentColumns([]string{"secret_id", "updated_at"}),
	}).Create(binding).Error
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
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	result := write.Where("project_id = ? AND harness_config_id = ? AND env_name = ?", projectID, harnessConfigID, envName).
		Delete(&model.HarnessConfigSecretBinding{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
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
