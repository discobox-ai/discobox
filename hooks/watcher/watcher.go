package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher observes a root directory recursively and emits debounced snapshot
// diffs on Batches.
type Watcher struct {
	root string
	opts Options

	fw *fsnotify.Watcher

	batches chan Batch
	errs    chan error

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once

	mu       sync.Mutex
	snapshot map[string]Entry
	watched  map[string]struct{}
}

// New creates a recursive watcher rooted at root. root must name an existing
// directory. .git directories are always skipped.
func New(root string, opts Options) (*Watcher, error) {
	opts = normalizeOptions(opts)

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("watch root %q is not a directory", root)
	}

	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	w := &Watcher{
		root:    absRoot,
		opts:    opts,
		fw:      fw,
		batches: make(chan Batch, opts.BatchBuffer),
		errs:    make(chan error, opts.ErrorBuffer),
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
		watched: make(map[string]struct{}),
	}

	if opts.InitialSnapshot != nil {
		w.snapshot = cloneSnapshot(opts.InitialSnapshot)
	} else {
		w.snapshot, err = w.scan(nil)
		if err != nil {
			_ = fw.Close()
			cancel()
			return nil, err
		}
	}

	if err := w.addRecursive(absRoot); err != nil {
		_ = fw.Close()
		cancel()
		return nil, err
	}

	go w.run(opts.EmitInitial && opts.InitialSnapshot == nil)
	return w, nil
}

// Batches returns the channel of debounced semantic filesystem changes. It is
// closed after Close completes.
func (w *Watcher) Batches() <-chan Batch { return w.batches }

// Errors returns asynchronous watcher errors. It is closed after Close
// completes.
func (w *Watcher) Errors() <-chan error { return w.errs }

// Close releases native watcher resources and closes Batches and Errors.
func (w *Watcher) Close() error {
	var err error
	w.once.Do(func() {
		w.cancel()
		err = w.fw.Close()
		<-w.done
	})
	return err
}

// Snapshot returns the watcher's current full repository snapshot.
func (w *Watcher) Snapshot() map[string]Entry {
	w.mu.Lock()
	defer w.mu.Unlock()
	return cloneSnapshot(w.snapshot)
}

func (w *Watcher) run(emitInitial bool) {
	defer close(w.done)
	defer close(w.batches)
	defer close(w.errs)

	if emitInitial {
		w.mu.Lock()
		snap := cloneSnapshot(w.snapshot)
		w.mu.Unlock()
		var empty map[string]Entry
		w.sendBatch(Batch{Changes: diffSnapshots(empty, snap), Resync: true, Snapshot: snap})
	}

	var debounce *time.Timer
	var debounceC <-chan time.Time
	var resync *time.Ticker
	if w.opts.PeriodicResync > 0 {
		resync = time.NewTicker(w.opts.PeriodicResync)
		defer resync.Stop()
	}

	schedule := func() {
		if debounce == nil {
			debounce = time.NewTimer(w.opts.Debounce)
			debounceC = debounce.C
			return
		}
		if !debounce.Stop() {
			select {
			case <-debounce.C:
			default:
			}
		}
		debounce.Reset(w.opts.Debounce)
		debounceC = debounce.C
	}

	for {
		select {
		case <-w.ctx.Done():
			if debounce != nil {
				debounce.Stop()
			}
			return
		case event, ok := <-w.fw.Events:
			if !ok {
				return
			}
			if w.shouldIgnoreAbs(event.Name) {
				continue
			}
			if event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) {
				w.addIfDirectory(event.Name)
			}
			schedule()
		case err, ok := <-w.fw.Errors:
			if !ok {
				return
			}
			w.sendError(err)
			schedule()
		case <-debounceC:
			debounceC = nil
			w.rescan(true)
		case <-tickerC(resync):
			w.rescan(true)
		}
	}
}

func tickerC(t *time.Ticker) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}

func (w *Watcher) rescan(resync bool) {
	w.mu.Lock()
	prev := w.snapshot
	w.mu.Unlock()
	newSnap, err := w.scan(prev)
	if err != nil {
		w.sendError(err)
		return
	}
	if err := w.addRecursive(w.root); err != nil {
		w.sendError(err)
	}

	w.mu.Lock()
	oldSnap := w.snapshot
	changes := diffSnapshots(oldSnap, newSnap)
	w.snapshot = newSnap
	w.mu.Unlock()

	if len(changes) > 0 {
		w.sendBatch(Batch{Changes: changes, Resync: resync, Snapshot: cloneSnapshot(newSnap)})
	}
}

func (w *Watcher) sendBatch(batch Batch) {
	select {
	case <-w.ctx.Done():
	case w.batches <- batch:
	}
}

func (w *Watcher) sendError(err error) {
	if err == nil {
		return
	}
	select {
	case <-w.ctx.Done():
	case w.errs <- err:
	default:
	}
}

func (w *Watcher) scan(prevSnap map[string]Entry) (map[string]Entry, error) {
	snap := make(map[string]Entry)
	err := filepath.WalkDir(w.root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if path == w.root {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(w.root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		entry := Entry{Path: rel, IsDir: info.IsDir(), Size: info.Size(), Mode: info.Mode(), ModTime: info.ModTime()}
		if shouldSkipGit(rel, entry) || shouldSkipNodeModules(rel) || (w.opts.Ignore != nil && w.opts.Ignore(rel, entry)) {
			if entry.IsDir {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Mode.IsRegular() {
			// Reuse the previous content hash when size and mtime are unchanged so
			// we only read files whose stat actually moved. On a stat change (or an
			// unseeded/first scan) recompute; a read error leaves the hash empty and
			// falls back to metadata comparison.
			if prev, ok := prevSnap[rel]; ok && prev.Hash != "" && prev.Size == entry.Size && prev.ModTime.Equal(entry.ModTime) {
				entry.Hash = prev.Hash
			} else if h, herr := hashFile(path); herr == nil {
				entry.Hash = h
			}
		}
		snap[rel] = entry
		return nil
	})
	return snap, err
}

// hashFile returns a hex content digest for the regular file at path.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (w *Watcher) addRecursive(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if w.shouldIgnoreAbs(path) {
			if path == w.root {
				return nil
			}
			return fs.SkipDir
		}
		return w.addWatch(path)
	})
}

func (w *Watcher) addIfDirectory(path string) {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return
	}
	if err := w.addRecursive(path); err != nil {
		w.sendError(err)
	}
}

func (w *Watcher) addWatch(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	w.mu.Lock()
	if _, ok := w.watched[abs]; ok {
		w.mu.Unlock()
		return nil
	}
	w.mu.Unlock()

	if err := w.fw.Add(abs); err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	w.mu.Lock()
	w.watched[abs] = struct{}{}
	w.mu.Unlock()
	return nil
}

func (w *Watcher) shouldIgnoreAbs(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	if abs == w.root {
		return false
	}
	rel, err := filepath.Rel(w.root, abs)
	if err != nil || rel == "." || rel == "" {
		return false
	}
	rel = filepath.ToSlash(rel)
	entry := Entry{Path: rel, IsDir: true}
	return shouldSkipGit(rel, entry) || shouldSkipNodeModules(rel) || (w.opts.Ignore != nil && w.opts.Ignore(rel, entry))
}

func shouldSkipGit(rel string, entry Entry) bool {
	if rel == "." || rel == "" {
		return false
	}
	if rel == ".git" || strings.HasPrefix(rel, ".git/") {
		return true
	}
	return strings.Contains(rel, "/.git/") || (entry.IsDir && strings.HasSuffix(rel, "/.git"))
}

func shouldSkipNodeModules(rel string) bool {
	if rel == "." || rel == "" {
		return false
	}
	for _, part := range strings.Split(rel, "/") {
		if part == "node_modules" {
			return true
		}
	}
	return false
}
