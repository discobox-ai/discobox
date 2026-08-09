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

type ResourceSnapshot struct {
	ID         string    `gorm:"primaryKey" json:"id"`
	TerminalID string    `gorm:"index:idx_resource_snapshots_terminal_sampled,priority:1" json:"terminalId"`
	SampledAt  time.Time `gorm:"index:idx_resource_snapshots_terminal_sampled,priority:2" json:"sampledAt"`
	Source     string    `json:"source"`
	Data       []byte    `gorm:"type:json" json:"data"`
	CreatedAt  time.Time `gorm:"index" json:"createdAt"`
}

func (ResourceSnapshot) TableName() string { return "resource_snapshots" }

type HarnessHookLog struct {
	ID         string    `gorm:"primaryKey" json:"id"`
	TerminalID string    `gorm:"index:idx_harness_hook_logs_terminal_created,priority:1" json:"terminalId,omitempty"`
	Provider   string    `gorm:"index" json:"provider"`
	Event      string    `gorm:"index" json:"event"`
	Payload    []byte    `gorm:"type:json" json:"payload"`
	CreatedAt  time.Time `gorm:"index:idx_harness_hook_logs_terminal_created,priority:2" json:"createdAt"`
}

func (HarnessHookLog) TableName() string { return "harness_hook_logs" }

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

// ExecRecord is the durable, immutable identity/metadata of an exec, written
// once at create. systemd + the shim remain the source of truth for live state
// (ExecState); this record survives reboots (tmpfs runtime files and transient
// units do not) so metadata like harnessId/primary and the command are never lost
// — including when a shim runtime write drops the metadata field.
type ExecRecord struct {
	ExecID    string    `gorm:"primaryKey" json:"execId"`
	HarnessID string    `gorm:"index" json:"harnessId,omitempty"`
	Primary   bool      `json:"primary,omitempty"`
	Command   []byte    `gorm:"type:json" json:"-"`
	Workdir   string    `json:"workdir,omitempty"`
	TTY       bool      `json:"tty,omitempty"`
	Metadata  []byte    `gorm:"type:json" json:"-"`
	CreatedAt time.Time `json:"createdAt"`
}

func (ExecRecord) TableName() string { return "exec_records" }

type ExecEvent struct {
	ID        string    `gorm:"primaryKey" json:"id"`
	ExecID    string    `gorm:"index" json:"execId,omitempty"`
	Type      string    `gorm:"index" json:"type"`
	Message   string    `json:"message,omitempty"`
	Details   []byte    `gorm:"type:json" json:"-"`
	CreatedAt time.Time `gorm:"index" json:"createdAt"`
}

func (ExecEvent) TableName() string { return "exec_events" }

// ExecLogChunk is one compressed batch of an exec's stdin/stdout/stderr
// transcript, covering roughly one AsyncLogger flush window (see
// execs.LogSink). Data holds the compressed bytes; RawSize is the
// uncompressed size, kept for observability/debugging only.
type ExecLogChunk struct {
	ID          string    `gorm:"primaryKey" json:"id"`
	ExecID      string    `gorm:"index:idx_exec_log_chunks_exec_bucket,priority:1" json:"execId"`
	BucketStart time.Time `gorm:"index:idx_exec_log_chunks_exec_bucket,priority:2" json:"bucketStart"`
	Codec       string    `json:"codec"`
	Data        []byte    `json:"-"`
	RawSize     int       `json:"rawSize"`
	CreatedAt   time.Time `json:"createdAt"`
}

func (ExecLogChunk) TableName() string { return "exec_log_chunks" }
