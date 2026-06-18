// Package service contains hooks application operations used by daemon APIs.
package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	hooks "github.com/obot-platform/discobox/hooks"
	"github.com/obot-platform/discobox/hooks/api/model"
	"github.com/obot-platform/discobox/hooks/models"
	"github.com/obot-platform/discobox/hooks/store"
)

// Service coordinates API-level hook operations over persistent state.
type Service struct {
	store   *store.Store
	hookSet HookSet
}

// HookSet exposes the daemon's current discovered hooks to the service.
type HookSet interface {
	HookByID(id string) (hooks.Hook, bool)
}

// Config controls New.
type Config struct {
	Store   *store.Store
	HookSet HookSet
}

// New creates a hook service.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("store is required")
	}
	if cfg.HookSet == nil {
		return nil, fmt.Errorf("hook set is required")
	}
	return &Service{store: cfg.Store, hookSet: cfg.HookSet}, nil
}

// Status returns current session status.
func (s *Service) Status(ctx context.Context, sessionID, repoRoot string, running bool) (model.StatusResponse, error) {
	paused, err := s.GlobalPaused(ctx)
	if err != nil {
		return model.StatusResponse{}, err
	}
	queued, err := s.store.PendingCount(ctx)
	if err != nil {
		return model.StatusResponse{}, err
	}
	hooks, err := s.ListHooks(ctx)
	if err != nil {
		return model.StatusResponse{}, err
	}
	return model.StatusResponse{SessionID: sessionID, RepoRoot: repoRoot, Paused: paused, Running: running, Queued: int(queued), Hooks: hooks, UpdatedAt: time.Now().UTC()}, nil
}

// ListHooks returns discovered hooks with current execution status.
func (s *Service) ListHooks(ctx context.Context) ([]model.HookStatus, error) {
	rows, err := s.store.ListStatus(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]model.HookStatus, 0, len(rows))
	for _, row := range rows {
		out = append(out, model.HookStatus{
			Hook:       row.Hook,
			ConfigHash: row.ConfigHash,
			Status:     row.Status,
			Paused:     row.Paused,
			RunCount:   row.RunCount,
			FailCount:  row.FailCount,
			LastRunID:  row.LastRunID,
			LastError:  row.LastError,
			UpdatedAt:  row.UpdatedAt,
		})
	}
	return out, nil
}

// ListEvents returns durable audit events.
func (s *Service) ListEvents(ctx context.Context, req model.EventListRequest) ([]model.Event, error) {
	rows, err := s.store.ListEvents(ctx, store.EventQuery{HookID: req.HookID, Limit: req.Limit})
	if err != nil {
		return nil, err
	}
	out := make([]model.Event, 0, len(rows))
	for _, row := range rows {
		out = append(out, model.Event(row))
	}
	return out, nil
}

// ListRuns returns hook run history.
func (s *Service) ListRuns(ctx context.Context, req model.RunListRequest) ([]model.Run, error) {
	rows, err := s.store.ListRuns(ctx, req.HookID, req.Limit)
	if err != nil {
		return nil, err
	}
	out := make([]model.Run, 0, len(rows))
	for _, row := range rows {
		out = append(out, model.Run{
			ID:           row.ID,
			InvocationID: row.InvocationID,
			HookID:       row.HookID,
			Status:       row.Status,
			ExitCode:     row.ExitCode,
			ChangedFiles: changedFilesToAPI(row.ChangedFiles),
			ChangeIDs:    row.ChangeIDs,
			Error:        row.Error,
			StartedAt:    row.StartedAt,
			FinishedAt:   row.FinishedAt,
		})
	}
	return out, nil
}

// ListObservedChanges returns observed filesystem changes.
func (s *Service) ListObservedChanges(ctx context.Context, req model.ListRequest) ([]model.ObservedFileChange, error) {
	rows, err := s.store.ListObservedChanges(ctx, req.Limit)
	if err != nil {
		return nil, err
	}
	out := make([]model.ObservedFileChange, 0, len(rows))
	for _, row := range rows {
		out = append(out, model.ObservedFileChange{
			ID:         row.ID,
			Path:       row.Path,
			Kind:       string(row.Kind),
			BaseCommit: row.BaseCommit,
			Diff:       row.Diff,
			CreatedAt:  row.CreatedAt,
		})
	}
	return out, nil
}

// ListWorkspaceSnapshots returns captured workspace snapshots.
func (s *Service) ListWorkspaceSnapshots(ctx context.Context, req model.ListRequest) ([]model.WorkspaceSnapshot, error) {
	rows, err := s.store.ListWorkspaceSnapshots(ctx, req.Limit)
	if err != nil {
		return nil, err
	}
	out := make([]model.WorkspaceSnapshot, 0, len(rows))
	for _, row := range rows {
		omitted := make([]model.SnapshotOmission, 0, len(row.OmittedFiles))
		for _, omittedFile := range row.OmittedFiles {
			omitted = append(omitted, model.SnapshotOmission{
				Path:       omittedFile.Path,
				Kind:       string(omittedFile.Kind),
				Reason:     omittedFile.Reason,
				SizeBytes:  omittedFile.SizeBytes,
				LimitBytes: omittedFile.LimitBytes,
			})
		}
		out = append(out, model.WorkspaceSnapshot{
			ID:                row.ID,
			ParentID:          row.ParentID,
			BaseCommit:        row.BaseCommit,
			TreeHash:          row.TreeHash,
			PatchBytes:        row.PatchBytes,
			ChangedFiles:      changedFilesToAPI(row.ChangedFiles),
			OmittedFiles:      omitted,
			MaxFileBytes:      row.MaxFileBytes,
			ObservedChangeIDs: row.ObservedChangeIDs,
			CreatedAt:         row.CreatedAt,
		})
	}
	return out, nil
}

// ListQueue returns queued hook work.
func (s *Service) ListQueue(ctx context.Context, req model.ListRequest) ([]model.QueuedHook, error) {
	rows, err := s.store.ListPending(ctx, req.Limit)
	if err != nil {
		return nil, err
	}
	out := make([]model.QueuedHook, 0, len(rows))
	for _, row := range rows {
		out = append(out, model.QueuedHook{
			HookID:          row.HookID,
			Position:        row.Position,
			ChangedFiles:    changedFilesToAPI(row.ChangedFiles),
			ChangeIDs:       row.ChangeIDs,
			Blocked:         row.Blocked,
			BlockedByHookID: row.BlockedByHookID,
			CreatedAt:       row.CreatedAt,
			UpdatedAt:       row.UpdatedAt,
		})
	}
	return out, nil
}

// SetGlobalExecution pauses or resumes global hook execution.
func (s *Service) SetGlobalExecution(ctx context.Context, req model.ExecutionPatchRequest) (model.ExecutionResponse, error) {
	if req.Paused {
		return model.ExecutionResponse{Paused: true}, s.store.PauseGlobal(ctx)
	}
	return model.ExecutionResponse{Paused: false}, s.store.ResumeGlobal(ctx)
}

// SetHookExecution pauses or resumes one hook.
func (s *Service) SetHookExecution(ctx context.Context, hookID string, req model.ExecutionPatchRequest) (model.ExecutionResponse, error) {
	if _, ok := s.hookSet.HookByID(hookID); !ok {
		return model.ExecutionResponse{}, ErrNotFound
	}
	if req.Paused {
		return model.ExecutionResponse{Paused: true}, s.store.PauseHook(ctx, hookID)
	}
	return model.ExecutionResponse{Paused: false}, s.store.ResumeHook(ctx, hookID)
}

// RunHook enqueues a hook if it has not already succeeded, unless Force is set.
func (s *Service) RunHook(ctx context.Context, hookID string, req model.RunRequest) (model.RunResponse, error) {
	h, ok := s.hookSet.HookByID(hookID)
	if !ok {
		return model.RunResponse{}, ErrNotFound
	}
	req.Phase = strings.TrimSpace(strings.ToLower(req.Phase))
	if phase := strings.ToLower(h.NormalizedPhase()); phase != "" && req.Phase != phase {
		reason := "phase_required"
		if req.Phase != "" {
			reason = "phase_mismatch"
		}
		return model.RunResponse{HookID: hookID, Skipped: true, Reason: reason}, nil
	}
	shouldRun, reason, err := s.shouldRunHook(ctx, hookID, req.Force)
	if err != nil {
		return model.RunResponse{}, err
	}
	if !shouldRun {
		return model.RunResponse{HookID: hookID, Skipped: true, Reason: reason}, nil
	}
	files, changeIDs, err := s.forceRunInputs(ctx, hookID, req.Force)
	if err != nil {
		return model.RunResponse{}, err
	}
	if err := s.store.EnqueueWithChangeIDs(ctx, []string{hookID}, files, changeIDs); err != nil {
		return model.RunResponse{}, err
	}
	return model.RunResponse{Enqueued: true, HookID: hookID}, nil
}

// Output returns the latest captured output for a hook.
func (s *Service) Output(ctx context.Context, hookID string) (model.OutputResponse, error) {
	rows, err := s.store.ListStatus(ctx)
	if err != nil {
		return model.OutputResponse{}, err
	}
	for _, row := range rows {
		if row.Hook.ID != hookID {
			continue
		}
		if row.LastRunID == "" {
			return model.OutputResponse{HookID: hookID}, nil
		}
		logs, err := s.store.ListHookLogs(ctx, store.HookLogQuery{HookID: hookID, RunID: row.LastRunID})
		if err != nil {
			return model.OutputResponse{}, err
		}
		return model.OutputResponse{HookID: hookID, Output: hookLogOutput(logs)}, nil
	}
	return model.OutputResponse{}, ErrNotFound
}

// GlobalPaused reports whether global hook execution is paused.
func (s *Service) GlobalPaused(ctx context.Context) (bool, error) {
	v, ok, err := s.store.GetDaemonState(ctx, "paused")
	return ok && v == "true", err
}

func (s *Service) shouldRunHook(ctx context.Context, id string, force bool) (bool, string, error) {
	if force {
		return true, "", nil
	}
	rows, err := s.store.ListStatus(ctx)
	if err != nil {
		return false, "", err
	}
	for _, row := range rows {
		if row.Hook.ID != id {
			continue
		}
		switch row.Status {
		case models.StatusIdle:
			if row.RunCount == 0 {
				return true, "", nil
			}
			return false, "already_ran", nil
		case models.StatusFailure:
			return true, "", nil
		case models.StatusSuccess:
			return false, "already_succeeded", nil
		case models.StatusQueued:
			return false, "already_queued", nil
		case models.StatusRunning:
			return false, "already_running", nil
		default:
			return true, "", nil
		}
	}
	return false, "not_found", nil
}

func (s *Service) forceRunInputs(ctx context.Context, hookID string, force bool) ([]models.ChangedFile, []string, error) {
	if !force {
		return nil, nil, nil
	}
	runs, err := s.store.ListRuns(ctx, hookID, 1)
	if err != nil {
		return nil, nil, err
	}
	if len(runs) == 0 {
		return nil, nil, nil
	}
	return runs[0].ChangedFiles, runs[0].ChangeIDs, nil
}

func hookLogOutput(logs []models.HookLog) string {
	if len(logs) == 0 {
		return ""
	}
	var b strings.Builder
	for _, log := range logs {
		b.WriteString(log.Line)
		b.WriteByte('\n')
	}
	return b.String()
}

func changedFilesToAPI(files []models.ChangedFile) []model.ChangedFile {
	if len(files) == 0 {
		return nil
	}
	out := make([]model.ChangedFile, 0, len(files))
	for _, file := range files {
		out = append(out, model.ChangedFile{Path: file.Path, Kind: string(file.Kind)})
	}
	return out
}
