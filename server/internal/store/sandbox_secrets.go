package store

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/pkg/secretformat"
	"github.com/discobox-ai/discobox/pkg/secretroute"
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

// ListSandboxSecrets returns every secret assignment for a sandbox: each
// sentinel provisioned into it and registered with its proxy.
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

// NewSandboxBinding is a binding built but not stored: a freshly minted
// sentinel for one variable and secret on one discobox, injected into it like
// every other (ADR 26-10-27-880). Approving an agent's request and giving a
// grant at create both bind this way.
func (s *Store) NewSandboxBinding(ctx context.Context, projectID, sandboxID, envName string, secret *model.Secret) (*model.SandboxSecret, error) {
	format := s.SentinelFormat(ctx, secret)
	sentinel, err := secretformat.MintSentinel(format)
	if err != nil {
		return nil, err
	}
	return &model.SandboxSecret{
		ProjectID: projectID,
		SandboxID: sandboxID,
		SecretID:  secret.ID,
		EnvName:   envName,
		Sentinel:  sentinel,
		Format:    format,
	}, nil
}

// FindSandboxSecretByEnv returns a sandbox's binding of one environment
// variable, or ErrNotFound.
func (s *Store) FindSandboxSecretByEnv(ctx context.Context, projectID, sandboxID, envName string) (*model.SandboxSecret, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out model.SandboxSecret
	err = read.Where("project_id = ? AND sandbox_id = ? AND env_name = ?", projectID, sandboxID, envName).First(&out).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
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
// writer: a harness-config rebind repoints an assignment at the secret its
// binding now names, re-minting the sentinel when the new secret's format
// differs.
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

// SecretSentinelRoutes maps each of a discobox's injected sentinels to the
// routes of the secret it stands for, when that secret carries routes (ADR
// 26-10-02-924): every request the sentinel is sent in must fall on one.
// assignments are the discobox's injected secrets.
func (s *Store) SecretSentinelRoutes(ctx context.Context, projectID string, assignments []model.SandboxSecret) (map[string][]secretroute.Route, error) {
	var out map[string][]secretroute.Route
	for _, assignment := range assignments {
		secret, err := s.GetSecret(ctx, projectID, assignment.SecretID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		routes := secret.ProxyRoutes()
		if len(routes) == 0 {
			continue
		}
		if out == nil {
			out = map[string][]secretroute.Route{}
		}
		out[assignment.Sentinel] = routes
	}
	return out, nil
}

// ListSandboxIDsHoldingSecret returns the discoboxes with a sentinel injected
// for a secret, so a change to where the secret may be sent reaches them.
func (s *Store) ListSandboxIDsHoldingSecret(ctx context.Context, projectID, secretID string) ([]string, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	err = read.Model(&model.SandboxSecret{}).
		Where("project_id = ? AND secret_id = ?", projectID, secretID).
		Distinct("sandbox_id").Order("sandbox_id ASC").
		Pluck("sandbox_id", &out).Error
	return out, err
}

// ReplaceSandboxHarnessSecrets changes only harness-owned injected bindings;
// explicit bindings remain with the sandbox.
func (s *Store) ReplaceSandboxHarnessSecrets(ctx context.Context, sandboxID string, assignments []*model.SandboxSecret) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	if err := write.Where("sandbox_id = ? AND source_harness_config_id <> ''", sandboxID).Delete(&model.SandboxSecret{}).Error; err != nil {
		return err
	}
	for _, assignment := range assignments {
		if err := s.CreateSandboxSecret(ctx, assignment); err != nil {
			return err
		}
	}
	return nil
}
