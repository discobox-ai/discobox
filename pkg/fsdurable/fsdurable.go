// Package fsdurable holds the filesystem calls that make a write durable
// rather than merely visible, for the places that stage a file and rename it
// into position.
//
// It exists because the durability step is the one every such site forgets:
// a rename is ordered against the directory's own entries, and flushing those
// is a separate call from flushing the file. Each copy of that call is a place
// the platform differences below have to be rediscovered, so there is one.
package fsdurable

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// SyncDir flushes a directory's own entries, which is what makes a rename
// durable rather than merely visible to this boot.
//
// Windows cannot open a directory as a file and has no equivalent call, so
// there it is a no-op rather than an error: NTFS orders metadata for itself,
// and failing a write over a call the platform does not have would be worse
// than the guarantee is worth.
func SyncDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	return nil
}

// SyncRootDir flushes the entries of the directory name in root, as SyncDir
// does for a path, without following a link out of root: for a directory
// another party can write. It is a no-op on Windows, as SyncDir is.
func SyncRootDir(root *os.Root, name string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := root.Open(name)
	if err != nil {
		return fmt.Errorf("sync %s: %w", name, err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", name, err)
	}
	return nil
}

// WriteFile writes data to path by a rename of a file of its own, synced,
// at mode 0600: once it returns, path holds data whole after a crash, and a
// reader never sees part of it. The directory path is in must exist.
func WriteFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // gone once renamed; left only by a failure
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(path))
}

// Remove removes path, synced: once it returns, path is gone after a crash
// too. A path already gone is not an error.
func Remove(path string) error {
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return SyncDir(filepath.Dir(path))
}
