package store

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

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
			"binding_id": assignment.BindingID,
			"secret_id":  assignment.SecretID,
			"sentinel":   assignment.Sentinel,
			"format":     assignment.Format,
			"raw":        assignment.Raw,
			// A row repointed, or made raw again, has handed this binding's
			// value over to nobody yet.
			"raw_delivered_revision": 0,
		}).Error
}

// RecordRawDelivery records that a raw binding's value at a secret revision
// is being handed to the pool of the sandbox a row delivers it to (ADR
// 26-10-27-840 §5): the row's delivered revision moves, and the binding's
// delivered event is written with it, once per revision and row. The row and
// the binding are read again here, the row locked: one gone, repointed to
// another binding, or no longer raw is ErrNotFound, and nothing may be handed
// out. A revision already recorded writes nothing.
//
// The binding's row is written in the same transaction, unchanged, so the
// event is ordered against every other change to the binding as theirs are
// (recordBindingEventTx).
func (s *Store) RecordRawDelivery(ctx context.Context, assignment *model.SandboxSecret, bindingID string, revision int64) error {
	return s.Transaction(ctx, func(_ *Store, tx *gorm.DB) error {
		var row model.SandboxSecret
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", assignment.ID).First(&row).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var binding model.SecretBinding
		if err := tx.Where("project_id = ? AND id = ?", row.ProjectID, bindingID).First(&binding).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if row.Raw == "" || row.BindingID != binding.ID || binding.Raw != row.Raw {
			return ErrNotFound
		}
		if row.RawDeliveredRevision == revision {
			return nil
		}
		if err := tx.Model(&model.SandboxSecret{}).Where("id = ?", row.ID).Update("raw_delivered_revision", revision).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.SecretBinding{}).Where("id = ?", binding.ID).UpdateColumn("id", gorm.Expr("id")).Error; err != nil {
			return err
		}
		return recordBindingEventTx(tx, &binding, model.SecretBindingEventDelivered,
			BindingChange{Actor: model.SecretBindingActorSystem, Cause: row.SandboxID})
	})
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

// SentinelRouteLists are the route lists a pool holds each of a sandbox's
// injected sentinels to (ADR 26-10-27-880 §4): the secret's, the binding's
// when it has any, and its parent's when it has one with any. Every sentinel
// has an entry, since the pool sends a sentinel with none nowhere; one whose
// binding or secret is gone gets a single empty list, which allows nothing.
func (s *Store) SentinelRouteLists(ctx context.Context, projectID string, assignments []model.SandboxSecret) (map[string][][]secretroute.Route, error) {
	out := make(map[string][][]secretroute.Route, len(assignments))
	for _, assignment := range assignments {
		lists, err := s.bindingRouteLists(ctx, projectID, assignment.BindingID, assignment.SecretID)
		if err != nil {
			return nil, err
		}
		out[assignment.Sentinel] = lists
	}
	return out, nil
}

// bindingRouteLists are the route lists a credential delivered under a
// binding is held to. Nowhere, as one empty list, when the binding or its
// secret is gone, or the binding is of another secret than the row says.
func (s *Store) bindingRouteLists(ctx context.Context, projectID, bindingID, secretID string) ([][]secretroute.Route, error) {
	nowhere := [][]secretroute.Route{{}}
	if bindingID == "" {
		return nowhere, nil
	}
	binding, err := s.SecretBindingByExactID(ctx, projectID, bindingID)
	if errors.Is(err, ErrNotFound) {
		return nowhere, nil
	}
	if err != nil {
		return nil, err
	}
	if binding.SecretID != secretID {
		return nowhere, nil
	}
	secret, err := s.GetSecret(ctx, projectID, binding.SecretID)
	if errors.Is(err, ErrNotFound) {
		return nowhere, nil
	}
	if err != nil {
		return nil, err
	}
	var parent *model.SecretBinding
	if binding.ParentID != "" {
		if parent, err = s.SecretBindingByExactID(ctx, projectID, binding.ParentID); errors.Is(err, ErrNotFound) {
			return nowhere, nil
		} else if err != nil {
			return nil, err
		}
	}
	return RouteLists(secret, binding, parent), nil
}

// RouteLists are the route lists a credential is held to under binding, and
// parent when it was handed on under one: the secret's always, first, and the
// binding's and the parent's when they have any. It is the one builder of
// them: what the pool is sent, what resolve checks, and what a listing shows.
func RouteLists(secret *model.Secret, binding, parent *model.SecretBinding) [][]secretroute.Route {
	lists := [][]secretroute.Route{secret.Routes}
	if len(binding.Routes) > 0 {
		lists = append(lists, binding.Routes)
	}
	if parent != nil && len(parent.Routes) > 0 {
		lists = append(lists, parent.Routes)
	}
	return lists
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

// DeleteSandboxSecret removes one sentinel row: a binding that no longer
// applies to its sandbox.
func (s *Store) DeleteSandboxSecret(ctx context.Context, id string) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Where("id = ?", id).Delete(&model.SandboxSecret{}).Error
}
