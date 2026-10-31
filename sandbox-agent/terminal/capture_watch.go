package terminal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watch directories, not file inodes: editors and credential writers replace
// files with rename. Ancestors cover directories that do not exist yet and
// directories replaced during a login. Events trigger normalization, never a
// decision that the file's bytes are safe to save.
func watchHarnessConfig(ctx context.Context, home string, paths []string, observe func(), debounce, backstop time.Duration) {
	home = filepath.Clean(home)
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	targets := make([]string, 0, len(paths))
	for _, path := range paths {
		targets = append(targets, filepath.Join(home, filepath.FromSlash(path)))
	}
	watcher, err := fsnotify.NewWatcher()
	var events <-chan fsnotify.Event
	var errors <-chan error
	if err == nil {
		defer watcher.Close()
		events, errors = watcher.Events, watcher.Errors
	}
	refresh := func() {
		if watcher == nil {
			return
		}
		for _, target := range targets {
			// Arm parents before children: a directory created after its
			// parent's watch is reported by that parent, and one created before
			// is found by the Stat below. Arming children first loses a
			// directory created between checking it and watching its parent.
			var dirs []string
			for dir := filepath.Dir(target); withinCaptureHome(home, dir); dir = filepath.Dir(dir) {
				dirs = append(dirs, dir)
				if dir == home {
					break
				}
			}
			for i := len(dirs) - 1; i >= 0; i-- {
				dir := dirs[i]
				// Avoid watching symlinks into unrelated trees. This check is
				// best-effort (Add uses a path); exports independently enforce
				// os.Root confinement and never trust an event's contents.
				resolved, resolveErr := filepath.EvalSymlinks(dir)
				if resolveErr != nil || !withinCaptureHome(home, resolved) {
					break
				}
				if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() || watcher.Add(dir) != nil {
					break
				}
			}
		}
	}
	refresh()
	observe()
	ticker := time.NewTicker(backstop)
	defer ticker.Stop()
	timer := time.NewTimer(debounce)
	timer.Stop()
	defer timer.Stop()
	var settled <-chan time.Time
	schedule := func() {
		timer.Reset(debounce)
		settled = timer.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if !captureEventRelevant(event.Name, targets) {
				continue
			}
			if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
				for _, dir := range watcher.WatchList() {
					if withinCaptureHome(filepath.Clean(event.Name), dir) {
						_ = watcher.Remove(dir)
					}
				}
			}
			refresh()
			schedule()
		case _, ok := <-errors:
			if !ok {
				errors = nil
				continue
			}
			// Overflow means events were lost; rescan after the burst settles.
			refresh()
			schedule()
		case <-settled:
			settled = nil
			observe()
		case <-ticker.C:
			// Also repairs missing watches and covers filesystems that do not
			// report writes from outside this sandbox.
			refresh()
			observe()
		}
	}
}

func withinCaptureHome(home, path string) bool {
	rel, err := filepath.Rel(home, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func captureEventRelevant(name string, targets []string) bool {
	name = filepath.Clean(name)
	for _, target := range targets {
		if name == target || strings.HasPrefix(target, name+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
