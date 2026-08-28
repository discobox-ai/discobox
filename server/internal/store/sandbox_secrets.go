package store

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// CreateSandboxSecret persists a sandbox secret assignment.
func (s *Store) CreateSandboxSecret(ctx context.Context, assignment *model.SandboxSecret) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Create(assignment).Error
}

// ListSandboxSecrets returns the secret assignments for a sandbox.
func (s *Store) ListSandboxSecrets(ctx context.Context, projectID, sandboxID string) ([]model.SandboxSecret, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.SandboxSecret
	err = read.Where("project_id = ? AND sandbox_id = ?", projectID, sandboxID).
		Order("env_name ASC").Find(&out).Error
	return out, err
}

// GetSandboxSecretBySentinel returns the assignment for a sandbox and sentinel.
func (s *Store) GetSandboxSecretBySentinel(ctx context.Context, sandboxID, sentinel string) (*model.SandboxSecret, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out model.SandboxSecret
	if err := read.Where("sandbox_id = ? AND sentinel = ?", sandboxID, sentinel).First(&out).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &out, nil
}

// DeleteSandboxSecrets removes all secret assignments for a sandbox.
func (s *Store) DeleteSandboxSecrets(ctx context.Context, sandboxID string) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Where("sandbox_id = ?", sandboxID).Delete(&model.SandboxSecret{}).Error
}

// UpdateSandboxSecret persists a changed assignment. Rebinding is the only
// writer: it repoints an assignment at the secret its binding now names, and
// re-mints the sentinel when the new secret's format differs.
func (s *Store) UpdateSandboxSecret(ctx context.Context, assignment *model.SandboxSecret) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Model(&model.SandboxSecret{}).
		Where("id = ?", assignment.ID).
		Updates(map[string]any{
			"secret_id": assignment.SecretID,
			"sentinel":  assignment.Sentinel,
			"format":    assignment.Format,
		}).Error
}

// deleteSandboxSecretsBySecret removes every assignment naming a secret. It runs
// inside DeleteSecret's transaction: an assignment whose secret is gone is a
// sentinel the proxy still swaps on but can never resolve, which reaches the
// harness as an unexplained 401 rather than a missing credential.
func (s *Store) deleteSandboxSecretsBySecret(tx *gorm.DB, secretID string) error {
	return tx.Where("secret_id = ?", secretID).Delete(&model.SandboxSecret{}).Error
}

// ListSandboxIDsForHarnessConfig returns the non-archived sandboxes running a
// harness config, which are the sandboxes a binding change has to reach.
func (s *Store) ListSandboxIDsForHarnessConfig(ctx context.Context, projectID, harnessConfigID string) ([]string, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	err = read.Model(&model.Sandbox{}).
		Where("project_id = ? AND harness_config_id = ?", projectID, harnessConfigID).
		Where("state NOT IN ?", []string{model.SandboxStateArchived, model.SandboxStateDeleted}).
		Order("id ASC").
		Pluck("id", &out).Error
	return out, err
}
