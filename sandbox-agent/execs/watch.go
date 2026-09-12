package execs

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// defaultFallbackInterval is how often a watcher with no systemd subscription
// sweeps instead. It is deliberately far longer than the two-second poll it
// replaced: a sweep is one batched query rather than a process per exec, and
// nothing is expected to reach it — it exists so a sandbox whose dbus-daemon
// is missing converges slowly rather than not at all (ADR 0115 §3).
const defaultFallbackInterval = time.Minute

// coalesceWindow is how long runtime-file events are gathered before the execs
// they name are reconciled. A shim writes its status file more than once as a
// command ends, and an exec reconciled once for the batch is reconciled with
// the last of those writes rather than each of them.
const coalesceWindow = 50 * time.Millisecond

// Watcher converges exec state from notifications (ADR 0115). It replaced a
// loop that reconciled every exec every two seconds, which cost a systemd
// query per exec per tick and, in the steady state every sandbox spends most
// of its life in, learned nothing.
//
// Two notifications reach it, because neither sees what the other does:
//
//   - The exec's runtime file changing, which is the shim writing its own exit.
//     This is the ordinary end of every exec, and the only accurate source of
//     an exit status — systemd collects a transient unit as it dies.
//   - systemd reporting a change to the exec's unit, which is the only way to
//     learn about an end the shim could not write: killed, out of memory, or a
//     unit that did not survive a reboot.
type Watcher struct {
	manager          *Manager
	logger           *slog.Logger
	fallbackInterval time.Duration
}

type WatcherConfig struct {
	Manager *Manager
	Logger  *slog.Logger
	// FallbackInterval overrides how often a watcher running without a systemd
	// subscription sweeps. Zero takes the default.
	FallbackInterval time.Duration
}

func NewWatcher(cfg WatcherConfig) *Watcher {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	interval := cfg.FallbackInterval
	if interval <= 0 {
		interval = defaultFallbackInterval
	}
	return &Watcher{manager: cfg.Manager, logger: logger, fallbackInterval: interval}
}

// Run reconciles every exec once, then converges on notifications until ctx
// ends.
func (w *Watcher) Run(ctx context.Context) {
	if w.manager == nil {
		return
	}
	// Both notifications are established *before* the first sweep, never after.
	// Anything that happens from here on is queued in a channel this loop will
	// drain; a change between a sweep and a subscription would have no second
	// chance, because there is no periodic sweep to catch it later. The cost is
	// that early events may describe state the sweep also reads, which
	// reconciles to the same answer twice — the harmless direction to err in.
	files, err := w.watchRuntimeDir()
	if err != nil {
		// Without this, an exec that ends normally is noticed only when its unit
		// is collected, and the exit status the shim wrote is never read.
		w.logger.Error("watch the exec runtime directory; falling back to sweeping for exits",
			"error", err, "interval", w.fallbackInterval)
	} else {
		defer files.Close()
	}

	units, err := w.manager.WatchUnits(ctx)
	if err != nil {
		units = nil
		w.logger.Warn("subscribe to systemd unit changes; sweeping on an interval instead",
			"error", err, "interval", w.fallbackInterval)
	}
	var (
		fallback <-chan time.Time
		ticker   *time.Ticker
	)
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
	}()
	degrade := func() {
		if ticker != nil {
			return
		}
		ticker = time.NewTicker(w.fallbackInterval)
		fallback = ticker.C
	}
	// Either notification being absent is a reason to sweep. A runtime
	// watch that could not be established (the host's inotify limit is a real
	// way to reach this) leaves nothing at all watching for ordinary exits.
	if units == nil || files == nil {
		degrade()
	}

	var (
		fileEvents chan fsnotify.Event
		fileErrors chan error
	)
	if files != nil {
		fileEvents, fileErrors = files.Events, files.Errors
	}

	// Now that both are listening, take in whatever happened before they were.
	w.manager.Sweep(ctx)
	pending := map[string]struct{}{}
	var coalescing <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-fileEvents:
			if !ok {
				fileEvents, fileErrors = nil, nil
				continue
			}
			if !event.Has(fsnotify.Create) && !event.Has(fsnotify.Write) {
				continue
			}
			id := execIDFromRuntimeFile(event.Name)
			if id == "" {
				continue
			}
			pending[id] = struct{}{}
			if coalescing == nil {
				coalescing = time.After(coalesceWindow)
			}
		case err, ok := <-fileErrors:
			if !ok {
				fileEvents, fileErrors = nil, nil
				continue
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				// The kernel queue overflowed and dropped events, so which
				// runtime files changed is unknown — the file side of the same
				// problem a dropped signal burst is on the unit side, and
				// answered the same way. It is reachable exactly when a sandbox
				// is busiest: the startup sweep and a D-Bus round trip per
				// coalesced id both run while the queue is filling.
				w.logger.Warn("exec runtime directory events overflowed; sweeping", "error", err)
				w.manager.Sweep(ctx)
				continue
			}
			w.logger.Warn("exec runtime directory watch reported an error", "error", err)
		case <-coalescing:
			coalescing = nil
			for id := range pending {
				delete(pending, id)
				w.manager.ObserveRuntime(ctx, id)
			}
		case unit, ok := <-units:
			if !ok {
				// The subscription ended under us. Sweeping is the honest
				// fallback: something may have changed while it was going away.
				w.logger.Warn("systemd unit subscription ended; sweeping on an interval instead",
					"interval", w.fallbackInterval)
				units = nil
				degrade()
				w.manager.Sweep(ctx)
				continue
			}
			if unit == "" {
				// Changes were dropped rather than delivered, so which units
				// they named is unknown.
				w.manager.Sweep(ctx)
				continue
			}
			w.manager.ObserveUnit(ctx, unit)
		case <-fallback:
			w.manager.Sweep(ctx)
			// Degraded is a state to leave, not to settle into: the dbus-daemon
			// may have come up, or the connection that dropped may be back.
			if resumed, err := w.manager.WatchUnits(ctx); err == nil {
				w.logger.Info("resumed systemd unit change notifications")
				units = resumed
				fallback = nil
				if ticker != nil {
					ticker.Stop()
					ticker = nil
				}
			}
		}
	}
}

func (w *Watcher) watchRuntimeDir() (*fsnotify.Watcher, error) {
	dir := w.manager.runtimeDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := watcher.Add(dir); err != nil {
		watcher.Close()
		return nil, err
	}
	return watcher, nil
}

// execIDFromRuntimeFile names the exec a runtime file belongs to, and nothing
// for any other file in the directory.
func execIDFromRuntimeFile(path string) string {
	name := filepath.Base(path)
	id, ok := strings.CutSuffix(name, ".json")
	if !ok {
		return ""
	}
	return id
}
