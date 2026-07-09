package watcher

import (
	"io/fs"
	"path/filepath"
	"sort"
	"time"
)

// ChangeKind describes the semantic kind of a filesystem change.
type ChangeKind string

const (
	// Created reports a path that exists in the new snapshot but not the old one.
	Created ChangeKind = "created"
	// Modified reports a path whose metadata changed between snapshots.
	Modified ChangeKind = "modified"
	// Deleted reports a path that existed in the old snapshot but not the new one.
	Deleted ChangeKind = "deleted"
)

// Entry is the stable metadata recorded for one repository-relative path.
type Entry struct {
	Path    string
	IsDir   bool
	Size    int64
	Mode    fs.FileMode
	ModTime time.Time
	// Hash is the content digest for regular files. It lets change detection
	// compare bytes instead of mtime, so tools that rewrite a file with
	// identical content but a fresh mtime (go mod tidy, go work sync, and other
	// unconditional writers) do not register as changes. Empty for directories,
	// symlinks, and other non-regular files.
	Hash string
}

// Change is one semantic filesystem change.
type Change struct {
	Path  string
	Kind  ChangeKind
	Entry *Entry
}

// Batch contains changes emitted after a debounce/resync cycle.
type Batch struct {
	Changes []Change
	Resync  bool
	// Snapshot is the full repository snapshot after Changes were detected.
	Snapshot map[string]Entry
}

// IgnoreFunc can skip a path from snapshots. The path is repository-relative and
// slash-normalized. Returning true for a directory prunes the directory tree.
type IgnoreFunc func(path string, entry Entry) bool

// Options configures a Watcher.
type Options struct {
	// Debounce is the quiet period after native filesystem notifications before a
	// snapshot diff is emitted. A small default is used when unset.
	Debounce time.Duration
	// InitialSnapshot seeds the previous snapshot. When nil, New scans Root and
	// uses that as the baseline without emitting initial created events.
	InitialSnapshot map[string]Entry
	// EmitInitial emits a created change for every path in the initial snapshot.
	// It is ignored when InitialSnapshot is provided.
	EmitInitial bool
	// PeriodicResync rescans and diffs even without kernel notifications when > 0.
	PeriodicResync time.Duration
	// Ignore skips additional paths. .git directories are always ignored.
	Ignore IgnoreFunc
	// BatchBuffer controls the size of the Batches channel. Defaults to 16.
	BatchBuffer int
	// ErrorBuffer controls the size of the Errors channel. Defaults to 16.
	ErrorBuffer int
}

const defaultDebounce = 100 * time.Millisecond

func normalizeOptions(opts Options) Options {
	if opts.Debounce <= 0 {
		opts.Debounce = defaultDebounce
	}
	if opts.BatchBuffer <= 0 {
		opts.BatchBuffer = 16
	}
	if opts.ErrorBuffer <= 0 {
		opts.ErrorBuffer = 16
	}
	return opts
}

func cloneSnapshot(in map[string]Entry) map[string]Entry {
	out := make(map[string]Entry, len(in))
	for path, entry := range in {
		entry.Path = filepath.ToSlash(entry.Path)
		out[filepath.ToSlash(path)] = entry
	}
	return out
}

func diffSnapshots(oldSnap, newSnap map[string]Entry) []Change {
	changes := make([]Change, 0)

	oldPaths := make([]string, 0, len(oldSnap))
	for path := range oldSnap {
		oldPaths = append(oldPaths, path)
	}
	sort.Strings(oldPaths)
	for _, path := range oldPaths {
		oldEntry := oldSnap[path]
		newEntry, ok := newSnap[path]
		if !ok {
			changes = append(changes, Change{Path: path, Kind: Deleted})
			continue
		}
		if !sameEntry(oldEntry, newEntry) {
			entry := newEntry
			changes = append(changes, Change{Path: path, Kind: Modified, Entry: &entry})
		}
	}

	newPaths := make([]string, 0, len(newSnap))
	for path := range newSnap {
		if _, ok := oldSnap[path]; !ok {
			newPaths = append(newPaths, path)
		}
	}
	sort.Strings(newPaths)
	for _, path := range newPaths {
		entry := newSnap[path]
		changes = append(changes, Change{Path: path, Kind: Created, Entry: &entry})
	}

	return changes
}

func sameEntry(a, b Entry) bool {
	if a.Path != b.Path || a.IsDir != b.IsDir || a.Mode != b.Mode {
		return false
	}
	if a.Mode.IsRegular() {
		// Compare content, not mtime: many tools rewrite regular files (go.sum,
		// go.work, formatter output) with identical bytes but a new mtime, which
		// must not count as a change or the file-change hooks loop forever.
		return a.Size == b.Size && a.Hash == b.Hash
	}
	// Non-regular files (directories, symlinks) carry no content hash; fall back
	// to size and mtime metadata.
	return a.Size == b.Size && a.ModTime.Equal(b.ModTime)
}
