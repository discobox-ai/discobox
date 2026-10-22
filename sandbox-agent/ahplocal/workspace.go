package ahplocal

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// Workspace is this discobox's filesystem and git, as the user the host runs
// as reaches them: there is no privilege here to cross, only that user's own
// permissions (ADR 26-10-22-853 §1).
type Workspace struct{}

var _ ahphost.Workspace = Workspace{}

func (Workspace) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (Workspace) WriteFile(path string, write ahphost.FileWrite) error {
	if write.IfMatch != "" {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if etag(info) != write.IfMatch {
			return ahphost.ErrChanged
		}
	}
	flags := os.O_WRONLY | os.O_CREATE
	if write.CreateOnly {
		flags |= os.O_EXCL
	}
	if write.Mode == ahptypes.ResourceWriteModeTruncate {
		f, err := os.OpenFile(path, flags, 0o644) //nolint:gosec // G302/G304: a file the user's editor writes, as their files are.
		if err != nil {
			return err
		}
		if err := f.Truncate(write.Position); err != nil {
			_ = f.Close()
			return err
		}
		if _, err := f.WriteAt(write.Data, write.Position); err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	}
	// append and insert keep every byte there is and splice the data in: at
	// Position bytes back from the end, or from the start.
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err == nil && write.CreateOnly {
		return fmt.Errorf("%s: %w", path, fs.ErrExist)
	}
	at := write.Position
	if write.Mode == ahptypes.ResourceWriteModeAppend {
		at = int64(len(old)) - write.Position
	}
	if at < 0 || at > int64(len(old)) {
		return fmt.Errorf("position %d is outside %s: %w", write.Position, path, fs.ErrInvalid)
	}
	out := append(append(append([]byte{}, old[:at]...), write.Data...), old[at:]...)
	perm := fs.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
	}
	return os.WriteFile(path, out, perm) //nolint:gosec // G306: the file's own mode, or what a new file the user makes gets.
}

func (Workspace) ReadDir(path string) ([]ahphost.DirEntry, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	out := make([]ahphost.DirEntry, 0, len(entries))
	for _, entry := range entries {
		// A symlink lists as what it points at, which is what a folder
		// picker opens.
		info, err := os.Stat(filepath.Join(path, entry.Name()))
		out = append(out, ahphost.DirEntry{Name: entry.Name(), Dir: err == nil && info.IsDir()})
	}
	return out, nil
}

func (Workspace) Stat(path string, follow bool) (ahphost.FileInfo, error) {
	resolved := path
	var (
		info fs.FileInfo
		err  error
	)
	if follow {
		if resolved, err = filepath.EvalSymlinks(path); err != nil {
			return ahphost.FileInfo{}, err
		}
		info, err = os.Stat(resolved)
	} else {
		info, err = os.Lstat(path)
	}
	if err != nil {
		return ahphost.FileInfo{}, err
	}
	out := ahphost.FileInfo{Path: resolved, Type: ahptypes.ResourceTypeFile, Size: info.Size(), ModTime: info.ModTime(), Etag: etag(info)}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		out.Type = ahptypes.ResourceTypeSymlink
	case info.IsDir():
		out.Type = ahptypes.ResourceTypeDirectory
	}
	return out, nil
}

// etag is a file's version as Stat offers it and WriteFile's IfMatch checks
// it: its size and modification time.
func etag(info fs.FileInfo) string {
	return fmt.Sprintf("%x-%x", info.Size(), info.ModTime().UnixNano())
}

func (Workspace) Mkdir(path string) error {
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return fmt.Errorf("%s is not a directory: %w", path, fs.ErrExist)
	}
	return os.MkdirAll(path, 0o755) //nolint:gosec // G301: a folder the user makes, as theirs are.
}

func (Workspace) Remove(path string, recursive bool) error {
	if !recursive {
		return os.Remove(path)
	}
	if _, err := os.Lstat(path); err != nil {
		return err
	}
	return os.RemoveAll(path)
}

// checkTransfer refuses a copy or move that would destroy its own source —
// onto itself, or into itself — and one onto something that exists unless
// it may be replaced. It says whether to exists.
func checkTransfer(from, to string, replace bool) (bool, error) {
	if _, err := os.Lstat(from); err != nil {
		return false, err
	}
	if from == to {
		return false, fmt.Errorf("%s onto itself: %w", from, fs.ErrInvalid)
	}
	if rel, err := filepath.Rel(from, to); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, fmt.Errorf("%s into itself: %w", from, fs.ErrInvalid)
	}
	_, err := os.Lstat(to)
	if err == nil && !replace {
		return true, fmt.Errorf("%s: %w", to, fs.ErrExist)
	}
	return err == nil, nil
}

// Copy copies beside to first and puts the copy in place only once it is
// whole, so a copy that fails leaves to as it was.
func (Workspace) Copy(from, to string, replace bool) error {
	exists, err := checkTransfer(from, to, replace)
	if err != nil {
		return err
	}
	staged, err := sibling(to, "copy")
	if err != nil {
		return err
	}
	if err := copyTree(from, staged); err != nil {
		_ = os.RemoveAll(staged)
		return err
	}
	return replaceWith(to, staged, exists)
}

// Move renames, replacing to only once from is in hand: across filesystems,
// where a rename cannot go, it copies and then removes from.
func (w Workspace) Move(from, to string, replace bool) error {
	exists, err := checkTransfer(from, to, replace)
	if err != nil {
		return err
	}
	if err := replaceWith(to, from, exists); !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := w.Copy(from, to, replace); err != nil {
		return err
	}
	return os.RemoveAll(from)
}

// replaceWith renames src to dst. A dst that exists is set aside first, and
// put back if the rename fails, since a directory cannot be renamed over.
func replaceWith(dst, src string, exists bool) error {
	if !exists {
		return os.Rename(src, dst)
	}
	aside, err := sibling(dst, "old")
	if err != nil {
		return err
	}
	if err := os.Rename(dst, aside); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		_ = os.Rename(aside, dst)
		return err
	}
	return os.RemoveAll(aside)
}

// sibling is a fresh name beside path, on its filesystem.
func sibling(path, what string) (string, error) {
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".ahp-"+what+"-"+hex.EncodeToString(suffix)), nil
}

// copyTree copies a file, symlink or directory, keeping modes.
func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target) //nolint:gosec // G122: the user's own tree, with their permissions; a link they can swap is theirs to follow.
		}
		in, err := os.Open(p) //nolint:gosec // G122: as above, no privilege is crossed.
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm()) //nolint:gosec // G304: a path below the copy's own destination.
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	})
}

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
