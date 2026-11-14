package imagecache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/discobox-ai/discobox/pkg/fsdurable"
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
	path, err := l.blobPath(digest)
	if err != nil {
		return err
	}
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		return nil
	}
	part, err := l.createPartial(digest, -1, nil)
	if err != nil {
		return err
	}
	defer part.discard()
	if _, err := io.Copy(part, r); err != nil {
		return fmt.Errorf("write %s: %w", digest, err)
	}
	return part.commit()
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

// partialBlob is a blob being written: a temporary beside its destination,
// hashed as it fills and renamed into place only when its digest — and its
// size, when one is declared — match. What it holds survives a transfer that
// ends early, so the next transfer carries on from there rather than from the
// first byte (#1104).
type partialBlob struct {
	digest string
	// size is the declared size, or negative when none is.
	size int64
	path string
	file *fsdurable.File
	hash hash.Hash
	// written is how many bytes the temporary holds, and so where a resumed
	// transfer starts.
	written int64
	// held, when set, counts the bytes the temporary holds toward a progress
	// report — never more than the declared size, and taken back off when the
	// temporary is emptied — so a report says what is on disk.
	held *atomic.Int64
	// verified marks a temporary checked and synced: all that is left is
	// commit's close and rename, and an attempt after a failed one retries
	// only that.
	verified bool
}

// createPartial opens an empty temporary for the blob named by digest.
func (l *Layout) createPartial(digest string, size int64, held *atomic.Int64) (*partialBlob, error) {
	path, err := l.blobPath(digest)
	if err != nil {
		return nil, err
	}
	file, err := fsdurable.Create(path, 0o600)
	if err != nil {
		return nil, err
	}
	return &partialBlob{digest: digest, size: size, path: path, file: file, hash: sha256.New(), held: held}, nil
}

// Write appends to the temporary, hashing exactly the bytes that reached it.
func (p *partialBlob) Write(b []byte) (int, error) {
	n, err := p.file.Write(b)
	p.hash.Write(b[:n])
	before := p.written
	p.written += int64(n)
	if p.held != nil {
		p.held.Add(p.countable(p.written) - p.countable(before))
	}
	return n, err
}

// countable is how much of n bytes held counts toward progress: a source that
// sends more than the declared size has not brought the blob any closer.
func (p *partialBlob) countable(n int64) int64 {
	if p.size >= 0 {
		return min(n, p.size)
	}
	return n
}

// restart empties the temporary, for a transfer that starts again from the
// first byte.
func (p *partialBlob) restart() error {
	if p.held != nil {
		p.held.Add(-p.countable(p.written))
	}
	p.written = 0
	p.hash.Reset()
	if err := p.file.Truncate(0); err != nil {
		return err
	}
	_, err := p.file.Seek(0, io.SeekStart)
	return err
}

// commit renames the temporary into place once it holds the blob. One that is
// still short is kept as it is, to be resumed; one holding more than the
// declared size, or bytes of another digest, can never become the blob and is
// emptied. One that was verified and then failed to be renamed is not checked
// again: calling commit again retries the rename.
func (p *partialBlob) commit() error {
	if !p.verified {
		if err := p.verify(); err != nil {
			return err
		}
	}
	// Synced, and its rename, before it is published: a blob is trusted by
	// its name. A Commit that failed at its rename is called again.
	if err := p.file.Commit(); err != nil {
		return fmt.Errorf("write %s: %w", p.digest, err)
	}
	return nil
}

// verify checks that the temporary holds the blob, then syncs it. A sync that
// fails leaves the file open and whole, for a later attempt to fetch again;
// once it has synced, the temporary counts as verified and only commit's
// close and rename are left, which commit retries without writing.
func (p *partialBlob) verify() error {
	if p.size >= 0 && p.written < p.size {
		return fmt.Errorf("write %s: received %d bytes, not the %d its manifest declares", p.digest, p.written, p.size)
	}
	if p.size >= 0 && p.written > p.size {
		err := fmt.Errorf("write %s: received %d bytes, not the %d its manifest declares", p.digest, p.written, p.size)
		return errors.Join(err, p.restart())
	}
	if got := "sha256:" + hex.EncodeToString(p.hash.Sum(nil)); got != p.digest {
		err := fmt.Errorf("write %s: received bytes with digest %s", p.digest, got)
		return errors.Join(err, p.restart())
	}
	// Synced here, not only in commit, so a full disk fails the fetch, which
	// is fetched again, rather than the rename.
	if err := p.file.Sync(); err != nil {
		return fmt.Errorf("write %s: %w", p.digest, err)
	}
	p.verified = true
	return nil
}

// discard removes the temporary, unless it was committed.
func (p *partialBlob) discard() {
	_ = p.file.Discard()
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
		if !isTemp(name) && keep("sha256:"+name) {
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
		if !isTemp(name) {
			removed++
		}
	}
	return removed, errors.Join(failures...)
}
