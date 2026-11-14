package fsdurable

import (
	"io/fs"
	"os"
	"runtime"
)

// SyncTree flushes every regular file and directory under dir, dir too: a
// tree staged beside where it is to go, before the rename that puts it there
// and anything that vouches for it is written. It is read through a root, so
// no link in it is followed out of it.
func SyncTree(dir string) error {
	return syncTree(dir, true)
}

// SyncDirs flushes every directory under dir, dir too, as SyncTree does, but
// no file: for a tree whose files are durable already, such as hard links of
// synced files, or ones each written through a File.
func SyncDirs(dir string) error {
	return syncTree(dir, false)
}

func syncTree(dir string, files bool) error {
	if runtime.GOOS == "windows" {
		return nil // SyncIn flushes nothing there; see SyncDir.
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && (!files || !d.Type().IsRegular()) {
			return nil
		}
		return SyncIn(root, name)
	})
}
