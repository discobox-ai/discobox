package ahplocal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"

	"github.com/discobox-ai/discobox/pkg/ahphost"
)

// watchBatch is how long a watch gathers changes before it reports them, so
// a checkout or a build is a few batches and not one report per file.
const watchBatch = 200 * time.Millisecond

// watchDirLimit bounds how many directories a recursive watch follows; a
// tree past it is watched only as deep as that reaches.
const watchDirLimit = 20000

// untrackedCountLimit is the largest untracked file whose lines are counted;
// a larger one is listed without a count.
const untrackedCountLimit = 1 << 20

// Workspace is this discobox's working tree as only a host in it reaches it
// (ADR 26-10-22-853 §1): inotify watches and git, as the user the host runs
// as. Its files are Boxes' (files.go), done as the files API does them.
type Workspace struct{}

var _ ahphost.Workspace = Workspace{}

// Watch watches root with inotify, and each directory below it as it appears
// when recursive.
func (Workspace) Watch(root string, recursive bool, excludes, includes []string, report func([]ahptypes.ResourceChange)) (func(), error) {
	if _, err := os.Stat(root); err != nil {
		return nil, err
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &watch{root: root, recursive: recursive, excludes: excludes, includes: includes, watcher: watcher}
	if err := w.add(root); err != nil {
		_ = watcher.Close()
		return nil, err
	}
	go w.run(report)
	return func() { _ = watcher.Close() }, nil
}

type watch struct {
	root      string
	recursive bool
	excludes  []string
	includes  []string
	watcher   *fsnotify.Watcher
	dirs      int
}

// add watches dir, and the directories below it when recursive, skipping
// what is excluded.
func (w *watch) add(dir string) error {
	if !w.recursive {
		w.dirs++
		return w.watcher.Add(dir)
	}
	return filepath.WalkDir(dir, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			if p == dir {
				return err
			}
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		if rel := w.rel(p); rel != "" && matches(w.excludes, rel) {
			return filepath.SkipDir
		}
		if w.dirs >= watchDirLimit {
			return filepath.SkipAll
		}
		w.dirs++
		if err := w.watcher.Add(p); err != nil && p == dir {
			return err
		}
		return nil
	})
}

func (w *watch) rel(p string) string {
	rel, err := filepath.Rel(w.root, p)
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

// reported is whether a change to p is one the watch reports.
func (w *watch) reported(p string) bool {
	rel := w.rel(p)
	if rel == "" {
		return true
	}
	if matches(w.excludes, rel) {
		return false
	}
	return len(w.includes) == 0 || matches(w.includes, rel)
}

// matches is whether rel, a slash-separated path below the root, matches a
// pattern: a glob against the whole path or against any trailing part of it,
// so `**/node_modules` and `node_modules` both name every node_modules.
func matches(patterns []string, rel string) bool {
	parts := strings.Split(rel, "/")
	for _, pattern := range patterns {
		pattern = strings.TrimPrefix(strings.TrimSuffix(pattern, "/**"), "**/")
		for i := range parts {
			if ok, _ := path.Match(pattern, strings.Join(parts[i:], "/")); ok {
				return true
			}
		}
	}
	return false
}

// run reports the watch's changes in batches until it is closed.
func (w *watch) run(report func([]ahptypes.ResourceChange)) {
	pending := map[string]ahptypes.ResourceChangeType{}
	var flush <-chan time.Time
	for {
		select {
		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			if !w.reported(event.Name) {
				continue
			}
			kind := ahptypes.ResourceChangeTypeUpdated
			switch {
			case event.Has(fsnotify.Create):
				kind = ahptypes.ResourceChangeTypeAdded
				if w.recursive {
					if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
						_ = w.add(event.Name)
					}
				}
			case event.Has(fsnotify.Remove), event.Has(fsnotify.Rename):
				kind = ahptypes.ResourceChangeTypeDeleted
			case event.Has(fsnotify.Chmod):
				continue
			}
			// A file made and changed within one batch is still one that was
			// added.
			if prev, ok := pending[event.Name]; !ok || prev != ahptypes.ResourceChangeTypeAdded || kind == ahptypes.ResourceChangeTypeDeleted {
				pending[event.Name] = kind
			}
			if flush == nil {
				flush = time.After(watchBatch)
			}
		case _, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
		case <-flush:
			flush = nil
			changes := make([]ahptypes.ResourceChange, 0, len(pending))
			for name, kind := range pending {
				changes = append(changes, ahptypes.ResourceChange{Uri: (&url.URL{Scheme: "file", Path: name}).String(), Type: kind})
			}
			pending = map[string]ahptypes.ResourceChangeType{}
			report(changes)
		}
	}
}

// Changes is the working tree's changes against base, from git: tracked
// changes from git diff, and untracked files as added.
func (Workspace) Changes(ctx context.Context, dir, base string) ([]ahphost.Changed, error) {
	top, err := toplevel(ctx, dir)
	if err != nil {
		return nil, err
	}
	statuses, err := git(ctx, top, "diff", "--no-renames", "-z", "--name-status", base, "--")
	if err != nil {
		return nil, err
	}
	numstat, err := git(ctx, top, "diff", "--no-renames", "-z", "--numstat", base, "--")
	if err != nil {
		return nil, err
	}
	untracked, err := git(ctx, top, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	counts := map[string][2]*int64{}
	for _, record := range splitZ(numstat) {
		fields := strings.SplitN(record, "\t", 3)
		if len(fields) == 3 {
			counts[fields[2]] = [2]*int64{parseCount(fields[0]), parseCount(fields[1])}
		}
	}
	var out []ahphost.Changed
	records := splitZ(statuses)
	for i := 0; i+1 < len(records); i += 2 {
		status, rel := records[i], records[i+1]
		change := ahphost.Changed{Path: filepath.Join(top, rel), Before: !strings.HasPrefix(status, "A"), After: !strings.HasPrefix(status, "D")}
		if c, ok := counts[rel]; ok {
			change.Added, change.Removed = c[0], c[1]
		}
		out = append(out, change)
	}
	for _, rel := range splitZ(untracked) {
		change := ahphost.Changed{Path: filepath.Join(top, rel), After: true}
		if lines, ok := countLines(change.Path); ok {
			change.Added, change.Removed = &lines, new(int64)
		}
		out = append(out, change)
	}
	return out, nil
}

// ReadRevision reads a file as rev has it, from the repository it is in.
func (Workspace) ReadRevision(ctx context.Context, path, rev string) ([]byte, error) {
	if strings.HasPrefix(rev, "-") {
		return nil, fmt.Errorf("revision %q: %w", rev, fs.ErrInvalid)
	}
	dir := filepath.Dir(path)
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) || dir == "/" {
			return nil, err
		}
		dir = filepath.Dir(dir)
	}
	top, err := toplevel(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", err, fs.ErrNotExist)
	}
	rel, err := filepath.Rel(top, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("%s is outside its repository: %w", path, fs.ErrNotExist)
	}
	data, err := git(ctx, top, "show", rev+":"+filepath.ToSlash(rel))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", err, fs.ErrNotExist)
	}
	return data, nil
}

func toplevel(ctx context.Context, dir string) (string, error) {
	out, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// git runs git in dir and returns what it printed. The subcommands are this
// file's; what a client names is a directory and a revision checked not to
// be an option, and git runs as the user the host does.
func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: see above.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func splitZ(out []byte) []string {
	out = bytes.TrimSuffix(out, []byte{0})
	if len(out) == 0 {
		return nil
	}
	return strings.Split(string(out), "\x00")
}

// parseCount is a numstat count; a binary file's "-" is none.
func parseCount(s string) *int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

func countLines(path string) (int64, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > untrackedCountLimit {
		return 0, false
	}
	data, err := os.ReadFile(path)
	if err != nil || bytes.IndexByte(data, 0) >= 0 {
		return 0, false
	}
	n := int64(bytes.Count(data, []byte{'\n'}))
	if len(data) > 0 && data[len(data)-1] != '\n' {
		n++
	}
	return n, true
}
