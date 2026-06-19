package orchestration

import (
	"context"
	"time"
)

// Store is the persistence boundary for Queue and Dispatcher.
//
// Implementations are expected to make ClaimJob atomic across concurrent
// dispatchers.
type Store interface {
	// CreateJob persists a new pending job. Implementations should assign an ID
	// and timestamps if the caller left them empty. Options control store-level
	// creation behavior such as resource uniqueness.
	CreateJob(ctx context.Context, job *Job, opts ...CreateJobOption) error

	// GetJob loads one job by ID.
	GetJob(ctx context.Context, id string) (*Job, error)

	// GetLatestJobForResource returns the newest job for a resource, regardless
	// of status.
	GetLatestJobForResource(ctx context.Context, resource Resource) (*Job, error)

	// CountRecentJobsForResource counts jobs with the same type and resource
	// created on or after since. Queue uses this to apply core submission
	// backoff before appending a new job.
	CountRecentJobsForResource(ctx context.Context, jobType Type, resource Resource, since time.Time) (int, error)

	// HasActiveJobForResource reports whether any pending, backoff, or running
	// job exists for the resource, regardless of job type.
	HasActiveJobForResource(ctx context.Context, resource Resource) (bool, error)

	// ClaimJob atomically selects one pending or backoff job of any provided type
	// whose scheduled time has arrived and marks it running for workerID. It
	// should return nil, nil when no job is available. It must not claim a job
	// whose resource already has a running job, regardless of the running job's
	// type.
	ClaimJob(ctx context.Context, types []Type, workerID string) (*Job, error)

	// CompleteJob marks a running job completed.
	CompleteJob(ctx context.Context, id string) error

	// CancelJob marks a running job canceled without retrying it.
	CancelJob(ctx context.Context, id string, message string) error

	// FailJob records an execution error. If attempts remain, it should requeue
	// the job as pending with a retry delay of retryBackoff. Otherwise it should
	// mark the job failed.
	FailJob(ctx context.Context, id string, message string, retryBackoff time.Duration) error

	// CleanupStaleJobs resets abandoned running jobs whose StartedAt is older
	// than staleAfter. It returns the number of jobs recovered.
	CleanupStaleJobs(ctx context.Context, staleAfter time.Duration) (int64, error)

	// TryAcquireLeadership attempts to acquire or renew dispatcher leadership for
	// workerID. It returns true when this worker may claim jobs.
	TryAcquireLeadership(ctx context.Context, workerID string, timeout time.Duration) (bool, error)

	// ReleaseLeadership releases leadership if workerID currently owns it.
	ReleaseLeadership(ctx context.Context, workerID string) error
}

// CreateJobOptions controls Store.CreateJob behavior.
type CreateJobOptions struct {
	// UniqueResource requires the store to atomically reject the create when
	// another active job already owns the same non-empty Resource. Stores should
	// release that ownership when the owning job reaches completed or failed.
	UniqueResource bool
}

// CreateJobOption configures Store.CreateJob.
type CreateJobOption func(*CreateJobOptions)

// WithUniqueResource requires active-job uniqueness for the job's Resource.
func WithUniqueResource() CreateJobOption {
	return func(opts *CreateJobOptions) {
		opts.UniqueResource = true
	}
}

// ResolveCreateJobOptions applies CreateJobOption values to their concrete
// options. Store implementations should call this at the start of CreateJob.
func ResolveCreateJobOptions(options ...CreateJobOption) CreateJobOptions {
	var opts CreateJobOptions
	for _, option := range options {
		if option != nil {
			option(&opts)
		}
	}
	return opts
}
