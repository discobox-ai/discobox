package store

import (
	"context"

	"gorm.io/gorm"

	"github.com/obot-platform/discobox/server/internal/model"
)

func (s *Store) ListAgentConfigs(ctx context.Context, projectID string) ([]model.AgentConfig, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var configs []model.AgentConfig
	err = read.
		Where("project_id = ?", projectID).
		Order("created_at ASC").
		Find(&configs).Error
	return configs, err
}

func (s *Store) CreateAgentConfig(ctx context.Context, config *model.AgentConfig) error {
	_, err := withResourceEvent(ctx, s, model.EventActionCreated, func(tx *gorm.DB) (*model.AgentConfig, error) {
		if err := tx.Create(config).Error; err != nil {
			return nil, err
		}
		return config, nil
	})
	return err
}

func (s *Store) GetAgentConfig(ctx context.Context, projectID, configID string) (*model.AgentConfig, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.AgentConfig](read.Where("project_id = ?", projectID), "id", configID)
}

func (s *Store) GetAgentConfigByName(ctx context.Context, projectID, name string) (*model.AgentConfig, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var config model.AgentConfig
	if err := read.Where("project_id = ? AND name = ?", projectID, name).First(&config).Error; err != nil {
		return nil, mapNotFound(err)
	}
	return &config, nil
}

func (s *Store) UpdateAgentConfig(ctx context.Context, config *model.AgentConfig) error {
	_, err := withResourceEvent(ctx, s, model.EventActionUpdated, func(tx *gorm.DB) (*model.AgentConfig, error) {
		if err := tx.Save(config).Error; err != nil {
			return nil, err
		}
		return config, nil
	})
	return err
}

func (s *Store) DeleteAgentConfig(ctx context.Context, projectID, configID string) error {
	_, err := withResourceEvent(ctx, s, model.EventActionDeleted, func(tx *gorm.DB) (*model.AgentConfig, error) {
		config, err := firstByID[model.AgentConfig](tx.Where("project_id = ?", projectID), "id", configID)
		if err != nil {
			return nil, err
		}
		if err := tx.Delete(config).Error; err != nil {
			return nil, err
		}
		return config, nil
	})
	return err
}

func (s *Store) ListAgentConfigSnapshots(ctx context.Context, projectID string) ([]model.AgentConfig, error) {
	return s.ListAgentConfigs(ctx, projectID)
}
