// Package meta reads and writes the sandbox's meta file, ~/.discobox/meta.json
// under the sandbox user's home: the sandbox's description and tags (ADR 0136).
// The file is their system of record: the agent working in the sandbox edits
// it directly, the status endpoint reports what it holds, and a write through
// the API is applied to it here. The shape of the file and the rules for its
// contents are the root sandboxmeta package's.
package meta

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/sandboxmeta"
)

// ErrInvalidFile is a meta file that exists but is not valid. A write refuses
// it rather than replacing it: whoever wrote it meant something, and
// overwriting it with a change that named none of it would lose that.
var ErrInvalidFile = errors.New("the meta file is not valid")

// ErrInvalidChange is a change that would leave the file invalid, or that sets
// and removes the same tag.
var ErrInvalidChange = errors.New("invalid meta change")

// Owner is who the meta file and any directory made for it belong to: the
// sandbox user, so the agent working in the sandbox can edit what an API write
// left behind. Nil ids leave ownership with this process, which is right when
// the sandbox runs as this process's own identity.
type Owner struct {
	UID *int64
	GID *int64
}

// File is the sandbox's meta file.
type File struct {
	path  string
	owner Owner
	now   func() time.Time
	// mu serializes this process's read-modify-write cycles. An edit made
	// inside the sandbox between one's read and its rename is overwritten by
	// the rename; the file is replaced whole, so it is never left half written.
	mu sync.Mutex
}

// New is the meta file under home.
func New(home string, owner Owner) *File {
	return &File{path: filepath.Join(home, sandboxmeta.RelativePath), owner: owner, now: time.Now}
}

// Path is where the file is.
func (f *File) Path() string { return f.path }

// Read returns what the file holds and when it was read. A missing file is
// empty meta, not an error: a sandbox nobody has described or tagged has no
// file.
func (f *File) Read() (sandboxmeta.Meta, time.Time, error) {
	observedAt := f.now().UTC()
	meta, _, err := f.read()
	return meta, observedAt, err
}

// Update applies change to the file and returns what it holds after, and when
// it was written. A change that would leave the file invalid is refused
// (ErrInvalidChange), and so is a file that was not valid to begin with
// (ErrInvalidFile).
func (f *File) Update(change sandboxmeta.Change) (sandboxmeta.Meta, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	current, _, err := f.read()
	if err != nil {
		return sandboxmeta.Meta{}, time.Time{}, err
	}
	next, err := sandboxmeta.Apply(current, change)
	if err != nil {
		return sandboxmeta.Meta{}, time.Time{}, fmt.Errorf("%w: %w", ErrInvalidChange, err)
	}
	if err := f.write(next); err != nil {
		return sandboxmeta.Meta{}, time.Time{}, err
	}
	return next, f.now().UTC(), nil
}

// Seed writes the description the sandbox was created with, when there is no
// meta file yet and there is one to write. Once the file exists it is the
// description, whatever the sandbox was created with: emptying it is how the
// description is cleared, and deleting it is how it is reseeded.
func (f *File) Seed(description string) error {
	if description == "" {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, exists, err := f.read()
	if exists || err != nil {
		// A file that is there but invalid is somebody's, and not this
		// seed's to replace.
		return nil
	}
	seeded, err := sandboxmeta.Apply(sandboxmeta.Meta{}, sandboxmeta.Change{Description: &description})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidChange, err)
	}
	return f.write(seeded)
}

// read reports what the file holds, and whether there is a file at all.
func (f *File) read() (sandboxmeta.Meta, bool, error) {
	// Stat first, so a FIFO or a device left at the path cannot stall a
	// status report on a read that never returns.
	info, err := os.Stat(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return sandboxmeta.Meta{Tags: map[string]string{}}, false, nil
	}
	if err != nil {
		return sandboxmeta.Meta{}, true, fmt.Errorf("read %s: %w", f.path, err)
	}
	if !info.Mode().IsRegular() {
		return sandboxmeta.Meta{}, true, fmt.Errorf("%w: %s is not a regular file", ErrInvalidFile, f.path)
	}
	file, err := os.Open(f.path)
	if err != nil {
		return sandboxmeta.Meta{}, true, fmt.Errorf("read %s: %w", f.path, err)
	}
	defer file.Close()
	// One byte past the limit, so an oversized file is reported as one rather
	// than silently truncated into something that might parse.
	data, err := io.ReadAll(io.LimitReader(file, sandboxmeta.MaxFileSize+1))
	if err != nil {
		return sandboxmeta.Meta{}, true, fmt.Errorf("read %s: %w", f.path, err)
	}
	meta, err := sandboxmeta.Parse(data)
	if err != nil {
		return sandboxmeta.Meta{}, true, fmt.Errorf("%w: %s: %w", ErrInvalidFile, f.path, err)
	}
	return meta, true, nil
}

// write replaces the file: written beside it and renamed over it, so a reader
// sees the old contents or the new and never a partial file, and a symlink at
// the path is replaced rather than written through.
func (f *File) write(meta sandboxmeta.Meta) error {
	data, err := sandboxmeta.Encode(meta)
	if err != nil {
		return err
	}
	dir := filepath.Dir(f.path)
	created, err := mkdirAllTracked(dir, 0o755)
	if err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	for _, path := range created {
		if err := f.chown(path); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(dir, ".meta-*.json")
	if err != nil {
		return fmt.Errorf("write %s: %w", f.path, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) //nolint:errcheck // Gone after a successful rename; only a failed write leaves it.
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", f.path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", f.path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", f.path, err)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", f.path, err)
	}
	if err := f.chown(tmpPath); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, f.path); err != nil {
		return fmt.Errorf("write %s: %w", f.path, err)
	}
	return nil
}

func (f *File) chown(path string) error {
	if f.owner.UID == nil && f.owner.GID == nil {
		return nil
	}
	uid, gid := -1, -1
	if f.owner.UID != nil {
		uid = int(*f.owner.UID)
	}
	if f.owner.GID != nil {
		gid = int(*f.owner.GID)
	}
	if err := os.Lchown(path, uid, gid); err != nil {
		return fmt.Errorf("give %s to the sandbox user: %w", path, err)
	}
	return nil
}

// mkdirAllTracked is os.MkdirAll that reports the directories it created, so
// only those are given to the sandbox user and a directory that was already
// there keeps its owner.
func mkdirAllTracked(path string, perm os.FileMode) ([]string, error) {
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return nil, fmt.Errorf("%s exists and is not a directory", path)
		}
		return nil, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var created []string
	if parent := filepath.Dir(path); parent != path {
		above, err := mkdirAllTracked(parent, perm)
		if err != nil {
			return nil, err
		}
		created = above
	}
	if err := os.Mkdir(path, perm); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	return append(created, path), nil
}
