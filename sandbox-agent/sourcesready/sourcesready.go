// Package sourcesready holds the sandbox's first harness launch until its
// sources are actually in place.
//
// A source the client delivers by push is not there when the container is
// created: pool-agent parks an empty repository for the push to land in, and
// only materializes the checkout on the resume that follows (ADR 0001). The
// container is created and started before any of that — it has to be, because
// the push is proxied through pool-agent and needs the sandbox provisioned —
// so without this the harness would come up in an empty workspace, be handed
// the prompt, and start working on nothing.
//
// The wait is on pool-agent's signal that the sandbox is settled, not on the
// sources themselves: the project layer inside a delivered source can change
// the harness command, and pool-agent rebuilds the container when it does, so
// "the checkout finished" is one step too early to start running.
package sourcesready

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/obot-platform/discobox/sandboxconfig"
)

// backstopInterval re-checks the file in case an fsnotify event is dropped, or
// the config volume sits on a filesystem that does not deliver host-side writes
// as events at all. It is the safety net, not the mechanism: the watch is what
// makes the wait end promptly.
const backstopInterval = time.Second

// Gate returns the wait a first harness launch must clear, or nil when there is
// nothing to wait for.
//
// nil is the answer for every sandbox whose sources were materialized before
// its container existed — every clone-delivered one, and every sandbox created
// before this contract, whose config names no source that awaits delivery. The
// launch path is then exactly what it was, with no file to stat.
func Gate(sources []sandboxconfig.Source, path string, logger *slog.Logger) func(context.Context) error {
	if !sandboxconfig.SourcesAwaitDelivery(sources) {
		return nil
	}
	if path == "" {
		path = sandboxconfig.SourcesReadyPath
	}
	return func(ctx context.Context) error {
		return Wait(ctx, path, logger)
	}
}

// Wait blocks until path exists, ctx is done, or the wait cannot be set up.
//
// The common case costs one stat: by the time anything asks to launch a
// harness, the source has usually long since been delivered — the client
// pushed, pool-agent materialized, and the file was written before this
// process even started. Only a launch that genuinely races delivery waits, and
// it wakes on the file appearing rather than on a poll interval.
func Wait(ctx context.Context, path string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	if exists(path) {
		return nil
	}
	// The watch goes on the directory: the file does not exist yet, so there is
	// nothing to watch, and pool-agent creates it rather than modifying it.
	watcher, err := fsnotify.NewWatcher()
	if err == nil {
		if addErr := watcher.Add(filepath.Dir(path)); addErr != nil {
			_ = watcher.Close()
			watcher = nil
			logger.Warn("watch sandbox config directory for source delivery", "error", addErr)
		}
	} else {
		logger.Warn("watch for source delivery", "error", err)
	}
	if watcher != nil {
		defer watcher.Close()
	}
	// Re-check after the watch is armed. A file created between the first stat
	// and the watch would otherwise produce no event to wake on.
	if exists(path) {
		return nil
	}
	logger.Info("waiting for the sandbox's source to be delivered before launching the harness", "signal", path)
	ticker := time.NewTicker(backstopInterval)
	defer ticker.Stop()
	var events chan fsnotify.Event
	var errs chan error
	if watcher != nil {
		events, errs = watcher.Events, watcher.Errors
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if filepath.Clean(event.Name) != filepath.Clean(path) {
				continue
			}
			if exists(path) {
				return nil
			}
		case watchErr, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			// A failed watch degrades to the backstop rather than failing the
			// launch: the sandbox is waiting on a file that is coming.
			logger.Warn("source delivery watch", "error", watchErr)
		case <-ticker.C:
			if exists(path) {
				return nil
			}
		}
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
