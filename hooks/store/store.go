package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/obot-platform/discobox/gormdb"
	hooks "github.com/obot-platform/discobox/hooks"
	hookapi "github.com/obot-platform/discobox/hooks/api"
	"github.com/obot-platform/discobox/hooks/models"
	"github.com/obot-platform/discobox/hooks/watcher"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// ObservedFileChange is returned by observed-change APIs with kind restored to
// the watcher enum used by the daemon.
type ObservedFileChange struct {
	ID         string             `json:"id"`
	Path       string             `json:"path"`
	Kind       watcher.ChangeKind `json:"kind"`
	BaseCommit string             `json:"base_commit"`
	Diff       string             `json:"diff"`
	CreatedAt  time.Time          `json:"created_at"`
}

// SnapshotOmission records a path intentionally omitted from a workspace snapshot.
type SnapshotOmission struct {
	Path       string             `json:"path"`
	Kind       watcher.ChangeKind `json:"kind"`
	Reason     string             `json:"reason"`
	SizeBytes  int64              `json:"size_bytes,omitempty"`
	LimitBytes int64              `json:"limit_bytes,omitempty"`
}

// WorkspaceSnapshot is returned by workspace snapshot APIs.
type WorkspaceSnapshot struct {
	ID                string               `json:"id"`
	ParentID          string               `json:"parent_id,omitempty"`
	BaseCommit        string               `json:"base_commit,omitempty"`
	TreeHash          string               `json:"tree_hash,omitempty"`
	Patch             []byte               `json:"-"`
	PatchBytes        int64                `json:"patch_bytes"`
	ChangedFiles      []models.ChangedFile `json:"changed_files,omitempty"`
	OmittedFiles      []SnapshotOmission   `json:"omitted_files,omitempty"`
	MaxFileBytes      int64                `json:"max_file_bytes"`
	ObservedChangeIDs []string             `json:"observed_change_ids,omitempty"`
	CreatedAt         time.Time            `json:"created_at"`
}

// StatusRow is returned by ListStatus.
type StatusRow struct {
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

// RunRow is returned by ListRuns.
type RunRow struct {
	ID           string               `json:"id"`
	InvocationID string               `json:"invocation_id,omitempty"`
	HookID       string               `json:"hook_id"`
	Status       models.Status        `json:"status"`
	ExitCode     int                  `json:"exit_code"`
	ChangedFiles []models.ChangedFile `json:"changed_files,omitempty"`
	ChangeIDs    []string             `json:"change_ids,omitempty"`
	Error        string               `json:"error,omitempty"`
	StartedAt    time.Time            `json:"started_at"`
	FinishedAt   *time.Time           `json:"finished_at,omitempty"`
}

// Event records one audit-trail event for daemon, hook, queue, and API actions.
type Event struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	HookID    string         `json:"hook_id,omitempty"`
	RunID     string         `json:"run_id,omitempty"`
	Message   string         `json:"message,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// Diagnostic is one current language-server diagnostic for an LSP hook.
type Diagnostic struct {
	ID        string    `json:"id"`
	HookID    string    `json:"hook_id"`
	URI       string    `json:"uri"`
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

// DiagnosticQuery filters ListDiagnostics.
type DiagnosticQuery struct {
	HookID string
	Limit  int
}

// DaemonSessionRow is returned by daemon session lifecycle APIs.
type DaemonSessionRow struct {
	ID            string     `json:"id"`
	SessionID     string     `json:"session_id"`
	RepoRoot      string     `json:"repo_root"`
	Version       int64      `json:"version"`
	PID           int        `json:"pid"`
	StartedAt     time.Time  `json:"started_at"`
	LastHeartbeat time.Time  `json:"last_heartbeat"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
	EndReason     string     `json:"end_reason,omitempty"`
}

// HookLogQuery filters ListHookLogs.
type HookLogQuery struct {
	HookID string
	RunID  string
	Limit  int
}

// EventQuery filters ListEvents.
type EventQuery struct {
	HookID         string
	Limit          int
	AfterCreatedAt time.Time
	AfterID        string
	Ascending      bool
}

// PendingRow is one queued hook item from the internal pending_hooks table.
type PendingRow struct {
	HookID          string               `json:"hook_id"`
	Position        int64                `json:"position"`
	ChangedFiles    []models.ChangedFile `json:"changed_files,omitempty"`
	ChangeIDs       []string             `json:"change_ids,omitempty"`
	Blocked         bool                 `json:"blocked"`
	BlockedByHookID string               `json:"blocked_by_hook_id,omitempty"`
	CreatedAt       time.Time            `json:"created_at"`
	UpdatedAt       time.Time            `json:"updated_at"`
}

// Store owns GORM-backed hook persistence.
type Store struct {
	write *gorm.DB
	read  *gorm.DB
	pools *gormdb.Pools
}

// Options controls Open.
type Options struct {
	Path    string
	DSN     string
	ReadDSN string
	Driver  gormdb.Driver
	Logger  logger.Interface
}

// Open opens hook store write/read pools with the shared gormdb opener and runs
// migrations. Path is preferred for session SQLite databases; DSN may be used
// for tests or alternate gormdb-supported backends.
func Open(ctx context.Context, opts Options) (*Store, error) {
	dsn := opts.DSN
	if dsn == "" {
		if opts.Path == "" {
			return nil, fmt.Errorf("store path or dsn is required")
		}
		dsn = opts.Path
	}
	pools, err := gormdb.Open(gormdb.Config{Driver: opts.Driver, DSN: dsn, ReadDSN: opts.ReadDSN, Logger: opts.Logger})
	if err != nil {
		return nil, fmt.Errorf("open hook store db: %w", err)
	}
	s := &Store{write: pools.Write, read: pools.Read, pools: pools}
	if err := s.Migrate(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// New wraps an existing GORM DB and runs migrations.
func New(ctx context.Context, db *gorm.DB) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("gorm db is nil")
	}
	s := &Store{write: db, read: db}
	if err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// NewWithPools wraps existing GORM write/read handles and runs migrations.
func NewWithPools(ctx context.Context, write, read *gorm.DB) (*Store, error) {
	if write == nil {
		return nil, fmt.Errorf("gorm write db is nil")
	}
	if read == nil {
		read = write
	}
	s := &Store{write: write, read: read}
	if err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// DB exposes the write GORM handle for tests and diagnostics.
func (s *Store) DB() *gorm.DB { return s.write }

// Close closes the underlying database pool.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	if s.pools != nil {
		return s.pools.Close()
	}
	return closeGormDB(s.write)
}

// Migrate initializes the hook schema.
func (s *Store) Migrate(ctx context.Context) error {
	return s.write.WithContext(ctx).AutoMigrate(models.AllModels()...)
}

// RefreshDefinitions replaces the current discovered hook definition set and
// creates idle statuses for newly discovered hooks atomically.
func (s *Store) RefreshDefinitions(ctx context.Context, defs []hooks.Hook) error {
	now := time.Now().UTC()
	return s.write.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		ids := make([]string, 0, len(defs))
		changedLSPIDs := map[string]struct{}{}
		for _, def := range defs {
			if def.ID == "" {
				return fmt.Errorf("hook definition id is required")
			}
			ids = append(ids, def.ID)
			row, err := definitionFromHook(def, now)
			if err != nil {
				return err
			}
			var existing models.HookDefinition
			err = tx.Where("id = ?", def.ID).First(&existing).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if err == nil && existing.ConfigHash != row.ConfigHash {
				if existing.Engine == string(hooks.HookEngineLSP) {
					if err := tx.Where("hook_id = ?", def.ID).Delete(&models.HookDiagnostic{}).Error; err != nil {
						return err
					}
				}
				if def.IsLSP() {
					changedLSPIDs[def.ID] = struct{}{}
				}
			}
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, UpdateAll: true}).Create(&row).Error; err != nil {
				return err
			}
			status := models.HookStatus{HookID: def.ID, Status: string(models.StatusIdle), UpdatedAt: now}
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "hook_id"}}, DoNothing: true}).Create(&status).Error; err != nil {
				return err
			}
			if def.IsLSP() {
				if err := tx.Where("hook_id = ?", def.ID).Delete(&models.PendingHook{}).Error; err != nil {
					return err
				}
			}
			pruned, err := pruneLSPDiagnosticsForHookTx(tx, def)
			if err != nil {
				return err
			}
			if pruned {
				changedLSPIDs[def.ID] = struct{}{}
			}
		}
		if len(ids) == 0 {
			if err := tx.Where("1 = 1").Delete(&models.HookDefinition{}).Error; err != nil {
				return err
			}
			if err := tx.Where("1 = 1").Delete(&models.HookStatus{}).Error; err != nil {
				return err
			}
			if err := tx.Where("1 = 1").Delete(&models.PendingHook{}).Error; err != nil {
				return err
			}
			return tx.Where("1 = 1").Delete(&models.HookDiagnostic{}).Error
		}
		if err := tx.Where("id NOT IN ?", ids).Delete(&models.HookDefinition{}).Error; err != nil {
			return err
		}
		if err := tx.Where("hook_id NOT IN ?", ids).Delete(&models.HookStatus{}).Error; err != nil {
			return err
		}
		if err := tx.Where("hook_id NOT IN ?", ids).Delete(&models.PendingHook{}).Error; err != nil {
			return err
		}
		if err := tx.Where("hook_id NOT IN ?", ids).Delete(&models.HookDiagnostic{}).Error; err != nil {
			return err
		}
		for hookID := range changedLSPIDs {
			if err := setLSPStatusFromDiagnosticsTx(tx, hookID, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// Enqueue merges changed files into queued hook rows. Existing rows retain their
// queue position while changed files are de-duplicated by path and kind.
func (s *Store) Enqueue(ctx context.Context, hookIDs []string, changes []models.ChangedFile) error {
	return s.EnqueueWithChangeIDs(ctx, hookIDs, changes, nil)
}

// EnqueueWithChangeIDs merges changed files and observed change record IDs into
// queued hook rows. Existing rows retain their queue position.
func (s *Store) EnqueueWithChangeIDs(ctx context.Context, hookIDs []string, changes []models.ChangedFile, changeIDs []string) error {
	files := normalizeChangedFiles(changes)
	changeIDs = normalizeIDs(changeIDs)
	now := time.Now().UTC()
	return s.write.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		next, err := nextPendingPosition(tx)
		if err != nil {
			return err
		}
		seen := map[string]struct{}{}
		for _, id := range hookIDs {
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			inputFiles := files
			inputIDs := changeIDs
			failedFiles, failedIDs, err := lastFailedRunInputs(tx, id)
			if err != nil {
				return err
			}
			if len(failedFiles) > 0 {
				inputFiles = mergeChangedFiles(failedFiles, inputFiles)
			}
			if len(failedIDs) > 0 {
				inputIDs = mergeIDs(failedIDs, inputIDs)
			}
			if err := mergeInputsIntoPending(tx, id, inputFiles, inputIDs, now, &next); err != nil {
				return err
			}
			if err := tx.Model(&models.HookStatus{}).Where("hook_id = ?", id).Updates(map[string]any{"status": string(models.StatusQueued), "updated_at": now}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// EnqueueChanges converts watcher changes and enqueues the matching hook IDs.
func (s *Store) EnqueueChanges(ctx context.Context, hookIDs []string, changes []watcher.Change) error {
	return s.Enqueue(ctx, hookIDs, ChangedFilesFromWatcher(changes))
}

// NextPending returns the first unblocked queued hook.
func (s *Store) NextPending(ctx context.Context) (*PendingRow, error) {
	return s.NextPendingForPhases(ctx, nil)
}

// ListPending returns all queued hook rows in queue order.
func (s *Store) ListPending(ctx context.Context, limit int) ([]PendingRow, error) {
	q := s.read.WithContext(ctx).Order("position asc")
	if limit > 0 {
		q = q.Limit(limit)
	}
	var rows []models.PendingHook
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]PendingRow, 0, len(rows))
	for _, row := range rows {
		converted, err := pendingToRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, converted)
	}
	return out, nil
}

// NextPendingForPhases returns the first unblocked queued hook allowed by the
// active phase set. Unphased hooks are always allowed and are preferred before
// phased hooks. If phases is empty, only unphased hooks are eligible.
func (s *Store) NextPendingForPhases(ctx context.Context, phases []string) (*PendingRow, error) {
	phases = normalizePhaseList(phases)
	var row models.PendingHook
	query := s.read.WithContext(ctx).
		Joins("JOIN hook_definitions ON hook_definitions.id = pending_hooks.hook_id").
		Where("pending_hooks.blocked = ?", false)
	if len(phases) == 0 {
		query = query.Where("hook_definitions.phase = ?", "")
	} else {
		query = query.Where("hook_definitions.phase = ? OR hook_definitions.phase IN ?", "", phases)
	}
	err := query.
		Order("CASE WHEN hook_definitions.phase = '' THEN 0 ELSE 1 END").
		Order("pending_hooks.position asc").
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out, err := pendingToRow(row)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func normalizePhaseList(phases []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(phases))
	for _, phase := range phases {
		phase = strings.TrimSpace(strings.ToLower(phase))
		if phase == "" {
			continue
		}
		if _, ok := seen[phase]; ok {
			continue
		}
		seen[phase] = struct{}{}
		out = append(out, phase)
	}
	sort.Strings(out)
	return out
}

// MarkRunning marks a hook running and appends a run-history row.
func (s *Store) MarkRunning(ctx context.Context, hookID string, files []models.ChangedFile) (*RunRow, error) {
	return s.MarkRunningWithChangeIDs(ctx, hookID, files, nil)
}

// MarkRunningWithChangeIDs marks a hook running, appends a run row, records the
// invocation, and links observed file-change records used as inputs.
func (s *Store) MarkRunningWithChangeIDs(ctx context.Context, hookID string, files []models.ChangedFile, changeIDs []string) (*RunRow, error) {
	if hookID == "" {
		return nil, fmt.Errorf("hook id is required")
	}
	now := time.Now().UTC()
	var out RunRow
	err := s.write.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if len(files) == 0 {
			var pending models.PendingHook
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("hook_id = ?", hookID).First(&pending).Error; err == nil {
				var err error
				files, err = pendingChangedFiles(pending)
				if err != nil {
					return err
				}
				if len(changeIDs) == 0 {
					changeIDs, err = pendingChangeIDs(pending)
					if err != nil {
						return err
					}
				}
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}
		if err := tx.Where("hook_id = ?", hookID).Delete(&models.PendingHook{}).Error; err != nil {
			return err
		}
		changeIDs = normalizeIDs(changeIDs)
		payload, err := marshalJSON(normalizeChangedFiles(files))
		if err != nil {
			return err
		}
		idsPayload, err := marshalJSON(changeIDs)
		if err != nil {
			return err
		}
		run := models.HookRun{HookID: hookID, Status: string(models.StatusRunning), ChangedFiles: payload, ChangeIDs: idsPayload, StartedAt: now}
		if err := tx.Create(&run).Error; err != nil {
			return err
		}
		invocation := models.HookInvocation{HookID: hookID, RunID: run.ID, RequestedAt: now}
		if err := tx.Create(&invocation).Error; err != nil {
			return err
		}
		for _, changeID := range changeIDs {
			if err := tx.Create(&models.HookInvocationChange{InvocationID: invocation.ID, ChangeID: changeID}).Error; err != nil {
				return err
			}
		}
		run.InvocationID = invocation.ID
		if err := tx.Save(&run).Error; err != nil {
			return err
		}
		updates := map[string]any{"status": string(models.StatusRunning), "last_run_id": run.ID, "updated_at": now}
		if err := tx.Model(&models.HookStatus{}).Where("hook_id = ?", hookID).Updates(updates).Error; err != nil {
			return err
		}
		out = runToRow(run)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// FinishRun records a terminal run result, updates current status, and advances
// or blocks the queue according to success/failure.
func (s *Store) FinishRun(ctx context.Context, runID string, result models.RunResult) error {
	if runID == "" {
		return fmt.Errorf("run id is required")
	}
	if !result.Status.Valid() || result.Status == models.StatusQueued || result.Status == models.StatusRunning || result.Status == models.StatusIdle {
		return fmt.Errorf("finish status must be success or failure, got %q", result.Status)
	}
	finished := result.FinishedAt
	if finished.IsZero() {
		finished = time.Now().UTC()
	}
	return s.write.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var run models.HookRun
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", runID).First(&run).Error; err != nil {
			return err
		}
		run.Status = string(result.Status)
		run.ExitCode = result.ExitCode
		run.Error = result.Error
		run.FinishedAt = &finished
		if err := tx.Save(&run).Error; err != nil {
			return err
		}
		updates := map[string]any{"status": string(result.Status), "run_count": gorm.Expr("run_count + 1"), "last_run_id": run.ID, "last_error": result.Error, "updated_at": finished}
		if result.Status == models.StatusFailure {
			updates["fail_count"] = gorm.Expr("fail_count + 1")
		}
		if err := tx.Model(&models.HookStatus{}).Where("hook_id = ?", run.HookID).Updates(updates).Error; err != nil {
			return err
		}
		if result.Status == models.StatusFailure {
			if err := mergeRunInputsIntoPending(tx, run, finished); err != nil {
				return err
			}
			return tx.Model(&models.PendingHook{}).Where("hook_id <> ?", run.HookID).Updates(map[string]any{"blocked": true, "blocked_by_hook_id": run.HookID, "updated_at": finished}).Error
		}
		return tx.Model(&models.PendingHook{}).Where("blocked_by_hook_id = ?", run.HookID).Updates(map[string]any{"blocked": false, "blocked_by_hook_id": "", "updated_at": finished}).Error
	})
}

// ReconcileRunningRuns marks orphaned running runs as failed and requeues their
// inputs. It is intended for daemon startup, before any hook child processes are
// owned by the current daemon.
func (s *Store) ReconcileRunningRuns(ctx context.Context, reason string) (int, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "hook run interrupted before daemon startup"
	}
	now := time.Now().UTC()
	reconciled := 0
	err := s.write.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var runs []models.HookRun
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("status = ?", string(models.StatusRunning)).Order("started_at asc, id asc").Find(&runs).Error; err != nil {
			return err
		}
		if len(runs) == 0 {
			return nil
		}
		next, err := nextPendingPosition(tx)
		if err != nil {
			return err
		}
		affectedHooks := map[string]struct{}{}
		for _, run := range runs {
			run.Status = string(models.StatusFailure)
			run.ExitCode = 1
			run.Error = reason
			finishedAt := now
			run.FinishedAt = &finishedAt
			if err := tx.Save(&run).Error; err != nil {
				return err
			}
			if err := tx.Model(&models.HookStatus{}).Where("hook_id = ?", run.HookID).Updates(map[string]any{
				"status":      string(models.StatusFailure),
				"run_count":   gorm.Expr("run_count + 1"),
				"fail_count":  gorm.Expr("fail_count + 1"),
				"last_run_id": run.ID,
				"last_error":  reason,
				"updated_at":  now,
			}).Error; err != nil {
				return err
			}
			files, ids := runInputs(run)
			if err := mergeInputsIntoPending(tx, run.HookID, files, ids, now, &next); err != nil {
				return err
			}
			affectedHooks[run.HookID] = struct{}{}
			reconciled++
		}
		for hookID := range affectedHooks {
			if err := tx.Model(&models.HookStatus{}).Where("hook_id = ?", hookID).Updates(map[string]any{"status": string(models.StatusQueued), "updated_at": now}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return reconciled, err
}

func (s *Store) PauseHook(ctx context.Context, hookID string) error {
	return s.setHookPaused(ctx, hookID, true)
}
func (s *Store) ResumeHook(ctx context.Context, hookID string) error {
	return s.setHookPaused(ctx, hookID, false)
}

func (s *Store) setHookPaused(ctx context.Context, hookID string, paused bool) error {
	if hookID == "" {
		return fmt.Errorf("hook id is required")
	}
	return s.write.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return tx.Model(&models.HookStatus{}).Where("hook_id = ?", hookID).Updates(map[string]any{"paused": paused, "updated_at": time.Now().UTC()}).Error
	})
}

func (s *Store) PauseGlobal(ctx context.Context) error {
	return s.SetDaemonState(ctx, "paused", "true")
}
func (s *Store) ResumeGlobal(ctx context.Context) error {
	return s.SetDaemonState(ctx, "paused", "false")
}

func (s *Store) SetDaemonState(ctx context.Context, key, value string) error {
	if key == "" {
		return fmt.Errorf("daemon state key is required")
	}
	now := time.Now().UTC()
	state := models.DaemonState{Key: key, Value: value, UpdatedAt: now}
	return s.write.WithContext(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, UpdateAll: true}).Create(&state).Error
}

func (s *Store) GetDaemonState(ctx context.Context, key string) (string, bool, error) {
	var state models.DaemonState
	err := s.read.WithContext(ctx).First(&state, "key = ?", key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return state.Value, true, nil
}

// StartDaemonSession closes any unended daemon sessions at their last heartbeat,
// emits termination audit events for them, then creates a fresh daemon session
// and daemon.started event.
func (s *Store) StartDaemonSession(ctx context.Context, sessionID, repoRoot string, version int64, pid int) (*DaemonSessionRow, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("session id is required")
	}
	now := time.Now().UTC()
	var out *DaemonSessionRow
	err := s.write.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var stale []models.DaemonSession
		if err := tx.Where("session_id = ? AND ended_at IS NULL", sessionID).Order("started_at asc, id asc").Find(&stale).Error; err != nil {
			return err
		}
		for _, row := range stale {
			endedAt := row.LastHeartbeat
			if endedAt.IsZero() {
				endedAt = row.StartedAt
			}
			if endedAt.IsZero() {
				endedAt = now
			}
			if err := tx.Model(&models.DaemonSession{}).Where("id = ? AND ended_at IS NULL", row.ID).Updates(map[string]any{"ended_at": endedAt, "end_reason": "terminated"}).Error; err != nil {
				return err
			}
			if err := recordEventTx(tx, Event{Type: "daemon.terminated", Message: "hook daemon terminated without graceful shutdown", Details: map[string]any{"daemon_session_id": row.ID, "session_id": row.SessionID, "repo_root": row.RepoRoot, "version": row.Version, "pid": row.PID, "started_at": row.StartedAt, "last_heartbeat": row.LastHeartbeat, "ended_at": endedAt, "end_reason": "terminated"}, CreatedAt: now}); err != nil {
				return err
			}
		}
		row := models.DaemonSession{SessionID: sessionID, RepoRoot: repoRoot, Version: version, PID: pid, StartedAt: now, LastHeartbeat: now}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		if err := recordEventTx(tx, Event{Type: "daemon.started", Message: "hook daemon started", Details: map[string]any{"daemon_session_id": row.ID, "session_id": sessionID, "repo_root": repoRoot, "version": version, "pid": pid, "started_at": row.StartedAt}, CreatedAt: now}); err != nil {
			return err
		}
		converted := daemonSessionToRow(row)
		out = &converted
		return nil
	})
	return out, err
}

// HeartbeatDaemonSession updates the liveness timestamp for a running daemon.
func (s *Store) HeartbeatDaemonSession(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("daemon session id is required")
	}
	return s.write.WithContext(ctx).Model(&models.DaemonSession{}).Where("id = ? AND ended_at IS NULL", id).Update("last_heartbeat", time.Now().UTC()).Error
}

// EndDaemonSession records a graceful daemon end and emits a shutdown event.
func (s *Store) EndDaemonSession(ctx context.Context, id, reason string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("daemon session id is required")
	}
	if strings.TrimSpace(reason) == "" {
		reason = "shutdown"
	}
	now := time.Now().UTC()
	return s.write.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row models.DaemonSession
		if err := tx.Where("id = ?", id).First(&row).Error; err != nil {
			return err
		}
		updates := map[string]any{"last_heartbeat": now, "ended_at": now, "end_reason": reason}
		if err := tx.Model(&models.DaemonSession{}).Where("id = ?", id).Updates(updates).Error; err != nil {
			return err
		}
		return recordEventTx(tx, Event{Type: "daemon.shutdown", Message: "hook daemon shut down", Details: map[string]any{"daemon_session_id": row.ID, "session_id": row.SessionID, "repo_root": row.RepoRoot, "version": row.Version, "pid": row.PID, "started_at": row.StartedAt, "last_heartbeat": now, "ended_at": now, "end_reason": reason}, CreatedAt: now})
	})
}

func (s *Store) ListStatus(ctx context.Context) ([]StatusRow, error) {
	var defs []models.HookDefinition
	if err := s.read.WithContext(ctx).Order("id asc").Find(&defs).Error; err != nil {
		return nil, err
	}
	out := make([]StatusRow, 0, len(defs))
	for _, def := range defs {
		var st models.HookStatus
		if err := s.read.WithContext(ctx).First(&st, "hook_id = ?", def.ID).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
		h, err := definitionToHook(def)
		if err != nil {
			return nil, err
		}
		out = append(out, StatusRow{Hook: h, ConfigHash: def.ConfigHash, Status: models.Status(st.Status), Paused: st.Paused, RunCount: st.RunCount, FailCount: st.FailCount, LastRunID: st.LastRunID, LastError: st.LastError, UpdatedAt: st.UpdatedAt})
	}
	return out, nil
}

func (s *Store) ListRuns(ctx context.Context, hookID string, limit int) ([]RunRow, error) {
	q := s.read.WithContext(ctx).Order("started_at desc, id desc")
	if hookID != "" {
		q = q.Where("hook_id = ?", hookID)
	}
	if limit > 0 {
		q = q.Limit(limit)
	}
	var runs []models.HookRun
	if err := q.Find(&runs).Error; err != nil {
		return nil, err
	}
	out := make([]RunRow, 0, len(runs))
	for _, run := range runs {
		out = append(out, runToRow(run))
	}
	return out, nil
}

// RecordObservedChanges appends durable records for filesystem changes observed
// by the daemon. Callers should pass changes in observation order; all are
// recorded, including repeated changes to the same path.
func (s *Store) RecordObservedChanges(ctx context.Context, changes []ObservedFileChange) ([]ObservedFileChange, error) {
	if len(changes) == 0 {
		return nil, nil
	}
	now := time.Now().UTC()
	rows := make([]models.ObservedFileChange, 0, len(changes))
	for _, change := range changes {
		path := filepath.ToSlash(strings.TrimSpace(change.Path))
		if path == "" {
			continue
		}
		created := change.CreatedAt
		if created.IsZero() {
			created = now
		}
		rows = append(rows, models.ObservedFileChange{Path: path, Kind: string(change.Kind), BaseCommit: change.BaseCommit, Diff: change.Diff, CreatedAt: created})
	}
	if len(rows) == 0 {
		return nil, nil
	}
	if err := s.write.WithContext(ctx).Create(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]ObservedFileChange, 0, len(rows))
	for _, row := range rows {
		out = append(out, observedToRow(row))
	}
	return out, nil
}

// ListObservedChanges returns observed file changes newest first.
func (s *Store) ListObservedChanges(ctx context.Context, limit int) ([]ObservedFileChange, error) {
	q := s.read.WithContext(ctx).Order("created_at desc, id desc")
	if limit > 0 {
		q = q.Limit(limit)
	}
	var rows []models.ObservedFileChange
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]ObservedFileChange, 0, len(rows))
	for _, row := range rows {
		out = append(out, observedToRow(row))
	}
	return out, nil
}

// RecordWorkspaceSnapshot appends one debounced workspace snapshot row.
func (s *Store) RecordWorkspaceSnapshot(ctx context.Context, snapshot WorkspaceSnapshot) (*WorkspaceSnapshot, error) {
	created := snapshot.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	changed, err := marshalJSON(normalizeChangedFiles(snapshot.ChangedFiles))
	if err != nil {
		return nil, err
	}
	omitted, err := marshalJSON(normalizeSnapshotOmissions(snapshot.OmittedFiles))
	if err != nil {
		return nil, err
	}
	observedIDs, err := marshalJSON(normalizeIDs(snapshot.ObservedChangeIDs))
	if err != nil {
		return nil, err
	}
	patchBytes := snapshot.PatchBytes
	if patchBytes == 0 && len(snapshot.Patch) > 0 {
		patchBytes = int64(len(snapshot.Patch))
	}
	row := models.WorkspaceSnapshot{
		ParentID:          strings.TrimSpace(snapshot.ParentID),
		BaseCommit:        strings.TrimSpace(snapshot.BaseCommit),
		TreeHash:          strings.TrimSpace(snapshot.TreeHash),
		Patch:             append([]byte(nil), snapshot.Patch...),
		PatchBytes:        patchBytes,
		ChangedFiles:      changed,
		OmittedFiles:      omitted,
		MaxFileBytes:      snapshot.MaxFileBytes,
		ObservedChangeIDs: observedIDs,
		CreatedAt:         created,
	}
	if err := s.write.WithContext(ctx).Create(&row).Error; err != nil {
		return nil, err
	}
	out, err := workspaceSnapshotToRow(row)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// LatestWorkspaceSnapshot returns the newest workspace snapshot, if any.
func (s *Store) LatestWorkspaceSnapshot(ctx context.Context) (*WorkspaceSnapshot, error) {
	var row models.WorkspaceSnapshot
	err := s.read.WithContext(ctx).Order("created_at desc, id desc").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out, err := workspaceSnapshotToRow(row)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetWorkspaceSnapshot returns one workspace snapshot by ID.
func (s *Store) GetWorkspaceSnapshot(ctx context.Context, snapshotID string) (*WorkspaceSnapshot, error) {
	var row models.WorkspaceSnapshot
	err := s.read.WithContext(ctx).Where("id = ?", strings.TrimSpace(snapshotID)).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out, err := workspaceSnapshotToRow(row)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListWorkspaceSnapshots returns workspace snapshots newest first.
func (s *Store) ListWorkspaceSnapshots(ctx context.Context, limit int) ([]WorkspaceSnapshot, error) {
	q := s.read.WithContext(ctx).Order("created_at desc, id desc")
	if limit > 0 {
		q = q.Limit(limit)
	}
	var rows []models.WorkspaceSnapshot
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]WorkspaceSnapshot, 0, len(rows))
	for _, row := range rows {
		converted, err := workspaceSnapshotToRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, converted)
	}
	return out, nil
}

// RecordEvent appends one durable audit event.
func (s *Store) RecordEvent(ctx context.Context, event Event) (*Event, error) {
	if event.Type == "" {
		return nil, fmt.Errorf("event type is required")
	}
	if err := validateEventDetails(event); err != nil {
		panicEventValidationInTests(err)
		return nil, err
	}
	created := event.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	details, err := marshalJSON(event.Details)
	if err != nil {
		return nil, err
	}
	row := models.HookEvent{Type: event.Type, HookID: event.HookID, RunID: event.RunID, Message: event.Message, Details: details, CreatedAt: created}
	if err := s.write.WithContext(ctx).Create(&row).Error; err != nil {
		return nil, err
	}
	out, err := eventToRow(row)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListEvents returns audit events. Results are newest first by default and
// oldest first when query.Ascending is true.
func (s *Store) ListEvents(ctx context.Context, query EventQuery) ([]Event, error) {
	order := "created_at desc, id desc"
	if query.Ascending {
		order = "created_at asc, id asc"
	}
	q := s.read.WithContext(ctx).Order(order)
	if query.HookID != "" {
		q = q.Where("hook_id = ?", query.HookID)
	}
	if !query.AfterCreatedAt.IsZero() || query.AfterID != "" {
		q = q.Where("created_at > ? OR (created_at = ? AND id > ?)", query.AfterCreatedAt, query.AfterCreatedAt, query.AfterID)
	}
	if query.Limit > 0 {
		q = q.Limit(query.Limit)
	}
	var rows []models.HookEvent
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(rows))
	for _, row := range rows {
		event, err := eventToRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, nil
}

// GetEvent returns one audit event by ID.
func (s *Store) GetEvent(ctx context.Context, id string) (*Event, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("event id is required")
	}
	var row models.HookEvent
	if err := s.read.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	event, err := eventToRow(row)
	if err != nil {
		return nil, err
	}
	return &event, nil
}

// AppendHookLog appends one line of hook output.
func (s *Store) AppendHookLog(ctx context.Context, log models.HookLog) (*models.HookLog, error) {
	if log.HookID == "" {
		return nil, fmt.Errorf("hook id is required")
	}
	created := log.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	row := models.HookLog{HookID: log.HookID, RunID: log.RunID, Line: log.Line, CreatedAt: created}
	if err := s.write.WithContext(ctx).Create(&row).Error; err != nil {
		return nil, err
	}
	out := row
	return &out, nil
}

// AppendHookLogEvent appends one line of hook output and its audit event in one
// transaction.
func (s *Store) AppendHookLogEvent(ctx context.Context, log models.HookLog) (*models.HookLog, error) {
	if log.HookID == "" {
		return nil, fmt.Errorf("hook id is required")
	}
	created := log.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	row := models.HookLog{HookID: log.HookID, RunID: log.RunID, Line: log.Line, CreatedAt: created}
	if err := s.write.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		return recordEventTx(tx, Event{
			Type:      "hook.log",
			HookID:    row.HookID,
			RunID:     row.RunID,
			Message:   row.Line,
			Details:   map[string]any{"line_id": row.ID, "line": row.Line},
			CreatedAt: created,
		})
	}); err != nil {
		return nil, err
	}
	out := row
	return &out, nil
}

// ReplaceDiagnosticsForURI replaces the current diagnostics for one LSP hook URI.
func (s *Store) ReplaceDiagnosticsForURI(ctx context.Context, hookID, uri, path string, diagnostics []Diagnostic) error {
	hookID = strings.TrimSpace(hookID)
	uri = strings.TrimSpace(uri)
	path = filepath.ToSlash(strings.TrimSpace(path))
	if hookID == "" {
		return fmt.Errorf("hook id is required")
	}
	if uri == "" {
		return fmt.Errorf("diagnostic uri is required")
	}
	now := time.Now().UTC()
	return s.write.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("hook_id = ? AND uri = ?", hookID, uri).Delete(&models.HookDiagnostic{}).Error; err != nil {
			return err
		}
		rows := make([]models.HookDiagnostic, 0, len(diagnostics))
		for _, diagnostic := range diagnostics {
			message := strings.TrimSpace(diagnostic.Message)
			if message == "" {
				continue
			}
			diagPath := filepath.ToSlash(strings.TrimSpace(diagnostic.Path))
			if diagPath == "" {
				diagPath = path
			}
			diagURI := strings.TrimSpace(diagnostic.URI)
			if diagURI == "" {
				diagURI = uri
			}
			rows = append(rows, models.HookDiagnostic{
				HookID:    hookID,
				URI:       diagURI,
				Path:      diagPath,
				Severity:  normalizeSeverity(diagnostic.Severity),
				Source:    strings.TrimSpace(diagnostic.Source),
				Code:      strings.TrimSpace(diagnostic.Code),
				Message:   message,
				StartLine: diagnostic.StartLine,
				StartCol:  diagnostic.StartCol,
				EndLine:   diagnostic.EndLine,
				EndCol:    diagnostic.EndCol,
				UpdatedAt: now,
			})
		}
		if len(rows) > 0 {
			if err := tx.Create(&rows).Error; err != nil {
				return err
			}
		}
		return setLSPStatusFromDiagnosticsTx(tx, hookID, now)
	})
}

// SetLSPHookRunning marks an LSP hook as running while its server initializes.
func (s *Store) SetLSPHookRunning(ctx context.Context, hookID string) error {
	hookID = strings.TrimSpace(hookID)
	if hookID == "" {
		return fmt.Errorf("hook id is required")
	}
	return s.write.WithContext(ctx).Model(&models.HookStatus{}).Where("hook_id = ?", hookID).Updates(map[string]any{"status": string(models.StatusRunning), "last_error": "", "updated_at": time.Now().UTC()}).Error
}

// SetLSPHookReady records that an LSP hook has no pending lifecycle work. The
// terminal status is derived from its current diagnostics.
func (s *Store) SetLSPHookReady(ctx context.Context, hookID string) error {
	hookID = strings.TrimSpace(hookID)
	if hookID == "" {
		return fmt.Errorf("hook id is required")
	}
	now := time.Now().UTC()
	return s.write.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return setLSPStatusFromDiagnosticsTx(tx, hookID, now)
	})
}

// SetLSPHookError records an LSP hook lifecycle failure.
func (s *Store) SetLSPHookError(ctx context.Context, hookID, message string) error {
	hookID = strings.TrimSpace(hookID)
	message = strings.TrimSpace(message)
	if hookID == "" {
		return fmt.Errorf("hook id is required")
	}
	if message == "" {
		message = "language server error"
	}
	return s.write.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("hook_id = ?", hookID).Delete(&models.HookDiagnostic{}).Error; err != nil {
			return err
		}
		return tx.Model(&models.HookStatus{}).Where("hook_id = ?", hookID).Updates(map[string]any{"status": string(models.StatusFailure), "last_error": message, "updated_at": time.Now().UTC()}).Error
	})
}

// ListDiagnostics returns current LSP diagnostics ordered for display.
func (s *Store) ListDiagnostics(ctx context.Context, query DiagnosticQuery) ([]Diagnostic, error) {
	q := s.read.WithContext(ctx).Order("path asc, start_line asc, start_col asc, severity asc, id asc")
	if query.HookID != "" {
		q = q.Where("hook_id = ?", query.HookID)
	}
	if query.Limit > 0 {
		q = q.Limit(query.Limit)
	}
	var rows []models.HookDiagnostic
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Diagnostic, 0, len(rows))
	for _, row := range rows {
		out = append(out, diagnosticToRow(row))
	}
	return out, nil
}

func countDiagnosticsTx(tx *gorm.DB, hookID string) (int64, error) {
	var count int64
	err := tx.Model(&models.HookDiagnostic{}).Where("hook_id = ?", hookID).Count(&count).Error
	return count, err
}

func setLSPStatusFromDiagnosticsTx(tx *gorm.DB, hookID string, now time.Time) error {
	status := string(models.StatusSuccess)
	lastError := ""
	count, err := countDiagnosticsTx(tx, hookID)
	if err != nil {
		return err
	}
	if count > 0 {
		status = string(models.StatusFailure)
		lastError = fmt.Sprintf("%d diagnostics", count)
	}
	return tx.Model(&models.HookStatus{}).Where("hook_id = ?", hookID).Updates(map[string]any{"status": status, "last_error": lastError, "updated_at": now}).Error
}

func pruneLSPDiagnosticsForHookTx(tx *gorm.DB, hook hooks.Hook) (bool, error) {
	if !hook.IsLSP() {
		return false, nil
	}
	var rows []models.HookDiagnostic
	if err := tx.Select("id", "path").Where("hook_id = ?", hook.ID).Find(&rows).Error; err != nil {
		return false, err
	}
	staleIDs := make([]string, 0)
	for _, row := range rows {
		matched, err := lspDiagnosticPathMatchesHook(hook, row.Path)
		if err != nil {
			return false, err
		}
		if !matched {
			staleIDs = append(staleIDs, row.ID)
		}
	}
	if len(staleIDs) == 0 {
		return false, nil
	}
	if err := tx.Where("id IN ?", staleIDs).Delete(&models.HookDiagnostic{}).Error; err != nil {
		return false, err
	}
	return true, nil
}

func lspDiagnosticPathMatchesHook(hook hooks.Hook, path string) (bool, error) {
	path = filepath.ToSlash(strings.TrimSpace(path))
	if path == "" {
		return false, nil
	}
	matched, err := doublestar.Match(hook.Pattern, path)
	if err != nil || !matched {
		return matched, err
	}
	for _, pattern := range hook.Ignore {
		ignored, err := doublestar.Match(pattern, path)
		if err != nil {
			return false, err
		}
		if ignored {
			return false, nil
		}
	}
	return true, nil
}

func normalizeSeverity(severity string) string {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "error", "warning", "information", "hint":
		return strings.ToLower(strings.TrimSpace(severity))
	case "info":
		return "information"
	default:
		return "error"
	}
}

func diagnosticToRow(row models.HookDiagnostic) Diagnostic {
	return Diagnostic{
		ID:        row.ID,
		HookID:    row.HookID,
		URI:       row.URI,
		Path:      row.Path,
		Severity:  row.Severity,
		Source:    row.Source,
		Code:      row.Code,
		Message:   row.Message,
		StartLine: row.StartLine,
		StartCol:  row.StartCol,
		EndLine:   row.EndLine,
		EndCol:    row.EndCol,
		UpdatedAt: row.UpdatedAt,
	}
}

// ListHookLogs returns hook output lines oldest first.
func (s *Store) ListHookLogs(ctx context.Context, query HookLogQuery) ([]models.HookLog, error) {
	q := s.read.WithContext(ctx).Order("created_at asc, id asc")
	if query.HookID != "" {
		q = q.Where("hook_id = ?", query.HookID)
	}
	if query.RunID != "" {
		q = q.Where("run_id = ?", query.RunID)
	}
	if query.Limit > 0 {
		q = q.Limit(query.Limit)
	}
	var rows []models.HookLog
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return append([]models.HookLog(nil), rows...), nil
}

func (s *Store) PendingCount(ctx context.Context) (int64, error) {
	var count int64
	if err := s.read.WithContext(ctx).Model(&models.PendingHook{}).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// LoadWatchedSnapshot returns the last persisted watcher snapshot.
func (s *Store) LoadWatchedSnapshot(ctx context.Context) (map[string]watcher.Entry, error) {
	var rows []models.WatchedFile
	if err := s.read.WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make(map[string]watcher.Entry, len(rows))
	for _, row := range rows {
		path := filepath.ToSlash(row.Path)
		out[path] = watcher.Entry{Path: path, IsDir: row.IsDir, Size: row.Size, Mode: fs.FileMode(row.Mode), ModTime: row.ModTime}
	}
	return out, nil
}

// ReplaceWatchedSnapshot atomically replaces the persisted watcher snapshot.
func (s *Store) ReplaceWatchedSnapshot(ctx context.Context, snapshot map[string]watcher.Entry) error {
	now := time.Now().UTC()
	return s.write.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("1 = 1").Delete(&models.WatchedFile{}).Error; err != nil {
			return err
		}
		if len(snapshot) == 0 {
			return nil
		}
		rows := make([]models.WatchedFile, 0, len(snapshot))
		for path, entry := range snapshot {
			path = filepath.ToSlash(path)
			if path == "" || path == "." {
				continue
			}
			rows = append(rows, models.WatchedFile{Path: path, IsDir: entry.IsDir, Size: entry.Size, Mode: uint32(entry.Mode), ModTime: entry.ModTime, UpdatedAt: now})
		}
		return tx.Create(&rows).Error
	})
}

func ChangedFilesFromWatcher(changes []watcher.Change) []models.ChangedFile {
	out := make([]models.ChangedFile, 0, len(changes))
	for _, c := range changes {
		out = append(out, models.ChangedFile{Path: filepath.ToSlash(c.Path), Kind: c.Kind})
	}
	return normalizeChangedFiles(out)
}

func definitionFromHook(h hooks.Hook, now time.Time) (models.HookDefinition, error) {
	ignore, err := marshalJSON(h.Ignore)
	if err != nil {
		return models.HookDefinition{}, err
	}
	ext, err := marshalJSON(h.Extensions)
	if err != nil {
		return models.HookDefinition{}, err
	}
	hash, err := configHash(h)
	if err != nil {
		return models.HookDefinition{}, err
	}
	return models.HookDefinition{ID: h.ID, Name: h.Name, Description: h.Description, Type: string(h.Type), Engine: string(h.Engine), RunAs: string(h.RunAs), Blocking: h.Blocking, Pattern: h.Pattern, Ignore: ignore, Phase: h.Phase, Subagent: h.Subagent, LanguageID: h.LanguageID, MinSeverity: h.MinSeverity, Prompt: h.Prompt, AbsPath: h.AbsPath, RelPath: h.RelPath, HasShebang: h.HasShebang, Executable: h.Executable, Extensions: ext, ConfigHash: hash, CreatedAt: now, UpdatedAt: now}, nil
}

func definitionToHook(d models.HookDefinition) (hooks.Hook, error) {
	var ignore []string
	if len(d.Ignore) > 0 {
		if err := json.Unmarshal(d.Ignore, &ignore); err != nil {
			return hooks.Hook{}, err
		}
	}
	var ext map[string]any
	if len(d.Extensions) > 0 {
		if err := json.Unmarshal(d.Extensions, &ext); err != nil {
			return hooks.Hook{}, err
		}
	}
	return hooks.Hook{ID: d.ID, Name: d.Name, Description: d.Description, Type: hooks.HookType(d.Type), Engine: hooks.HookEngine(d.Engine), RunAs: hooks.RunAs(d.RunAs), Blocking: d.Blocking, Pattern: d.Pattern, Ignore: ignore, Phase: d.Phase, Subagent: d.Subagent, LanguageID: d.LanguageID, MinSeverity: d.MinSeverity, Prompt: d.Prompt, AbsPath: d.AbsPath, RelPath: d.RelPath, HasShebang: d.HasShebang, Executable: d.Executable, Extensions: ext}, nil
}

func pendingChangedFiles(p models.PendingHook) ([]models.ChangedFile, error) {
	var f []models.ChangedFile
	if len(p.ChangedFiles) > 0 {
		if err := json.Unmarshal(p.ChangedFiles, &f); err != nil {
			return nil, err
		}
	}
	return f, nil
}
func pendingChangeIDs(p models.PendingHook) ([]string, error) {
	var ids []string
	if len(p.ChangeIDs) > 0 {
		if err := json.Unmarshal(p.ChangeIDs, &ids); err != nil {
			return nil, err
		}
	}
	return normalizeIDs(ids), nil
}
func pendingToRow(p models.PendingHook) (PendingRow, error) {
	f, err := pendingChangedFiles(p)
	if err != nil {
		return PendingRow{}, err
	}
	ids, err := pendingChangeIDs(p)
	if err != nil {
		return PendingRow{}, err
	}
	return PendingRow{HookID: p.HookID, Position: p.Position, ChangedFiles: f, ChangeIDs: ids, Blocked: p.Blocked, BlockedByHookID: p.BlockedByHookID, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}, nil
}

func nextPendingPosition(tx *gorm.DB) (int64, error) {
	var maxPosition sql.NullInt64
	if err := tx.Model(&models.PendingHook{}).Select("max(position)").Scan(&maxPosition).Error; err != nil {
		return 0, err
	}
	return maxPosition.Int64 + 1, nil
}

func mergeInputsIntoPending(tx *gorm.DB, hookID string, files []models.ChangedFile, ids []string, now time.Time, next *int64) error {
	files = normalizeChangedFiles(files)
	ids = normalizeIDs(ids)
	var pending models.PendingHook
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("hook_id = ?", hookID).First(&pending).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		payload, err := marshalJSON(files)
		if err != nil {
			return err
		}
		idsPayload, err := marshalJSON(ids)
		if err != nil {
			return err
		}
		position := int64(1)
		if next != nil {
			position = *next
			(*next)++
		}
		pending = models.PendingHook{HookID: hookID, Position: position, ChangedFiles: payload, ChangeIDs: idsPayload, CreatedAt: now, UpdatedAt: now}
		return tx.Create(&pending).Error
	case err != nil:
		return err
	default:
		existing, err := pendingChangedFiles(pending)
		if err != nil {
			return err
		}
		pending.ChangedFiles, err = marshalJSON(mergeChangedFiles(existing, files))
		if err != nil {
			return err
		}
		existingIDs, err := pendingChangeIDs(pending)
		if err != nil {
			return err
		}
		pending.ChangeIDs, err = marshalJSON(mergeIDs(existingIDs, ids))
		if err != nil {
			return err
		}
		pending.Blocked = false
		pending.BlockedByHookID = ""
		pending.UpdatedAt = now
		return tx.Save(&pending).Error
	}
}

func runToRow(r models.HookRun) RunRow {
	var f []models.ChangedFile
	_ = json.Unmarshal(r.ChangedFiles, &f)
	var ids []string
	_ = json.Unmarshal(r.ChangeIDs, &ids)
	return RunRow{ID: r.ID, InvocationID: r.InvocationID, HookID: r.HookID, Status: models.Status(r.Status), ExitCode: r.ExitCode, ChangedFiles: f, ChangeIDs: normalizeIDs(ids), Error: r.Error, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt}
}

func lastFailedRunInputs(tx *gorm.DB, hookID string) ([]models.ChangedFile, []string, error) {
	var status models.HookStatus
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("hook_id = ?", hookID).First(&status).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if models.Status(status.Status) != models.StatusFailure || status.LastRunID == "" {
		return nil, nil, nil
	}
	var run models.HookRun
	err = tx.Where("id = ? AND hook_id = ?", status.LastRunID, hookID).First(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if models.Status(run.Status) != models.StatusFailure {
		return nil, nil, nil
	}
	files, ids := runInputs(run)
	return files, ids, nil
}

func mergeRunInputsIntoPending(tx *gorm.DB, run models.HookRun, updatedAt time.Time) error {
	var pending models.PendingHook
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("hook_id = ?", run.HookID).First(&pending).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	pendingFiles, err := pendingChangedFiles(pending)
	if err != nil {
		return err
	}
	pendingIDs, err := pendingChangeIDs(pending)
	if err != nil {
		return err
	}
	runFiles, runIDs := runInputs(run)
	pending.ChangedFiles, err = marshalJSON(mergeChangedFiles(runFiles, pendingFiles))
	if err != nil {
		return err
	}
	pending.ChangeIDs, err = marshalJSON(mergeIDs(runIDs, pendingIDs))
	if err != nil {
		return err
	}
	pending.Blocked = false
	pending.BlockedByHookID = ""
	pending.UpdatedAt = updatedAt
	return tx.Save(&pending).Error
}

func runInputs(run models.HookRun) ([]models.ChangedFile, []string) {
	var files []models.ChangedFile
	_ = json.Unmarshal(run.ChangedFiles, &files)
	var ids []string
	_ = json.Unmarshal(run.ChangeIDs, &ids)
	return normalizeChangedFiles(files), normalizeIDs(ids)
}

func observedToRow(c models.ObservedFileChange) ObservedFileChange {
	return ObservedFileChange{ID: c.ID, Path: c.Path, Kind: watcher.ChangeKind(c.Kind), BaseCommit: c.BaseCommit, Diff: c.Diff, CreatedAt: c.CreatedAt}
}

func workspaceSnapshotToRow(s models.WorkspaceSnapshot) (WorkspaceSnapshot, error) {
	var changed []models.ChangedFile
	if len(s.ChangedFiles) > 0 {
		if err := json.Unmarshal(s.ChangedFiles, &changed); err != nil {
			return WorkspaceSnapshot{}, err
		}
	}
	var omitted []SnapshotOmission
	if len(s.OmittedFiles) > 0 {
		if err := json.Unmarshal(s.OmittedFiles, &omitted); err != nil {
			return WorkspaceSnapshot{}, err
		}
	}
	var observedIDs []string
	if len(s.ObservedChangeIDs) > 0 {
		if err := json.Unmarshal(s.ObservedChangeIDs, &observedIDs); err != nil {
			return WorkspaceSnapshot{}, err
		}
	}
	return WorkspaceSnapshot{
		ID:                s.ID,
		ParentID:          s.ParentID,
		BaseCommit:        s.BaseCommit,
		TreeHash:          s.TreeHash,
		Patch:             append([]byte(nil), s.Patch...),
		PatchBytes:        s.PatchBytes,
		ChangedFiles:      normalizeChangedFiles(changed),
		OmittedFiles:      normalizeSnapshotOmissions(omitted),
		MaxFileBytes:      s.MaxFileBytes,
		ObservedChangeIDs: normalizeIDs(observedIDs),
		CreatedAt:         s.CreatedAt,
	}, nil
}

func daemonSessionToRow(s models.DaemonSession) DaemonSessionRow {
	return DaemonSessionRow{ID: s.ID, SessionID: s.SessionID, RepoRoot: s.RepoRoot, Version: s.Version, PID: s.PID, StartedAt: s.StartedAt, LastHeartbeat: s.LastHeartbeat, EndedAt: s.EndedAt, EndReason: s.EndReason}
}

func recordEventTx(tx *gorm.DB, event Event) error {
	if event.Type == "" {
		return fmt.Errorf("event type is required")
	}
	if err := validateEventDetails(event); err != nil {
		panicEventValidationInTests(err)
		return err
	}
	created := event.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	details, err := marshalJSON(event.Details)
	if err != nil {
		return err
	}
	return tx.Create(&models.HookEvent{Type: event.Type, HookID: event.HookID, RunID: event.RunID, Message: event.Message, Details: details, CreatedAt: created}).Error
}

func validateEventDetails(event Event) error {
	eventType := strings.TrimSpace(event.Type)
	if eventType == "" {
		return fmt.Errorf("event type is required")
	}
	known, ok := knownEventDetailSchemas()[eventType]
	if !ok {
		return fmt.Errorf("event type %q is not documented in KnownEventTypes", eventType)
	}
	var missing []string
	for _, detail := range known.Details {
		if !detail.Required {
			continue
		}
		if !eventDetailSet(event.Details, detail.Name) {
			missing = append(missing, detail.Name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("event type %q missing required detail fields: %s", eventType, strings.Join(missing, ", "))
	}
	return nil
}

func knownEventDetailSchemas() map[string]hookapi.EventTypeInfo {
	out := map[string]hookapi.EventTypeInfo{}
	for _, eventType := range hookapi.KnownEventTypes() {
		out[eventType.Type] = eventType
	}
	return out
}

func eventDetailSet(details map[string]any, name string) bool {
	value, ok := details[name]
	if !ok || value == nil {
		return false
	}
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v) != ""
	case time.Time:
		return !v.IsZero()
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Slice:
		return !rv.IsNil()
	}
	return true
}

func panicEventValidationInTests(err error) {
	if err != nil && strings.HasSuffix(os.Args[0], ".test") {
		panic(err)
	}
}

func eventToRow(e models.HookEvent) (Event, error) {
	details := map[string]any{}
	if len(e.Details) > 0 {
		if err := json.Unmarshal(e.Details, &details); err != nil {
			return Event{}, err
		}
	}
	return Event{ID: e.ID, Type: e.Type, HookID: e.HookID, RunID: e.RunID, Message: e.Message, Details: details, CreatedAt: e.CreatedAt}, nil
}
func marshalJSON(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}
func configHash(h hooks.Hook) (string, error) {
	b, err := json.Marshal(h)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
func closeGormDB(db *gorm.DB) error {
	if db == nil {
		return nil
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func normalizeChangedFiles(in []models.ChangedFile) []models.ChangedFile {
	out := make([]models.ChangedFile, 0, len(in))
	seen := map[string]models.ChangedFile{}
	for _, f := range in {
		f.Path = filepath.ToSlash(f.Path)
		if f.Path == "" {
			continue
		}
		seen[f.Path+"\x00"+string(f.Kind)] = f
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, seen[k])
	}
	return out
}
func mergeChangedFiles(a, b []models.ChangedFile) []models.ChangedFile {
	return normalizeChangedFiles(append(a, b...))
}

func normalizeIDs(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, id := range in {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func mergeIDs(a, b []string) []string {
	return normalizeIDs(append(a, b...))
}

func normalizeSnapshotOmissions(in []SnapshotOmission) []SnapshotOmission {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]SnapshotOmission{}
	for _, omission := range in {
		omission.Path = filepath.ToSlash(strings.TrimSpace(omission.Path))
		omission.Reason = strings.TrimSpace(omission.Reason)
		if omission.Path == "" || omission.Reason == "" {
			continue
		}
		seen[omission.Path+"\x00"+string(omission.Kind)+"\x00"+omission.Reason] = omission
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]SnapshotOmission, 0, len(keys))
	for _, key := range keys {
		out = append(out, seen[key])
	}
	return out
}
