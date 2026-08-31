package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/hostscope"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/secrets"
)

const secretValuePurpose = "secrets.value"

func secretResourceID(secret *model.Secret) string {
	return secret.ProjectID + "/" + secret.ID
}

func (s *Store) sealSecretForWrite(ctx context.Context, secret *model.Secret) (*model.Secret, error) {
	if secret.ID == "" {
		if err := secret.BeforeCreate(nil); err != nil {
			return nil, err
		}
	}
	persisted := *secret
	ciphertext, err := secrets.SealIfUnsealed(ctx, s.sealer, secretValuePurpose, secretResourceID(secret), secret.EncryptedValue)
	if err != nil {
		return nil, fmt.Errorf("encrypt secret value: %w", err)
	}
	persisted.EncryptedValue = ciphertext
	return &persisted, nil
}

// OpenSecretValue decrypts a secret's value and deserializes it.
func (s *Store) OpenSecretValue(ctx context.Context, secret *model.Secret) (*model.SecretValue, error) {
	if secret == nil || len(secret.EncryptedValue) == 0 {
		return nil, nil
	}
	// Pass through plaintext rows (written before encryption was enabled) unchanged,
	// matching the guard in OpenSandboxSecretState.
	if s.sealer == nil || !secrets.IsSealed(secret.EncryptedValue) {
		var val model.SecretValue
		if err := json.Unmarshal(secret.EncryptedValue, &val); err != nil {
			return nil, fmt.Errorf("unmarshal secret value: %w", err)
		}
		return &val, nil
	}
	plaintext, err := secrets.Open(ctx, s.sealer, secretValuePurpose, secretResourceID(secret), secret.EncryptedValue)
	if err != nil {
		return nil, fmt.Errorf("decrypt secret value: %w", err)
	}
	var val model.SecretValue
	if err := json.Unmarshal(plaintext, &val); err != nil {
		return nil, fmt.Errorf("unmarshal secret value: %w", err)
	}
	return &val, nil
}

func (s *Store) CreateSecret(ctx context.Context, secret *model.Secret) error {
	sealed, err := s.sealSecretForWrite(ctx, secret)
	if err != nil {
		return err
	}
	_, err = withResourceEvent(ctx, s, model.EventActionCreated, func(tx *gorm.DB) (*model.Secret, error) {
		if err := tx.Create(sealed).Error; err != nil {
			return nil, err
		}
		return sealed, nil
	})
	if err != nil {
		return err
	}
	*secret = *sealed
	return nil
}

func (s *Store) GetSecret(ctx context.Context, projectID, secretID string) (*model.Secret, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.Secret](read.Where("project_id = ?", projectID), "id", secretID)
}

func (s *Store) ListSecrets(ctx context.Context, projectID string) ([]model.Secret, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.Secret
	err = read.Where("project_id = ? AND anonymous = ?", projectID, false).Order("created_at ASC").Find(&out).Error
	return out, err
}

func (s *Store) UpdateSecret(ctx context.Context, secret *model.Secret) error {
	sealed, err := s.sealSecretForWrite(ctx, secret)
	if err != nil {
		return err
	}
	_, err = withResourceEvent(ctx, s, model.EventActionUpdated, func(tx *gorm.DB) (*model.Secret, error) {
		if err := tx.Save(sealed).Error; err != nil {
			return nil, err
		}
		return sealed, nil
	})
	if err != nil {
		return err
	}
	*secret = *sealed
	return nil
}

// UpdateSecretValueIfUnchanged replaces a secret's encrypted value only if its
// row has not been updated since prevUpdatedAt. It is the atomic swap the OAuth
// refresh relies on: a concurrent refresh in another process bumps updated_at,
// so the loser sees zero rows affected (ErrGenerationConflict) and re-reads the
// winner's freshly rotated credential instead of clobbering it with a stale one.
func (s *Store) UpdateSecretValueIfUnchanged(ctx context.Context, secret *model.Secret, prevUpdatedAt time.Time) error {
	sealed, err := s.sealSecretForWrite(ctx, secret)
	if err != nil {
		return err
	}
	_, err = withResourceEvent(ctx, s, model.EventActionUpdated, func(tx *gorm.DB) (*model.Secret, error) {
		result := tx.Model(&model.Secret{}).
			Where("project_id = ? AND id = ? AND updated_at = ?", sealed.ProjectID, sealed.ID, prevUpdatedAt).
			Update("encrypted_value", sealed.EncryptedValue)
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected == 0 {
			return nil, ErrGenerationConflict
		}
		return sealed, nil
	})
	if err != nil {
		return err
	}
	*secret = *sealed
	return nil
}

func (s *Store) DeleteSecret(ctx context.Context, projectID, secretID string) error {
	_, err := withResourceEvent(ctx, s, model.EventActionDeleted, func(tx *gorm.DB) (*model.Secret, error) {
		sec, err := firstByID[model.Secret](tx.Where("project_id = ?", projectID), "id", secretID)
		if err != nil {
			return nil, err
		}
		// Nullify the encrypted value before soft-deleting so ciphertext is not
		// retained in the database even as a soft-deleted row.
		if err := tx.Model(sec).Update("encrypted_value", nil).Error; err != nil {
			return nil, err
		}
		// Drop harness-config bindings, standing grants, and sandbox sentinel
		// assignments that reference this secret so nothing dangles.
		if err := s.deleteHarnessConfigSecretBindingsBySecret(tx, secretID); err != nil {
			return nil, err
		}
		if err := s.deleteSecretGrantsBySecret(tx, secretID); err != nil {
			return nil, err
		}
		if err := s.deleteSandboxSecretsBySecret(tx, secretID); err != nil {
			return nil, err
		}
		if err := tx.Delete(sec).Error; err != nil {
			return nil, err
		}
		return sec, nil
	})
	return err
}

// MatchSecret finds the most specific secret for a project+type+host combination.
// An exact host match beats a wildcard (empty host) match. Returns an error if
// the result is ambiguous (multiple secrets at the same specificity).
func (s *Store) MatchSecret(ctx context.Context, projectID, secretType, host string) (*model.Secret, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	// The host is filtered in Go, for the reason FindLiveGrant is: a binding
	// covers the hosts beneath it (hostscope.Covers), which SQL equality cannot
	// express — and a secret bound to github.com is a candidate for a request
	// about api.github.com, or the reactive path would open a request for a
	// credential the project already holds.
	var rows []model.Secret
	err = read.
		Where("project_id = ? AND anonymous = ? AND type = ?", projectID, false, secretType).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	var candidates []model.Secret
	for _, row := range rows {
		if hostscope.Covers(row.Host, host) {
			candidates = append(candidates, row)
		}
	}
	if len(candidates) == 0 {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "no matching secret found for the requested type and host")
	}
	// The narrowest binding that answers wins: the host itself, then a host
	// above it, then a secret bound to nothing.
	best := hostscope.Specificity(candidates[0].Host, host)
	for _, c := range candidates[1:] {
		best = min(best, hostscope.Specificity(c.Host, host))
	}
	var pool []model.Secret
	for _, c := range candidates {
		if hostscope.Specificity(c.Host, host) == best {
			pool = append(pool, c)
		}
	}
	if len(pool) > 1 {
		return nil, apperrors.NewStatusError(http.StatusConflict, "ambiguous secret match: multiple secrets of the same type and host exist")
	}
	return &pool[0], nil
}

func (s *Store) CreateSecretRequest(ctx context.Context, req *model.SecretRequest) error {
	_, err := withResourceEvent(ctx, s, model.EventActionCreated, func(tx *gorm.DB) (*model.SecretRequest, error) {
		if err := tx.Create(req).Error; err != nil {
			return nil, err
		}
		return req, nil
	})
	return err
}

// FindPendingSecretRequest returns the most recent pending secret request for a
// specific secret, host, and requesting principal. It powers on-demand sentinel
// resolution so repeated proxy lookups reuse an open request instead of piling
// up duplicates while it waits for approval.
func (s *Store) FindPendingSecretRequest(ctx context.Context, projectID, secretID, host, requestedBy string) (*model.SecretRequest, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var req model.SecretRequest
	err = read.Where("project_id = ? AND secret_id = ? AND host = ? AND requested_by = ? AND status = ?",
		projectID, secretID, host, requestedBy, model.SecretRequestStatusPending).
		Order("created_at DESC").First(&req).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &req, nil
}

// FindPendingAgentCredentialRequest returns the open protocol-originated
// request for a sandbox's environment variable and destination host, or
// ErrNotFound.
//
// It keys on (sandbox, env, host) rather than on the secret the way the
// reactive path does, because a protocol request names no secret: choosing one
// is part of the approval. An agent that retries its ask therefore reuses its
// open request instead of adding another line to the approval inbox.
func (s *Store) FindPendingAgentCredentialRequest(ctx context.Context, projectID, sandboxID, envName, host string) (*model.SecretRequest, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.SecretRequest
	err = read.Where("project_id = ? AND sandbox_id = ? AND env_name = ? AND host = ? AND status = ?",
		projectID, sandboxID, envName, host, model.SecretRequestStatusPending).
		Order("created_at DESC").Find(&out).Error
	if err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].FromProtocol() {
			return &out[i], nil
		}
	}
	return nil, ErrNotFound
}

func (s *Store) GetSecretRequest(ctx context.Context, projectID, requestID string) (*model.SecretRequest, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.SecretRequest](read.Where("project_id = ?", projectID), "id", requestID)
}

// ListSecretRequests returns a project's secret requests, optionally filtered to
// a single status.
func (s *Store) ListSecretRequests(ctx context.Context, projectID, status string) ([]model.SecretRequest, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	query := read.Where("project_id = ?", projectID)
	if status = strings.TrimSpace(status); status != "" {
		query = query.Where("status = ?", status)
	}
	var out []model.SecretRequest
	err = query.Order("created_at ASC").Find(&out).Error
	return out, err
}

// UpdateSecretRequestIfPending atomically transitions a SecretRequest out of
// pending status. It returns ErrGenerationConflict if the request is no longer
// pending (concurrent approve or deny beat this caller).
func (s *Store) UpdateSecretRequestIfPending(ctx context.Context, req *model.SecretRequest) error {
	_, err := withResourceEvent(ctx, s, model.EventActionUpdated, func(tx *gorm.DB) (*model.SecretRequest, error) {
		result := tx.Model(&model.SecretRequest{}).
			Where("project_id = ? AND id = ? AND status = ?", req.ProjectID, req.ID, model.SecretRequestStatusPending).
			Select("*").
			Updates(req)
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected == 0 {
			return nil, ErrGenerationConflict
		}
		return req, nil
	})
	return err
}
