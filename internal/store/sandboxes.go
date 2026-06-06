package store

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/obot-platform/disco2/internal/model"
)

type SandboxGetOption func(*sandboxGetOptions)

type sandboxGetOptions struct {
	generation *int64
}

func WithGeneration(generation int64) SandboxGetOption {
	return func(opts *sandboxGetOptions) {
		opts.generation = &generation
	}
}

func (s *Store) ListSandboxes(ctx context.Context, projectID string) ([]model.Sandbox, error) {
	var sandboxes []model.Sandbox
	err := s.read.WithContext(ctx).
		Preload("CreatedBy").
		Preload("ProviderInstance").
		Where("project_id = ?", projectID).
		Order("created_at ASC").
		Find(&sandboxes).Error
	return sandboxes, err
}

func (s *Store) CreateSandbox(ctx context.Context, sandbox *model.Sandbox) error {
	_, err := withResourceEvent(ctx, s, model.EventActionCreated, func(tx *gorm.DB) (*model.Sandbox, error) {
		if err := tx.Create(sandbox).Error; err != nil {
			return nil, err
		}
		return sandbox, nil
	})
	return err
}

func (s *Store) GetSandbox(ctx context.Context, projectID, sandboxID string, options ...SandboxGetOption) (*model.Sandbox, error) {
	var opts sandboxGetOptions
	for _, option := range options {
		if option != nil {
			option(&opts)
		}
	}

	var sandbox model.Sandbox
	query := s.read.WithContext(ctx).
		Preload("CreatedBy").
		Preload("ProviderInstance").
		Where("project_id = ? AND id = ?", projectID, sandboxID)
	if opts.generation != nil {
		query = query.Where("generation = ?", *opts.generation)
	}
	err := query.First(&sandbox).Error
	if err != nil {
		if opts.generation != nil && errors.Is(mapNotFound(err), ErrNotFound) {
			return nil, ErrGenerationConflict
		}
		return nil, mapNotFound(err)
	}
	return &sandbox, nil
}

func (s *Store) UpdateSandbox(ctx context.Context, sandbox *model.Sandbox, options ...SandboxGetOption) error {
	var opts sandboxGetOptions
	for _, option := range options {
		if option != nil {
			option(&opts)
		}
	}

	_, err := withResourceEvent(ctx, s, model.EventActionUpdated, func(tx *gorm.DB) (*model.Sandbox, error) {
		if opts.generation == nil {
			if err := tx.Save(sandbox).Error; err != nil {
				return nil, err
			}
			return sandbox, nil
		}

		result := tx.Model(&model.Sandbox{}).
			Where("project_id = ? AND id = ? AND generation = ?", sandbox.ProjectID, sandbox.ID, *opts.generation).
			Select("*").
			Updates(sandbox)
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected == 0 {
			return nil, ErrGenerationConflict
		}
		return sandbox, nil
	})
	return err
}

func (s *Store) DeleteSandbox(ctx context.Context, projectID, sandboxID string) error {
	_, err := withResourceEvent(ctx, s, model.EventActionDeleted, func(tx *gorm.DB) (*model.Sandbox, error) {
		var sandbox model.Sandbox
		if err := tx.First(&sandbox, "project_id = ? AND id = ?", projectID, sandboxID).Error; err != nil {
			return nil, mapNotFound(err)
		}
		if err := tx.Delete(&sandbox).Error; err != nil {
			return nil, err
		}
		return &sandbox, nil
	})
	return err
}

func (s *Store) ListSandboxSnapshots(ctx context.Context, projectID string) ([]model.Sandbox, error) {
	var sandboxes []model.Sandbox
	err := s.read.WithContext(ctx).
		Preload("CreatedBy").
		Preload("ProviderInstance").
		Where("project_id = ?", projectID).
		Order("created_at ASC").
		Find(&sandboxes).Error
	return sandboxes, err
}
