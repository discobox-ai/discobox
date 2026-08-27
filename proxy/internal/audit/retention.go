package audit

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// SweepResult reports what one retention pass reclaimed.
type SweepResult struct {
	HTTPRows  int64
	SOCKSRows int64
	Files     int64
	Bytes     int64
}

// Empty reports whether the pass reclaimed nothing.
func (r SweepResult) Empty() bool {
	return r.HTTPRows == 0 && r.SOCKSRows == 0 && r.Files == 0
}

// Sweep deletes audit rows written before cutoff and reclaims the spool files
// holding the request/response bodies and upgraded-stream captures they name.
//
// Rows go by their creation timestamp and files by their modification time,
// against the one cutoff. Pairing them that way is exact rather than
// approximate: a spool file is written across the life of the exchange it
// belongs to and that exchange's row is written when it ends, so a file last
// modified before the cutoff can only belong to a row that is also before it.
// What sweeping files on their own terms adds is the set the rows cannot
// reach — spools whose event was dropped because the audit queue was full
// (which is the recorder's designed behavior under burst, not an anomaly), and
// spools left behind by a crash mid-write. Deleting only what a row names would
// leak both forever.
//
// The one file this reasoning does not cover is a spool still being written: an
// upgraded stream can stay open and idle past the cutoff, and its row does not
// exist yet. Those are tracked open by the recorder and skipped here.
//
// The response cache is deliberately not swept. Its entries are keyed by
// content digest and bounded by an LRU byte ceiling, so age says nothing about
// their value — see proxy/internal/cache.
func (r *Recorder) Sweep(ctx context.Context, cutoff time.Time) (SweepResult, error) {
	var result SweepResult
	if r == nil || !r.enabled {
		return result, nil
	}
	var errs []error
	if r.db != nil {
		db := r.db.WithContext(ctx)
		// Rows first: a row that outlives its spool file answers a body request
		// with a missing file, while a spool file that outlives its row is
		// merely unreachable and is reclaimed by the file pass below.
		res := db.Where("created_at < ?", cutoff).Delete(&HTTPExchange{})
		if res.Error != nil {
			errs = append(errs, fmt.Errorf("delete expired http audit rows: %w", res.Error))
		}
		result.HTTPRows = res.RowsAffected
		res = db.Where("created_at < ?", cutoff).Delete(&SOCKSConnect{})
		if res.Error != nil {
			errs = append(errs, fmt.Errorf("delete expired socks audit rows: %w", res.Error))
		}
		result.SOCKSRows = res.RowsAffected
	}
	for _, dir := range []string{r.streamDir, r.bodyDir} {
		files, bytes, err := r.sweepSpoolDir(ctx, dir, cutoff)
		result.Files += files
		result.Bytes += bytes
		if err != nil {
			errs = append(errs, err)
		}
	}
	return result, errors.Join(errs...)
}

// sweepSpoolDir removes every spool file under dir last modified before cutoff,
// then drops the per-client directories left empty behind them. Those
// directories are named by sandbox ID, so without this a pool accumulates one
// empty directory per sandbox it has ever hosted.
//
// The tree is walked through an os.Root. Every path below is then resolved
// against that root rather than against the process's view of the filesystem,
// so a symlink planted in the spool tree cannot make a delete land outside it.
func (r *Recorder) sweepSpoolDir(ctx context.Context, dir string, cutoff time.Time) (int64, int64, error) {
	if dir == "" {
		return 0, 0, nil
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		// A spool directory that was never created is not an error: no stream
		// or body has been recorded on this pool yet.
		if errors.Is(err, fs.ErrNotExist) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("open spool dir: %w", err)
	}
	defer func() { _ = root.Close() }()

	var files, bytes int64
	var dirs []string
	var errs []error
	walkErr := fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		if entry.IsDir() {
			if path != "." {
				dirs = append(dirs, path)
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
			return nil
		}
		if !info.ModTime().Before(cutoff) || r.spoolIsOpen(path) {
			return nil
		}
		if err := root.Remove(path); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, fmt.Errorf("remove spool file: %w", err))
			}
			return nil
		}
		files++
		bytes += info.Size()
		return nil
	})
	if walkErr != nil {
		errs = append(errs, walkErr)
	}
	// Deepest first. Remove refuses a non-empty directory, which is exactly the
	// test wanted, so no separate emptiness check is needed.
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = root.Remove(dirs[i])
	}
	return files, bytes, errors.Join(errs...)
}

// trackSpool marks a spool file as being written and returns the release. The
// retention sweep skips tracked files: an upgraded stream can outlive the
// retention window while still open, and it has no row until it closes.
func (r *Recorder) trackSpool(relativePath string) func() {
	key := spoolKey(relativePath)
	if r == nil || key == "" {
		return func() {}
	}
	r.spoolMu.Lock()
	if r.openSpool == nil {
		r.openSpool = map[string]struct{}{}
	}
	r.openSpool[key] = struct{}{}
	r.spoolMu.Unlock()
	return func() {
		r.spoolMu.Lock()
		delete(r.openSpool, key)
		r.spoolMu.Unlock()
	}
}

func (r *Recorder) spoolIsOpen(relativePath string) bool {
	key := spoolKey(relativePath)
	if r == nil || key == "" {
		return false
	}
	r.spoolMu.Lock()
	defer r.spoolMu.Unlock()
	_, ok := r.openSpool[key]
	return ok
}

// spoolKey normalizes a spool path for comparison. Records carry slash-separated
// paths while a directory walk yields native ones, so both are converted before
// they are matched.
func spoolKey(relativePath string) string {
	if relativePath == "" {
		return ""
	}
	return filepath.Clean(filepath.FromSlash(relativePath))
}
