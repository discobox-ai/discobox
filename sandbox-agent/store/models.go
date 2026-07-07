package store

import "time"

// AgentState is a small key/value table for durable sandbox-agent state that
// must survive sandbox restarts, such as whether the primary terminal has been
// launched before.
type AgentState struct {
	Key       string    `gorm:"primaryKey" json:"key"`
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (AgentState) TableName() string { return "agent_state" }

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

type AgentHookLog struct {
	ID         string    `gorm:"primaryKey" json:"id"`
	TerminalID string    `gorm:"index:idx_agent_hook_logs_terminal_created,priority:1" json:"terminalId,omitempty"`
	Provider   string    `gorm:"index" json:"provider"`
	Event      string    `gorm:"index" json:"event"`
	Payload    []byte    `gorm:"type:json" json:"payload"`
	CreatedAt  time.Time `gorm:"index:idx_agent_hook_logs_terminal_created,priority:2" json:"createdAt"`
}

func (AgentHookLog) TableName() string { return "agent_hook_logs" }

type ExecState struct {
	ExecID     string     `gorm:"primaryKey" json:"execId"`
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

func (ExecState) TableName() string { return "exec_states" }

type ExecEvent struct {
	ID        string    `gorm:"primaryKey" json:"id"`
	ExecID    string    `gorm:"index" json:"execId,omitempty"`
	Type      string    `gorm:"index" json:"type"`
	Message   string    `json:"message,omitempty"`
	Details   []byte    `gorm:"type:json" json:"-"`
	CreatedAt time.Time `gorm:"index" json:"createdAt"`
}

func (ExecEvent) TableName() string { return "exec_events" }
