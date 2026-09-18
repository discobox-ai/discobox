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
	"fmt"
	"os"
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
