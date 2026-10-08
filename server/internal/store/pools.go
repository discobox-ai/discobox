package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/discobox-ai/discobox/platform"
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
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Create(pool).Error
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
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Model(pool).Select("name", "cpu_vcpus", "memory_bytes", "storage_bytes", "updated_at").Updates(pool).Error
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
	return write.Delete(pool).Error
}

// CountWorkSandboxesForPool counts what somebody would lose if this pool went:
// its discoboxes, except the project's own judge, which Discobox put there and
// makes again wherever the project's judge belongs (ADR 26-09-22-838 §1). Only a delete
// gate asks this. The reconcilers count every discobox, because a judge is as
// real to a pool host as anything else it runs.
func (s *Store) CountWorkSandboxesForPool(ctx context.Context, projectID, poolID string) (int64, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return 0, err
	}
	var count int64
	err = read.Model(&model.Sandbox{}).
		Where("project_id = ? AND pool_id = ?", projectID, poolID).
		Where("id <> coalesce((select judge_sandbox_id from projects where id = ?), '')", projectID).
		Count(&count).Error
	return count, err
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

// ListSandboxIDsForPool returns the ID of every sandbox row on the pool. A row
// is kept until the pool agent confirms its tree is gone (ADR 0022 §3), so this
// is every sandbox whose tree the pool may still hold by intent.
func (s *Store) ListSandboxIDsForPool(ctx context.Context, projectID, poolID string) ([]string, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	err = read.Model(&model.Sandbox{}).
		Where("project_id = ? AND pool_id = ?", projectID, poolID).
		Pluck("id", &ids).Error
	return ids, err
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

// UpdatePoolWithGeneration persists lifecycle and runtime fields only when the
// generation matches. Agent observations and other telemetry have separate
// writers and must survive a reconcile holding an older snapshot.
func (s *Store) UpdatePoolWithGeneration(ctx context.Context, pool *model.Pool, generation int64) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	result := write.Model(&model.Pool{}).
		Where("id = ? AND generation = ?", pool.ID, generation).
		Select("desired_state", "state", "state_changed_at", "generation",
			"observed_generation", "error_message", "runtime_state", "reconciled_at", "revoked_at", "updated_at").
		Updates(pool)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrGenerationConflict
	}
	return nil
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
// heartbeat, synchronously, immediately after this call returns. The platform
// the agent declares it hosts is recorded with its key (recordPoolPlatform).
func (s *Store) RegisterPool(ctx context.Context, poolID string, hosts platform.Platform, tokenHash []byte, publicKey, keyType string) (*model.Pool, error) {
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
		recordPoolPlatform(&pool, hosts)
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

// UpdatePoolStatus records an agent heartbeat: the platform the pool hosts,
// scheduling flags, reported capacity, and conditions.
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
func (s *Store) UpdatePoolStatus(ctx context.Context, poolID string, hosts platform.Platform, ready, schedulable, degraded bool, availableCPUVCPUs float64, availableMemoryBytes, availableStorageBytes int64, conditions []byte) (*model.Pool, error) {
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
		recordPoolPlatform(&pool, hosts)
		pool.Ready = ready
		pool.Schedulable = schedulable
		pool.Degraded = degraded
		pool.AvailableCPUVCPUs = availableCPUVCPUs
		pool.AvailableMemoryBytes = availableMemoryBytes
		pool.AvailableStorageBytes = availableStorageBytes
		pool.Conditions = conditions
		pool.LastSeenAt = &now
		pool.StatusReportedAt = &now
		return tx.Save(&pool).Error
	})
	if err != nil {
		return nil, err
	}
	return &pool, nil
}

// RecordPoolProvisionProgress stores what a provider driver is doing to bring a
// pool host up.
//
// A narrow two-column update rather than a Save of the row: this is written as
// often as twice a second while an image pulls, against a row the reconcile
// driving that work is holding, and every other column on it belongs to
// somebody else.
//
// Its readers poll: the client for its status line, and an attach waiting on a
// sandbox hosted here, which reads the advancing timestamp as proof that a slow
// host is still coming up rather than stalled (ADR 0081).
func (s *Store) RecordPoolProvisionProgress(ctx context.Context, poolID string, progress json.RawMessage, observedAt time.Time) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.WithContext(ctx).Model(&model.Pool{}).
		Where("id = ?", poolID).
		Updates(map[string]any{
			"provision_progress":    progress,
			"provision_progress_at": observedAt.UTC(),
		}).Error
}

// RecordPoolResources stores what a pool reported it is consuming (ADR 0071, resource accounting).
//
// A narrow two-column update rather than a Save, for the same reason as the
// progress writers above: this is telemetry arriving on its own schedule
// against a row the pool's reconcile also writes, and every other column on it
// belongs to somebody else.
func (s *Store) RecordPoolResources(ctx context.Context, poolID string, resources json.RawMessage, reportedAt time.Time) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	result := write.WithContext(ctx).Model(&model.Pool{}).
		Where("id = ?", poolID).
		Updates(map[string]any{
			"resources":             resources,
			"resources_reported_at": reportedAt.UTC(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// recordPoolPlatform records the platform a pool's agent declared, inside the
// registration or heartbeat that carried it (ADR 0145 §1).
//
// An agent from before platforms declares none (hosts is zero), and is still a
// pool that schedules sandboxes — a provider may fall back to the previous
// release's agent image, and a pool's agent is replaced only after the server
// upgrades. Such a pool keeps whatever was recorded for it, which may be
// nothing: no guess is written, because a pool may be any architecture, and a
// guess written onto a sandbox placed there is never corrected.
//
// The pool's sandboxes are not touched: a sandbox takes its platform when it
// is placed (SchedulablePoolForSandbox), which is the one place its harness's
// image is checked against it.
func recordPoolPlatform(pool *model.Pool, hosts platform.Platform) {
	if !hosts.IsZero() {
		pool.Platform = hosts
	}
}

// SchedulablePoolForSandbox gates placement on current heartbeat health and
// the agent's schedulable flag, independently of a blocked runtime reconcile.
// Pending/registering runtimes remain gated during upstream image preload.
// No capacity is gated: sandboxes share their pool's CPU, memory, and storage
// with no per-sandbox reservation (docs/adr/0029).
// There is no candidate search; the sandbox's assigned pool is its host.
//
// The sandbox's platform is settled here too (ADR 0145 §1), once the pool is
// schedulable, because only a reporting agent has declared what it hosts. A
// sandbox with none yet — created while its pool's agent had not declared one
// — takes the pool's, written to its row. Its harness's image must be
// published for it, which is refused with a *platform.UnpublishedError naming
// what it is published for; a sandbox of another platform than its pool's is
// refused with a *platform.MismatchError. Neither is ErrNotFound: a pool of the
// wrong platform is not one on its way up, and a caller that waited on it
// would wait for good.
//
// sandbox may stand in for a row not written yet — an import restores the tree
// first — and then carries the platform its tree belongs to, which is all
// there is to check; one from an archive written before platforms carries
// none, and lands on its pool, whose platform the row then takes when it is
// placed.
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
		(pool.State != model.PoolStateActive && pool.State != model.PoolStateOffline) ||
		!pool.IsReady() || !pool.Schedulable {
		return nil, ErrNotFound
	}
	// A pool whose agent has declared nothing — one from before platforms —
	// places as every pool did before them a sandbox that has no platform
	// either: there is nothing to settle it from, or to check it against. A
	// sandbox that has one — only an import's tree can bring one onto such a
	// pool — is refused: its platform cannot be checked against a pool that
	// has not said what it hosts, and a tree is never moved onto another
	// platform unchecked (ADR 0145 §8).
	if pool.Platform.IsZero() {
		recorded, err := s.recordedSandboxPlatform(ctx, sandbox)
		if err != nil {
			return nil, err
		}
		if !recorded.IsZero() {
			return nil, &platform.MismatchError{Sandbox: recorded, Pool: pool.Platform}
		}
		return pool, nil
	}
	sandboxPlatform, err := s.settleSandboxPlatform(ctx, sandbox, pool)
	if err != nil {
		return nil, err
	}
	if err := platform.Place(sandboxPlatform, pool.Platform); err != nil {
		return nil, err
	}
	return pool, nil
}

// recordedSandboxPlatform is the platform the sandbox already has: the one a
// stand-in carries, or its row's. Zero for one that has none yet.
func (s *Store) recordedSandboxPlatform(ctx context.Context, sandbox *model.Sandbox) (platform.Platform, error) {
	if !sandbox.Platform.IsZero() || sandbox.ID == "" {
		return sandbox.Platform, nil
	}
	read, err := s.getRead(ctx)
	if err != nil {
		return platform.Platform{}, err
	}
	var row model.Sandbox
	err = read.WithContext(ctx).Select("id", "platform").
		First(&row, "id = ? AND project_id = ?", sandbox.ID, sandbox.ProjectID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return platform.Platform{}, nil
	}
	return row.Platform, err
}

// settleSandboxPlatform is the platform the sandbox runs on: its row's, or,
// for a row with none yet, its pool's, recorded on the row. The harness the
// row names must be published for it. A stand-in with no row answers the
// platform it carries, or its pool's when it carries none.
func (s *Store) settleSandboxPlatform(ctx context.Context, sandbox *model.Sandbox, pool *model.Pool) (platform.Platform, error) {
	standIn := sandbox.Platform
	if standIn.IsZero() {
		standIn = pool.Platform
	}
	if sandbox.ID == "" {
		return standIn, nil
	}
	write, err := s.getWrite(ctx)
	if err != nil {
		return platform.Platform{}, err
	}
	var row model.Sandbox
	err = write.WithContext(ctx).Select("id", "platform", "harness_config_id").
		First(&row, "id = ? AND project_id = ?", sandbox.ID, sandbox.ProjectID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return standIn, nil
	}
	if err != nil {
		return platform.Platform{}, err
	}
	settled := row.Platform
	if settled.IsZero() {
		settled = pool.Platform
	}
	if row.HarnessConfigID != nil && *row.HarnessConfigID != "" {
		var harness model.HarnessConfig
		err := write.WithContext(ctx).Select("id", "slug", "platforms").
			First(&harness, "id = ?", *row.HarnessConfigID).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return platform.Platform{}, err
		}
		if err == nil {
			if err := harness.Platforms.Publishes(settled); err != nil {
				return platform.Platform{}, fmt.Errorf("harness %q: %w", harness.Slug, err)
			}
		}
	}
	if row.Platform.IsZero() && !settled.IsZero() {
		if err := write.WithContext(ctx).Model(&model.Sandbox{}).
			Where("id = ? AND platform = ''", row.ID).
			UpdateColumn("platform", settled).Error; err != nil {
			return platform.Platform{}, err
		}
	}
	return settled, nil
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

// BeginPoolHealthChecks invalidates reports from the previous server process.
// It runs before workers or HTTP listeners start. Identity and lifecycle survive.
func (s *Store) BeginPoolHealthChecks(ctx context.Context) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.WithContext(ctx).Model(&model.Pool{}).Where("1 = 1").
		UpdateColumn("health_check_started_at", time.Now().UTC()).Error
}
