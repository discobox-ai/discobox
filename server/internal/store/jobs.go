package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/orchestration"
)

const leaderID = "default"

// CreateJob persists a new pending job.
func (s *Store) CreateJob(ctx context.Context, job *orchestration.Job, options ...orchestration.CreateJobOption) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	opts := orchestration.ResolveCreateJobOptions(options...)
	now := time.Now()
	if job.ID == "" {
		var err error
		job.ID, err = orchestration.NewID()
		if err != nil {
			return err
		}
	}
	if job.Status == "" {
		job.Status = orchestration.StatusPending
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
		key := resourceKey(job.Resource)
		activeResourceKey = &key
	}

	projectID, err := projectIDForJob(ctx, write, job)
	if err != nil {
		return err
	}
	row := rowFromJob(job, projectID, activeResourceKey)
	if err := write.Create(row).Error; err != nil {
		if isUniqueConstraintError(err) {
			return orchestration.ErrJobAlreadyExists
		}
		return err
	}
	*job = row.toJob()
	return nil
}

// GetJob loads one job by ID.
func (s *Store) GetJob(ctx context.Context, id string) (*orchestration.Job, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	row, err := firstByID[jobRow](read, "id", id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, orchestration.ErrJobNotFound
		}
		return nil, err
	}
	job := row.toJob()
	return &job, nil
}

// GetLatestJobForResource returns the newest job for a resource.
func (s *Store) GetLatestJobForResource(ctx context.Context, resource orchestration.Resource) (*orchestration.Job, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var row jobRow
	if err := read.
		Where("resource_type = ? AND resource_id = ?", resource.Type, resource.ID).
		Order("created_at DESC").
		First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, orchestration.ErrJobNotFound
		}
		return nil, err
	}
	job := row.toJob()
	return &job, nil
}

// CountRecentJobsForResource counts jobs recently appended for a resource.
func (s *Store) CountRecentJobsForResource(ctx context.Context, jobType orchestration.Type, resource orchestration.Resource, since time.Time) (int, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return 0, err
	}
	var count int64
	if err := read.Model(&jobRow{}).
		Where("type = ? AND resource_type = ? AND resource_id = ? AND created_at >= ?",
			jobType,
			resource.Type,
			resource.ID,
			since,
		).
		Count(&count).Error; err != nil {
		return 0, err
	}
	return int(count), nil
}

// ListJobsForProject returns durable jobs associated with resources in a project.
func (s *Store) ListJobsForProject(ctx context.Context, projectID string) ([]orchestration.Job, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var rows []jobRow
	if err := read.
		Where("project_id = ?", projectID).
		Order("created_at DESC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	jobs := make([]orchestration.Job, 0, len(rows))
	for _, row := range rows {
		jobs = append(jobs, row.toJob())
	}
	return jobs, nil
}

// GetJobForProject loads one durable job when its resource belongs to the project.
func (s *Store) GetJobForProject(ctx context.Context, projectID, jobID string) (*orchestration.Job, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	row, err := firstByID[jobRow](read.Where("project_id = ?", projectID), "id", jobID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, orchestration.ErrJobNotFound
		}
		return nil, err
	}
	job := row.toJob()
	return &job, nil
}

// ForceJobForProject makes a delayed pending/backoff job runnable immediately.
func (s *Store) ForceJobForProject(ctx context.Context, projectID, jobID string) (*orchestration.Job, error) {
	write, err := s.getWrite(ctx)
	if err != nil {
		return nil, err
	}
	var out *orchestration.Job
	err = write.Transaction(func(tx *gorm.DB) error {
		var row jobRow
		if err := tx.Where("project_id = ?", projectID).First(&row, "id = ?", jobID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return orchestration.ErrJobNotFound
			}
			return err
		}
		now := time.Now()
		if row.Status == orchestration.StatusPending || row.Status == orchestration.StatusBackoff {
			if err := tx.Model(&jobRow{}).
				Where("id = ? AND project_id = ?", jobID, projectID).
				Updates(map[string]any{
					"status":       orchestration.StatusPending,
					"scheduled_at": now,
					"updated_at":   now,
				}).Error; err != nil {
				return err
			}
			if err := tx.First(&row, "id = ?", jobID).Error; err != nil {
				return err
			}
		}
		job := row.toJob()
		out = &job
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// HasActiveJobForResource reports whether any pending, backoff, or running job
// exists for the resource, regardless of job type.
func (s *Store) HasActiveJobForResource(ctx context.Context, resource orchestration.Resource) (bool, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return false, err
	}
	var count int64
	err = read.Model(&jobRow{}).
		Where("resource_type = ? AND resource_id = ? AND status IN ?",
			resource.Type,
			resource.ID,
			[]orchestration.Status{orchestration.StatusPending, orchestration.StatusBackoff, orchestration.StatusRunning},
		).
		Count(&count).Error
	return count > 0, err
}

// ClaimJob atomically claims one runnable job of the given types.
func (s *Store) ClaimJob(ctx context.Context, types []orchestration.Type, workerID string) (*orchestration.Job, error) {
	if len(types) == 0 {
		return nil, nil
	}
	write, err := s.getWrite(ctx)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	var claimed *orchestration.Job

	err = write.Transaction(func(tx *gorm.DB) error {
		var candidates []jobRow
		if err := tx.
			Where("type IN ? AND status IN ? AND scheduled_at <= ?", types, []orchestration.Status{orchestration.StatusPending, orchestration.StatusBackoff}, now).
			Order("priority DESC, scheduled_at ASC, created_at ASC").
			Limit(50).
			Find(&candidates).Error; err != nil {
			return err
		}

		for _, candidate := range candidates {
			if candidate.ResourceType != "" || candidate.ResourceID != "" {
				var running int64
				if err := tx.Model(&jobRow{}).
					Where("resource_type = ? AND resource_id = ? AND status = ? AND id != ?",
						candidate.ResourceType,
						candidate.ResourceID,
						orchestration.StatusRunning,
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
				Where("id = ? AND status IN ?", candidate.ID, []orchestration.Status{orchestration.StatusPending, orchestration.StatusBackoff}).
				Updates(map[string]any{
					"status":              orchestration.StatusRunning,
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
			if err := tx.First(&row, "id = ?", candidate.ID).Error; err != nil {
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
	now := time.Now()
	return write.Model(&jobRow{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"status":              orchestration.StatusCompleted,
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
	now := time.Now()
	return write.Model(&jobRow{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"status":              orchestration.StatusCanceled,
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
	return write.Transaction(func(tx *gorm.DB) error {
		var row jobRow
		if err := tx.First(&row, "id = ?", id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return orchestration.ErrJobNotFound
			}
			return err
		}

		now := time.Now()
		if row.Attempts < row.MaxAttempts {
			var activeResourceKey *string
			if row.ResourceType != "" && row.ResourceID != "" {
				key := resourceKey(orchestration.Resource{Type: row.ResourceType, ID: row.ResourceID})
				activeResourceKey = &key
			}
			return tx.Model(&jobRow{}).
				Where("id = ?", id).
				Updates(map[string]any{
					"status":              orchestration.StatusPending,
					"active_resource_key": activeResourceKey,
					"error":               message,
					"worker_id":           nil,
					"started_at":          nil,
					"scheduled_at":        now.Add(retryBackoff),
					"updated_at":          now,
				}).Error
		}

		return tx.Model(&jobRow{}).
			Where("id = ?", id).
			Updates(map[string]any{
				"status":              orchestration.StatusFailed,
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
	cutoff := time.Now().Add(-staleAfter)
	result := write.Model(&jobRow{}).
		Where("status = ? AND started_at <= ?", orchestration.StatusRunning, cutoff).
		Updates(map[string]any{
			"status":     orchestration.StatusPending,
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
	now := time.Now()
	cutoff := now.Add(-timeout)
	acquired := false

	err = write.Transaction(func(tx *gorm.DB) error {
		var row leaderRow
		err := tx.First(&row, "id = ?", leaderID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if err := tx.Create(&leaderRow{
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
			if err := tx.Model(&leaderRow{}).Where("id = ?", leaderID).Updates(updates).Error; err != nil {
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
	return write.
		Where("id = ? AND worker_id = ?", leaderID, workerID).
		Delete(&leaderRow{}).Error
}

type jobRow struct {
	ID                string               `gorm:"primaryKey;type:text"`
	ProjectID         string               `gorm:"column:project_id;type:text;index"`
	Type              orchestration.Type   `gorm:"not null;type:text;index:idx_jobqueue_ready,priority:2"`
	Payload           json.RawMessage      `gorm:"type:text;not null"`
	Status            orchestration.Status `gorm:"not null;type:text;index:idx_jobqueue_ready,priority:1"`
	Priority          int                  `gorm:"not null;default:0;index:idx_jobqueue_priority"`
	Attempts          int                  `gorm:"not null;default:0"`
	MaxAttempts       int                  `gorm:"column:max_attempts;not null;default:1"`
	Error             *string              `gorm:"type:text"`
	WorkerID          *string              `gorm:"column:worker_id;type:text"`
	ScheduledAt       time.Time            `gorm:"column:scheduled_at;not null;index:idx_jobqueue_ready,priority:3"`
	StartedAt         *time.Time           `gorm:"column:started_at"`
	CompletedAt       *time.Time           `gorm:"column:completed_at"`
	ResourceType      string               `gorm:"column:resource_type;type:text;index:idx_jobqueue_resource,priority:1"`
	ResourceID        string               `gorm:"column:resource_id;type:text;index:idx_jobqueue_resource,priority:2"`
	ActiveResourceKey *string              `gorm:"column:active_resource_key;type:text;uniqueIndex:idx_jobqueue_active_resource_key"`
	CreatedAt         time.Time            `gorm:"autoCreateTime;index:idx_jobqueue_ready,priority:4"`
	UpdatedAt         time.Time            `gorm:"autoUpdateTime"`
}

func (jobRow) TableName() string {
	return "jobqueue_jobs"
}

func rowFromJob(job *orchestration.Job, projectID string, activeResourceKey *string) *jobRow {
	return &jobRow{
		ID:                job.ID,
		ProjectID:         projectID,
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

func projectIDForJob(ctx context.Context, db *gorm.DB, job *orchestration.Job) (string, error) {
	projectID, err := projectIDForJobResource(ctx, db, job.Resource)
	if err != nil || projectID != "" {
		return projectID, err
	}
	var payload struct {
		ProjectID string `json:"projectId"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return "", err
	}
	return payload.ProjectID, nil
}

func projectIDForJobResource(ctx context.Context, db *gorm.DB, resource orchestration.Resource) (string, error) {
	if resource.Type == "" || resource.ID == "" {
		return "", nil
	}
	var projectID string
	var err error
	switch resource.Type {
	case "sandbox":
		err = db.WithContext(ctx).Model(&model.Sandbox{}).
			Select("project_id").
			Where("id = ?", resource.ID).
			Scan(&projectID).Error
	case "worker":
		err = db.WithContext(ctx).Model(&model.Worker{}).
			Select("project_id").
			Where("id = ?", resource.ID).
			Scan(&projectID).Error
	case "provider":
		err = db.WithContext(ctx).Model(&model.SandboxProviderInstance{}).
			Select("project_id").
			Where("id = ?", resource.ID).
			Scan(&projectID).Error
	default:
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return projectID, nil
}

func (r jobRow) toJob() orchestration.Job {
	return orchestration.Job{
		ID:          r.ID,
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
		Resource: orchestration.Resource{
			Type: r.ResourceType,
			ID:   r.ResourceID,
		},
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}
}

func resourceKey(resource orchestration.Resource) string {
	return resource.Type + "\x00" + resource.ID
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

func BackfillJobProjectIDs(ctx context.Context, db *gorm.DB) error {
	return db.WithContext(ctx).Exec(`
UPDATE jobqueue_jobs
SET project_id = COALESCE(
	(SELECT project_id FROM sandboxes WHERE jobqueue_jobs.resource_type = 'sandbox' AND sandboxes.id = jobqueue_jobs.resource_id),
	(SELECT project_id FROM workers WHERE jobqueue_jobs.resource_type = 'worker' AND workers.id = jobqueue_jobs.resource_id),
	(SELECT project_id FROM sandbox_provider_instances WHERE jobqueue_jobs.resource_type = 'provider' AND sandbox_provider_instances.id = jobqueue_jobs.resource_id)
)
WHERE project_id IS NULL OR project_id = ''
`).Error
}

type leaderRow struct {
	ID          string    `gorm:"primaryKey;type:text"`
	WorkerID    string    `gorm:"column:worker_id;not null;type:text"`
	HeartbeatAt time.Time `gorm:"column:heartbeat_at;not null"`
	AcquiredAt  time.Time `gorm:"column:acquired_at;not null"`
}

func (leaderRow) TableName() string {
	return "jobqueue_leaders"
}

// JobModels returns durable job queue models.
func JobModels() []any {
	return []any{&jobRow{}, &leaderRow{}}
}
