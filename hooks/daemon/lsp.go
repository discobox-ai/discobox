package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"

	hooks "github.com/obot-platform/discobox/hooks"
	"github.com/obot-platform/discobox/hooks/lspclient"
	"github.com/obot-platform/discobox/hooks/runner"
	"github.com/obot-platform/discobox/hooks/store"
	"github.com/obot-platform/discobox/hooks/watcher"
)

type lspRuntime struct {
	hook    hooks.Hook
	client  *lspclient.Client
	mu      sync.Mutex
	open    map[string]struct{}
	pending []watcher.Change
}

func (r *runtimeState) syncLSPHooks() {
	disc := r.currentDiscovery()
	wanted := map[string]hooks.Hook{}
	for _, hook := range disc.Hooks {
		if hook.IsLSP() {
			wanted[hook.ID] = hook
		}
	}

	r.mu.Lock()
	for id, rt := range r.lspClients {
		if _, ok := wanted[id]; ok {
			continue
		}
		delete(r.lspClients, id)
		r.clearPendingLSPHook(id)
		go func(rt *lspRuntime) {
			if rt.client != nil {
				_ = rt.client.Close()
			}
		}(rt)
	}
	for id, hook := range wanted {
		if existing, ok := r.lspClients[id]; ok {
			existing.hook = hook
			continue
		}
		r.lspClients[id] = &lspRuntime{hook: hook, open: map[string]struct{}{}}
		go r.startLSPHook(hook)
	}
	r.mu.Unlock()
}

func (r *runtimeState) startLSPHook(hook hooks.Hook) {
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
			r.recordLSPDiagnostics(hook, uri, diagnostics)
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
	rt := r.lspClients[hook.ID]
	if rt == nil {
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
	_ = r.recordEvent("lsp.started", hook.ID, "", "language server started", map[string]any{"language_id": hook.LanguageID, "path": hook.RelPath})
	if len(pending) > 0 {
		go r.handleLSPChanges(hook, pending)
	}
}

func (r *runtimeState) handleLSPChanges(hook hooks.Hook, changes []watcher.Change) {
	rt := r.lspRuntimeForHook(hook)
	if rt == nil {
		_ = r.store.SetLSPHookRunning(r.ctx, hook.ID)
		return
	}
	rt.mu.Lock()
	client := rt.client
	if client == nil {
		rt.pending = append(rt.pending, changes...)
		rt.mu.Unlock()
		_ = r.store.SetLSPHookRunning(r.ctx, hook.ID)
		return
	}
	rt.mu.Unlock()
	for _, change := range changes {
		path := filepath.ToSlash(strings.TrimSpace(change.Path))
		if path == "" {
			continue
		}
		uri := lspclient.FileURI(filepath.Join(r.cfg.RepoRoot, filepath.FromSlash(path)))
		ctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
		var err error
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
	path := lspclient.PathFromURI(r.cfg.RepoRoot, uri)
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

func (r *runtimeState) clearPendingLSPHook(hookID string) {
	prefix := hookID + "\x00"
	r.mu.Lock()
	for key := range r.pendingLSP {
		if strings.HasPrefix(key, prefix) {
			delete(r.pendingLSP, key)
		}
	}
	r.lastActivity = time.Now().UTC()
	r.mu.Unlock()
}

func lspPendingKey(hookID, uri string) string {
	return hookID + "\x00" + uri
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
