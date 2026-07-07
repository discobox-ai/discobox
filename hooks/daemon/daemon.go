package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	hooks "github.com/obot-platform/discobox/hooks"
	hookapigen "github.com/obot-platform/discobox/hooks/api/gen"
	"github.com/obot-platform/discobox/hooks/api/model"
	"github.com/obot-platform/discobox/hooks/manager"
	"github.com/obot-platform/discobox/hooks/matcher"
	"github.com/obot-platform/discobox/hooks/models"
	"github.com/obot-platform/discobox/hooks/parser"
	"github.com/obot-platform/discobox/hooks/runner"
	"github.com/obot-platform/discobox/hooks/store"
	"github.com/obot-platform/discobox/hooks/watcher"
	"gorm.io/gorm/logger"
)

const defaultDebounce = 5 * time.Second
const defaultSnapshotDebounce = 15 * time.Second
const defaultSnapshotMinInterval = time.Minute
const defaultLSPDiagnosticsGrace = 5 * time.Second
const defaultMaxParallelHooks = 3

// daemonMatcherOptions disables matcher-level Git-ignore checks because the
// daemon already applies and audits Git-ignore filtering in filterIgnoredChanges.
// If that daemon-level check fails, the documented policy is fail-open: keep the
// batch and let hook matching continue rather than retrying in matcher.Match and
// dropping the batch on the same Git-ignore failure.
var daemonMatcherOptions = matcher.Options{DisableGitIgnore: true}

// Config configures one session-scoped daemon runtime.
type Config struct {
	SessionID           string
	RepoRoot            string
	DBPath              string
	SocketPath          string
	RuntimePath         string
	TempDir             string
	Version             int64
	Debounce            time.Duration
	SnapshotDebounce    time.Duration
	SnapshotMinInterval time.Duration
	IdleTimeout         time.Duration
	MaxParallelHooks    int
}

type PingResponse = model.PingResponse
type StatusResponse = model.StatusResponse
type HooksResponse = model.HooksResponse
type EventsResponse = model.EventsResponse
type ExecutionPatchRequest = model.ExecutionPatchRequest
type ExecutionResponse = model.ExecutionResponse
type RunRequest = model.RunRequest
type RunResponse = model.RunResponse
type OutputResponse = model.OutputResponse
type WaitResponse = model.WaitResponse
type ShutdownResponse = model.ShutdownResponse

// Run starts a session daemon and blocks until ctx is canceled, /shutdown is
// called, idle timeout elapses, or a fatal startup error occurs.
func Run(ctx context.Context, cfg Config) error {
	r, err := newRuntime(ctx, cfg)
	if err != nil {
		return err
	}
	return r.run(ctx)
}

type runtimeState struct {
	cfg       Config
	store     *store.Store
	manager   *manager.Manager
	discovery *parser.Discovery
	server    *http.Server
	listener  net.Listener
	watcher   *watcher.Watcher
	sessionID string

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu              sync.Mutex
	pendingBatch    []watcher.Change
	pendingSnapshot map[string]watcher.Entry
	lastActivity    time.Time
	snapshotPending bool
	snapshotRunning bool
	snapshotDirty   bool
	lastSnapshotAt  time.Time
	snapshotCapture func(context.Context) (*store.WorkspaceSnapshot, error)
	activeRequests  int64
	drainSignal     chan struct{}
	snapshotSignal  chan struct{}
	lspClients      map[string]*lspRuntime
	pendingLSP      map[string]time.Time
}

func newRuntime(ctx context.Context, cfg Config) (*runtimeState, error) {
	if strings.TrimSpace(cfg.SessionID) == "" {
		return nil, fmt.Errorf("session id is required")
	}
	if cfg.RepoRoot == "" {
		return nil, fmt.Errorf("repo root is required")
	}
	root, err := filepath.Abs(cfg.RepoRoot)
	if err != nil {
		return nil, err
	}
	cfg.RepoRoot = root
	if cfg.DBPath == "" {
		return nil, fmt.Errorf("db path is required")
	}
	if cfg.SocketPath == "" {
		return nil, fmt.Errorf("socket path is required")
	}
	if cfg.TempDir == "" {
		cfg.TempDir = filepath.Join(filepath.Dir(cfg.SocketPath), "tmp")
	}
	tempDir, err := filepath.Abs(cfg.TempDir)
	if err != nil {
		return nil, err
	}
	cfg.TempDir = tempDir
	if err := resetRuntimeTempDir(cfg.TempDir); err != nil {
		return nil, err
	}
	if cfg.Debounce <= 0 {
		cfg.Debounce = defaultDebounce
	}
	if cfg.SnapshotDebounce <= 0 {
		cfg.SnapshotDebounce = defaultSnapshotDebounce
	}
	if cfg.SnapshotMinInterval <= 0 {
		cfg.SnapshotMinInterval = defaultSnapshotMinInterval
	}
	if cfg.MaxParallelHooks <= 0 {
		cfg.MaxParallelHooks = defaultMaxParallelHooks
	}

	st, err := store.Open(ctx, store.Options{Path: cfg.DBPath, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, err
	}
	disc, err := parser.Discover(cfg.RepoRoot)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	if err := st.RefreshDefinitions(ctx, disc.Hooks); err != nil {
		_ = st.Close()
		return nil, err
	}

	rctx, cancel := context.WithCancel(context.Background())
	r := &runtimeState{cfg: cfg, store: st, discovery: disc, ctx: rctx, cancel: cancel, done: make(chan struct{}), lastActivity: time.Now().UTC(), drainSignal: make(chan struct{}, 1), snapshotSignal: make(chan struct{}, 1), lspClients: map[string]*lspRuntime{}, pendingLSP: map[string]time.Time{}}
	session, err := st.StartDaemonSession(ctx, cfg.SessionID, cfg.RepoRoot, cfg.Version, os.Getpid())
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	mgr, err := manager.New(manager.Config{Store: st, Hooks: disc.Hooks, SessionID: cfg.SessionID, RepoRoot: cfg.RepoRoot, Version: cfg.Version, Cancel: cancel, SignalRun: r.signalDrain, ActivateLSP: r.requestLSPRun})
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	r.manager = mgr
	r.sessionID = session.ID
	return r, nil
}

func (r *runtimeState) run(parent context.Context) (err error) {
	defer func() {
		if r.watcher != nil {
			_ = r.watcher.Close()
		}
		r.closeLSPClients()
		r.cancel()
		if r.server != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = r.server.Shutdown(ctx)
			cancel()
		}
		if r.listener != nil {
			_ = r.listener.Close()
		}
		r.removeRuntimeFile()
		_ = os.Remove(r.cfg.SocketPath)
		if r.sessionID != "" {
			_ = r.store.EndDaemonSession(context.Background(), r.sessionID, "shutdown")
		}
		if closeErr := r.store.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()

	if err := r.startServer(); err != nil {
		return err
	}
	if err := r.writeRuntimeFile(); err != nil {
		return err
	}
	r.syncLSPHooks()
	if _, err := r.store.ReconcileRunningRuns(r.ctx, "hook run interrupted before daemon startup"); err != nil {
		return err
	}
	go r.serve()

	initialSnapshot, err := r.store.LoadWatchedSnapshot(r.ctx)
	if err != nil {
		return err
	}
	w, err := watcher.New(r.cfg.RepoRoot, watcher.Options{Debounce: 200 * time.Millisecond, InitialSnapshot: initialSnapshot, PeriodicResync: time.Second, Ignore: ignoreHookRuntimePaths(r.cfg)})
	if err != nil {
		return err
	}
	if initialSnapshot == nil {
		initialChanges := initialWorkingTreeChanges(r.ctx, r.cfg.RepoRoot)
		if len(initialChanges) > 0 {
			r.addBatch(initialChanges, w.Snapshot())
		} else if err := r.store.ReplaceWatchedSnapshot(r.ctx, w.Snapshot()); err != nil {
			_ = w.Close()
			return err
		}
	}
	r.watcher = w
	go r.watchLoop(w)
	go r.schedulerLoop()
	go r.snapshotLoop()
	go r.drainLoop()
	go r.idleLoop()
	go r.heartbeatLoop()
	r.signalDrain()

	select {
	case <-parent.Done():
		return parent.Err()
	case <-r.ctx.Done():
		return nil
	}
}

func (r *runtimeState) startServer() error {
	if err := os.MkdirAll(filepath.Dir(r.cfg.SocketPath), 0o755); err != nil {
		return err
	}
	if err := prepareSocketPath(r.cfg.SocketPath); err != nil {
		return err
	}
	ln, err := (&net.ListenConfig{}).Listen(r.ctx, "unix", r.cfg.SocketPath)
	if err != nil {
		return err
	}
	routes, err := r.generatedRoutes()
	if err != nil {
		_ = ln.Close()
		r.listener = nil
		return err
	}
	r.listener = ln
	r.server = &http.Server{
		Handler:           r.withRequestTracking(routes),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
	}
	return nil
}

func prepareSocketPath(path string) error {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	conn, err := (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext(context.Background(), "unix", path)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("hook daemon socket %s is already in use", path)
	}
	return os.Remove(path)
}

func resetRuntimeTempDir(path string) error {
	path = filepath.Clean(path)
	if path == "." || path == string(filepath.Separator) {
		return fmt.Errorf("refusing to reset unsafe temporary directory %q", path)
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("clean temporary directory %s: %w", path, err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("create temporary directory %s: %w", path, err)
	}
	return nil
}

type runtimeFile struct {
	SessionID string    `json:"session_id"`
	RepoRoot  string    `json:"repo_root"`
	Version   int64     `json:"version"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

func (r *runtimeState) writeRuntimeFile() error {
	if r.cfg.RuntimePath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(r.cfg.RuntimePath), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(runtimeFile{
		SessionID: r.cfg.SessionID,
		RepoRoot:  r.cfg.RepoRoot,
		Version:   r.cfg.Version,
		PID:       os.Getpid(),
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	return os.WriteFile(r.cfg.RuntimePath, append(data, '\n'), 0o600)
}

func (r *runtimeState) removeRuntimeFile() {
	if r.cfg.RuntimePath == "" {
		return
	}
	data, err := os.ReadFile(r.cfg.RuntimePath)
	if err == nil {
		var runtime runtimeFile
		if json.Unmarshal(data, &runtime) == nil && runtime.PID != 0 && runtime.PID != os.Getpid() {
			return
		}
	}
	_ = os.Remove(r.cfg.RuntimePath)
}

func (r *runtimeState) serve() {
	if err := r.server.Serve(r.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		r.cancel()
	}
}

func (r *runtimeState) generatedRoutes() (http.Handler, error) {
	generated, err := hookapigen.NewServer(
		&generatedHandler{manager: r.manager, wait: r.wait},
		hookapigen.WithNotFound(func(w http.ResponseWriter, req *http.Request) { http.NotFound(w, req) }),
		hookapigen.WithMethodNotAllowed(func(w http.ResponseWriter, _ *http.Request, _ string) { methodNotAllowed(w) }),
	)
	if err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet && req.URL.Path == "/events/stream" {
			r.handleEventsStream(w, req)
			return
		}
		if req.Method == http.MethodGet && req.URL.Path == "/wait" {
			r.handleWait(w, req)
			return
		}
		generated.ServeHTTP(w, req)
	}), nil
}

func (r *runtimeState) routes() http.Handler {
	h, err := r.generatedRoutes()
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeError(w, http.StatusInternalServerError, err) })
	}
	return h
}

func (r *runtimeState) withRequestTracking(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		atomic.AddInt64(&r.activeRequests, 1)
		r.touch()
		defer func() {
			atomic.AddInt64(&r.activeRequests, -1)
			r.touch()
		}()
		next.ServeHTTP(w, req)
	})
}

func (r *runtimeState) watchLoop(w *watcher.Watcher) {
	for {
		select {
		case <-r.ctx.Done():
			return
		case batch, ok := <-w.Batches():
			if !ok {
				return
			}
			if len(batch.Changes) > 0 {
				r.addBatch(batch.Changes, batch.Snapshot)
			} else if batch.Snapshot != nil {
				if err := r.store.ReplaceWatchedSnapshot(r.ctx, batch.Snapshot); err != nil {
					_ = r.recordEvent("watch.snapshot.persist.failed", "", "", "watch snapshot persist failed", map[string]any{"files": len(batch.Snapshot), "error": err.Error()})
				}
			}
		case <-w.Errors():
		}
	}
}

func (r *runtimeState) schedulerLoop() {
	var timer *time.Timer
	var timerC <-chan time.Time
	for {
		select {
		case <-r.ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-timerC:
			timerC = nil
			r.flushBatch()
		case <-time.After(100 * time.Millisecond):
			if r.hasBatch() && timerC == nil {
				if timer == nil {
					timer = time.NewTimer(r.cfg.Debounce)
				} else {
					timer.Reset(r.cfg.Debounce)
				}
				timerC = timer.C
			}
		}
	}
}

func (r *runtimeState) snapshotLoop() {
	var timer *time.Timer
	var timerC <-chan time.Time
	stopTimer := func() {
		if timer == nil {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timerC = nil
	}
	resetTimer := func(delay time.Duration) {
		if delay < 0 {
			delay = 0
		}
		if timer == nil {
			timer = time.NewTimer(delay)
		} else {
			timer.Reset(delay)
		}
		timerC = timer.C
	}
	nextDelay := func() time.Duration {
		delay := r.cfg.SnapshotDebounce
		if delay < 0 {
			delay = 0
		}
		if r.cfg.SnapshotMinInterval <= 0 {
			return delay
		}
		r.mu.Lock()
		lastSnapshotAt := r.lastSnapshotAt
		r.mu.Unlock()
		if lastSnapshotAt.IsZero() {
			return delay
		}
		minDelay := time.Until(lastSnapshotAt.Add(r.cfg.SnapshotMinInterval))
		if minDelay > delay {
			return minDelay
		}
		return delay
	}
	defer stopTimer()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-r.snapshotSignal:
			r.mu.Lock()
			r.snapshotPending = true
			r.lastActivity = time.Now().UTC()
			running := r.snapshotRunning
			r.mu.Unlock()
			if !running {
				stopTimer()
				resetTimer(nextDelay())
			}
		case <-timerC:
			timerC = nil
			r.runSnapshot()
			r.mu.Lock()
			rerun := r.snapshotDirty || r.snapshotPending
			if rerun {
				r.snapshotDirty = false
				r.snapshotPending = true
			}
			r.mu.Unlock()
			if rerun {
				resetTimer(nextDelay())
			}
		}
	}
}

func (r *runtimeState) drainLoop() {
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-r.drainSignal:
			r.drainAvailable()
		}
	}
}

func (r *runtimeState) drainAvailable() {
	for r.startOneAsync() {
	}
}

func (r *runtimeState) startOneAsync() bool {
	if r.manager.RunningCount() >= r.cfg.MaxParallelHooks {
		return false
	}
	pending, h, ok := r.nextRunnable()
	if !ok {
		return false
	}
	r.manager.SetHookRunning(h.ID, true)
	r.touch()
	go func() {
		res := r.runHook(r.ctx, h, pending.ChangedFiles, pending.ChangeIDs)
		r.manager.SetHookRunning(h.ID, false)
		r.touch()
		if res.Success {
			r.signalDrain()
		}
	}()
	return true
}

func (r *runtimeState) idleLoop() {
	if r.cfg.IdleTimeout <= 0 {
		return
	}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-t.C:
			if r.isIdle() {
				r.cancel()
				return
			}
		}
	}
}

func (r *runtimeState) heartbeatLoop() {
	if r.sessionID == "" {
		return
	}
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-t.C:
			if err := r.store.HeartbeatDaemonSession(r.ctx, r.sessionID); err != nil {
				_ = r.recordEvent("daemon.heartbeat.failed", "", "", "daemon heartbeat failed", map[string]any{"daemon_session_id": r.sessionID, "session_id": r.cfg.SessionID, "repo_root": r.cfg.RepoRoot, "error": err.Error()})
			}
		}
	}
}

func (r *runtimeState) addBatch(changes []watcher.Change, snapshot map[string]watcher.Entry) {
	r.mu.Lock()
	r.pendingBatch = append(r.pendingBatch, changes...)
	if snapshot != nil {
		r.pendingSnapshot = cloneWatcherSnapshot(snapshot)
	}
	r.lastActivity = time.Now().UTC()
	r.mu.Unlock()
}

func (r *runtimeState) requestSnapshot() {
	r.mu.Lock()
	if r.snapshotRunning {
		r.snapshotDirty = true
		r.lastActivity = time.Now().UTC()
		r.mu.Unlock()
		return
	}
	r.snapshotPending = true
	r.lastActivity = time.Now().UTC()
	r.mu.Unlock()
	select {
	case r.snapshotSignal <- struct{}{}:
	default:
	}
}

func (r *runtimeState) runSnapshot() {
	r.mu.Lock()
	if r.snapshotRunning {
		r.snapshotDirty = true
		r.mu.Unlock()
		return
	}
	r.snapshotPending = false
	r.snapshotRunning = true
	r.snapshotDirty = false
	r.lastSnapshotAt = time.Now().UTC()
	r.lastActivity = time.Now().UTC()
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.snapshotRunning = false
		r.lastActivity = time.Now().UTC()
		r.mu.Unlock()
	}()
	capture := r.snapshotCapture
	if capture == nil {
		capture = r.captureWorkspaceSnapshot
	}
	snapshot, err := capture(r.ctx)
	if err != nil {
		_ = r.recordEvent("workspace.snapshot.failed", "", "", "workspace snapshot failed", map[string]any{"repo_root": r.cfg.RepoRoot, "error": err.Error()})
		return
	}
	if snapshot == nil {
		return
	}
	_ = r.recordEvent("workspace.snapshot.created", "", "", "workspace snapshot created", map[string]any{
		"snapshot_id":         snapshot.ID,
		"parent_id":           snapshot.ParentID,
		"base_commit":         snapshot.BaseCommit,
		"tree_hash":           snapshot.TreeHash,
		"patch_bytes":         snapshot.PatchBytes,
		"changed_files":       len(snapshot.ChangedFiles),
		"omitted_files":       nonNilSnapshotOmissions(snapshot.OmittedFiles),
		"max_file_bytes":      snapshot.MaxFileBytes,
		"observed_change_ids": nonNilStrings(snapshot.ObservedChangeIDs),
	})
}

func (r *runtimeState) hasBatch() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pendingBatch) > 0
}
func (r *runtimeState) flushBatch() {
	r.mu.Lock()
	changes := append([]watcher.Change(nil), r.pendingBatch...)
	snapshot := cloneWatcherSnapshot(r.pendingSnapshot)
	r.pendingBatch = nil
	r.pendingSnapshot = nil
	r.mu.Unlock()
	if len(changes) == 0 {
		r.persistWatchedSnapshot(snapshot)
		return
	}
	changes = r.filterIgnoredChanges(changes)
	if len(changes) == 0 {
		r.persistWatchedSnapshot(snapshot)
		return
	}
	if batchTouchesHookConfig(changes) {
		if err := r.reloadDiscovery(r.ctx); err != nil {
			_ = r.recordEvent("discovery.reload.failed", "", "", "hook discovery reload failed", map[string]any{"repo_root": r.cfg.RepoRoot, "error": err.Error()})
			return
		}
	}
	r.handleLSPChanges(changes)
	observed, err := r.recordObservedChanges(r.ctx, changes)
	if err != nil {
		_ = r.recordEvent("file.change.record.failed", "", "", "file change record failed", map[string]any{"changes": len(changes), "changed_files": store.ChangedFilesFromWatcher(changes), "error": err.Error()})
		return
	}
	r.requestSnapshot()
	changeIDsByKey := observedChangeIDsByKey(observed)
	disc := r.currentDiscovery()
	res, err := matcher.Match(r.cfg.RepoRoot, disc.Hooks, changes, disc.GlobalIgnore, daemonMatcherOptions)
	if err != nil {
		return
	}
	for _, m := range res.Matches {
		if m.Hook.IsLSP() {
			continue
		}
		ids := []string{m.HookID}
		changedFiles := store.ChangedFilesFromWatcher(m.Changes)
		changeIDs := changeIDsForWatcherChanges(m.Changes, changeIDsByKey)
		if err := r.store.EnqueueWithChangeIDs(r.ctx, ids, changedFiles, changeIDs); err != nil {
			_ = r.recordEvent("hook.enqueue.failed", m.HookID, "", "hook enqueue failed", map[string]any{"error": err.Error(), "changes": len(m.Changes), "changed_files": changedFiles, "change_ids": changeIDs})
			return
		}
		_ = r.recordEvent("hook.enqueued", m.HookID, "", "hook enqueued from file changes", map[string]any{"changes": len(m.Changes), "changed_files": changedFiles, "change_ids": changeIDs})
	}
	r.persistWatchedSnapshot(snapshot)
	r.signalDrain()
	r.touch()
}

func (r *runtimeState) persistWatchedSnapshot(snapshot map[string]watcher.Entry) {
	if snapshot == nil {
		return
	}
	if err := r.store.ReplaceWatchedSnapshot(r.ctx, snapshot); err != nil {
		_ = r.recordEvent("watch.snapshot.persist.failed", "", "", "watch snapshot persist failed", map[string]any{"files": len(snapshot), "error": err.Error()})
	}
}

func cloneWatcherSnapshot(in map[string]watcher.Entry) map[string]watcher.Entry {
	if in == nil {
		return nil
	}
	out := make(map[string]watcher.Entry, len(in))
	for path, entry := range in {
		out[path] = entry
	}
	return out
}

func initialWorkingTreeChanges(ctx context.Context, repoRoot string) []watcher.Change {
	if gitOutput(ctx, repoRoot, "rev-parse", "--verify", "HEAD") == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain=v1", "-z", "--untracked-files=all")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return nil
	}
	entries := strings.Split(string(out), "\x00")
	changes := make([]watcher.Change, 0, len(entries))
	for i := 0; i < len(entries); i++ {
		entry := entries[i]
		if entry == "" {
			continue
		}
		if len(entry) < 4 {
			continue
		}
		status := entry[:2]
		path := filepath.ToSlash(strings.TrimSpace(entry[3:]))
		if status[0] == 'R' || status[0] == 'C' {
			oldPath := ""
			if i+1 < len(entries) {
				oldPath = filepath.ToSlash(strings.TrimSpace(entries[i+1]))
				i++
			}
			if status[0] == 'R' && oldPath != "" {
				changes = append(changes, watcher.Change{Path: oldPath, Kind: watcher.Deleted})
			}
			if path != "" {
				changes = append(changes, watcher.Change{Path: path, Kind: watcher.Created})
			}
			continue
		}
		if path == "" {
			continue
		}
		changes = append(changes, watcher.Change{Path: path, Kind: gitStatusChangeKind(status)})
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Path == changes[j].Path {
			return changes[i].Kind < changes[j].Kind
		}
		return changes[i].Path < changes[j].Path
	})
	return changes
}

func gitStatusChangeKind(status string) watcher.ChangeKind {
	if len(status) < 2 {
		return watcher.Modified
	}
	x, y := status[0], status[1]
	if status == "??" || x == 'A' {
		return watcher.Created
	}
	if x == 'D' || y == 'D' {
		return watcher.Deleted
	}
	return watcher.Modified
}

func (r *runtimeState) filterIgnoredChanges(changes []watcher.Change) []watcher.Change {
	kept := make([]watcher.Change, 0, len(changes))
	for _, change := range changes {
		if isNodeModulesPath(change.Path) {
			continue
		}
		kept = append(kept, change)
	}
	if len(kept) == 0 {
		return nil
	}
	paths := make([]string, 0, len(kept))
	for _, change := range kept {
		paths = append(paths, change.Path)
	}
	ignored, err := matcher.GitIgnored(r.cfg.RepoRoot, paths, 0)
	if err != nil {
		_ = r.recordEvent("file.change.ignore.failed", "", "", "git ignore check failed", map[string]any{"repo_root": r.cfg.RepoRoot, "changes": len(kept), "error": err.Error()})
		return kept
	}
	out := kept[:0]
	for _, change := range kept {
		if ignored[filepath.ToSlash(change.Path)] {
			continue
		}
		out = append(out, change)
	}
	return out
}

func isNodeModulesPath(path string) bool {
	path = filepath.ToSlash(strings.TrimSpace(path))
	if path == "" || path == "." {
		return false
	}
	return path == "node_modules" || strings.HasPrefix(path, "node_modules/") || strings.Contains(path, "/node_modules/")
}

func (r *runtimeState) currentDiscovery() *parser.Discovery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.discovery
}

func (r *runtimeState) reloadDiscovery(ctx context.Context) error {
	disc, err := parser.Discover(r.cfg.RepoRoot)
	if err != nil {
		return err
	}
	if err := r.store.RefreshDefinitions(ctx, disc.Hooks); err != nil {
		return err
	}
	r.manager.ReplaceHooks(disc.Hooks)
	r.mu.Lock()
	r.discovery = disc
	r.lastActivity = time.Now().UTC()
	r.mu.Unlock()
	r.syncLSPHooks()
	_ = r.recordEvent("discovery.reloaded", "", "", "hook discovery reloaded", map[string]any{"repo_root": r.cfg.RepoRoot, "hooks": len(disc.Hooks)})
	return nil
}

func batchTouchesHookConfig(changes []watcher.Change) bool {
	for _, change := range changes {
		if isHookConfigPath(change.Path) {
			return true
		}
	}
	return false
}

func isHookConfigPath(path string) bool {
	path = strings.Trim(strings.TrimSpace(filepath.ToSlash(path)), "/")
	return path == ".discobox" || path == parser.HooksDirName || strings.HasPrefix(path, parser.HooksDirName+"/") || path == ".discobot/hooks" || strings.HasPrefix(path, ".discobot/hooks/")
}

func (r *runtimeState) recordObservedChanges(ctx context.Context, changes []watcher.Change) ([]store.ObservedFileChange, error) {
	baseCommit := gitOutput(ctx, r.cfg.RepoRoot, "rev-parse", "HEAD")
	rows := make([]store.ObservedFileChange, 0, len(changes))
	for _, change := range changes {
		path := filepath.ToSlash(strings.TrimSpace(change.Path))
		if path == "" {
			continue
		}
		rows = append(rows, store.ObservedFileChange{
			Path:       path,
			Kind:       change.Kind,
			BaseCommit: baseCommit,
			Diff:       gitDiffForPath(ctx, r.cfg.RepoRoot, baseCommit, path, change.Kind),
		})
	}
	recorded, err := r.store.RecordObservedChanges(ctx, rows)
	if err != nil {
		return nil, err
	}
	for _, change := range recorded {
		_ = r.recordEvent("file.change.observed", "", "", "file change observed", map[string]any{
			"change_id":   change.ID,
			"path":        change.Path,
			"kind":        string(change.Kind),
			"base_commit": change.BaseCommit,
			"diff":        change.Diff,
			"created_at":  change.CreatedAt,
		})
	}
	return recorded, nil
}

func gitOutput(ctx context.Context, repoRoot string, args ...string) string {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitDiffForPath(ctx context.Context, repoRoot, baseCommit, path string, kind watcher.ChangeKind) string {
	args := []string{"diff", "--no-ext-diff"}
	if baseCommit != "" {
		args = append(args, baseCommit)
	}
	args = append(args, "--", path)
	if out, ok := gitDiffOutput(ctx, repoRoot, args...); ok && out != "" {
		return out
	}
	if kind != watcher.Created {
		return ""
	}
	abs := filepath.Join(repoRoot, filepath.FromSlash(path))
	if out, ok := gitDiffOutput(ctx, repoRoot, "diff", "--no-ext-diff", "--no-index", "--", os.DevNull, abs); ok {
		return out
	}
	return ""
}

func gitDiffOutput(ctx context.Context, repoRoot string, args ...string) (string, bool) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return "", false
		}
	}
	return string(out), true
}

func observedChangeIDsByKey(changes []store.ObservedFileChange) map[string][]string {
	out := make(map[string][]string, len(changes))
	for _, change := range changes {
		key := observedChangeKey(change.Path, change.Kind)
		out[key] = append(out[key], change.ID)
	}
	return out
}

func changeIDsForWatcherChanges(changes []watcher.Change, byKey map[string][]string) []string {
	ids := make([]string, 0, len(changes))
	for _, change := range changes {
		key := observedChangeKey(change.Path, change.Kind)
		if keyIDs := byKey[key]; len(keyIDs) > 0 {
			ids = append(ids, keyIDs...)
		}
	}
	return ids
}

func observedChangeKey(path string, kind watcher.ChangeKind) string {
	return filepath.ToSlash(strings.TrimSpace(path)) + "\x00" + string(kind)
}

func (r *runtimeState) hookByID(id string) (hooks.Hook, bool) {
	return r.manager.HookByID(id)
}

func (r *runtimeState) drainOne() bool {
	pending, h, ok := r.nextRunnable()
	if !ok {
		return false
	}
	r.manager.SetHookRunning(h.ID, true)
	res := r.runHook(r.ctx, h, pending.ChangedFiles, pending.ChangeIDs)
	r.manager.SetHookRunning(h.ID, false)
	if res.Success {
		return true
	}
	if len(r.manager.ActiveHookIDs()) > 0 {
		r.manager.ClearActiveHooks()
	}
	return false
}

func (r *runtimeState) nextRunnable() (*store.PendingRow, hooks.Hook, bool) {
	paused, _ := r.globalPaused()
	if paused {
		return nil, hooks.Hook{}, false
	}
	activeHookIDs := r.manager.ActiveHookIDs()
	pending, err := r.store.NextPendingExcluding(r.ctx, activeHookIDs, r.manager.RunningHookIDs())
	if err != nil {
		return nil, hooks.Hook{}, false
	}
	if pending == nil {
		if len(activeHookIDs) > 0 {
			if r.manager.RunningCount() == 0 {
				r.manager.ClearActiveHooks()
			}
		}
		return nil, hooks.Hook{}, false
	}
	h, ok := r.hookByID(pending.HookID)
	if !ok {
		return nil, hooks.Hook{}, false
	}
	statuses, err := r.store.ListStatus(r.ctx)
	if err != nil {
		return nil, hooks.Hook{}, false
	}
	for _, st := range statuses {
		if st.Hook.ID == pending.HookID && st.Paused {
			return nil, hooks.Hook{}, false
		}
	}
	return pending, h, true
}

func (r *runtimeState) runHook(ctx context.Context, h hooks.Hook, files []models.ChangedFile, changeIDs []string) runner.Result {
	r.touch()

	changedPaths := make([]string, 0, len(files))
	for _, f := range files {
		changedPaths = append(changedPaths, f.Path)
	}
	sort.Strings(changedPaths)
	runRow, err := r.store.MarkRunningWithChangeIDs(ctx, h.ID, files, changeIDs)
	if err != nil {
		return runner.Result{Success: false, ExitCode: runner.ExitCodeStartup, Err: err}
	}
	changeIDsDetail := nonNilStrings(runRow.ChangeIDs)
	_ = r.recordEvent("hook.run.started", h.ID, runRow.ID, "hook run started", map[string]any{"changed_files": len(changedPaths), "changed_paths": changedPaths, "change_ids": changeIDsDetail, "invocation_id": runRow.InvocationID})
	logs := &hookLogWriter{runtime: r, hookID: h.ID, runID: runRow.ID}

	var res runner.Result
	if !h.IsScript() {
		res = runner.Result{Success: false, ExitCode: runner.ExitCodeStartup, Err: fmt.Errorf("unsupported hook engine %q", h.Engine)}
	} else {
		res = runner.Run(ctx, runner.Request{Hook: runnerHookDefinition(h), SessionID: r.cfg.SessionID, RepoRoot: r.cfg.RepoRoot, Workspace: r.cfg.RepoRoot, RunID: runRow.ID, ChangedFiles: changedPaths, DBPath: r.cfg.DBPath, SocketPath: r.cfg.SocketPath, OutputWriter: logs})
	}
	logs.Flush()
	status := models.StatusSuccess
	if !res.Success {
		status = models.StatusFailure
	}
	errMsg := ""
	if res.Err != nil {
		errMsg = res.Err.Error()
	}
	_ = r.store.FinishRun(ctx, runRow.ID, models.RunResult{Status: status, ExitCode: res.ExitCode, Error: errMsg, FinishedAt: time.Now().UTC()})
	_ = r.recordEvent("hook.run.finished", h.ID, runRow.ID, "hook run finished", map[string]any{"status": string(status), "exit_code": res.ExitCode, "success": res.Success, "error": errMsg})
	return res
}

func runnerHookDefinition(h hooks.Hook) runner.HookDefinition {
	cmd := h.AbsPath
	if cmd == "" {
		cmd = h.RelPath
	}
	return runner.HookDefinition{ID: h.ID, Name: h.Name, Type: string(h.Type), Engine: string(h.Engine), Path: h.RelPath, Pattern: h.Pattern, Command: cmd}
}

func nonNilStrings(in []string) []string {
	return append([]string{}, in...)
}

func nonNilSnapshotOmissions(in []store.SnapshotOmission) []store.SnapshotOmission {
	return append([]store.SnapshotOmission{}, in...)
}

func (r *runtimeState) handleEventsStream(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}
	limit, err := eventLimit(req, 100)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	hookID := req.URL.Query().Get("hook_id")
	cursorCreatedAt, cursorID, err := r.eventStreamCursor(req.Context(), req.Header.Get("Last-Event-ID"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, ": connected\n\n")
	flusher.Flush()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		events, err := r.store.ListEvents(req.Context(), store.EventQuery{
			HookID:         hookID,
			Limit:          limit,
			AfterCreatedAt: cursorCreatedAt,
			AfterID:        cursorID,
			Ascending:      true,
		})
		if err != nil {
			return
		}
		for _, event := range events {
			apiEvent := model.Event(event)
			if err := writeSSEEvent(w, apiEvent); err != nil {
				return
			}
			cursorCreatedAt = event.CreatedAt
			cursorID = event.ID
		}
		if len(events) > 0 {
			flusher.Flush()
		}
		select {
		case <-req.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *runtimeState) handleWait(w http.ResponseWriter, req *http.Request) {
	timeout, err := waitTimeout(req, 10*time.Minute)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	resp, err := r.wait(req.Context(), timeout)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func waitTimeout(req *http.Request, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(req.URL.Query().Get("timeout_seconds"))
	if raw == "" {
		return fallback, nil
	}
	var seconds int
	if _, err := fmt.Sscanf(raw, "%d", &seconds); err != nil || seconds < 0 {
		return 0, fmt.Errorf("invalid timeout_seconds %q", raw)
	}
	return time.Duration(seconds) * time.Second, nil
}

func (r *runtimeState) wait(ctx context.Context, timeout time.Duration) (model.WaitResponse, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		resp, err := r.waitSnapshot(ctx)
		if err != nil {
			return model.WaitResponse{}, err
		}
		if resp.Settled {
			return resp, nil
		}
		select {
		case <-ctx.Done():
			resp.Settled = false
			return resp, nil
		case <-r.ctx.Done():
			resp.Settled = false
			return resp, nil
		case <-ticker.C:
		}
	}
}

func (r *runtimeState) waitSnapshot(ctx context.Context) (model.WaitResponse, error) {
	r.expireStalePendingLSP(ctx)

	r.mu.Lock()
	running := r.manager.Running()
	pendingChanges := len(r.pendingBatch) > 0
	pendingSnapshot := r.snapshotPending || r.snapshotRunning || r.snapshotDirty
	pendingLSP := len(r.pendingLSP) > 0
	r.mu.Unlock()

	queued, err := r.store.PendingCount(ctx)
	if err != nil {
		return model.WaitResponse{}, err
	}
	paused, err := r.manager.GlobalPaused(ctx)
	if err != nil {
		return model.WaitResponse{}, err
	}
	eligiblePending := false
	if !paused {
		pending, err := r.store.NextPendingExcluding(ctx, r.manager.ActiveHookIDs(), nil)
		if err != nil {
			return model.WaitResponse{}, err
		}
		eligiblePending = pending != nil
	}
	hooksList, err := r.manager.ListHooks(ctx)
	if err != nil {
		return model.WaitResponse{}, err
	}
	return model.WaitResponse{
		Settled:         !running && !pendingChanges && !eligiblePending && !pendingLSP,
		Running:         running,
		Queued:          int(queued),
		PendingChanges:  pendingChanges,
		PendingSnapshot: pendingSnapshot,
		PendingLSP:      pendingLSP,
		Hooks:           hooksList,
		UpdatedAt:       time.Now().UTC(),
	}, nil
}

func (r *runtimeState) eventStreamCursor(ctx context.Context, lastEventID string) (time.Time, string, error) {
	lastEventID = strings.TrimSpace(lastEventID)
	if lastEventID != "" {
		event, err := r.store.GetEvent(ctx, lastEventID)
		if err != nil {
			return time.Time{}, "", err
		}
		if event != nil {
			return event.CreatedAt, event.ID, nil
		}
	}
	events, err := r.store.ListEvents(ctx, store.EventQuery{Limit: 1})
	if err != nil {
		return time.Time{}, "", err
	}
	if len(events) == 0 {
		return time.Time{}, "", nil
	}
	return events[0].CreatedAt, events[0].ID, nil
}

func eventLimit(req *http.Request, fallback int) (int, error) {
	limit := fallback
	if raw := strings.TrimSpace(req.URL.Query().Get("limit")); raw != "" {
		if _, err := fmt.Sscanf(raw, "%d", &limit); err != nil || limit < 0 {
			return 0, fmt.Errorf("invalid limit %q", raw)
		}
	}
	return limit, nil
}

func writeSSEEvent(w io.Writer, event model.Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "id: %s\n", sseField(event.ID)); err != nil {
		return err
	}
	if event.Type != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", sseField(event.Type)); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", payload)
	return err
}

func sseField(value string) string {
	value = strings.ReplaceAll(value, "\r", "")
	value = strings.ReplaceAll(value, "\n", "")
	return value
}

func (r *runtimeState) globalPaused() (bool, error) {
	return r.manager.GlobalPaused(r.ctx)
}
func (r *runtimeState) recordEvent(eventType, hookID string, runID string, message string, details map[string]any) error {
	if r == nil {
		return nil
	}
	if r.manager != nil {
		return r.manager.RecordEvent(r.ctx, eventType, hookID, runID, message, details)
	}
	if r.store == nil {
		return nil
	}
	_, err := r.store.RecordEvent(r.ctx, store.Event{Type: eventType, HookID: hookID, RunID: runID, Message: message, Details: details})
	return err
}

type hookLogWriter struct {
	runtime *runtimeState
	hookID  string
	runID   string

	mu      sync.Mutex
	partial []byte
}

func (w *hookLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range p {
		if b == '\n' {
			w.emitLocked(string(trimTrailingCR(w.partial)))
			w.partial = w.partial[:0]
			continue
		}
		w.partial = append(w.partial, b)
	}
	return len(p), nil
}

func (w *hookLogWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.partial) == 0 {
		return
	}
	w.emitLocked(string(trimTrailingCR(w.partial)))
	w.partial = w.partial[:0]
}

func (w *hookLogWriter) emitLocked(line string) {
	if w == nil || w.runtime == nil || w.runtime.store == nil {
		return
	}
	_, _ = w.runtime.store.AppendHookLogEvent(w.runtime.ctx, models.HookLog{HookID: w.hookID, RunID: w.runID, Line: line})
}

func trimTrailingCR(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\r' {
		return b[:len(b)-1]
	}
	return b
}
func (r *runtimeState) isIdle() bool {
	if atomic.LoadInt64(&r.activeRequests) != 0 {
		return false
	}
	r.expireStalePendingLSP(r.ctx)

	r.mu.Lock()
	running := r.manager.Running()
	batch := len(r.pendingBatch) > 0
	snapshotActive := r.snapshotPending || r.snapshotRunning
	pendingLSP := len(r.pendingLSP) > 0
	since := time.Since(r.lastActivity)
	r.mu.Unlock()
	if running || batch || snapshotActive || pendingLSP || since < r.cfg.IdleTimeout {
		return false
	}
	pending, err := r.store.NextPending(r.ctx)
	return err == nil && pending == nil
}
func (r *runtimeState) touch() { r.mu.Lock(); r.lastActivity = time.Now().UTC(); r.mu.Unlock() }
func (r *runtimeState) signalDrain() {
	select {
	case r.drainSignal <- struct{}{}:
	default:
	}
}

func ignoreHookRuntimePaths(cfg Config) watcher.IgnoreFunc {
	return func(path string, _ watcher.Entry) bool {
		for _, p := range []string{cfg.DBPath, cfg.SocketPath} {
			if p == "" {
				continue
			}
			rel, err := filepath.Rel(cfg.RepoRoot, p)
			if err != nil {
				continue
			}
			rel = filepath.ToSlash(rel)
			path = filepath.ToSlash(path)
			if path == rel || strings.HasPrefix(path, strings.TrimSuffix(rel, "/")+"/") {
				return true
			}
		}
		return false
	}
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return
	}
}
func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
}
