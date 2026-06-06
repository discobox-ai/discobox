package jobqueue

import (
	"encoding/json"
	"time"
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
		Type:        payload.JobType(),
		Payload:     data,
		Status:      StatusPending,
		Priority:    priority,
		MaxAttempts: maxAttempts,
		ScheduledAt: scheduledAt,
		Resource:    payload.Resource(),
	}, nil
}
