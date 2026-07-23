// Package secretswatch watches the sandbox's resolved-secrets file
// (/run/discobox/secrets/secrets.json) and exposes its current
// envName->sentinel contents. The file is refreshed independently of
// sandbox.json — on grant approval, rotation, or OAuth refresh — so it is
// watched live rather than read once at boot (ADR 0012 §3).
package secretswatch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	// DefaultPath is where pool-agent writes the sandbox's resolved secret
	// sentinels: root-owned, mode 0600, readable only by sandbox-agent.
	DefaultPath = "/run/discobox/secrets/secrets.json"

	pollInterval     = 2 * time.Second
	backstopInterval = 30 * time.Second
)

// Watcher exposes the live contents of the secrets file.
type Watcher struct {
	path string

	mu  sync.RWMutex
	env map[string]string
}

// Watch starts watching path (or DefaultPath if empty) in the background,
// stopping when ctx is done. onError, if set, is called with non-fatal read
// or watch errors; a missing file is not an error (no secrets assigned yet).
func Watch(ctx context.Context, path string, onError func(error)) *Watcher {
	if path == "" {
		path = DefaultPath
	}
	w := &Watcher{path: path}
	w.reload(onError)
	go w.run(ctx, onError)
	return w
}

// Env returns the current envName->sentinel map. The caller must not mutate
// the result.
func (w *Watcher) Env() map[string]string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.env
}

func (w *Watcher) reload(onError func(error)) {
	data, err := os.ReadFile(w.path)
	if err != nil {
		if !os.IsNotExist(err) && onError != nil {
			onError(err)
		}
		w.mu.Lock()
		if os.IsNotExist(err) {
			w.env = nil
		}
		w.mu.Unlock()
		return
	}
	var env map[string]string
	if err := json.Unmarshal(data, &env); err != nil {
		if onError != nil {
			onError(err)
		}
		return
	}
	w.mu.Lock()
	w.env = env
	w.mu.Unlock()
}

func (w *Watcher) run(ctx context.Context, onError func(error)) {
	// The file is written atomically (write temp + rename), so watch the
	// containing directory for the rename rather than the file itself.
	watcher, err := fsnotify.NewWatcher()
	if err == nil {
		if addErr := watcher.Add(filepath.Dir(w.path)); addErr != nil {
			_ = watcher.Close()
			watcher = nil
			if onError != nil {
				onError(addErr)
			}
		}
	} else if onError != nil {
		onError(err)
	}
	if watcher == nil {
		w.poll(ctx, onError)
		return
	}
	defer watcher.Close()

	// Backstop the event stream in case an fsnotify event is dropped.
	ticker := time.NewTicker(backstopInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if filepath.Clean(event.Name) == filepath.Clean(w.path) {
				w.reload(onError)
			}
		case watchErr, ok := <-watcher.Errors:
			if !ok {
				return
			}
			if watchErr != nil && onError != nil {
				onError(watchErr)
			}
		case <-ticker.C:
			w.reload(onError)
		}
	}
}

// poll reloads on a fixed interval. It is the fallback when an fsnotify
// watcher cannot be established.
func (w *Watcher) poll(ctx context.Context, onError func(error)) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.reload(onError)
		}
	}
}
