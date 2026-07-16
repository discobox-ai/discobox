package store

import (
	"context"

	"gorm.io/gorm"

	"github.com/obot-platform/discobox/server/internal/model"
)

func (s *Store) ListHarnessConfigs(ctx context.Context, projectID string) ([]model.HarnessConfig, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var configs []model.HarnessConfig
	err = read.
		Where("project_id = ?", projectID).
		Order("created_at ASC").
		Find(&configs).Error
	return configs, err
}

// ListDefinitionBackedHarnessConfigs returns every harness config across all
// projects that was created from a built-in definition. Used to refresh stored
// images when a definition's resolved image changes.
func (s *Store) ListDefinitionBackedHarnessConfigs(ctx context.Context) ([]model.HarnessConfig, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var configs []model.HarnessConfig
	err = read.
		Where("definition_id <> ''").
		Order("created_at ASC").
		Find(&configs).Error
	return configs, err
}

func (s *Store) CreateHarnessConfig(ctx context.Context, config *model.HarnessConfig) error {
	_, err := withResourceEvent(ctx, s, model.EventActionCreated, func(tx *gorm.DB) (*model.HarnessConfig, error) {
		if err := tx.Create(config).Error; err != nil {
			return nil, err
		}
		return config, nil
	})
	return err
}

func (s *Store) GetHarnessConfig(ctx context.Context, projectID, configID string) (*model.HarnessConfig, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.HarnessConfig](read.Where("project_id = ?", projectID), "id", configID)
}

func (s *Store) GetHarnessConfigByName(ctx context.Context, projectID, name string) (*model.HarnessConfig, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var config model.HarnessConfig
	if err := read.Where("project_id = ? AND name = ?", projectID, name).First(&config).Error; err != nil {
		return nil, mapNotFound(err)
	}
	return &config, nil
}

func (s *Store) GetHarnessConfigBySlug(ctx context.Context, projectID, slug string) (*model.HarnessConfig, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var config model.HarnessConfig
	if err := read.Where("project_id = ? AND slug = ?", projectID, slug).First(&config).Error; err != nil {
		return nil, mapNotFound(err)
	}
	return &config, nil
}

func (s *Store) UpdateHarnessConfig(ctx context.Context, config *model.HarnessConfig) error {
	_, err := withResourceEvent(ctx, s, model.EventActionUpdated, func(tx *gorm.DB) (*model.HarnessConfig, error) {
		if err := tx.Save(config).Error; err != nil {
			return nil, err
		}
		return config, nil
	})
	return err
}

func (s *Store) DeleteHarnessConfig(ctx context.Context, projectID, configID string) error {
	_, err := withResourceEvent(ctx, s, model.EventActionDeleted, func(tx *gorm.DB) (*model.HarnessConfig, error) {
		config, err := firstByID[model.HarnessConfig](tx.Where("project_id = ?", projectID), "id", configID)
		if err != nil {
			return nil, err
		}
		if err := tx.Model(&model.Project{}).
			Where("id = ? AND default_harness_config_id = ?", projectID, configID).
			Update("default_harness_config_id", "").Error; err != nil {
			return nil, err
		}
		if err := s.deleteHarnessConfigSecretBindingsByHarnessConfig(tx, configID); err != nil {
			return nil, err
		}
		// A live sandbox still using the config blocks deletion with a clear error.
		var liveRefs int64
		if err := tx.Model(&model.Sandbox{}).Where("harness_config_id = ?", configID).Count(&liveRefs).Error; err != nil {
			return nil, err
		}
		if liveRefs > 0 {
			return nil, ErrInUse
		}
		// Soft-deleted sandboxes still hold the FK; clear it so leftover deleted
		// sandboxes do not block deletion.
		if err := tx.Unscoped().Model(&model.Sandbox{}).
			Where("harness_config_id = ? AND deleted_at IS NOT NULL", configID).
			Update("harness_config_id", nil).Error; err != nil {
			return nil, err
		}
		if err := tx.Delete(config).Error; err != nil {
			return nil, err
		}
		return config, nil
	})
	return err
}

func (s *Store) ListHarnessConfigSnapshots(ctx context.Context, projectID string) ([]model.HarnessConfig, error) {
	return s.ListHarnessConfigs(ctx, projectID)
}
