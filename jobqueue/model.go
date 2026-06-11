package jobqueue

import (
	"encoding/json"
	"time"
)

// Status is the persisted lifecycle state of a job.
type Status string

const (
	// StatusPending means the job is waiting to be claimed.
	StatusPending Status = "pending"

	// StatusRunning means the job has been claimed by a dispatcher worker.
	StatusRunning Status = "running"

	// StatusCompleted means the job finished successfully.
	StatusCompleted Status = "completed"

	// StatusFailed means the job exhausted its attempts or cannot be processed.
	StatusFailed Status = "failed"

	// StatusCanceled means the job was superseded or intentionally abandoned.
	StatusCanceled Status = "canceled"
)

// Job is the durable representation of queued work.
type Job struct {
	// ID is the stable job identifier assigned by the Store.
	ID string `json:"id"`

	// TenantID identifies the tenant context used to execute this job.
	TenantID string `json:"tenantId,omitempty"`

	// Type selects the registered Executor.
	Type Type `json:"type"`

	// Payload is the JSON-encoded application payload.
	Payload json.RawMessage `json:"payload"`

	// Status is the persisted lifecycle state.
	Status Status `json:"status"`

	// Priority orders claim selection. Higher values run first.
	Priority int `json:"priority"`

	// Attempts is the number of times this job has been claimed for execution.
	Attempts int `json:"attempts"`

	// MaxAttempts is the maximum number of execution attempts before failure.
	MaxAttempts int `json:"maxAttempts"`

	// Error stores the latest execution or dispatch error message.
	Error *string `json:"error,omitempty"`

	// WorkerID identifies the dispatcher that claimed the current or last
	// attempt.
	WorkerID *string `json:"workerId,omitempty"`

	// ScheduledAt is the earliest time the job may be claimed.
	ScheduledAt time.Time `json:"scheduledAt"`

	// StartedAt is set when a dispatcher claims the job.
	StartedAt *time.Time `json:"startedAt,omitempty"`

	// CompletedAt is set when the job reaches a terminal state.
	CompletedAt *time.Time `json:"completedAt,omitempty"`

	// Resource is used for active-job deduplication and execution serialization
	// across job types.
	Resource Resource `json:"resource"`

	// CreatedAt is the storage creation time.
	CreatedAt time.Time `json:"createdAt"`

	// UpdatedAt is the storage update time.
	UpdatedAt time.Time `json:"updatedAt"`
}

// Leader is the durable ownership record used by multi-process dispatchers.
type Leader struct {
	// ID identifies the singleton leadership row or named leadership scope.
	ID string `json:"id"`

	// WorkerID is the dispatcher instance currently holding leadership.
	WorkerID string `json:"workerId"`

	// HeartbeatAt is the last successful leadership renewal time.
	HeartbeatAt time.Time `json:"heartbeatAt"`

	// AcquiredAt is when the current worker first acquired leadership.
	AcquiredAt time.Time `json:"acquiredAt"`
}
