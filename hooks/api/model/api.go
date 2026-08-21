package model

import (
	"time"

	hooks "github.com/discobox-ai/discobox/hooks"
	"github.com/discobox-ai/discobox/hooks/models"
)

// PingResponse is returned by GET /ping.
type PingResponse struct {
	OK        bool   `json:"ok"`
	SessionID string `json:"session_id"`
	Version   int64  `json:"version"`
}

// StatusResponse is returned by GET /status.
type StatusResponse struct {
	SessionID string       `json:"session_id"`
	RepoRoot  string       `json:"repo_root"`
	Paused    bool         `json:"paused"`
	Running   bool         `json:"running"`
	Queued    int          `json:"queued"`
	Hooks     []HookStatus `json:"hooks"`
	UpdatedAt time.Time    `json:"updated_at"`
}

// WaitResponse is returned by GET /wait after waiting for terminal daemon work.
type WaitResponse struct {
	Settled         bool         `json:"settled"`
	Running         bool         `json:"running"`
	Queued          int          `json:"queued"`
	PendingChanges  bool         `json:"pending_changes"`
	PendingSnapshot bool         `json:"pending_snapshot"`
	PendingLSP      bool         `json:"pending_lsp"`
	Hooks           []HookStatus `json:"hooks"`
	UpdatedAt       time.Time    `json:"updated_at"`
}

// HooksResponse is returned by GET /hooks.
type HooksResponse struct {
	Hooks []HookStatus `json:"hooks"`
}

// EventsResponse is returned by GET /events.
type EventsResponse struct {
	Events []Event `json:"events"`
}

// RunsResponse is returned by GET /runs.
type RunsResponse struct {
	Runs []Run `json:"runs"`
}

// ChangesResponse is returned by GET /changes.
type ChangesResponse struct {
	Changes []ObservedFileChange `json:"changes"`
}

// SnapshotsResponse is returned by GET /snapshots.
type SnapshotsResponse struct {
	Snapshots []WorkspaceSnapshot `json:"snapshots"`
}

// QueueResponse is returned by GET /queue.
type QueueResponse struct {
	Queue []QueuedHook `json:"queue"`
}

// DiagnosticsResponse is returned by GET /diagnostics.
type DiagnosticsResponse struct {
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// HookStatus is the API view returned for hook status lists.
type HookStatus struct {
	Hook       hooks.Hook    `json:"hook"`
	ConfigHash string        `json:"config_hash,omitempty"`
	Status     models.Status `json:"status"`
	Paused     bool          `json:"paused"`
	RunCount   int64         `json:"run_count"`
	FailCount  int64         `json:"fail_count"`
	LastRunID  string        `json:"last_run_id,omitempty"`
	LastError  string        `json:"last_error,omitempty"`
	UpdatedAt  time.Time     `json:"updated_at"`
}

// Event is the API view returned for audit events.
type Event struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	HookID    string         `json:"hook_id,omitempty"`
	RunID     string         `json:"run_id,omitempty"`
	Message   string         `json:"message,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// ChangedFile is the API view of a file input to a hook run or queue item.
type ChangedFile struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

// Run is the API view of one hook run.
type Run struct {
	ID           string        `json:"id"`
	InvocationID string        `json:"invocation_id,omitempty"`
	HookID       string        `json:"hook_id"`
	Status       models.Status `json:"status"`
	ExitCode     int           `json:"exit_code"`
	ChangedFiles []ChangedFile `json:"changed_files,omitempty"`
	ChangeIDs    []string      `json:"change_ids,omitempty"`
	Error        string        `json:"error,omitempty"`
	StartedAt    time.Time     `json:"started_at"`
	FinishedAt   *time.Time    `json:"finished_at,omitempty"`
}

// ObservedFileChange is the API view of one daemon-observed filesystem change.
type ObservedFileChange struct {
	ID         string    `json:"id"`
	Path       string    `json:"path"`
	Kind       string    `json:"kind"`
	BaseCommit string    `json:"base_commit,omitempty"`
	Diff       string    `json:"diff,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// SnapshotOmission is the API view of one file omitted from snapshot capture.
type SnapshotOmission struct {
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	Reason     string `json:"reason"`
	SizeBytes  int64  `json:"size_bytes,omitempty"`
	LimitBytes int64  `json:"limit_bytes,omitempty"`
}

// WorkspaceSnapshot is the API view of one captured workspace snapshot.
type WorkspaceSnapshot struct {
	ID                string             `json:"id"`
	ParentID          string             `json:"parent_id,omitempty"`
	BaseCommit        string             `json:"base_commit,omitempty"`
	TreeHash          string             `json:"tree_hash,omitempty"`
	Patch             string             `json:"patch,omitempty"`
	PatchBytes        int64              `json:"patch_bytes"`
	ChangedFiles      []ChangedFile      `json:"changed_files,omitempty"`
	OmittedFiles      []SnapshotOmission `json:"omitted_files,omitempty"`
	MaxFileBytes      int64              `json:"max_file_bytes"`
	ObservedChangeIDs []string           `json:"observed_change_ids,omitempty"`
	CreatedAt         time.Time          `json:"created_at"`
}

// QueuedHook is the API view of one queued hook item.
type QueuedHook struct {
	HookID          string        `json:"hook_id"`
	Position        int64         `json:"position"`
	ChangedFiles    []ChangedFile `json:"changed_files,omitempty"`
	ChangeIDs       []string      `json:"change_ids,omitempty"`
	Blocked         bool          `json:"blocked"`
	BlockedByHookID string        `json:"blocked_by_hook_id,omitempty"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

// Diagnostic is the API view of one current LSP diagnostic.
type Diagnostic struct {
	ID        string    `json:"id"`
	HookID    string    `json:"hook_id"`
	URI       string    `json:"uri,omitempty"`
	Path      string    `json:"path"`
	Severity  string    `json:"severity"`
	Source    string    `json:"source,omitempty"`
	Code      string    `json:"code,omitempty"`
	Message   string    `json:"message"`
	StartLine int       `json:"start_line"`
	StartCol  int       `json:"start_col"`
	EndLine   int       `json:"end_line"`
	EndCol    int       `json:"end_col"`
	UpdatedAt time.Time `json:"updated_at"`
}

// DiagnosticListRequest contains query parameters for GET /diagnostics.
type DiagnosticListRequest struct {
	HookID string
	Limit  int
}

// EventListRequest contains query parameters for GET /events.
type EventListRequest struct {
	HookID string
	Limit  int
}

// ListRequest contains common query parameters for list endpoints.
type ListRequest struct {
	Limit int
}

// RunListRequest contains query parameters for GET /runs.
type RunListRequest struct {
	HookID string
	Limit  int
}

// ExecutionPatchRequest is accepted by PATCH /execution and PATCH /hooks/{id}/execution.
type ExecutionPatchRequest struct {
	Paused bool `json:"paused"`
}

// ExecutionResponse is returned by execution mutation endpoints.
type ExecutionResponse struct {
	Paused bool `json:"paused"`
}

// RunRequest is accepted by POST /hooks/{id}/run.
type RunRequest struct {
	Force bool `json:"force"`
}

// RunResponse is returned by POST /hooks/{id}/run.
type RunResponse struct {
	Enqueued bool   `json:"enqueued"`
	Skipped  bool   `json:"skipped,omitempty"`
	Reason   string `json:"reason,omitempty"`
	HookID   string `json:"hook_id"`
}

// OutputResponse is returned by GET /hooks/{id}/output.
type OutputResponse struct {
	HookID string `json:"hook_id"`
	Output string `json:"output"`
}

// ShutdownResponse is returned by POST /shutdown.
type ShutdownResponse struct {
	OK bool `json:"ok"`
}
