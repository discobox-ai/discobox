package orchestration

import (
	"context"
	"time"
)

// QueueConfig controls enqueue-time defaults.
type QueueConfig struct {
	// DefaultPriority is used when a payload does not implement Prioritized.
	// Higher priority jobs are claimed before lower priority jobs.
	DefaultPriority int

	// DefaultMaxAttempts is used when a payload does not implement MaxAttempter.
	// Values less than one should be treated as one.
	DefaultMaxAttempts int

	// ResourceBackoffThreshold is the number of recently appended jobs for the
	// same type/resource allowed before stores should delay newly appended jobs.
	// A zero value uses the default. A negative value disables submission
	// backoff.
	ResourceBackoffThreshold int

	// ResourceBackoffWindow is the recent-job window used for resource
	// submission backoff. A zero value uses the default.
	ResourceBackoffWindow time.Duration

	// ResourceBackoffBaseDelay is the first delay after threshold. A zero value
	// uses the default.
	ResourceBackoffBaseDelay time.Duration

	// ResourceBackoffMaxDelay caps resource submission backoff. A zero value
	// uses the default.
	ResourceBackoffMaxDelay time.Duration
}

// Queue persists application-owned payloads as durable jobs.
//
// Queue does not know concrete job types. It only inspects the Payload protocol
// and optional behavior interfaces, then stores the JSON representation.
type Queue struct {
	store      Store
	cfg        QueueConfig
	notifyFunc func()
}

// JobStore is the minimal persistence capability needed to append a job.
type JobStore interface {
	CreateJob(context.Context, *Job, ...CreateJobOption) error
	CountRecentJobsForResource(context.Context, Type, Resource, time.Time) (int, error)
}

// NewQueue creates a queue over the given Store.
func NewQueue(store Store, cfg QueueConfig) *Queue {
	return &Queue{store: store, cfg: cfg}
}

// SetNotifyFunc registers a callback invoked after a job is created.
//
// Dispatchers use this to wake up immediately instead of waiting for a poll.
// The callback should be non-blocking or very cheap because Enqueue calls it
// after persistence succeeds.
func (q *Queue) SetNotifyFunc(fn func()) {
	q.notifyFunc = fn
}

// Enqueue appends a new durable job for the given payload.
func (q *Queue) Enqueue(ctx context.Context, payload Payload) (*Job, error) {
	job, _, err := AppendJob(ctx, q.store, payload, q.cfg)
	if err != nil {
		return nil, err
	}

	if q.notifyFunc != nil {
		q.notifyFunc()
	}
	return job, nil
}

// AppendJob converts payload into a Job and persists it as a new pending row.
// Existing job rows are never rewritten or reused.
func AppendJob(ctx context.Context, store JobStore, payload Payload, cfg QueueConfig) (*Job, bool, error) {
	return AppendJobWithOptions(ctx, store, payload, cfg)
}

// AppendJobWithOptions converts payload into a Job and persists it with the
// supplied store create options.
func AppendJobWithOptions(ctx context.Context, store JobStore, payload Payload, cfg QueueConfig, opts ...CreateJobOption) (*Job, bool, error) {
	job, err := JobFromPayload(payload, cfg)
	if err != nil {
		return nil, false, err
	}
	if err := ApplyResourceBackoff(ctx, store, job, time.Now()); err != nil {
		return nil, false, err
	}
	if err := store.CreateJob(ctx, job, opts...); err != nil {
		return nil, false, err
	}
	return job, true, nil
}
