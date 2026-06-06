package jobqueue

import (
	"context"
)

// QueueConfig controls enqueue-time defaults.
type QueueConfig struct {
	// DefaultPriority is used when a payload does not implement Prioritized.
	// Higher priority jobs are claimed before lower priority jobs.
	DefaultPriority int

	// DefaultMaxAttempts is used when a payload does not implement MaxAttempter.
	// Values less than one should be treated as one.
	DefaultMaxAttempts int
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

// Enqueue stores or reuses an active job for the given payload.
//
// Enqueue delegates duplicate policy to Store.EnsureActiveJobForPayload. For
// payloads with a Resource, duplicate active jobs are reused unless the payload
// implements DuplicateAllower and returns true.
func (q *Queue) Enqueue(ctx context.Context, payload Payload) (*Job, error) {
	job, created, err := q.store.EnsureActiveJobForPayload(ctx, payload, q.cfg)
	if err != nil {
		return nil, err
	}

	if created && q.notifyFunc != nil {
		q.notifyFunc()
	}
	return job, nil
}
