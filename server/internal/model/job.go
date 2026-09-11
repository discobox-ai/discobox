package model

import (
	"encoding/json"
	"time"
)

// Job is a read-only API view of one reconcile dirty mark: a resource the
// reconcile engine has pending, running, scheduled, or backing off. Nothing
// stores jobs; resources/jobs projects them from the engine's dirty set.
type Job struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Status       string          `json:"status"`
	Attempts     int             `json:"attempts"`
	Error        *string         `json:"error,omitempty"`
	Message      *string         `json:"message,omitempty"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
	WorkerID     *string         `json:"workerId,omitempty"`
	ResourceType string          `json:"resourceType"`
	ResourceID   string          `json:"resourceId"`
	ScheduledAt  time.Time       `json:"scheduledAt"`
	StartedAt    *time.Time      `json:"startedAt,omitempty"`
	CompletedAt  *time.Time      `json:"completedAt,omitempty"`
	CreatedAt    time.Time       `json:"createdAt"`
	UpdatedAt    time.Time       `json:"updatedAt"`
}
