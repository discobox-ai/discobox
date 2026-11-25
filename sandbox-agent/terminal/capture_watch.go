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
	if err != nil {
		watcher = nil // Only the backstop runs.
	}
	// changed carries drained notifications to the loop below (nil without a
	// watcher). See forwardCaptureEvents for why they are not drained here.
	var changed <-chan captureChange
	if watcher != nil {
		forwarded := make(chan captureChange, 1)
		forwarderDone := make(chan struct{})
		go forwardCaptureEvents(watcher, targets, forwarded, forwarderDone)
		changed = forwarded
		defer func() {
			// A failed Close (Windows: the reader was never woken) leaves the
			// channels open; waiting for the forwarder then would never return.
			if watcher.Close() == nil {
				<-forwarderDone
			}
		}()
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
		case change := <-changed:
			for _, gone := range change.gone {
				for _, dir := range watcher.WatchList() {
					if withinCaptureHome(gone, dir) {
						_ = watcher.Remove(dir)
					}
				}
			}
			// An error (overflow) means events were lost; the refresh and the
			// observation after the burst settles rescan regardless.
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

// captureChange is what the forwarder accumulated since the loop last took
// it: the relevant paths renamed away, whose descendant watches must be
// dropped. A change with no paths still means "refresh and observe".
type captureChange struct {
	gone []string
}

// forwardCaptureEvents drains the watcher's Events and Errors for as long as
// they are open, and never blocks on the loop that calls Add and Remove.
// fsnotify's Windows backend services Add and Remove on the same thread that
// sends events and errors (Errors is unbuffered), and sends on them while
// servicing a Remove: a caller that drains them on the goroutine that calls
// Add or Remove deadlocks with it. Changes accumulate into the one pending
// value in out (capacity 1) until the loop takes it.
func forwardCaptureEvents(watcher *fsnotify.Watcher, targets []string, out chan captureChange, done chan<- struct{}) {
	defer close(done)
	events, errors := watcher.Events, watcher.Errors
	for events != nil || errors != nil {
		var gone string
		select {
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if !captureEventRelevant(event.Name, targets) {
				continue
			}
			// Only a directory renamed away needs its descendants' watches
			// dropped: they follow the moved tree. A removed directory's
			// watches go with it in every backend, and Remove finds a watch
			// by what is at the path now (by path in inotify and kqueue, by
			// the path's current inode on Windows): dropping by path after
			// the directory was recreated drops the replacement's watches.
			if event.Has(fsnotify.Rename) {
				gone = filepath.Clean(event.Name)
			}
		case _, ok := <-errors:
			if !ok {
				errors = nil
				continue
			}
		}
		var pending captureChange
		select {
		case pending = <-out:
		default:
		}
		if gone != "" {
			pending.gone = append(pending.gone, gone)
		}
		out <- pending
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
