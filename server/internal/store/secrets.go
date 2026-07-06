package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"gorm.io/gorm"

	"github.com/obot-platform/discobox/server/internal/apperrors"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/secrets"
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
	err = read.Where("project_id = ?", projectID).Order("created_at ASC").Find(&out).Error
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
	var candidates []model.Secret
	err = read.
		Where("project_id = ? AND type = ? AND (host = ? OR host = '')", projectID, secretType, host).
		Order("CASE WHEN host = '' THEN 1 ELSE 0 END ASC").
		Find(&candidates).Error
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "no matching secret found for the requested type and host")
	}
	// Prefer exact host matches over wildcard matches.
	var exact, wildcard []model.Secret
	for _, c := range candidates {
		if c.Host == host && host != "" {
			exact = append(exact, c)
		} else {
			wildcard = append(wildcard, c)
		}
	}
	pool := exact
	if len(pool) == 0 {
		pool = wildcard
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

func (s *Store) GetSecretRequest(ctx context.Context, projectID, requestID string) (*model.SecretRequest, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.SecretRequest](read.Where("project_id = ?", projectID), "id", requestID)
}

func (s *Store) ListSecretRequests(ctx context.Context, projectID string) ([]model.SecretRequest, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.SecretRequest
	err = read.Where("project_id = ?", projectID).Order("created_at ASC").Find(&out).Error
	return out, err
}

func (s *Store) UpdateSecretRequest(ctx context.Context, req *model.SecretRequest) error {
	_, err := withResourceEvent(ctx, s, model.EventActionUpdated, func(tx *gorm.DB) (*model.SecretRequest, error) {
		if err := tx.Save(req).Error; err != nil {
			return nil, err
		}
		return req, nil
	})
	return err
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
