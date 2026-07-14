// Package models contains hooks database models and storage-facing value types.
package models

import (
	"time"

	"github.com/obot-platform/discobox/hooks/watcher"
	"github.com/obot-platform/discobox/id"
	"gorm.io/gorm"
)

// Status is the durable scheduler state for a hook.
type Status string

const (
	StatusIdle    Status = "idle"
	StatusQueued  Status = "queued"
	StatusRunning Status = "running"
	StatusSuccess Status = "success"
	StatusFailure Status = "failure"
)

// Valid reports whether s is one of the supported status values.
func (s Status) Valid() bool {
	switch s {
	case StatusIdle, StatusQueued, StatusRunning, StatusSuccess, StatusFailure:
		return true
	default:
		return false
	}
}

// ChangedFile is the JSON shape persisted for hook inputs.
type ChangedFile struct {
	Path string             `json:"path"`
	Kind watcher.ChangeKind `json:"kind"`
}

// RunResult contains the terminal fields for a hook run.
type RunResult struct {
	Status     Status
	ExitCode   int
	Error      string
	FinishedAt time.Time
}

// HookDefinition stores one discovered hook definition.
type HookDefinition struct {
	ID          string    `gorm:"primaryKey" json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Type        string    `json:"type"`
	Engine      string    `json:"engine"`
	RunAs       string    `json:"run_as,omitempty"`
	Blocking    bool      `json:"blocking"`
	Pattern     string    `json:"pattern,omitempty"`
	Ignore      []byte    `gorm:"type:json" json:"-"`
	Phase       string    `json:"phase,omitempty"`
	Subagent    string    `json:"subagent,omitempty"`
	LanguageID  string    `json:"language_id,omitempty"`
	MinSeverity string    `json:"min_severity,omitempty"`
	Prompt      string    `json:"prompt,omitempty"`
	AbsPath     string    `json:"abs_path,omitempty"`
	RelPath     string    `json:"rel_path,omitempty"`
	HasShebang  bool      `json:"has_shebang"`
	Executable  bool      `json:"executable"`
	Extensions  []byte    `gorm:"type:json" json:"-"`
	ConfigHash  string    `gorm:"index" json:"config_hash,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (HookDefinition) TableName() string { return "hook_definitions" }

// HookStatus stores the current durable status for a hook.
type HookStatus struct {
	HookID    string    `gorm:"primaryKey" json:"hook_id"`
	Status    string    `gorm:"index" json:"status"`
	Paused    bool      `json:"paused"`
	RunCount  int64     `json:"run_count"`
	FailCount int64     `json:"fail_count"`
	LastRunID string    `json:"last_run_id,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (HookStatus) TableName() string { return "hook_statuses" }

// HookRun stores one hook run-history row.
type HookRun struct {
	ID           string     `gorm:"primaryKey" json:"id"`
	InvocationID string     `gorm:"index" json:"invocation_id,omitempty"`
	HookID       string     `gorm:"index" json:"hook_id"`
	Status       string     `gorm:"index" json:"status"`
	ExitCode     int        `json:"exit_code"`
	ChangedFiles []byte     `gorm:"type:json" json:"-"`
	ChangeIDs    []byte     `gorm:"type:json" json:"-"`
	Error        string     `json:"error,omitempty"`
	StartedAt    time.Time  `gorm:"index" json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
}

func (HookRun) TableName() string              { return "hook_runs" }
func (r *HookRun) BeforeCreate(*gorm.DB) error { return ensureGeneratedID(&r.ID, "hkrun") }

// PendingHook stores one serial queue item.
type PendingHook struct {
	HookID          string    `gorm:"primaryKey" json:"hook_id"`
	Position        int64     `gorm:"index" json:"position"`
	ChangedFiles    []byte    `gorm:"type:json" json:"-"`
	ChangeIDs       []byte    `gorm:"type:json" json:"-"`
	Blocked         bool      `gorm:"index" json:"blocked"`
	BlockedByHookID string    `json:"blocked_by_hook_id,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func (PendingHook) TableName() string { return "pending_hooks" }

// DaemonState stores daemon key/value state.
type DaemonState struct {
	Key       string    `gorm:"primaryKey" json:"key"`
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (DaemonState) TableName() string { return "daemon_states" }

// DaemonSession stores one daemon process lifetime for this session database.
type DaemonSession struct {
	ID            string     `gorm:"primaryKey" json:"id"`
	SessionID     string     `gorm:"index" json:"session_id"`
	RepoRoot      string     `json:"repo_root"`
	Version       int64      `json:"version"`
	PID           int        `json:"pid"`
	StartedAt     time.Time  `gorm:"index" json:"started_at"`
	LastHeartbeat time.Time  `gorm:"index" json:"last_heartbeat"`
	EndedAt       *time.Time `gorm:"index" json:"ended_at,omitempty"`
	EndReason     string     `gorm:"index" json:"end_reason,omitempty"`
}

func (DaemonSession) TableName() string              { return "daemon_sessions" }
func (s *DaemonSession) BeforeCreate(*gorm.DB) error { return ensureGeneratedID(&s.ID, "hksess") }

// HookEvent stores one audit-trail event.
type HookEvent struct {
	ID        string    `gorm:"primaryKey" json:"id"`
	Type      string    `gorm:"index" json:"type"`
	HookID    string    `gorm:"index" json:"hook_id,omitempty"`
	RunID     string    `gorm:"index" json:"run_id,omitempty"`
	Message   string    `json:"message,omitempty"`
	Details   []byte    `gorm:"type:json" json:"-"`
	CreatedAt time.Time `gorm:"index" json:"created_at"`
}

func (HookEvent) TableName() string              { return "hook_events" }
func (e *HookEvent) BeforeCreate(*gorm.DB) error { return ensureGeneratedID(&e.ID, "hkevt") }

// HookLog stores one line of hook output.
type HookLog struct {
	ID        string    `gorm:"primaryKey" json:"id"`
	HookID    string    `gorm:"index" json:"hook_id"`
	RunID     string    `gorm:"index" json:"run_id"`
	Line      string    `json:"line"`
	CreatedAt time.Time `gorm:"index" json:"created_at"`
}

func (HookLog) TableName() string              { return "hook_logs" }
func (l *HookLog) BeforeCreate(*gorm.DB) error { return ensureGeneratedID(&l.ID, "hklog") }

// HookDiagnostic stores one current diagnostic reported by an LSP hook.
type HookDiagnostic struct {
	ID        string    `gorm:"primaryKey" json:"id"`
	HookID    string    `gorm:"index" json:"hook_id"`
	URI       string    `gorm:"index" json:"uri"`
	Path      string    `gorm:"index" json:"path"`
	Severity  string    `gorm:"index" json:"severity"`
	Source    string    `json:"source,omitempty"`
	Code      string    `json:"code,omitempty"`
	Message   string    `json:"message"`
	StartLine int       `json:"start_line"`
	StartCol  int       `json:"start_col"`
	EndLine   int       `json:"end_line"`
	EndCol    int       `json:"end_col"`
	UpdatedAt time.Time `gorm:"index" json:"updated_at"`
}

func (HookDiagnostic) TableName() string              { return "hook_diagnostics" }
func (d *HookDiagnostic) BeforeCreate(*gorm.DB) error { return ensureGeneratedID(&d.ID, "hkdiag") }

// ObservedFileChange stores one daemon-observed file change.
type ObservedFileChange struct {
	ID         string    `gorm:"primaryKey" json:"id"`
	Path       string    `gorm:"index" json:"path"`
	Kind       string    `gorm:"index" json:"kind"`
	BaseCommit string    `gorm:"index" json:"base_commit,omitempty"`
	Diff       string    `json:"diff,omitempty"`
	CreatedAt  time.Time `gorm:"index" json:"created_at"`
}

func (ObservedFileChange) TableName() string              { return "observed_file_changes" }
func (c *ObservedFileChange) BeforeCreate(*gorm.DB) error { return ensureGeneratedID(&c.ID, "hkchg") }

// WorkspaceSnapshot stores one debounced DB-backed workspace snapshot.
type WorkspaceSnapshot struct {
	ID                string    `gorm:"primaryKey" json:"id"`
	ParentID          string    `gorm:"index" json:"parent_id,omitempty"`
	BaseCommit        string    `gorm:"index" json:"base_commit,omitempty"`
	TreeHash          string    `gorm:"index" json:"tree_hash,omitempty"`
	Patch             []byte    `json:"-"`
	PatchBytes        int64     `json:"patch_bytes"`
	ChangedFiles      []byte    `gorm:"type:json" json:"-"`
	OmittedFiles      []byte    `gorm:"type:json" json:"-"`
	MaxFileBytes      int64     `json:"max_file_bytes"`
	ObservedChangeIDs []byte    `gorm:"type:json" json:"-"`
	CreatedAt         time.Time `gorm:"index" json:"created_at"`
}

func (WorkspaceSnapshot) TableName() string              { return "workspace_snapshots" }
func (s *WorkspaceSnapshot) BeforeCreate(*gorm.DB) error { return ensureGeneratedID(&s.ID, "hksnap") }

// HookInvocation stores one hook invocation attempt.
type HookInvocation struct {
	ID          string    `gorm:"primaryKey" json:"id"`
	HookID      string    `gorm:"index" json:"hook_id"`
	RunID       string    `gorm:"index" json:"run_id"`
	RequestedAt time.Time `gorm:"index" json:"requested_at"`
}

func (HookInvocation) TableName() string              { return "hook_invocations" }
func (i *HookInvocation) BeforeCreate(*gorm.DB) error { return ensureGeneratedID(&i.ID, "hkinv") }

// HookInvocationChange stores one invocation-to-observed-change join row.
type HookInvocationChange struct {
	ID           string `gorm:"primaryKey" json:"id"`
	InvocationID string `gorm:"index" json:"invocation_id"`
	ChangeID     string `gorm:"index" json:"change_id"`
}

func (HookInvocationChange) TableName() string              { return "hook_invocation_changes" }
func (c *HookInvocationChange) BeforeCreate(*gorm.DB) error { return ensureGeneratedID(&c.ID, "hkivc") }

// WatchedFile stores the latest watcher snapshot entry for restart diffing.
type WatchedFile struct {
	Path      string    `gorm:"primaryKey" json:"path"`
	IsDir     bool      `json:"is_dir"`
	Size      int64     `json:"size"`
	Mode      uint32    `json:"mode"`
	ModTime   time.Time `json:"mod_time"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (WatchedFile) TableName() string { return "watched_files" }

// AllModels returns all database models owned by the hooks store.
func AllModels() []any {
	return []any{&HookDefinition{}, &HookStatus{}, &HookRun{}, &PendingHook{}, &DaemonState{}, &DaemonSession{}, &HookEvent{}, &HookLog{}, &HookDiagnostic{}, &ObservedFileChange{}, &WorkspaceSnapshot{}, &HookInvocation{}, &HookInvocationChange{}, &WatchedFile{}}
}

func ensureGeneratedID(target *string, prefix string) error {
	if *target != "" {
		return nil
	}
	generated, err := id.New(prefix)
	if err != nil {
		return err
	}
	*target = generated
	return nil
}
