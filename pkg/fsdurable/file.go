package fsdurable

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// File is a file staged beside the path it is to be: written through the
// embedded *os.File, then put at its path by Commit or CommitNew, synced, or
// thrown away by Discard. Until then the path is untouched, and a reader of
// it never sees part of what is written.
//
// Its staged name is ".<base>.<random>.tmp" beside the path (IsStaged), which
// a hard stop can leave behind: a directory that is listed skips such names.
type File struct {
	*os.File
	staged, target string
	// root, when set, holds staged and target, which are names in it
	// (CreateIn); otherwise they are paths.
	root   *os.Root
	closed bool
	done   bool
	// syncErr is a sync that failed: final, since a second fsync after a
	// failed one can report success over data the kernel already dropped.
	syncErr error
}

// Create stages a file for path, created with perm less the umask, as
// os.OpenFile creates one; a caller that wants exact bits chmods it. The
// directory path is in must exist.
func Create(path string, perm fs.FileMode) (*File, error) {
	return create(nil, path, perm)
}

// CreateIn stages a file for name in root, as Create does for a path, never
// following a link out of root at either name: for a directory another party
// can write.
func CreateIn(root *os.Root, name string, perm fs.FileMode) (*File, error) {
	return create(root, name, perm)
}

// stagedName is a fresh name beside target, as IsStaged knows it; target is
// a path, or a slash-separated name in a root.
func stagedName(target string, root *os.Root) (string, error) {
	dir, base := filepath.Split(target)
	if root != nil {
		dir, base = path.Split(filepath.ToSlash(target))
	}
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return dir + "." + base + "." + hex.EncodeToString(random) + ".tmp", nil
}

func create(root *os.Root, target string, perm fs.FileMode) (*File, error) {
	for range 16 {
		staged, err := stagedName(target, root)
		if err != nil {
			return nil, err
		}
		flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
		var f *os.File
		if root != nil {
			f, err = root.OpenFile(staged, flags, perm)
		} else {
			f, err = os.OpenFile(staged, flags, perm) //nolint:gosec // G304: the caller's own path, staged beside it.
		}
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return &File{File: f, staged: staged, target: target, root: root}, nil
	}
	return nil, &fs.PathError{Op: "create", Path: target, Err: fs.ErrExist}
}

// IsStaged reports whether name, a base name, is one a File stages under.
func IsStaged(name string) bool {
	return strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".tmp")
}

// Commit syncs the file and renames it over its path, then syncs the
// directory: once it returns, the path holds what was written after a crash
// too. A Commit that failed at the rename can be called again; one that
// failed at the sync cannot, since a second fsync after a failed one proves
// nothing: every later Commit returns that failure, and the caller stages
// the write again.
//
// The one exception is a directory the caller may write but not read, which
// cannot be opened to flush: the rename stands and Commit returns nil, since
// the file is at its path, but the rename is not flushed. The agents' own
// records and markers are in directories they read, so it never reaches them.
func (f *File) Commit() error {
	if err := f.flush(); err != nil {
		return err
	}
	var err error
	if f.root != nil {
		err = f.root.Rename(f.staged, f.target)
	} else {
		err = os.Rename(f.staged, f.target)
	}
	if err != nil {
		return err
	}
	f.done = true
	return f.syncDir()
}

// CommitNew is Commit for a path that must not exist yet: the file is linked
// at it, and an error satisfying errors.Is(err, fs.ErrExist) says another
// file holds it. The staged file is gone either way once it returns. A
// directory the caller cannot read is not flushed, as for Commit.
func (f *File) CommitNew() error {
	if err := f.flush(); err != nil {
		return err
	}
	var err error
	if f.root != nil {
		err = f.root.Link(f.staged, f.target)
	} else {
		err = os.Link(f.staged, f.target)
	}
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			_ = f.Discard()
		}
		return err
	}
	f.done = true
	if err := f.remove(f.staged); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return f.syncDir()
}

// Discard closes and removes the staged file, unless it was committed. It is
// safe to defer beside Commit.
func (f *File) Discard() error {
	if f.done {
		return nil
	}
	f.done = true
	var err error
	if !f.closed {
		f.closed = true
		err = f.Close()
	}
	if rmErr := f.remove(f.staged); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		err = errors.Join(err, rmErr)
	}
	return err
}

// flush syncs and closes the staged file, once; a failed sync is final.
func (f *File) flush() error {
	if f.done {
		return fs.ErrClosed
	}
	if f.closed {
		return nil
	}
	if f.syncErr != nil {
		return f.syncErr
	}
	if err := f.Sync(); err != nil {
		f.syncErr = err
		return err
	}
	f.closed = true
	return f.Close()
}

func (f *File) remove(name string) error {
	if f.root != nil {
		return f.root.Remove(name)
	}
	return os.Remove(name)
}

// syncDir flushes the directory the file was committed in, but for one the
// caller cannot read (Commit).
func (f *File) syncDir() error {
	var err error
	if f.root != nil {
		err = SyncIn(f.root, path.Dir(filepath.ToSlash(f.target)))
	} else {
		err = SyncDir(filepath.Dir(f.target))
	}
	if errors.Is(err, fs.ErrPermission) {
		return nil
	}
	return err
}

// Symlink makes path a symbolic link to target, replacing whatever path was
// by a rename of a link staged beside it, and syncs the directory: path is
// the old entry or the new one, after a crash too, and a link at path is
// replaced rather than followed. The directory path is in must exist.
func Symlink(target, path string) error {
	for range 16 {
		staged, err := stagedName(path, nil)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, staged); errors.Is(err, fs.ErrExist) {
			continue
		} else if err != nil {
			return err
		}
		if err := os.Rename(staged, path); err != nil {
			_ = os.Remove(staged)
			return err
		}
		return SyncDir(filepath.Dir(path))
	}
	return &fs.PathError{Op: "symlink", Path: path, Err: fs.ErrExist}
}
