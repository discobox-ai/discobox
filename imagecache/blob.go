package imagecache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PutBlob writes the blob r carries under digest, refusing bytes whose digest
// is not that one. Nothing is written under the name until all of it is in and
// checked, so a blob's name stays a claim about its content: what a staging
// downloads and what a caller hands in arrive the same way. A blob the layout
// already holds is not written again, and r is not read.
//
// It is how a layout that holds blobs no entry names — the server's asset store
// (ADR 26-10-16-423 §13) — is filled from whatever has them.
func (l *Layout) PutBlob(digest string, r io.Reader) error {
	if err := os.MkdirAll(l.blobDir(), 0o700); err != nil {
		return fmt.Errorf("create blob store: %w", err)
	}
	return l.writeBlob(digest, -1, func(w io.Writer) error {
		_, err := io.Copy(w, r)
		return err
	})
}

// OpenBlob reads the blob named by digest, failing the read that would end it
// if its bytes are not that digest's, and reports its size. A blob the layout
// does not hold is an error wrapping ErrNotStaged.
func (l *Layout) OpenBlob(digest string) (io.ReadCloser, int64, error) {
	path, err := l.blobPath(digest)
	if err != nil {
		return nil, 0, err
	}
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, fmt.Errorf("%w: blob %s", ErrNotStaged, digest)
	}
	if err != nil {
		return nil, 0, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, 0, err
	}
	return &verifyingReader{file: file, hash: sha256.New(), want: Descriptor{Digest: digest, Size: info.Size()}}, info.Size(), nil
}

// RemoveBlob deletes the blob named by digest, which is how one found damaged
// on disk is given up so that it is fetched again. A blob the layout does not
// hold is already gone.
func (l *Layout) RemoveBlob(digest string) error {
	path, err := l.blobPath(digest)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// writeBlob writes what fill produces into a temporary beside the blob's
// destination, hashing it as it goes, and renames it into place only when its
// digest — and its size, when size is not negative — match. A blob already
// present is left as it is and fill is not called.
func (l *Layout) writeBlob(digest string, size int64, fill func(io.Writer) error) error {
	path, err := l.blobPath(digest)
	if err != nil {
		return err
	}
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && (size < 0 || info.Size() == size) {
		return nil
	}
	file, err := os.CreateTemp(filepath.Dir(path), tempPrefix+strings.TrimPrefix(digest, "sha256:")[:16]+"-*")
	if err != nil {
		return err
	}
	temp := file.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(temp)
		}
	}()
	hash := sha256.New()
	counter := &countingWriter{}
	fillErr := fill(io.MultiWriter(file, hash, counter))
	// Sync before the rename that publishes it: on ext4 or xfs the rename can
	// reach the journal ahead of the data, and a blob is trusted by its name.
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(fillErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("write %s: %w", digest, err)
	}
	if size >= 0 && counter.written != size {
		return fmt.Errorf("write %s: received %d bytes, not the %d its manifest declares", digest, counter.written, size)
	}
	if got := "sha256:" + hex.EncodeToString(hash.Sum(nil)); got != digest {
		return fmt.Errorf("write %s: received bytes with digest %s", digest, got)
	}
	if err := os.Rename(temp, path); err != nil {
		return fmt.Errorf("write %s: %w", digest, err)
	}
	committed = true
	return nil
}

// PruneBlobs removes every blob keep does not want that was written before
// cutoff, and every temporary left from before it, from a layout whose blobs
// no entry names — what keeps them is the caller's to say. The cutoff is what
// keeps a blob written for a read in flight, which nothing names yet. It
// reports how many blobs it removed.
func (l *Layout) PruneBlobs(keep func(digest string) bool, cutoff time.Time) (int, error) {
	entries, err := os.ReadDir(l.blobDir())
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	var failures []error
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, tempPrefix) && keep("sha256:"+name) {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(l.blobDir(), name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			failures = append(failures, err)
			continue
		}
		if !strings.HasPrefix(name, tempPrefix) {
			removed++
		}
	}
	return removed, errors.Join(failures...)
}
