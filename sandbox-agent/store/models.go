package store

import "time"

type TerminalState struct {
	TerminalID string     `gorm:"primaryKey" json:"terminalId"`
	Unit       string     `gorm:"index" json:"unit,omitempty"`
	Status     string     `gorm:"index" json:"status"`
	PID        int64      `json:"pid,omitempty"`
	ExitCode   *int64     `json:"exitCode,omitempty"`
	Error      string     `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	ExitedAt   *time.Time `json:"exitedAt,omitempty"`
	ObservedAt time.Time  `gorm:"index" json:"observedAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
}

func (TerminalState) TableName() string { return "terminal_states" }

type TerminalEvent struct {
	ID         string    `gorm:"primaryKey" json:"id"`
	TerminalID string    `gorm:"index" json:"terminalId,omitempty"`
	Type       string    `gorm:"index" json:"type"`
	Message    string    `json:"message,omitempty"`
	Details    []byte    `gorm:"type:json" json:"-"`
	CreatedAt  time.Time `gorm:"index" json:"createdAt"`
}

func (TerminalEvent) TableName() string { return "terminal_events" }

type ResourceSnapshot struct {
	ID         string    `gorm:"primaryKey" json:"id"`
	TerminalID string    `gorm:"index:idx_resource_snapshots_terminal_sampled,priority:1" json:"terminalId"`
	SampledAt  time.Time `gorm:"index:idx_resource_snapshots_terminal_sampled,priority:2" json:"sampledAt"`
	Source     string    `json:"source"`
	Data       []byte    `gorm:"type:json" json:"data"`
	CreatedAt  time.Time `gorm:"index" json:"createdAt"`
}

func (ResourceSnapshot) TableName() string { return "resource_snapshots" }
