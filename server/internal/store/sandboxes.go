package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/secrets"
)

type SandboxGetOption func(*sandboxGetOptions)

type sandboxGetOptions struct {
	generation *int64
}

type SandboxID struct {
	ProjectID string
	SandboxID string
}

type SandboxStore struct {
	*Store
	reload *Store
}

func (s *Store) Sandboxes() *SandboxStore {
	return &SandboxStore{Store: s, reload: s}
}

func (s *SandboxStore) Transaction(ctx context.Context, fn func(context.Context, *SandboxStore) error) error {
	return s.Store.Transaction(ctx, func(txStore *Store, _ *gorm.DB) error {
		return fn(ctx, &SandboxStore{Store: txStore, reload: s.reload})
	})
}

func (s *SandboxStore) Get(ctx context.Context, id SandboxID) (*model.Sandbox, error) {
	return s.GetSandbox(ctx, id.ProjectID, id.SandboxID)
}

func (s *SandboxStore) Create(ctx context.Context, sandbox *model.Sandbox) error {
	return s.CreateSandbox(ctx, sandbox)
}

func (s *SandboxStore) Update(ctx context.Context, sandbox *model.Sandbox) error {
	return s.UpdateSandbox(ctx, sandbox)
}

func (s *SandboxStore) UpdateWithGeneration(ctx context.Context, sandbox *model.Sandbox, generation int64) error {
	return s.UpdateSandbox(ctx, sandbox, WithGeneration(generation))
}

func (s *SandboxStore) Generation(sandbox *model.Sandbox) int64 {
	return sandbox.Generation
}

func (s *SandboxStore) ID(sandbox *model.Sandbox) SandboxID {
	return SandboxID{ProjectID: sandbox.ProjectID, SandboxID: sandbox.ID}
}

func (s *SandboxStore) Reload(ctx context.Context, id SandboxID) (*model.Sandbox, error) {
	return s.reload.GetSandbox(ctx, id.ProjectID, id.SandboxID)
}

func WithGeneration(generation int64) SandboxGetOption {
	return func(opts *sandboxGetOptions) {
		opts.generation = &generation
	}
}

// SandboxOperationRef identifies one sandbox by its project scope.
type SandboxOperationRef struct {
	ProjectID string
	SandboxID string
}

// ListSandboxIDsWithStaleOperations returns refs of sandboxes whose recorded
// operation has been in flight (pending/running) since before cutoff. It is
// the reconcile engine's lost-mark backstop.
func (s *Store) ListSandboxIDsWithStaleOperations(ctx context.Context, cutoff time.Time) ([]SandboxOperationRef, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var rows []model.Sandbox
	if err := read.Model(&model.Sandbox{}).
		Select("project_id", "id").
		Where("last_operation_status IN ?", []string{model.SandboxOperationStatusPending, model.SandboxOperationStatusRunning}).
		Where("updated_at < ?", cutoff).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	refs := make([]SandboxOperationRef, 0, len(rows))
	for _, row := range rows {
		refs = append(refs, SandboxOperationRef{ProjectID: row.ProjectID, SandboxID: row.ID})
	}
	return refs, nil
}

// ListSandboxes lists a project's sandboxes. A non-empty sourceRoot restricts
// the result to sandboxes whose primary source resolves to that repository root.
// A non-empty originKey restricts it to sandboxes created from one client host
// and project directory. The two filters are independent: sourceRoot asks what a
// sandbox runs against, originKey asks where it was started from.
func (s *Store) ListSandboxes(ctx context.Context, projectID, sourceRoot, originKey string) ([]model.Sandbox, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	query := read.
		Preload("Project").
		Preload("Pool").
		Preload("Pool.ProviderInstance").
		Preload("HarnessConfig").
		Where("project_id = ?", projectID)
	if sourceRoot != "" {
		query = query.Where("source_root = ?", sourceRoot)
	}
	if originKey != "" {
		query = query.Where("origin_key = ?", originKey)
	}
	var sandboxes []model.Sandbox
	err = query.
		Order("created_at ASC").
		Find(&sandboxes).Error
	return sandboxes, err
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

	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	query := read.
		Preload("Project").
		Preload("Pool").
		Preload("Pool.ProviderInstance").
		Preload("HarnessConfig").
		Where("project_id = ?", projectID)
	if opts.generation != nil {
		sandbox, err := firstByID[model.Sandbox](query, "id", sandboxID)
		if err != nil {
			return nil, err
		}
		if sandbox.Generation != *opts.generation {
			return nil, ErrGenerationConflict
		}
		return sandbox, nil
	}
	return firstByID[model.Sandbox](query, "id", sandboxID)
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

func (s *Store) DeleteSandbox(ctx context.Context, projectID, sandboxID string, options ...SandboxGetOption) error {
	var opts sandboxGetOptions
	for _, option := range options {
		if option != nil {
			option(&opts)
		}
	}

	_, err := withResourceEvent(ctx, s, model.EventActionDeleted, func(tx *gorm.DB) (*model.Sandbox, error) {
		query := tx.Where("project_id = ?", projectID)
		if opts.generation != nil {
			query = query.Where("generation = ?", *opts.generation)
		}
		sandbox, err := firstByID[model.Sandbox](query, "id", sandboxID)
		if err != nil {
			if opts.generation != nil && errors.Is(err, ErrNotFound) {
				return nil, ErrGenerationConflict
			}
			return nil, err
		}
		if err := deleteSandboxSecretsTx(tx, projectID, sandboxID); err != nil {
			return nil, err
		}
		if err := tx.Delete(sandbox).Error; err != nil {
			return nil, err
		}
		return sandbox, nil
	})
	return err
}

// deleteSandboxSecretsTx removes a sandbox's secret assignments and the anonymous
// secrets that were created for them. Referenced (non-anonymous) secrets are left
// untouched.
func deleteSandboxSecretsTx(tx *gorm.DB, projectID, sandboxID string) error {
	var assignments []model.SandboxSecret
	if err := tx.Where("sandbox_id = ?", sandboxID).Find(&assignments).Error; err != nil {
		return err
	}
	if len(assignments) == 0 {
		return nil
	}
	secretIDs := make([]string, 0, len(assignments))
	for _, assignment := range assignments {
		secretIDs = append(secretIDs, assignment.SecretID)
	}
	if err := tx.Where("sandbox_id = ?", sandboxID).Delete(&model.SandboxSecret{}).Error; err != nil {
		return err
	}
	// Nullify ciphertext before soft-deleting anonymous secrets so no secret
	// value is retained even in a soft-deleted row.
	if err := tx.Model(&model.Secret{}).
		Where("project_id = ? AND anonymous = ? AND id IN ?", projectID, true, secretIDs).
		Update("encrypted_value", nil).Error; err != nil {
		return err
	}
	return tx.Where("project_id = ? AND anonymous = ? AND id IN ?", projectID, true, secretIDs).
		Delete(&model.Secret{}).Error
}

func (s *Store) ListSandboxSnapshots(ctx context.Context, projectID string) ([]model.Sandbox, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var sandboxes []model.Sandbox
	err = read.
		Preload("Project").
		Preload("Pool").
		Preload("Pool.ProviderInstance").
		Preload("HarnessConfig").
		Where("project_id = ?", projectID).
		Order("created_at ASC").
		Find(&sandboxes).Error
	return sandboxes, err
}

func (s *Store) sealSandboxForWrite(ctx context.Context, sandbox *model.Sandbox) (*model.Sandbox, error) {
	if sandbox.ID == "" {
		if err := sandbox.BeforeCreate(nil); err != nil {
			return nil, err
		}
	}
	persisted := *sandbox
	ciphertext, err := secrets.SealIfUnsealed(ctx, s.sealer, "sandboxes.secret_state", sandboxSecretResourceID(sandbox), sandbox.SecretState)
	if err != nil {
		return nil, fmt.Errorf("encrypt sandbox secret state: %w", err)
	}
	persisted.SecretState = ciphertext
	return &persisted, nil
}

// OpenSandboxSecretState decrypts a sandbox's secret state for consumers that
// need to use it. It returns a copy and does not modify the model.
func (s *Store) OpenSandboxSecretState(ctx context.Context, sandbox *model.Sandbox) ([]byte, error) {
	if sandbox == nil || len(sandbox.SecretState) == 0 {
		return nil, nil
	}
	if s.sealer == nil || !secrets.IsSealed(sandbox.SecretState) {
		return append([]byte(nil), sandbox.SecretState...), nil
	}
	plaintext, err := secrets.Open(ctx, s.sealer, "sandboxes.secret_state", sandboxSecretResourceID(sandbox), sandbox.SecretState)
	if err != nil {
		return nil, fmt.Errorf("decrypt sandbox secret state: %w", err)
	}
	return plaintext, nil
}

func sandboxSecretResourceID(sandbox *model.Sandbox) string {
	return sandbox.ProjectID + "/" + sandbox.ID
}
