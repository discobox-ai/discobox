// Package manager owns the long-lived hook manager used by the daemon.
package manager

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	hooks "github.com/obot-platform/discobox/hooks"
	"github.com/obot-platform/discobox/hooks/api/model"
	"github.com/obot-platform/discobox/hooks/service"
	"github.com/obot-platform/discobox/hooks/store"
)

// Manager coordinates hook-domain operations that are shared by the daemon's
// runtime loops and local socket API adapter.
type Manager struct {
	store     *store.Store
	service   *service.Service
	sessionID string
	repoRoot  string
	version   int64
	cancel    context.CancelFunc
	signalRun func()

	mu          sync.Mutex
	hooksByID   map[string]hooks.Hook
	runningHook int
	runningByID map[string]int
	activePhase map[string]struct{}
}

// Config controls New.
type Config struct {
	Store     *store.Store
	Hooks     []hooks.Hook
	SessionID string
	RepoRoot  string
	Version   int64
	Cancel    context.CancelFunc
	SignalRun func()
}

// New creates a hook manager.
func New(cfg Config) (*Manager, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("store is required")
	}
	if cfg.SessionID == "" {
		return nil, fmt.Errorf("session id is required")
	}
	if cfg.RepoRoot == "" {
		return nil, fmt.Errorf("repo root is required")
	}
	m := &Manager{
		store:       cfg.Store,
		sessionID:   cfg.SessionID,
		repoRoot:    cfg.RepoRoot,
		version:     cfg.Version,
		cancel:      cfg.Cancel,
		signalRun:   cfg.SignalRun,
		hooksByID:   map[string]hooks.Hook{},
		runningByID: map[string]int{},
		activePhase: map[string]struct{}{},
	}
	for _, h := range cfg.Hooks {
		m.hooksByID[h.ID] = h
	}
	svc, err := service.New(service.Config{Store: cfg.Store, HookSet: m})
	if err != nil {
		return nil, err
	}
	m.service = svc
	return m, nil
}

// ReplaceHooks replaces the discovered hook set after configuration reload.
func (m *Manager) ReplaceHooks(hooksList []hooks.Hook) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hooksByID = map[string]hooks.Hook{}
	for _, h := range hooksList {
		m.hooksByID[h.ID] = h
	}
}

// HookByID returns a discovered hook by ID.
func (m *Manager) HookByID(id string) (hooks.Hook, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.hooksByID[id]
	return h, ok
}

// SetRunning records whether an anonymous hook process is currently running.
func (m *Manager) SetRunning(running bool) {
	m.SetHookRunning("", running)
}

// SetHookRunning records whether a hook process is currently running.
func (m *Manager) SetHookRunning(hookID string, running bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if running {
		m.runningHook++
		if hookID != "" {
			m.runningByID[hookID]++
		}
		return
	}
	if m.runningHook > 0 {
		m.runningHook--
	}
	if hookID != "" {
		if m.runningByID[hookID] <= 1 {
			delete(m.runningByID, hookID)
		} else {
			m.runningByID[hookID]--
		}
	}
}

// RunningHookIDs returns hook IDs that currently have an in-flight run.
func (m *Manager) RunningHookIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.runningByID))
	for id := range m.runningByID {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// RunningCount returns the number of in-flight hook processes.
func (m *Manager) RunningCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runningHook
}

// Running reports whether a hook process is currently running.
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runningHook > 0
}

// Ping returns daemon reachability metadata.
func (m *Manager) Ping(_ context.Context) model.PingResponse {
	return model.PingResponse{OK: true, SessionID: m.sessionID, Version: m.version}
}

// Status returns current session status.
func (m *Manager) Status(ctx context.Context) (model.StatusResponse, error) {
	return m.service.Status(ctx, m.sessionID, m.repoRoot, m.Running())
}

// ListHooks returns discovered hooks with status metadata.
func (m *Manager) ListHooks(ctx context.Context) ([]model.HookStatus, error) {
	return m.service.ListHooks(ctx)
}

// ListEvents returns durable audit events.
func (m *Manager) ListEvents(ctx context.Context, req model.EventListRequest) ([]model.Event, error) {
	return m.service.ListEvents(ctx, req)
}

// ListRuns returns hook run history.
func (m *Manager) ListRuns(ctx context.Context, req model.RunListRequest) ([]model.Run, error) {
	return m.service.ListRuns(ctx, req)
}

// ListDiagnostics returns current LSP diagnostics.
func (m *Manager) ListDiagnostics(ctx context.Context, req model.DiagnosticListRequest) ([]model.Diagnostic, error) {
	return m.service.ListDiagnostics(ctx, req)
}

// ListObservedChanges returns observed filesystem changes.
func (m *Manager) ListObservedChanges(ctx context.Context, req model.ListRequest) ([]model.ObservedFileChange, error) {
	return m.service.ListObservedChanges(ctx, req)
}

// ListWorkspaceSnapshots returns captured workspace snapshots.
func (m *Manager) ListWorkspaceSnapshots(ctx context.Context, req model.ListRequest) ([]model.WorkspaceSnapshot, error) {
	return m.service.ListWorkspaceSnapshots(ctx, req)
}

// GetWorkspaceSnapshot returns one captured workspace snapshot.
func (m *Manager) GetWorkspaceSnapshot(ctx context.Context, snapshotID string) (model.WorkspaceSnapshot, error) {
	return m.service.GetWorkspaceSnapshot(ctx, snapshotID)
}

// ListQueue returns queued hook work.
func (m *Manager) ListQueue(ctx context.Context, req model.ListRequest) ([]model.QueuedHook, error) {
	return m.service.ListQueue(ctx, req)
}

// SetGlobalExecution pauses or resumes all hook execution.
func (m *Manager) SetGlobalExecution(ctx context.Context, req model.ExecutionPatchRequest) (model.ExecutionResponse, error) {
	resp, err := m.service.SetGlobalExecution(ctx, req)
	if !req.Paused {
		m.SignalRun()
	}
	if err != nil {
		return model.ExecutionResponse{}, err
	}
	if req.Paused {
		_ = m.RecordEvent(ctx, "execution.paused", "", "", "all hook execution paused", map[string]any{"paused": true, "scope": "global"})
	} else {
		_ = m.RecordEvent(ctx, "execution.resumed", "", "", "all hook execution resumed", map[string]any{"paused": false, "scope": "global"})
	}
	return resp, nil
}

// SetHookExecution pauses or resumes one hook.
func (m *Manager) SetHookExecution(ctx context.Context, hookID string, req model.ExecutionPatchRequest) (model.ExecutionResponse, error) {
	resp, err := m.service.SetHookExecution(ctx, hookID, req)
	if !req.Paused {
		m.SignalRun()
	}
	if err != nil {
		return model.ExecutionResponse{}, err
	}
	if req.Paused {
		_ = m.RecordEvent(ctx, "hook.paused", hookID, "", "hook paused", map[string]any{"paused": true, "scope": "hook"})
	} else {
		_ = m.RecordEvent(ctx, "hook.resumed", hookID, "", "hook resumed", map[string]any{"paused": false, "scope": "hook"})
	}
	return resp, nil
}

// RunHook enqueues a hook run when API-level skip/force semantics allow it.
func (m *Manager) RunHook(ctx context.Context, hookID string, req model.RunRequest) (model.RunResponse, error) {
	resp, err := m.service.RunHook(ctx, hookID, req)
	if err != nil {
		return model.RunResponse{}, err
	}
	if resp.Skipped {
		if resp.Reason == "already_queued" {
			if req.Phase != "" {
				m.ActivatePhase(req.Phase)
			}
			m.SignalRun()
		}
		_ = m.RecordEvent(ctx, "hook.run.skipped", hookID, "", "hook run skipped", map[string]any{"reason": resp.Reason, "force": req.Force, "phase": req.Phase, "enqueued": resp.Enqueued})
	} else {
		if req.Phase != "" {
			m.ActivatePhase(req.Phase)
		}
		_ = m.RecordEvent(ctx, "hook.run.requested", hookID, "", "hook run requested", map[string]any{"force": req.Force, "phase": req.Phase, "enqueued": resp.Enqueued})
		m.SignalRun()
	}
	return resp, nil
}

// Output returns the latest captured hook output.
func (m *Manager) Output(ctx context.Context, hookID string) (model.OutputResponse, error) {
	return m.service.Output(ctx, hookID)
}

// Shutdown requests daemon shutdown.
func (m *Manager) Shutdown(ctx context.Context) model.ShutdownResponse {
	_ = m.RecordEvent(ctx, "daemon.shutdown.requested", "", "", "hook daemon shutdown requested", map[string]any{"session_id": m.sessionID, "repo_root": m.repoRoot})
	if m.cancel != nil {
		go m.cancel()
	}
	return model.ShutdownResponse{OK: true}
}

// GlobalPaused reports whether global hook execution is paused.
func (m *Manager) GlobalPaused(ctx context.Context) (bool, error) {
	return m.service.GlobalPaused(ctx)
}

// ActivatePhase allows queued hooks in phase to run until no eligible pending
// work remains.
func (m *Manager) ActivatePhase(phase string) {
	phase = strings.TrimSpace(strings.ToLower(phase))
	if phase == "" {
		return
	}
	m.mu.Lock()
	m.activePhase[phase] = struct{}{}
	m.mu.Unlock()
}

// ActivePhases returns the currently allowed phase set for queue draining.
func (m *Manager) ActivePhases() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.activePhase))
	for phase := range m.activePhase {
		out = append(out, phase)
	}
	sort.Strings(out)
	return out
}

// ClearActivePhases clears temporary phase run permissions.
func (m *Manager) ClearActivePhases() {
	m.mu.Lock()
	m.activePhase = map[string]struct{}{}
	m.mu.Unlock()
}

// SignalRun wakes the daemon's queue drain loop.
func (m *Manager) SignalRun() {
	if m.signalRun != nil {
		m.signalRun()
	}
}

// RecordEvent appends an audit event. Callers in runtime hot paths may ignore
// the returned error when audit recording must not make daemon work fail.
func (m *Manager) RecordEvent(ctx context.Context, eventType, hookID string, runID string, message string, details map[string]any) error {
	if m == nil || m.store == nil {
		return nil
	}
	_, err := m.store.RecordEvent(ctx, store.Event{Type: eventType, HookID: hookID, RunID: runID, Message: message, Details: details})
	return err
}
