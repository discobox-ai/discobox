package store

import (
	"context"
	"errors"
	"fmt"

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
	if err != nil {
		return nil, err
	}
	for i := range sandboxes {
		if err := s.openSandboxSecretState(ctx, &sandboxes[i]); err != nil {
			return nil, err
		}
	}
	return sandboxes, nil
}

func (s *Store) CreateSandbox(ctx context.Context, sandbox *model.Sandbox) error {
	_, err := withResourceEvent(ctx, s, model.EventActionCreated, func(tx *gorm.DB) (*model.Sandbox, error) {
		persisted, err := s.sealSandboxForWrite(ctx, sandbox)
		if err != nil {
			return nil, err
		}
		if err := tx.Create(persisted).Error; err != nil {
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
	if err := s.openSandboxSecretState(ctx, &sandbox); err != nil {
		return nil, err
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
		persisted, err := s.sealSandboxForWrite(ctx, sandbox)
		if err != nil {
			return nil, err
		}
		if opts.generation == nil {
			if err := tx.Save(persisted).Error; err != nil {
				return nil, err
			}
			return sandbox, nil
		}

		result := tx.Model(&model.Sandbox{}).
			Where("project_id = ? AND id = ? AND generation = ?", sandbox.ProjectID, sandbox.ID, *opts.generation).
			Select("*").
			Updates(persisted)
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
	if err != nil {
		return nil, err
	}
	for i := range sandboxes {
		if err := s.openSandboxSecretState(ctx, &sandboxes[i]); err != nil {
			return nil, err
		}
	}
	return sandboxes, nil
}

func (s *Store) sealSandboxForWrite(ctx context.Context, sandbox *model.Sandbox) (*model.Sandbox, error) {
	if sandbox.ID == "" {
		if err := sandbox.BeforeCreate(nil); err != nil {
			return nil, err
		}
	}
	persisted := *sandbox
	if s.sealer == nil || len(sandbox.SecretState) == 0 {
		persisted.SecretState = append([]byte(nil), sandbox.SecretState...)
		return &persisted, nil
	}
	ciphertext, err := s.sealer.Seal(ctx, "sandboxes.secret_state", sandboxSecretResourceID(sandbox), sandbox.SecretState)
	if err != nil {
		return nil, fmt.Errorf("encrypt sandbox secret state: %w", err)
	}
	persisted.SecretState = ciphertext
	return &persisted, nil
}

func (s *Store) openSandboxSecretState(ctx context.Context, sandbox *model.Sandbox) error {
	if s.sealer == nil || len(sandbox.SecretState) == 0 {
		return nil
	}
	plaintext, err := s.sealer.Open(ctx, "sandboxes.secret_state", sandboxSecretResourceID(sandbox), sandbox.SecretState)
	if err != nil {
		return fmt.Errorf("decrypt sandbox secret state: %w", err)
	}
	sandbox.SecretState = plaintext
	return nil
}

func sandboxSecretResourceID(sandbox *model.Sandbox) string {
	return sandbox.ProjectID + "/" + sandbox.ID
}
