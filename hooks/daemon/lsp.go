package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	hooks "github.com/obot-platform/discobox/hooks"
	"github.com/obot-platform/discobox/hooks/lspclient"
	"github.com/obot-platform/discobox/hooks/runner"
	"github.com/obot-platform/discobox/hooks/store"
	"github.com/obot-platform/discobox/hooks/watcher"
)

type lspRuntime struct {
	hook     hooks.Hook
	client   *lspclient.Client
	mu       sync.Mutex
	starting bool
	open     map[string]struct{}
	pending  []watcher.Change
}

func (r *runtimeState) syncLSPHooks() {
	disc := r.currentDiscovery()
	wanted := map[string]hooks.Hook{}
	for _, hook := range disc.Hooks {
		if hook.IsLSP() {
			wanted[hook.ID] = hook
		}
	}

	var registered []string
	r.mu.Lock()
	for id, rt := range r.lspClients {
		if _, ok := wanted[id]; ok {
			continue
		}
		delete(r.lspClients, id)
		r.clearPendingLSPHookLocked(id)
		go closeLSPRuntime(rt)
	}
	for id, hook := range wanted {
		if existing, ok := r.lspClients[id]; ok {
			existing.hook = hook
			continue
		}
		// The language server starts lazily on the first change that matches
		// the hook's pattern; see handleLSPHookChanges.
		rt := &lspRuntime{hook: hook, open: map[string]struct{}{}}
		r.lspClients[id] = rt
		registered = append(registered, id)
	}
	r.mu.Unlock()

	// A newly registered hook has no server running yet, so its persisted state
	// belongs to a previous daemon session. If that state was a failure, eagerly
	// start the server to re-verify it and hopefully clear stale errors; a clean
	// hook stays lazy and starts on the first matching change.
	for _, id := range registered {
		failing, err := r.store.LSPHookInFailure(r.ctx, id)
		if err != nil || !failing {
			continue
		}
		r.mu.Lock()
		rt := r.lspClients[id]
		r.mu.Unlock()
		if rt == nil {
			continue
		}
		rt.mu.Lock()
		hook := rt.hook
		rt.mu.Unlock()
		r.activateLSPHook(hook, rt, r.lspSeedChanges(r.ctx, hook))
	}
}

func (r *runtimeState) startLSPHook(hook hooks.Hook, rt *lspRuntime) {
	r.markPendingLSP(hook.ID, lspStartupKey(hook.ID))
	_ = r.store.SetLSPHookRunning(r.ctx, hook.ID)
	_ = r.recordEvent("lsp.starting", hook.ID, "", "language server starting", map[string]any{"language_id": hook.LanguageID, "path": hook.RelPath})
	req := runner.Request{
		Hook: runner.HookDefinition{
			ID:      hook.ID,
			Name:    hook.Name,
			Type:    string(hook.Type),
			Engine:  string(hook.Engine),
			Path:    hook.RelPath,
			Pattern: hook.Pattern,
			Command: hook.AbsPath,
		},
		SessionID:  r.cfg.SessionID,
		RepoRoot:   r.cfg.RepoRoot,
		Workspace:  r.cfg.RepoRoot,
		DBPath:     r.cfg.DBPath,
		SocketPath: r.cfg.SocketPath,
	}
	client, err := lspclient.Start(r.ctx, lspclient.Options{
		Command:    hook.AbsPath,
		RepoRoot:   r.cfg.RepoRoot,
		LanguageID: hook.LanguageID,
		Env:        runner.BuildEnv(req, "[]"),
		OnDiagnostic: func(uri string, diagnostics []lspclient.Diagnostic) {
			if !r.isCurrentLSPRuntime(hook.ID, rt) {
				return
			}
			currentHook := hook
			if h, ok := r.hookByID(hook.ID); ok {
				currentHook = h
			}
			r.recordLSPDiagnostics(currentHook, uri, diagnostics)
		},
	})
	if err != nil {
		msg := err.Error()
		r.clearPendingLSP(hook.ID, lspStartupKey(hook.ID))
		_ = r.store.SetLSPHookError(context.Background(), hook.ID, msg)
		_ = r.recordEvent("lsp.failed", hook.ID, "", "language server failed", map[string]any{"error": msg})
		return
	}

	r.mu.Lock()
	if r.lspClients[hook.ID] != rt {
		r.mu.Unlock()
		r.clearPendingLSP(hook.ID, lspStartupKey(hook.ID))
		_ = client.Close()
		return
	}
	rt.mu.Lock()
	rt.client = client
	var pending []watcher.Change
	pending = append(pending, rt.pending...)
	rt.pending = nil
	rt.mu.Unlock()
	r.mu.Unlock()
	r.clearPendingLSP(hook.ID, lspStartupKey(hook.ID))
	_ = r.store.SetLSPHookReady(r.ctx, hook.ID)
	_ = r.recordEvent("lsp.started", hook.ID, "", "language server started", map[string]any{"language_id": hook.LanguageID, "path": hook.RelPath})
	if len(pending) > 0 {
		go r.handleLSPHookChanges(hook, pending)
	}
}

// requestLSPRun activates an LSP hook in response to an explicit run request.
// An idle server is started (seeded with the working-tree files matching the
// hook pattern so it produces diagnostics immediately); a running server is
// refreshed against those same files. It reports whether a runtime was found.
// The server outlives the request, so all work uses the daemon context.
func (r *runtimeState) requestLSPRun(hookID string, _ bool) (bool, error) {
	r.mu.Lock()
	rt := r.lspClients[hookID]
	r.mu.Unlock()
	if rt == nil {
		return false, nil
	}
	rt.mu.Lock()
	hook := rt.hook
	rt.mu.Unlock()
	r.activateLSPHook(hook, rt, r.lspSeedChanges(r.ctx, hook))
	return true, nil
}

// activateLSPHook starts an idle language server (seeding it with the given
// changes so it publishes diagnostics) or refreshes a running one against those
// changes. Starting clears any diagnostics via SetLSPHookRunning, so a restart-
// triggered re-verify replaces stale rows with the server's fresh evaluation.
func (r *runtimeState) activateLSPHook(hook hooks.Hook, rt *lspRuntime, seed []watcher.Change) {
	rt.mu.Lock()
	if rt.client == nil {
		launch := !rt.starting
		rt.starting = true
		rt.pending = append(rt.pending, seed...)
		rt.mu.Unlock()
		if launch {
			go r.startLSPHook(hook, rt)
		}
		_ = r.store.SetLSPHookRunning(r.ctx, hook.ID)
		return
	}
	rt.mu.Unlock()
	if len(seed) > 0 {
		go r.handleLSPHookChanges(hook, seed)
	}
}

// lspSeedChanges returns the current working-tree changes matching the hook
// pattern, used to prime a language server started by an explicit run request.
func (r *runtimeState) lspSeedChanges(ctx context.Context, hook hooks.Hook) []watcher.Change {
	all := initialWorkingTreeChanges(ctx, r.cfg.RepoRoot)
	seed := make([]watcher.Change, 0, len(all))
	for _, change := range all {
		if matched, err := lspDiagnosticPathMatchesHook(hook, change.Path); err == nil && matched {
			seed = append(seed, change)
		}
	}
	return seed
}

func (r *runtimeState) handleLSPChanges(changes []watcher.Change) {
	if len(changes) == 0 {
		return
	}
	disc := r.currentDiscovery()
	for _, hook := range disc.Hooks {
		if !hook.IsLSP() {
			continue
		}
		r.handleLSPHookChanges(hook, changes)
	}
}

func (r *runtimeState) isCurrentLSPRuntime(hookID string, rt *lspRuntime) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return rt != nil && r.lspClients[hookID] == rt
}

func closeLSPRuntime(rt *lspRuntime) {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	client := rt.client
	rt.client = nil
	rt.mu.Unlock()
	if client != nil {
		_ = client.Close()
	}
}

func (r *runtimeState) handleLSPHookChanges(hook hooks.Hook, changes []watcher.Change) {
	rt := r.lspRuntimeForHook(hook)
	if rt == nil {
		return
	}
	rt.mu.Lock()
	client := rt.client
	if client == nil {
		if !rt.starting {
			// Lazy activation: only start the language server once a change
			// matches the hook's own pattern. Changes to other files begin
			// flowing to the server through its watchers after it is running.
			if !lspChangesMatchHookPattern(hook, changes) {
				rt.mu.Unlock()
				return
			}
			rt.starting = true
			rt.pending = append(rt.pending, changes...)
			rt.mu.Unlock()
			go r.startLSPHook(hook, rt)
			_ = r.store.SetLSPHookRunning(r.ctx, hook.ID)
			return
		}
		rt.pending = append(rt.pending, changes...)
		rt.mu.Unlock()
		_ = r.store.SetLSPHookRunning(r.ctx, hook.ID)
		return
	}
	rt.mu.Unlock()
	watchChanges := lspFileChanges(r.cfg.RepoRoot, client, changes)
	if len(watchChanges) > 0 {
		ctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
		err := client.DidChangeWatchedFiles(ctx, watchChanges)
		cancel()
		if err != nil {
			_ = r.store.SetLSPHookError(r.ctx, hook.ID, err.Error())
			_ = r.recordEvent("lsp.update.failed", hook.ID, "", "language server workspace update failed", map[string]any{"changes": len(watchChanges), "error": err.Error()})
			return
		}
	}
	for _, change := range changes {
		path := filepath.ToSlash(strings.TrimSpace(change.Path))
		if path == "" {
			continue
		}
		matched, err := lspDiagnosticPathMatchesHook(hook, path)
		if err != nil {
			_ = r.store.SetLSPHookError(r.ctx, hook.ID, err.Error())
			_ = r.recordEvent("lsp.update.failed", hook.ID, "", "language server file match failed", map[string]any{"path": path, "kind": string(change.Kind), "error": err.Error()})
			continue
		}
		if !matched {
			continue
		}
		uri := lspclient.FileURI(filepath.Join(r.cfg.RepoRoot, filepath.FromSlash(path)))
		ctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
		err = nil
		if change.Kind == watcher.Deleted {
			rt.mu.Lock()
			_, wasOpen := rt.open[path]
			delete(rt.open, path)
			rt.mu.Unlock()
			if wasOpen {
				err = client.DidClose(ctx, path)
			}
			if storeErr := r.store.ReplaceDiagnosticsForURI(r.ctx, hook.ID, uri, path, nil); storeErr != nil && err == nil {
				err = storeErr
			}
		} else {
			r.markPendingLSP(hook.ID, uri)
			rt.mu.Lock()
			_, wasOpen := rt.open[path]
			rt.open[path] = struct{}{}
			rt.mu.Unlock()
			if wasOpen {
				err = client.DidChange(ctx, path)
			} else {
				err = client.DidOpen(ctx, path)
			}
			if err == nil {
				err = client.DidSave(ctx, path)
			}
		}
		cancel()
		if change.Kind == watcher.Deleted {
			r.clearPendingLSP(hook.ID, uri)
		}
		if err != nil {
			r.clearPendingLSP(hook.ID, uri)
			_ = r.store.SetLSPHookError(r.ctx, hook.ID, err.Error())
			_ = r.recordEvent("lsp.update.failed", hook.ID, "", "language server file update failed", map[string]any{"path": path, "kind": string(change.Kind), "error": err.Error()})
			continue
		}
		_ = r.recordEvent("lsp.file.updated", hook.ID, "", "language server file updated", map[string]any{"path": path, "kind": string(change.Kind)})
	}
	r.touch()
}

func (r *runtimeState) lspRuntimeForHook(hook hooks.Hook) *lspRuntime {
	r.mu.Lock()
	defer r.mu.Unlock()
	rt := r.lspClients[hook.ID]
	if rt != nil {
		rt.hook = hook
	}
	return rt
}

func (r *runtimeState) recordLSPDiagnostics(hook hooks.Hook, uri string, diagnostics []lspclient.Diagnostic) {
	path := lspclient.PathFromURI(r.cfg.RepoRoot, uri)
	matched, err := lspDiagnosticPathMatchesHook(hook, path)
	if err != nil {
		r.clearPendingLSP(hook.ID, uri)
		_ = r.store.SetLSPHookError(r.ctx, hook.ID, err.Error())
		_ = r.recordEvent("lsp.diagnostics.persist.failed", hook.ID, "", "language server diagnostics persist failed", map[string]any{"uri": uri, "path": path, "error": err.Error()})
		return
	}
	if !matched {
		if err := r.store.ReplaceDiagnosticsForURI(r.ctx, hook.ID, uri, path, nil); err != nil {
			r.clearPendingLSP(hook.ID, uri)
			_ = r.store.SetLSPHookError(r.ctx, hook.ID, err.Error())
			_ = r.recordEvent("lsp.diagnostics.persist.failed", hook.ID, "", "language server diagnostics persist failed", map[string]any{"uri": uri, "path": path, "error": err.Error()})
			return
		}
		r.clearPendingLSP(hook.ID, uri)
		return
	}
	filtered := make([]store.Diagnostic, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		if !severityIncluded(diagnostic.Severity, hook.MinSeverity) {
			continue
		}
		filtered = append(filtered, store.Diagnostic{
			HookID:    hook.ID,
			URI:       diagnostic.URI,
			Path:      diagnostic.Path,
			Severity:  diagnostic.Severity,
			Source:    diagnostic.Source,
			Code:      diagnostic.Code,
			Message:   diagnostic.Message,
			StartLine: diagnostic.StartLine,
			StartCol:  diagnostic.StartCol,
			EndLine:   diagnostic.EndLine,
			EndCol:    diagnostic.EndCol,
		})
	}
	if err := r.store.ReplaceDiagnosticsForURI(r.ctx, hook.ID, uri, path, filtered); err != nil {
		r.clearPendingLSP(hook.ID, uri)
		_ = r.store.SetLSPHookError(r.ctx, hook.ID, err.Error())
		_ = r.recordEvent("lsp.diagnostics.persist.failed", hook.ID, "", "language server diagnostics persist failed", map[string]any{"uri": uri, "path": path, "error": err.Error()})
		return
	}
	r.clearPendingLSP(hook.ID, uri)
	_ = r.recordEvent("lsp.diagnostics.updated", hook.ID, "", "language server diagnostics updated", map[string]any{"uri": uri, "path": path, "diagnostics": len(filtered)})
	r.touch()
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

// lspFileChanges builds the workspace/didChangeWatchedFiles payload. When the
// server has registered watchers it declares which files it cares about, so we
// forward only matching changes; otherwise we forward every change.
func lspFileChanges(repoRoot string, client *lspclient.Client, changes []watcher.Change) []lspclient.FileChange {
	filter := client != nil && client.HasWatchers()
	out := make([]lspclient.FileChange, 0, len(changes))
	for _, change := range changes {
		path := filepath.ToSlash(strings.TrimSpace(change.Path))
		if path == "" {
			continue
		}
		if filter && !client.WatchesPath(path) {
			continue
		}
		out = append(out, lspclient.FileChange{
			URI:  lspclient.FileURI(filepath.Join(repoRoot, filepath.FromSlash(path))),
			Type: lspFileChangeType(change.Kind),
		})
	}
	return out
}

// lspChangesMatchHookPattern reports whether any change matches the hook's own
// document pattern, which gates lazy activation of the language server.
func lspChangesMatchHookPattern(hook hooks.Hook, changes []watcher.Change) bool {
	for _, change := range changes {
		path := filepath.ToSlash(strings.TrimSpace(change.Path))
		if path == "" {
			continue
		}
		if matched, err := lspDiagnosticPathMatchesHook(hook, path); err == nil && matched {
			return true
		}
	}
	return false
}

func lspFileChangeType(kind watcher.ChangeKind) lspclient.FileChangeType {
	switch kind {
	case watcher.Created:
		return lspclient.FileCreated
	case watcher.Deleted:
		return lspclient.FileDeleted
	default:
		return lspclient.FileChanged
	}
}

func (r *runtimeState) closeLSPClients() {
	r.mu.Lock()
	clients := make([]*lspclient.Client, 0, len(r.lspClients))
	for _, rt := range r.lspClients {
		rt.mu.Lock()
		if rt.client != nil {
			clients = append(clients, rt.client)
			rt.client = nil
		}
		rt.mu.Unlock()
	}
	r.lspClients = map[string]*lspRuntime{}
	r.pendingLSP = map[string]time.Time{}
	r.mu.Unlock()
	for _, client := range clients {
		_ = client.Close()
	}
}

func (r *runtimeState) markPendingLSP(hookID, uri string) {
	r.mu.Lock()
	if r.pendingLSP == nil {
		r.pendingLSP = map[string]time.Time{}
	}
	r.pendingLSP[lspPendingKey(hookID, uri)] = time.Now().UTC()
	r.lastActivity = time.Now().UTC()
	r.mu.Unlock()
}

func (r *runtimeState) clearPendingLSP(hookID, uri string) {
	r.mu.Lock()
	delete(r.pendingLSP, lspPendingKey(hookID, uri))
	r.lastActivity = time.Now().UTC()
	r.mu.Unlock()
}

func (r *runtimeState) clearPendingLSPHookLocked(hookID string) {
	prefix := hookID + "\x00"
	for key := range r.pendingLSP {
		if strings.HasPrefix(key, prefix) {
			delete(r.pendingLSP, key)
		}
	}
}

func (r *runtimeState) expireStalePendingLSP(ctx context.Context) {
	hookIDs := r.expirePendingLSP(time.Now().UTC())
	for _, hookID := range hookIDs {
		_ = r.store.SetLSPHookReady(ctx, hookID)
	}
}

func (r *runtimeState) expirePendingLSP(now time.Time) []string {
	stale := map[string]struct{}{}
	r.mu.Lock()
	for key, started := range r.pendingLSP {
		hookID, uri, ok := splitLSPPendingKey(key)
		if !ok || strings.HasPrefix(uri, "startup:") {
			continue
		}
		if now.Sub(started) < defaultLSPDiagnosticsGrace {
			continue
		}
		delete(r.pendingLSP, key)
		stale[hookID] = struct{}{}
	}
	if len(stale) > 0 {
		r.lastActivity = now
	}
	r.mu.Unlock()
	if len(stale) == 0 {
		return nil
	}
	hookIDs := make([]string, 0, len(stale))
	for hookID := range stale {
		hookIDs = append(hookIDs, hookID)
	}
	return hookIDs
}

func lspPendingKey(hookID, uri string) string {
	return hookID + "\x00" + uri
}

func splitLSPPendingKey(key string) (string, string, bool) {
	hookID, uri, ok := strings.Cut(key, "\x00")
	return hookID, uri, ok && hookID != "" && uri != ""
}

func lspStartupKey(hookID string) string {
	return "startup:" + hookID
}

func severityIncluded(severity, minSeverity string) bool {
	return severityRank(severity) <= severityRank(minSeverity)
}

func severityRank(severity string) int {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "", "hint":
		return 4
	case "information", "info":
		return 3
	case "warning":
		return 2
	case "error":
		return 1
	default:
		return 1
	}
}
