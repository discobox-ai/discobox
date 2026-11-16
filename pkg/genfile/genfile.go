// Package genfile writes a code generator's output so that a run which
// changes nothing touches nothing: a file whose bytes are already right keeps
// its inode and mtime, and one that changes is replaced whole by a rename.
//
// It exists because the repository's hooks watch the tree. A generator that
// rewrites its output in place, or deletes it and writes it again, shows the
// watcher a modified or deleted file for a moment even when the end result is
// byte-identical, and every hook over that path runs for nothing.
package genfile

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/discobox-ai/discobox/pkg/fsdurable"
)

// WriteFile is os.WriteFile for a generator's output. When path already holds
// data it is left alone. Otherwise data is staged beside path and renamed
// over it, so a reader never sees part of it; a file that exists keeps its
// mode, and a new one gets perm, as os.WriteFile would give them. The
// directory path is in must exist.
func WriteFile(path string, data []byte, perm fs.FileMode) error {
	info, err := os.Stat(path)
	switch {
	case err == nil:
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		old, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Equal(old, data) {
			return nil
		}
		perm = info.Mode().Perm()
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	return fsdurable.WriteFile(path, data, perm)
}

// SyncDir makes dst hold what a generator wrote into src: each regular file
// in src is written to dst by WriteFile, and a file in dst that owns claims
// as the generator's but src no longer has is removed. Anything else in dst
// (hand-written files, the generator's config) is left alone. dst is created
// when it does not exist.
func SyncDir(src, dst string, owns func(name string) bool) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil { //nolint:gosec // G301: generated source, as public as the rest of the tree.
		return err
	}
	made := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		name := entry.Name()
		data, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if err := WriteFile(filepath.Join(dst, name), data, info.Mode().Perm()); err != nil {
			return err
		}
		made[name] = true
	}
	existing, err := os.ReadDir(dst)
	if err != nil {
		return err
	}
	for _, entry := range existing {
		name := entry.Name()
		if entry.IsDir() || made[name] || !owns(name) {
			continue
		}
		if err := fsdurable.Remove(filepath.Join(dst, name)); err != nil {
			return err
		}
	}
	return nil
}
