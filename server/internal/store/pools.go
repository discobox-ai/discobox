package store

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/discobox-ai/discobox/server/internal/model"
)

func (s *Store) ListPools(ctx context.Context, projectID string) ([]model.Pool, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var pools []model.Pool
	err = read.
		Where("project_id = ?", projectID).
		Order("created_at ASC").
		Find(&pools).Error
	return pools, err
}

// ListPoolsForProviderInstance returns the pools bound to one provider
// instance, for provider-instance delete protection and status rollups.
func (s *Store) ListPoolsForProviderInstance(ctx context.Context, projectID, providerID string) ([]model.Pool, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var pools []model.Pool
	err = read.
		Where("project_id = ? AND provider_instance_id = ?", projectID, providerID).
		Order("created_at ASC").
		Find(&pools).Error
	return pools, err
}

func (s *Store) CreatePool(ctx context.Context, pool *model.Pool) error {
	_, err := withResourceEvent(ctx, s, model.EventActionCreated, func(tx *gorm.DB) (*model.Pool, error) {
		if err := tx.Create(pool).Error; err != nil {
			return nil, err
		}
		return pool, nil
	})
	return err
}

func (s *Store) GetPool(ctx context.Context, projectID, poolID string) (*model.Pool, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.Pool](read.Where("project_id = ?", projectID), "id", poolID)
}

func (s *Store) GetPoolByName(ctx context.Context, projectID, name string) (*model.Pool, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var pool model.Pool
	if err := read.Where("project_id = ? AND name = ?", projectID, name).First(&pool).Error; err != nil {
		return nil, mapNotFound(err)
	}
	return &pool, nil
}

func (s *Store) UpdatePool(ctx context.Context, pool *model.Pool) error {
	_, err := withResourceEvent(ctx, s, model.EventActionUpdated, func(tx *gorm.DB) (*model.Pool, error) {
		if err := tx.Save(pool).Error; err != nil {
			return nil, err
		}
		return pool, nil
	})
	return err
}

func (s *Store) DeletePool(ctx context.Context, projectID, poolID string) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	pool, err := firstByID[model.Pool](write.Where("project_id = ?", projectID), "id", poolID)
	if err != nil {
		return err
	}
	_, err = withResourceEvent(ctx, s, model.EventActionDeleted, func(tx *gorm.DB) (*model.Pool, error) {
		if err := tx.Delete(pool).Error; err != nil {
			return nil, err
		}
		return pool, nil
	})
	return err
}

func (s *Store) CountSandboxesForPool(ctx context.Context, projectID, poolID string) (int64, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return 0, err
	}
	var count int64
	err = read.Model(&model.Sandbox{}).
		Where("project_id = ? AND pool_id = ?", projectID, poolID).
		Count(&count).Error
	return count, err
}

type PoolGetOption func(*poolGetOptions)

type poolGetOptions struct {
	generation *int64
}

// WithPoolGeneration guards a read or write against a specific pool
// generation, surfacing ErrGenerationConflict when newer intent landed.
func WithPoolGeneration(generation int64) PoolGetOption {
	return func(opts *poolGetOptions) {
		opts.generation = &generation
	}
}

// GetPoolByID loads a pool by ID alone, for trusted control-plane paths that
// hold a pool ID without project scope (agent auth, reconcile dirty ids).
func (s *Store) GetPoolByID(ctx context.Context, poolID string, options ...PoolGetOption) (*model.Pool, error) {
	var opts poolGetOptions
	for _, option := range options {
		if option != nil {
			option(&opts)
		}
	}
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	pool, err := firstByID[model.Pool](read, "id", poolID)
	if err != nil {
		return nil, err
	}
	if opts.generation != nil && pool.Generation != *opts.generation {
		return nil, ErrGenerationConflict
	}
	return pool, nil
}

// UpdatePoolWithGeneration persists the pool only when its generation still
// matches, so reconciler writes lose cleanly to newer intent.
func (s *Store) UpdatePoolWithGeneration(ctx context.Context, pool *model.Pool, generation int64) error {
	_, err := withResourceEvent(ctx, s, model.EventActionUpdated, func(tx *gorm.DB) (*model.Pool, error) {
		result := tx.Model(&model.Pool{}).
			Where("id = ? AND generation = ?", pool.ID, generation).
			Select("*").
			Updates(pool)
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected == 0 {
			return nil, ErrGenerationConflict
		}
		return pool, nil
	})
	return err
}

// ListPoolsNeedingReconcile returns ids of pools whose generations disagree. It
// is the reconcile engine's lost-mark backstop, and it is deliberately the same
// query as ListSandboxRefsNeedingReconcile: under ADR 0017 §1 both scanners are
// one generation comparison, with no per-resource knowledge.
func (s *Store) ListPoolsNeedingReconcile(ctx context.Context) ([]model.Pool, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var pools []model.Pool
	err = read.Model(&model.Pool{}).
		Select("id", "project_id").
		Where("generation > observed_generation").
		Find(&pools).Error
	return pools, err
}

func (s *Store) CreatePoolBootstrapToken(ctx context.Context, token *model.PoolBootstrapToken) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Create(token).Error
}

// RegisterPool redeems a bootstrap token: it records the agent's public key
// and stamps the pool registered.
//
// Registration establishes identity and liveness, and nothing else. It does
// not write State, ErrorMessage, or ObservedGeneration — redeeming a token is
// not evidence that the reconciler finished converging the runtime, which is
// the only thing ObservedGeneration means; the reconciler derives `active`
// from RegisteredAt on the reconcile the registration marks dirty. It does not
// write the health flags either: the agent reports those over its own
// heartbeat, synchronously, immediately after this call returns.
func (s *Store) RegisterPool(ctx context.Context, poolID string, tokenHash []byte, publicKey, keyType string) (*model.Pool, error) {
	write, err := s.getWrite(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	var pool model.Pool
	err = write.Transaction(func(tx *gorm.DB) error {
		var token model.PoolBootstrapToken
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("pool_id = ? AND token_hash = ?", poolID, tokenHash).First(&token).Error; err != nil {
			return mapNotFound(err)
		}
		if token.UsedAt != nil || token.RevokedAt != nil || !token.ExpiresAt.After(now) {
			return ErrNotFound
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&pool, "id = ?", poolID).Error; err != nil {
			return mapNotFound(err)
		}
		token.UsedAt = &now
		if err := tx.Save(&token).Error; err != nil {
			return err
		}
		pool.PublicKey = publicKey
		pool.KeyType = keyType
		pool.RegisteredAt = &now
		pool.LastSeenAt = &now
		return tx.Save(&pool).Error
	})
	if err != nil {
		return nil, err
	}
	return &pool, nil
}

// UpdatePoolStatus records an agent heartbeat: scheduling flags, reported
// capacity, and conditions.
//
// It deliberately does not touch State or ErrorMessage. Health and State have
// different owners: the agent knows whether it can take work right now, while
// State and ErrorMessage are the reconciler's verdict on whether the pool's
// runtime converged. A heartbeat that also wrote State let a healthy agent
// repaint `active` over a recorded `offline` every few seconds, so a pool whose
// reconcile was failing read as active with an error message attached. A pool
// that recovers is returned to `active` by the reconcile that proves it, not by
// the heartbeat — the service layer marks an offline pool dirty when its agent
// reports back in, which is what makes that reconcile prompt.
func (s *Store) UpdatePoolStatus(ctx context.Context, poolID string, ready, schedulable, degraded bool, availableCPUVCPUs float64, availableMemoryBytes, availableStorageBytes int64, conditions []byte) (*model.Pool, error) {
	write, err := s.getWrite(ctx)
	if err != nil {
		return nil, err
	}
	if availableCPUVCPUs < 0 {
		availableCPUVCPUs = 0
	}
	if availableMemoryBytes < 0 {
		availableMemoryBytes = 0
	}
	if availableStorageBytes < 0 {
		availableStorageBytes = 0
	}
	now := time.Now().UTC()
	var pool model.Pool
	err = write.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&pool, "id = ?", poolID).Error; err != nil {
			return mapNotFound(err)
		}
		pool.Ready = ready
		pool.Schedulable = schedulable
		pool.Degraded = degraded
		pool.AvailableCPUVCPUs = availableCPUVCPUs
		pool.AvailableMemoryBytes = availableMemoryBytes
		pool.AvailableStorageBytes = availableStorageBytes
		pool.Conditions = conditions
		pool.LastSeenAt = &now
		return tx.Save(&pool).Error
	})
	if err != nil {
		return nil, err
	}
	return &pool, nil
}

// SchedulablePoolForSandbox gates placement: the sandbox's pool must be
// ready, schedulable, unrevoked, and not offline. Ready and Schedulable are
// the agent's own word; the offline check covers their blind spot — an agent
// that stopped answering leaves its last (stale) flags behind, and `offline`
// is the reconciler's verdict that the host is gone. No capacity is gated —
// sandboxes share their pool's overcommitted CPU/memory/storage envelope with
// no per-sandbox reservation (docs/adr/0029). There is no candidate search —
// the pool is the host.
func (s *Store) SchedulablePoolForSandbox(ctx context.Context, sandbox *model.Sandbox) (*model.Pool, error) {
	if sandbox == nil || sandbox.PoolID == "" {
		return nil, ErrNotFound
	}
	pool, err := s.GetPool(ctx, sandbox.ProjectID, sandbox.PoolID)
	if err != nil {
		return nil, err
	}
	if pool.RevokedAt != nil ||
		pool.DesiredState != model.DesiredStatePresent ||
		pool.State == model.PoolStateOffline ||
		!pool.Ready || !pool.Schedulable {
		return nil, ErrNotFound
	}
	return pool, nil
}

// PurgeSpentPoolBootstrapTokens deletes bootstrap tokens that can no longer be
// redeemed: expired, already used, or revoked.
func (s *Store) PurgeSpentPoolBootstrapTokens(ctx context.Context, expiredBefore time.Time) (int64, error) {
	write, err := s.getWrite(ctx)
	if err != nil {
		return 0, err
	}
	result := write.Where("expires_at < ? OR used_at IS NOT NULL OR revoked_at IS NOT NULL", expiredBefore).
		Delete(&model.PoolBootstrapToken{})
	return result.RowsAffected, result.Error
}
