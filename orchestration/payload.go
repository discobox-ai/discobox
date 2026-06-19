package orchestration

import (
	"context"
	"encoding/json"
	"time"
)

const (
	defaultResourceBackoffThreshold = 10
	defaultResourceBackoffWindow    = 15 * time.Minute
	defaultResourceBackoffBaseDelay = 30 * time.Second
	defaultResourceBackoffMaxDelay  = 15 * time.Minute
)

// JobFromPayload converts an application payload into a pending durable job
// using the same defaulting rules as Queue.Enqueue.
func JobFromPayload(payload Payload, cfg QueueConfig) (*Job, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	priority := cfg.DefaultPriority
	if p, ok := payload.(Prioritized); ok {
		priority = p.Priority()
	}

	maxAttempts := cfg.DefaultMaxAttempts
	if m, ok := payload.(MaxAttempter); ok {
		maxAttempts = m.MaxAttempts()
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	scheduledAt := time.Now()
	if s, ok := payload.(Schedulable); ok {
		if at := s.ScheduledAt(); !at.IsZero() {
			scheduledAt = at
		}
	}

	return &Job{
		Type:                     payload.JobType(),
		Payload:                  data,
		Status:                   StatusPending,
		Priority:                 priority,
		MaxAttempts:              maxAttempts,
		ScheduledAt:              scheduledAt,
		Resource:                 payload.Resource(),
		ResourceBackoffThreshold: cfg.resourceBackoffThreshold(),
		ResourceBackoffWindow:    cfg.resourceBackoffWindow(),
		ResourceBackoffBaseDelay: cfg.resourceBackoffBaseDelay(),
		ResourceBackoffMaxDelay:  cfg.resourceBackoffMaxDelay(),
	}, nil
}

func (cfg QueueConfig) resourceBackoffThreshold() int {
	if cfg.ResourceBackoffThreshold < 0 {
		return 0
	}
	if cfg.ResourceBackoffThreshold == 0 {
		return defaultResourceBackoffThreshold
	}
	return cfg.ResourceBackoffThreshold
}

func (cfg QueueConfig) resourceBackoffWindow() time.Duration {
	if cfg.ResourceBackoffWindow <= 0 {
		return defaultResourceBackoffWindow
	}
	return cfg.ResourceBackoffWindow
}

func (cfg QueueConfig) resourceBackoffBaseDelay() time.Duration {
	if cfg.ResourceBackoffBaseDelay <= 0 {
		return defaultResourceBackoffBaseDelay
	}
	return cfg.ResourceBackoffBaseDelay
}

func (cfg QueueConfig) resourceBackoffMaxDelay() time.Duration {
	if cfg.ResourceBackoffMaxDelay <= 0 {
		return defaultResourceBackoffMaxDelay
	}
	return cfg.ResourceBackoffMaxDelay
}

// ApplyResourceBackoff updates job to backoff when the store reports too many
// recent jobs for the same type and resource. Jobs without a resource or with
// backoff disabled are left unchanged.
func ApplyResourceBackoff(ctx context.Context, store JobStore, job *Job, now time.Time) error {
	if job == nil ||
		job.Resource.Type == "" ||
		job.Resource.ID == "" ||
		job.ResourceBackoffThreshold < 1 ||
		job.ResourceBackoffWindow <= 0 ||
		job.ResourceBackoffBaseDelay <= 0 {
		return nil
	}
	recent, err := store.CountRecentJobsForResource(ctx, job.Type, job.Resource, now.Add(-job.ResourceBackoffWindow))
	if err != nil {
		return err
	}
	if recent < job.ResourceBackoffThreshold {
		return nil
	}

	delay := ResourceBackoffDelay(recent-job.ResourceBackoffThreshold, job.ResourceBackoffBaseDelay, job.ResourceBackoffMaxDelay)
	scheduledAt := now.Add(delay)
	if scheduledAt.After(job.ScheduledAt) {
		job.ScheduledAt = scheduledAt
	}
	job.Status = StatusBackoff
	return nil
}

// ResourceBackoffDelay returns the exponential submission-backoff delay for the
// given number of jobs beyond the threshold.
func ResourceBackoffDelay(overThreshold int, baseDelay, maxDelay time.Duration) time.Duration {
	delay := baseDelay
	for range overThreshold {
		if maxDelay > 0 && delay >= maxDelay/2 {
			return maxDelay
		}
		delay *= 2
	}
	if maxDelay > 0 && delay > maxDelay {
		return maxDelay
	}
	return delay
}
