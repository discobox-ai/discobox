package store

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/obot-platform/discobox/server/internal/model"
)

// UpsertAgentConfigSecretBinding creates or replaces the binding for an agent
// config's environment variable.
func (s *Store) UpsertAgentConfigSecretBinding(ctx context.Context, binding *model.AgentConfigSecretBinding) error {
	_, err := withResourceEvent(ctx, s, model.EventActionUpdated, func(tx *gorm.DB) (*model.AgentConfigSecretBinding, error) {
		if binding.ID == "" {
			if err := binding.BeforeCreate(nil); err != nil {
				return nil, err
			}
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "agent_config_id"}, {Name: "env_name"}},
			DoUpdates: clause.AssignmentColumns([]string{"secret_id", "updated_at", "deleted_at"}),
		}).Create(binding).Error; err != nil {
			return nil, err
		}
		return binding, nil
	})
	return err
}

// ListAgentConfigSecretBindings returns an agent config's secret bindings.
func (s *Store) ListAgentConfigSecretBindings(ctx context.Context, projectID, agentConfigID string) ([]model.AgentConfigSecretBinding, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.AgentConfigSecretBinding
	err = read.Where("project_id = ? AND agent_config_id = ?", projectID, agentConfigID).
		Order("env_name ASC").Find(&out).Error
	return out, err
}

// DeleteAgentConfigSecretBinding removes the binding for an agent config's
// environment variable.
func (s *Store) DeleteAgentConfigSecretBinding(ctx context.Context, projectID, agentConfigID, envName string) error {
	_, err := withResourceEvent(ctx, s, model.EventActionDeleted, func(tx *gorm.DB) (*model.AgentConfigSecretBinding, error) {
		var binding model.AgentConfigSecretBinding
		if err := tx.Where("project_id = ? AND agent_config_id = ? AND env_name = ?", projectID, agentConfigID, envName).
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

// deleteAgentConfigSecretBindingsBySecret removes every binding that references a
// secret. Called when the secret is deleted so no binding dangles.
func (s *Store) deleteAgentConfigSecretBindingsBySecret(tx *gorm.DB, secretID string) error {
	return tx.Where("secret_id = ?", secretID).Delete(&model.AgentConfigSecretBinding{}).Error
}

// deleteAgentConfigSecretBindingsByAgentConfig removes every binding for an agent
// config. Called when the agent config is deleted.
func (s *Store) deleteAgentConfigSecretBindingsByAgentConfig(tx *gorm.DB, agentConfigID string) error {
	return tx.Where("agent_config_id = ?", agentConfigID).Delete(&model.AgentConfigSecretBinding{}).Error
}
