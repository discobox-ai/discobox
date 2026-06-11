package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/obot-platform/disco2/jobqueue"
)

const leaderID = "default"

// CreateJob persists a new pending job.
func (s *Store) CreateJob(ctx context.Context, job *jobqueue.Job, options ...jobqueue.CreateJobOption) error {
	tenantID, err := s.tenantID(ctx)
	if err != nil {
		return err
	}
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	opts := jobqueue.ResolveCreateJobOptions(options...)
	now := time.Now()
	job.TenantID = tenantID
	if job.ID == "" {
		var err error
		job.ID, err = jobqueue.NewID()
		if err != nil {
			return err
		}
	}
	if job.Status == "" {
		job.Status = jobqueue.StatusPending
	}
	if job.MaxAttempts < 1 {
		job.MaxAttempts = 1
	}
	if job.ScheduledAt.IsZero() {
		job.ScheduledAt = now
	}
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	job.UpdatedAt = now

	var activeResourceKey *string
	if opts.UniqueResource && job.Resource.Type != "" && job.Resource.ID != "" {
		key := tenantResourceKey(tenantID, job.Resource)
		activeResourceKey = &key
	}

	row := rowFromJob(job, activeResourceKey)
	if err := write.Create(row).Error; err != nil {
		if isUniqueConstraintError(err) {
			return jobqueue.ErrJobAlreadyExists
		}
		return err
	}
	*job = row.toJob()
	return nil
}

// EnsureActiveJobForPayload returns an active job for payload's resource,
// creating one when needed. This is intended for database-first orchestration
// paths that need the job row to be part of a larger transaction.
func (s *Store) EnsureActiveJobForPayload(ctx context.Context, payload jobqueue.Payload, cfg jobqueue.QueueConfig) (*jobqueue.Job, bool, error) {
	resource := payload.Resource()
	allowDuplicates := false
	if d, ok := payload.(jobqueue.DuplicateAllower); ok {
		allowDuplicates = d.AllowDuplicates()
	}

	if !allowDuplicates && resource.Type != "" && resource.ID != "" {
		job, err := s.getLatestPendingJobForResource(ctx, resource)
		if err == nil {
			updated, updateErr := s.updatePendingJobFromPayload(ctx, job.ID, payload, cfg)
			if updateErr != nil {
				return nil, false, updateErr
			}
			return updated, false, nil
		}
		if !errors.Is(err, jobqueue.ErrJobNotFound) {
			return nil, false, err
		}
	}

	job, err := jobqueue.JobFromPayload(payload, cfg)
	if err != nil {
		return nil, false, err
	}

	var opts []jobqueue.CreateJobOption
	if !allowDuplicates && resource.Type != "" && resource.ID != "" {
		opts = append(opts, jobqueue.WithUniqueResource())
	}
	if err := s.CreateJob(ctx, job, opts...); err != nil {
		if errors.Is(err, jobqueue.ErrJobAlreadyExists) && resource.Type != "" && resource.ID != "" {
			existing, getErr := s.getLatestPendingJobForResource(ctx, resource)
			if getErr != nil {
				return nil, false, getErr
			}
			updated, updateErr := s.updatePendingJobFromPayload(ctx, existing.ID, payload, cfg)
			if updateErr != nil {
				return nil, false, updateErr
			}
			return updated, false, nil
		}
		return nil, false, err
	}
	return job, true, nil
}

// GetJob loads one job by ID.
func (s *Store) GetJob(ctx context.Context, id string) (*jobqueue.Job, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	tenantID, err := s.tenantID(ctx)
	if err != nil {
		return nil, err
	}
	var row jobRow
	if err := read.First(&row, "tenant_id = ? AND id = ?", tenantID, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, jobqueue.ErrJobNotFound
		}
		return nil, err
	}
	job := row.toJob()
	return &job, nil
}

// GetLatestJobForResource returns the newest job for a resource.
func (s *Store) GetLatestJobForResource(ctx context.Context, resource jobqueue.Resource) (*jobqueue.Job, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	tenantID, err := s.tenantID(ctx)
	if err != nil {
		return nil, err
	}
	var row jobRow
	if err := read.
		Where("tenant_id = ? AND resource_type = ? AND resource_id = ?", tenantID, resource.Type, resource.ID).
		Order("created_at DESC").
		First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, jobqueue.ErrJobNotFound
		}
		return nil, err
	}
	job := row.toJob()
	return &job, nil
}

// HasActiveJobForResource reports whether any pending or running job exists for
// the resource, regardless of job type.
func (s *Store) HasActiveJobForResource(ctx context.Context, resource jobqueue.Resource) (bool, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return false, err
	}
	tenantID, err := s.tenantID(ctx)
	if err != nil {
		return false, err
	}
	var count int64
	err = read.Model(&jobRow{}).
		Where("tenant_id = ? AND resource_type = ? AND resource_id = ? AND status IN ?",
			tenantID,
			resource.Type,
			resource.ID,
			[]jobqueue.Status{jobqueue.StatusPending, jobqueue.StatusRunning},
		).
		Count(&count).Error
	return count > 0, err
}

func (s *Store) getLatestPendingJobForResource(ctx context.Context, resource jobqueue.Resource) (*jobqueue.Job, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	tenantID, err := s.tenantID(ctx)
	if err != nil {
		return nil, err
	}
	var row jobRow
	if err := read.
		Where("tenant_id = ? AND resource_type = ? AND resource_id = ? AND status = ?",
			tenantID,
			resource.Type,
			resource.ID,
			jobqueue.StatusPending,
		).
		Order("created_at DESC").
		First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, jobqueue.ErrJobNotFound
		}
		return nil, err
	}
	job := row.toJob()
	return &job, nil
}

func (s *Store) updatePendingJobFromPayload(ctx context.Context, id string, payload jobqueue.Payload, cfg jobqueue.QueueConfig) (*jobqueue.Job, error) {
	write, err := s.getWrite(ctx)
	if err != nil {
		return nil, err
	}
	tenantID, err := s.tenantID(ctx)
	if err != nil {
		return nil, err
	}
	job, err := jobqueue.JobFromPayload(payload, cfg)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	result := write.Model(&jobRow{}).
		Where("tenant_id = ? AND id = ? AND status = ?", tenantID, id, jobqueue.StatusPending).
		Updates(map[string]any{
			"type":          job.Type,
			"payload":       job.Payload,
			"priority":      job.Priority,
			"max_attempts":  job.MaxAttempts,
			"scheduled_at":  job.ScheduledAt,
			"resource_type": job.Resource.Type,
			"resource_id":   job.Resource.ID,
			"updated_at":    now,
		})
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, jobqueue.ErrJobNotFound
	}
	return s.GetJob(ctx, id)
}

// ClaimJob atomically claims one runnable job of the given types.
func (s *Store) ClaimJob(ctx context.Context, types []jobqueue.Type, workerID string) (*jobqueue.Job, error) {
	if len(types) == 0 {
		return nil, nil
	}
	write, err := s.getWrite(ctx)
	if err != nil {
		return nil, err
	}
	tenantID, err := s.tenantID(ctx)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	var claimed *jobqueue.Job

	err = write.Transaction(func(tx *gorm.DB) error {
		var candidates []jobRow
		if err := tx.
			Where("tenant_id = ? AND type IN ? AND status = ? AND scheduled_at <= ?", tenantID, types, jobqueue.StatusPending, now).
			Order("priority DESC, scheduled_at ASC, created_at ASC").
			Limit(50).
			Find(&candidates).Error; err != nil {
			return err
		}

		for _, candidate := range candidates {
			if candidate.ResourceType != "" || candidate.ResourceID != "" {
				var running int64
				if err := tx.Model(&jobRow{}).
					Where("tenant_id = ? AND resource_type = ? AND resource_id = ? AND status = ? AND id != ?",
						tenantID,
						candidate.ResourceType,
						candidate.ResourceID,
						jobqueue.StatusRunning,
						candidate.ID,
					).
					Count(&running).Error; err != nil {
					return err
				}
				if running > 0 {
					continue
				}
			}

			startedAt := now
			result := tx.Model(&jobRow{}).
				Where("tenant_id = ? AND id = ? AND status = ?", tenantID, candidate.ID, jobqueue.StatusPending).
				Updates(map[string]any{
					"status":              jobqueue.StatusRunning,
					"worker_id":           workerID,
					"started_at":          startedAt,
					"attempts":            gorm.Expr("attempts + ?", 1),
					"active_resource_key": nil,
					"updated_at":          now,
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				continue
			}

			var row jobRow
			if err := tx.First(&row, "tenant_id = ? AND id = ?", tenantID, candidate.ID).Error; err != nil {
				return err
			}
			job := row.toJob()
			claimed = &job
			return nil
		}

		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// CompleteJob marks a job completed.
func (s *Store) CompleteJob(ctx context.Context, id string) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	tenantID, err := s.tenantID(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	return write.Model(&jobRow{}).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		Updates(map[string]any{
			"status":              jobqueue.StatusCompleted,
			"active_resource_key": nil,
			"completed_at":        now,
			"updated_at":          now,
		}).Error
}

// CancelJob marks a job canceled.
func (s *Store) CancelJob(ctx context.Context, id string, message string) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	tenantID, err := s.tenantID(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	return write.Model(&jobRow{}).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		Updates(map[string]any{
			"status":              jobqueue.StatusCanceled,
			"active_resource_key": nil,
			"error":               message,
			"completed_at":        now,
			"updated_at":          now,
		}).Error
}

// FailJob records a failed attempt and requeues when attempts remain.
func (s *Store) FailJob(ctx context.Context, id string, message string, retryBackoff time.Duration) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	tenantID, err := s.tenantID(ctx)
	if err != nil {
		return err
	}
	return write.Transaction(func(tx *gorm.DB) error {
		var row jobRow
		if err := tx.First(&row, "tenant_id = ? AND id = ?", tenantID, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return jobqueue.ErrJobNotFound
			}
			return err
		}

		now := time.Now()
		if row.Attempts < row.MaxAttempts {
			delay := time.Duration(row.Attempts) * retryBackoff
			var pendingResourceKey *string
			if row.ResourceType != "" && row.ResourceID != "" {
				key := tenantResourceKey(tenantID, jobqueue.Resource{Type: row.ResourceType, ID: row.ResourceID})
				pendingResourceKey = &key
			}
			return tx.Model(&jobRow{}).
				Where("tenant_id = ? AND id = ?", tenantID, id).
				Updates(map[string]any{
					"status":              jobqueue.StatusPending,
					"active_resource_key": pendingResourceKey,
					"error":               message,
					"worker_id":           nil,
					"started_at":          nil,
					"scheduled_at":        now.Add(delay),
					"updated_at":          now,
				}).Error
		}

		return tx.Model(&jobRow{}).
			Where("tenant_id = ? AND id = ?", tenantID, id).
			Updates(map[string]any{
				"status":              jobqueue.StatusFailed,
				"active_resource_key": nil,
				"error":               message,
				"completed_at":        now,
				"updated_at":          now,
			}).Error
	})
}

// CleanupStaleJobs resets abandoned running jobs.
func (s *Store) CleanupStaleJobs(ctx context.Context, staleAfter time.Duration) (int64, error) {
	write, err := s.getWrite(ctx)
	if err != nil {
		return 0, err
	}
	tenantID, err := s.tenantID(ctx)
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-staleAfter)
	result := write.Model(&jobRow{}).
		Where("tenant_id = ? AND status = ? AND started_at <= ?", tenantID, jobqueue.StatusRunning, cutoff).
		Updates(map[string]any{
			"status":     jobqueue.StatusPending,
			"worker_id":  nil,
			"started_at": nil,
			"updated_at": time.Now(),
		})
	return result.RowsAffected, result.Error
}

// TryAcquireLeadership attempts to acquire or renew dispatcher leadership.
func (s *Store) TryAcquireLeadership(ctx context.Context, workerID string, timeout time.Duration) (bool, error) {
	write, err := s.getWrite(ctx)
	if err != nil {
		return false, err
	}
	tenantID, err := s.tenantID(ctx)
	if err != nil {
		return false, err
	}
	now := time.Now()
	cutoff := now.Add(-timeout)
	acquired := false

	err = write.Transaction(func(tx *gorm.DB) error {
		var row leaderRow
		err := tx.First(&row, "tenant_id = ? AND id = ?", tenantID, leaderID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if err := tx.Create(&leaderRow{
				TenantID:    tenantID,
				ID:          leaderID,
				WorkerID:    workerID,
				HeartbeatAt: now,
				AcquiredAt:  now,
			}).Error; err != nil {
				return err
			}
			acquired = true
			return nil
		}
		if err != nil {
			return err
		}

		if row.WorkerID == workerID || row.HeartbeatAt.Before(cutoff) {
			updates := map[string]any{
				"worker_id":    workerID,
				"heartbeat_at": now,
			}
			if row.WorkerID != workerID {
				updates["acquired_at"] = now
			}
			if err := tx.Model(&leaderRow{}).Where("tenant_id = ? AND id = ?", tenantID, leaderID).Updates(updates).Error; err != nil {
				return err
			}
			acquired = true
		}

		return nil
	})
	return acquired, err
}

// ReleaseLeadership releases leadership if workerID owns it.
func (s *Store) ReleaseLeadership(ctx context.Context, workerID string) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	tenantID, err := s.tenantID(ctx)
	if err != nil {
		return err
	}
	return write.
		Where("tenant_id = ? AND id = ? AND worker_id = ?", tenantID, leaderID, workerID).
		Delete(&leaderRow{}).Error
}

type jobRow struct {
	ID                string          `gorm:"primaryKey;type:text"`
	TenantID          string          `gorm:"column:tenant_id;not null;type:text;index"`
	Type              jobqueue.Type   `gorm:"not null;type:text;index:idx_jobqueue_ready,priority:2"`
	Payload           json.RawMessage `gorm:"type:text;not null"`
	Status            jobqueue.Status `gorm:"not null;type:text;index:idx_jobqueue_ready,priority:1"`
	Priority          int             `gorm:"not null;default:0;index:idx_jobqueue_priority"`
	Attempts          int             `gorm:"not null;default:0"`
	MaxAttempts       int             `gorm:"column:max_attempts;not null;default:1"`
	Error             *string         `gorm:"type:text"`
	WorkerID          *string         `gorm:"column:worker_id;type:text"`
	ScheduledAt       time.Time       `gorm:"column:scheduled_at;not null;index:idx_jobqueue_ready,priority:3"`
	StartedAt         *time.Time      `gorm:"column:started_at"`
	CompletedAt       *time.Time      `gorm:"column:completed_at"`
	ResourceType      string          `gorm:"column:resource_type;type:text;index:idx_jobqueue_resource,priority:1"`
	ResourceID        string          `gorm:"column:resource_id;type:text;index:idx_jobqueue_resource,priority:2"`
	ActiveResourceKey *string         `gorm:"column:active_resource_key;type:text;uniqueIndex:idx_jobqueue_active_resource_key"`
	CreatedAt         time.Time       `gorm:"autoCreateTime;index:idx_jobqueue_ready,priority:4"`
	UpdatedAt         time.Time       `gorm:"autoUpdateTime"`
}

func (jobRow) TableName() string {
	return "jobqueue_jobs"
}

func rowFromJob(job *jobqueue.Job, activeResourceKey *string) *jobRow {
	return &jobRow{
		ID:                job.ID,
		TenantID:          job.TenantID,
		Type:              job.Type,
		Payload:           job.Payload,
		Status:            job.Status,
		Priority:          job.Priority,
		Attempts:          job.Attempts,
		MaxAttempts:       job.MaxAttempts,
		Error:             job.Error,
		WorkerID:          job.WorkerID,
		ScheduledAt:       job.ScheduledAt,
		StartedAt:         job.StartedAt,
		CompletedAt:       job.CompletedAt,
		ResourceType:      job.Resource.Type,
		ResourceID:        job.Resource.ID,
		ActiveResourceKey: activeResourceKey,
		CreatedAt:         job.CreatedAt,
		UpdatedAt:         job.UpdatedAt,
	}
}

func (r jobRow) toJob() jobqueue.Job {
	return jobqueue.Job{
		ID:          r.ID,
		TenantID:    r.TenantID,
		Type:        r.Type,
		Payload:     r.Payload,
		Status:      r.Status,
		Priority:    r.Priority,
		Attempts:    r.Attempts,
		MaxAttempts: r.MaxAttempts,
		Error:       r.Error,
		WorkerID:    r.WorkerID,
		ScheduledAt: r.ScheduledAt,
		StartedAt:   r.StartedAt,
		CompletedAt: r.CompletedAt,
		Resource: jobqueue.Resource{
			Type: r.ResourceType,
			ID:   r.ResourceID,
		},
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}
}

func resourceKey(resource jobqueue.Resource) string {
	return resource.Type + "\x00" + resource.ID
}

func tenantResourceKey(tenantID string, resource jobqueue.Resource) string {
	return tenantID + "\x00" + resourceKey(resource)
}

func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint failed") ||
		strings.Contains(msg, "duplicate key value") ||
		strings.Contains(msg, "violates unique constraint")
}

type leaderRow struct {
	TenantID    string    `gorm:"column:tenant_id;primaryKey;type:text"`
	ID          string    `gorm:"primaryKey;type:text"`
	WorkerID    string    `gorm:"column:worker_id;not null;type:text"`
	HeartbeatAt time.Time `gorm:"column:heartbeat_at;not null"`
	AcquiredAt  time.Time `gorm:"column:acquired_at;not null"`
}

func (leaderRow) TableName() string {
	return "jobqueue_leaders"
}

// JobModels returns tenant-local durable job queue models.
func JobModels() []any {
	return []any{&jobRow{}, &leaderRow{}}
}
