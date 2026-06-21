package model

import (
	"encoding/json"
	"time"
)

// Job is a read-only API view of a durable orchestration job.
type Job struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Status       string          `json:"status"`
	Attempts     int             `json:"attempts"`
	MaxAttempts  int             `json:"maxAttempts"`
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
