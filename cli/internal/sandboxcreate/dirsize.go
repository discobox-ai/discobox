package sandboxcreate

import (
	"context"
	"io/fs"
	"path/filepath"
	"sync/atomic"
)

// DirectoryTotal is how much of a directory copying it into a sandbox would
// carry, as counted so far.
type DirectoryTotal struct {
	Bytes int64
	Files int64
	// Done is set once the walk has seen the whole directory, which is what
	// turns a total that is still climbing into a final number.
	Done bool
}

// DirectoryWalk is a directory measurement running in the background. A
// frontend polls Total while the question that started it is on screen, so the
// user watches the number arrive instead of waiting for it before being asked
// anything.
type DirectoryWalk struct {
	bytes atomic.Int64
	files atomic.Int64
	done  atomic.Bool
	stop  context.CancelFunc
}

// MeasureDirectory starts counting what copying dir would carry into a sandbox.
//
// It is an estimate of the directory as it sits on disk: what git ends up
// storing is smaller, because it compresses and because a .gitignore in the
// directory is honored. Overstating is the safe direction for the one question
// this answers — whether the directory is small enough to want copied at all.
//
// Anything it cannot read is skipped rather than failing the walk: a total
// short by an unreadable subtree still answers that question, and an error
// instead of a number does not.
func MeasureDirectory(ctx context.Context, dir string) *DirectoryWalk {
	ctx, cancel := context.WithCancel(ctx)
	walk := &DirectoryWalk{stop: cancel}
	go func() {
		defer cancel()
		_ = filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return filepath.SkipAll
			}
			if err != nil {
				if entry != nil && entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			// Directories cost nothing to carry, and a symlink is carried as
			// the link rather than as what it points at — following one would
			// count a target that is not being copied, and could count it
			// twice.
			if !entry.Type().IsRegular() {
				return nil
			}
			// A file that went away mid-walk, or cannot be stat'd, is left out
			// of the count rather than failing it.
			if info, statErr := entry.Info(); statErr == nil {
				walk.bytes.Add(info.Size())
				walk.files.Add(1)
			}
			return nil
		})
		// A stopped walk never reports a total as final: it stopped short of
		// the directory, and the number it has is not what copying would cost.
		if ctx.Err() == nil {
			walk.done.Store(true)
		}
	}()
	return walk
}

// Total is what the walk has counted so far, and whether that is all of it.
func (w *DirectoryWalk) Total() DirectoryTotal {
	return DirectoryTotal{
		Bytes: w.bytes.Load(),
		Files: w.files.Load(),
		Done:  w.done.Load(),
	}
}

// Stop ends a walk whose question has been answered. It is safe to call more
// than once, and on a walk that has already finished.
func (w *DirectoryWalk) Stop() { w.stop() }
