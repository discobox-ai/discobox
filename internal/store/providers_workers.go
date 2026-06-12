package store

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/obot-platform/discobox/internal/model"
)

type WorkerGetOption func(*workerGetOptions)

type workerGetOptions struct {
	generation *int64
}

func WithWorkerGeneration(generation int64) WorkerGetOption {
	return func(opts *workerGetOptions) {
		opts.generation = &generation
	}
}

func (s *Store) ListSandboxProviderInstances(ctx context.Context, projectID string) ([]model.SandboxProviderInstance, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var providers []model.SandboxProviderInstance
	err = read.
		Where("project_id = ?", projectID).
		Order("created_at ASC").
		Find(&providers).Error
	return providers, err
}

func (s *Store) CreateSandboxProviderInstance(ctx context.Context, provider *model.SandboxProviderInstance) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Create(provider).Error
}

func (s *Store) RestoreSandboxProviderInstance(ctx context.Context, provider *model.SandboxProviderInstance) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	result := write.Unscoped().
		Model(&model.SandboxProviderInstance{}).
		Where("project_id = ? AND id = ?", provider.ProjectID, provider.ID).
		Updates(map[string]any{
			"type":             provider.Type,
			"name":             provider.Name,
			"config":           provider.Config,
			"encrypted_config": provider.EncryptedConfig,
			"built_in":         provider.BuiltIn,
			"disabled":         provider.Disabled,
			"deleted_at":       nil,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetSandboxProviderInstance(ctx context.Context, projectID, providerID string) (*model.SandboxProviderInstance, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var provider model.SandboxProviderInstance
	err = read.
		Where("project_id = ? AND id = ?", projectID, providerID).
		First(&provider).Error
	if err != nil {
		return nil, mapNotFound(err)
	}
	return &provider, nil
}

func (s *Store) UpdateSandboxProviderInstance(ctx context.Context, provider *model.SandboxProviderInstance) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Save(provider).Error
}

func (s *Store) DeleteSandboxProviderInstance(ctx context.Context, projectID, providerID string) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	result := write.Where("project_id = ? AND id = ?", projectID, providerID).Delete(&model.SandboxProviderInstance{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CreateWorker(ctx context.Context, worker *model.Worker) error {
	_, err := withResourceEvent(ctx, s, model.EventActionCreated, func(tx *gorm.DB) (*model.Worker, error) {
		if err := tx.Create(worker).Error; err != nil {
			return nil, err
		}
		return worker, nil
	})
	return err
}

func (s *Store) CreateWorkerBootstrapToken(ctx context.Context, token *model.WorkerBootstrapToken) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Create(token).Error
}

func (s *Store) CreateWorkerWithBootstrapToken(ctx context.Context, worker *model.Worker, token *model.WorkerBootstrapToken) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(worker).Error; err != nil {
			return err
		}
		return tx.Create(token).Error
	})
}

func (s *Store) ListWorkers(ctx context.Context, projectID, providerID string) ([]model.Worker, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var workers []model.Worker
	query := read.Where("project_id = ?", projectID)
	if providerID != "" {
		query = query.Where("provider_instance_id = ?", providerID)
	}
	err = query.Order("created_at ASC").Find(&workers).Error
	return workers, err
}

func (s *Store) GetWorker(ctx context.Context, workerID string, options ...WorkerGetOption) (*model.Worker, error) {
	var opts workerGetOptions
	for _, option := range options {
		if option != nil {
			option(&opts)
		}
	}

	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var worker model.Worker
	query := read.Where("id = ?", workerID)
	if opts.generation != nil {
		query = query.Where("generation = ?", *opts.generation)
	}
	if err := query.First(&worker).Error; err != nil {
		if opts.generation != nil && errors.Is(mapNotFound(err), ErrNotFound) {
			return nil, ErrGenerationConflict
		}
		return nil, mapNotFound(err)
	}
	return &worker, nil
}

func (s *Store) UpdateWorker(ctx context.Context, worker *model.Worker, options ...WorkerGetOption) error {
	var opts workerGetOptions
	for _, option := range options {
		if option != nil {
			option(&opts)
		}
	}

	_, err := withResourceEvent(ctx, s, model.EventActionUpdated, func(tx *gorm.DB) (*model.Worker, error) {
		if opts.generation == nil {
			if err := tx.Save(worker).Error; err != nil {
				return nil, err
			}
			return worker, nil
		}

		result := tx.Model(&model.Worker{}).
			Where("id = ? AND generation = ?", worker.ID, *opts.generation).
			Select("*").
			Updates(worker)
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected == 0 {
			return nil, ErrGenerationConflict
		}
		return worker, nil
	})
	return err
}

func (s *Store) RegisterWorker(ctx context.Context, tenantID, workerID string, tokenHash []byte, publicKey, keyType string, authTokenHash []byte, authExpiresAt time.Time) (*model.Worker, error) {
	write, err := s.getWrite(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	var worker model.Worker
	err = write.Transaction(func(tx *gorm.DB) error {
		var token model.WorkerBootstrapToken
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND worker_id = ? AND token_hash = ?", tenantID, workerID, tokenHash).First(&token).Error; err != nil {
			return mapNotFound(err)
		}
		if token.UsedAt != nil || token.RevokedAt != nil || !token.ExpiresAt.After(now) {
			return ErrNotFound
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&worker, "id = ? AND tenant_id = ?", workerID, tenantID).Error; err != nil {
			return mapNotFound(err)
		}
		token.UsedAt = &now
		if err := tx.Save(&token).Error; err != nil {
			return err
		}
		worker.PublicKey = publicKey
		worker.KeyType = keyType
		worker.Ready = true
		worker.Schedulable = true
		worker.Degraded = false
		worker.RegisteredAt = &now
		worker.LastSeenAt = &now
		worker.Phase = model.WorkerPhaseActive
		worker.LastOperationStatus = model.OperationStatusSuccess
		worker.ObservedGeneration = worker.Generation
		if err := tx.Save(&worker).Error; err != nil {
			return err
		}
		auth := &model.WorkerAuthToken{TenantID: tenantID, WorkerID: workerID, TokenHash: authTokenHash, IssuedAt: now, ExpiresAt: authExpiresAt}
		return tx.Create(auth).Error
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &worker, nil
}

func (s *Store) ValidateWorkerAuthToken(ctx context.Context, tenantID, workerID string, tokenHash []byte) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	var token model.WorkerAuthToken
	err = write.
		Where("tenant_id = ? AND worker_id = ? AND token_hash = ? AND expires_at > ? AND revoked_at IS NULL", tenantID, workerID, tokenHash, now).
		First(&token).Error
	if err != nil {
		return mapNotFound(err)
	}
	token.LastUsedAt = &now
	return write.Save(&token).Error
}

func (s *Store) UpdateWorkerStatus(ctx context.Context, tenantID, workerID string, ready, schedulable, degraded bool, availableCPUVCPUs float64, availableMemoryBytes, availableStorageBytes int64, conditions []byte) (*model.Worker, error) {
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
	var worker model.Worker
	err = write.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&worker, "id = ? AND tenant_id = ?", workerID, tenantID).Error; err != nil {
			return mapNotFound(err)
		}
		worker.Ready = ready
		worker.Schedulable = schedulable
		worker.Degraded = degraded
		worker.AvailableCPUVCPUs = availableCPUVCPUs
		worker.AvailableMemoryBytes = availableMemoryBytes
		worker.AvailableStorageBytes = availableStorageBytes
		worker.Conditions = conditions
		worker.LastSeenAt = &now
		if ready {
			worker.Phase = model.WorkerPhaseActive
		}
		return tx.Save(&worker).Error
	})
	if err != nil {
		return nil, err
	}
	return &worker, nil
}

func (s *Store) FindSchedulableWorker(ctx context.Context, sandbox *model.Sandbox) (*model.Worker, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	if sandbox == nil || sandbox.ProviderInstanceID == nil {
		return nil, ErrNotFound
	}
	cpuVCPUs := sandbox.CPUVCPUs
	if cpuVCPUs <= 0 {
		cpuVCPUs = 1
	}
	memoryBytes := sandbox.MemoryBytes
	if memoryBytes < 0 {
		memoryBytes = 0
	}
	storageBytes := sandbox.StorageBytes
	if storageBytes < 0 {
		storageBytes = 0
	}
	var candidates []model.Worker
	err = read.
		Where("project_id = ? AND provider_instance_id = ? AND desired_state = ? AND ready = ? AND schedulable = ? AND available_cpu_vcpus >= ? AND available_memory_bytes >= ? AND available_storage_bytes >= ? AND revoked_at IS NULL", sandbox.ProjectID, *sandbox.ProviderInstanceID, model.WorkerDesiredStateActive, true, true, cpuVCPUs, memoryBytes, storageBytes).
		Order("RANDOM()").
		Limit(2).
		Find(&candidates).Error
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, ErrNotFound
	}
	best := candidates[0]
	for _, candidate := range candidates[1:] {
		if workerLessLoaded(candidate, best, cpuVCPUs, memoryBytes, storageBytes) {
			best = candidate
		}
	}
	return &best, nil
}

func workerLessLoaded(candidate, current model.Worker, cpuVCPUs float64, memoryBytes, storageBytes int64) bool {
	if candidate.Degraded != current.Degraded {
		return !candidate.Degraded
	}
	candidateScore := workerResourceScore(candidate, cpuVCPUs, memoryBytes, storageBytes)
	currentScore := workerResourceScore(current, cpuVCPUs, memoryBytes, storageBytes)
	if candidateScore != currentScore {
		return candidateScore > currentScore
	}
	if candidate.LastSeenAt != nil && current.LastSeenAt != nil && !candidate.LastSeenAt.Equal(*current.LastSeenAt) {
		return candidate.LastSeenAt.After(*current.LastSeenAt)
	}
	if candidate.LastSeenAt != nil && current.LastSeenAt == nil {
		return true
	}
	if candidate.LastSeenAt == nil && current.LastSeenAt != nil {
		return false
	}
	return candidate.CreatedAt.Before(current.CreatedAt)
}

func workerResourceScore(worker model.Worker, cpuVCPUs float64, memoryBytes, storageBytes int64) float64 {
	cpuRemainder := worker.AvailableCPUVCPUs - cpuVCPUs
	memoryRemainder := float64(worker.AvailableMemoryBytes - memoryBytes)
	storageRemainder := float64(worker.AvailableStorageBytes - storageBytes)
	return cpuRemainder + memoryRemainder/float64(1<<30) + storageRemainder/float64(1<<30)
}
