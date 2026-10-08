package audit

import (
	"bufio"
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// truncatedSuffix names the marker a truncated spool file is given beside it.
// It holds the file's size before the cut, so a reader can say how much of the
// recording it is reading. A marker rather than a column, because the pass
// works from the trees and has no index from a spool path back to its row, and
// a marker rather than a frame, because a body spool is raw bytes with nowhere
// to put one.
const truncatedSuffix = ".truncated"

// budgetLowWatermark is the share of the budget a pass reclaims down to, so a
// pool sitting at its budget does not run a pass on every write.
const budgetLowWatermark = 0.9

// ErrSpoolReclaimed is a spool file its row names that is no longer on disk:
// the spool budget deleted it, or a crash lost it. It is distinct from a row
// that recorded no file at all.
var ErrSpoolReclaimed = errors.New("spool file reclaimed")

// SpoolBudget bounds the body and stream spool trees together (ADR
// 26-10-08-698 §2). The budget is the lesser of MaxBytes and MaxPercent of the
// filesystem holding the spool; a zero term drops out of the minimum, and with
// both zero there is no budget.
type SpoolBudget struct {
	MaxBytes   int64
	MaxPercent float64
	// HeadBytes is what a truncated file keeps (§3).
	HeadBytes int64
}

// Enabled reports whether either term bounds the spool.
func (b SpoolBudget) Enabled() bool {
	return b.MaxBytes > 0 || b.MaxPercent > 0
}

// limit resolves the budget in bytes against a filesystem of total bytes,
// zero when total is unknown and no absolute term applies.
func (b SpoolBudget) limit(total int64) int64 {
	limit := b.MaxBytes
	if b.MaxPercent > 0 && total > 0 {
		share := int64(float64(total) * b.MaxPercent / 100)
		if limit <= 0 || share < limit {
			limit = share
		}
	}
	return limit
}

// BudgetResult reports what one budget pass found and reclaimed.
type BudgetResult struct {
	// Limit is the budget the pass enforced, in bytes.
	Limit int64
	// Bytes is the spool's size when the pass began, and Remaining when it
	// ended.
	Bytes     int64
	Remaining int64
	Truncated int64
	Deleted   int64
}

// OpenedSpool is a spool file opened for reading.
type OpenedSpool struct {
	*os.File
	// TruncatedFrom is the size the file had before the spool budget cut it to
	// its head, and zero when the file is whole.
	TruncatedFrom int64
}

// ConfigureSpoolBudget sets the budget the body and stream trees are held to.
func (r *Recorder) ConfigureSpoolBudget(budget SpoolBudget) {
	if r == nil {
		return
	}
	r.budget = budget
}

// SpoolBudget returns the budget the recorder was configured with.
func (r *Recorder) SpoolBudget() SpoolBudget {
	if r == nil {
		return SpoolBudget{}
	}
	return r.budget
}

// OverBudget is signaled when spool writes take the running total past the
// budget the last pass resolved. A signal coalesces with any not yet taken.
func (r *Recorder) OverBudget() <-chan struct{} {
	return r.overBudget
}

// addSpoolBytes counts bytes written to a spool file toward the running total
// and wakes the budget pass when they take it over. The total is an estimate
// between passes, which each correct from their own walk.
func (r *Recorder) addSpoolBytes(n int64) {
	if r == nil || n <= 0 {
		return
	}
	total := r.spoolBytes.Add(n)
	if limit := r.spoolLimit.Load(); limit > 0 && total > limit {
		select {
		case r.overBudget <- struct{}{}:
		default:
		}
	}
}

// spoolEntry is one file a budget pass found in a spool tree.
type spoolEntry struct {
	root   *os.Root
	stream bool
	path   string
	size   int64
	info   fs.FileInfo
	marker bool
}

// EnforceBudget holds the body and stream trees to the configured budget
// (ADR 26-10-08-698 §3). Over it, the pass truncates files to their head,
// largest first, before it deletes any whole file, and then deletes oldest
// first, until the trees are back under the low watermark. Rows are never
// deleted here: the age sweep owns them.
//
// Open spools are counted but never touched, for the reason Sweep skips them.
// For deciding what to reclaim, an open spool counts only as its head: its tail
// is cut by the first pass after it closes, so reclaiming other files to make
// room for that tail would have one large upload delete every small file
// before it.
func (r *Recorder) EnforceBudget(ctx context.Context) (BudgetResult, error) {
	var result BudgetResult
	if r == nil || !r.enabled || !r.budget.Enabled() {
		return result, nil
	}
	var errs []error
	total, err := filesystemTotal(r.spoolDirs()...)
	if err != nil && r.budget.MaxBytes <= 0 {
		return result, fmt.Errorf("size the spool filesystem: %w", err)
	}
	result.Limit = r.budget.limit(total)

	var entries []spoolEntry
	// bytes is what is on disk; deferred is the open spools' tails, which no
	// pass can reclaim until they close.
	var bytes, deferred int64
	head := r.budget.HeadBytes
	for _, tree := range []struct {
		dir    string
		stream bool
	}{{r.streamDir, true}, {r.bodyDir, false}} {
		if tree.dir == "" {
			continue
		}
		root, err := os.OpenRoot(tree.dir)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, fmt.Errorf("open spool dir: %w", err))
			}
			continue
		}
		defer func() { _ = root.Close() }()
		walkErr := fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				errs = append(errs, err)
				return nil
			}
			if entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				// A spool closed and reclaimed between listing and stat is
				// simply gone.
				if !errors.Is(err, fs.ErrNotExist) {
					errs = append(errs, err)
				}
				return nil
			}
			if r.spoolIsOpen(path) {
				size := openSpoolSize(root, path, info.Size())
				bytes += size
				deferred += max(0, size-head)
				return nil
			}
			bytes += info.Size()
			entries = append(entries, spoolEntry{
				root:   root,
				stream: tree.stream,
				path:   path,
				size:   info.Size(),
				info:   info,
				marker: strings.HasSuffix(path, truncatedSuffix),
			})
			return nil
		})
		if walkErr != nil {
			errs = append(errs, walkErr)
		}
	}
	result.Bytes = bytes
	r.spoolLimit.Store(result.Limit)
	r.spoolBytes.Store(bytes)
	bytes -= deferred
	if result.Limit <= 0 || bytes <= result.Limit {
		result.Remaining = bytes + deferred
		return result, errors.Join(errs...)
	}
	target := int64(float64(result.Limit) * budgetLowWatermark)

	// Markers ride with their files: they are never candidates of their own,
	// and a deleted file takes its marker with it.
	markers := map[string]int64{}
	files := entries[:0:0]
	for _, entry := range entries {
		if entry.marker {
			markers[markerKey(entry.root, entry.path)] = entry.size
			continue
		}
		files = append(files, entry)
	}

	// Tails first, largest file first; on a tie the older file goes first.
	slices.SortFunc(files, func(a, b spoolEntry) int {
		if c := cmp.Compare(b.size, a.size); c != 0 {
			return c
		}
		return a.info.ModTime().Compare(b.info.ModTime())
	})
	for i := range files {
		if bytes <= target || ctx.Err() != nil {
			break
		}
		file := &files[i]
		if file.size <= head {
			break
		}
		key := markerKey(file.root, file.path)
		_, marked := markers[key]
		// What the cut did is counted even when a later step of it failed,
		// so the total stays what is on disk.
		cut, markerSize, err := r.truncateSpool(*file, head, marked)
		if err != nil {
			errs = append(errs, err)
		}
		if markerSize > 0 {
			bytes += markerSize
			markers[key] = markerSize
		}
		if cut < file.size {
			bytes -= file.size - cut
			file.size = cut
			result.Truncated++
		}
	}

	// Then whole files, oldest first. A file whose cut failed still has its
	// tail, and goes before any file that is only a head: every tail goes
	// before any whole small file does (ADR 26-10-08-698 §3).
	slices.SortFunc(files, func(a, b spoolEntry) int {
		if aTail, bTail := a.size > head, b.size > head; aTail != bTail {
			if aTail {
				return -1
			}
			return 1
		}
		return a.info.ModTime().Compare(b.info.ModTime())
	})
	for _, file := range files {
		if bytes <= target || ctx.Err() != nil {
			break
		}
		if err := file.root.Remove(file.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove spool file: %w", err))
			continue
		}
		bytes -= file.size
		if markerSize, marked := markers[markerKey(file.root, file.path)]; marked {
			if err := file.root.Remove(file.path + truncatedSuffix); err == nil {
				bytes -= markerSize
			}
		}
		result.Deleted++
	}
	result.Remaining = bytes + deferred
	r.spoolBytes.Store(result.Remaining)
	return result, errors.Join(errs...)
}

// markerKey identifies the file a marker belongs to across both trees. A
// marker's own path is its file's plus truncatedSuffix.
func markerKey(root *os.Root, path string) string {
	return root.Name() + "\x00" + strings.TrimSuffix(path, truncatedSuffix)
}

// truncateSpool cuts one spool file to at most head bytes. Unless an earlier
// cut already did, it first writes a marker beside the file holding its size
// before the cut, so no file is ever short without saying so; a cut that then
// fails takes the marker back. It restores both files' modification time: the
// age sweep pairs a file with its row by that time, and a cut must not make an
// old file look new.
//
// It returns the file's size afterwards and the bytes of any marker it left,
// which are what is on disk even when it also returns an error.
func (r *Recorder) truncateSpool(entry spoolEntry, head int64, marked bool) (int64, int64, error) {
	cut := head
	if entry.stream {
		var err error
		if cut, err = streamCut(entry.root, entry.path, head); err != nil {
			return entry.size, 0, fmt.Errorf("find stream spool cut: %w", err)
		}
	}
	if cut >= entry.size {
		return entry.size, 0, nil
	}
	marker := entry.path + truncatedSuffix
	var markerSize int64
	if !marked {
		original := []byte(strconv.FormatInt(entry.size, 10))
		if err := entry.root.WriteFile(marker, original, 0o600); err != nil {
			_ = entry.root.Remove(marker)
			return entry.size, 0, fmt.Errorf("mark spool file before truncating it: %w", err)
		}
		markerSize = int64(len(original))
	}
	file, err := entry.root.OpenFile(entry.path, os.O_WRONLY, 0)
	if err == nil {
		err = file.Truncate(cut)
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		if markerSize > 0 && entry.root.Remove(marker) == nil {
			markerSize = 0
		}
		return entry.size, markerSize, fmt.Errorf("truncate spool file: %w", err)
	}
	modTime := entry.info.ModTime()
	if markerSize > 0 {
		_ = entry.root.Chtimes(marker, modTime, modTime)
	}
	if err := entry.root.Chtimes(entry.path, modTime, modTime); err != nil {
		return cut, markerSize, fmt.Errorf("restore truncated spool file time: %w", err)
	}
	return cut, markerSize, nil
}

// openSpoolSize is the size of a spool still being written, read through a
// handle of its own. A directory listing's size can lag the writes: Windows
// updates it only when the writer's handle closes, so a listing there sees an
// open spool as empty. listed is the fallback if the spool cannot be opened.
func openSpoolSize(root *os.Root, path string, listed int64) int64 {
	file, err := root.Open(path)
	if err != nil {
		return listed
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return listed
	}
	return info.Size()
}

// streamCut is where a stream spool can be cut and stay parseable: the end of
// the last whole frame at or before head, and never inside the stream header.
func streamCut(root *os.Root, path string, head int64) (int64, error) {
	file, err := root.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = file.Close() }()
	reader := bufio.NewReader(file)
	// Magic, version, start time, then two length-prefixed strings.
	offset := int64(len(upgradeStreamFileMagic) + 1 + 8)
	if _, err := reader.Discard(int(offset)); err != nil {
		return 0, err
	}
	for range 2 {
		var n uint16
		if err := binary.Read(reader, binary.BigEndian, &n); err != nil {
			return 0, err
		}
		if _, err := reader.Discard(int(n)); err != nil {
			return 0, err
		}
		offset += 2 + int64(n)
	}
	cut := offset
	for {
		frameType, err := reader.ReadByte()
		if err != nil {
			// The end of the file, or a frame cut short by a crash: either
			// way the last whole frame ends at cut.
			return cut, nil
		}
		var frame int64
		switch frameType {
		case streamFrameTypeData:
			if _, err := reader.Discard(8 + 1); err != nil {
				return cut, nil
			}
			var n uint32
			if err := binary.Read(reader, binary.BigEndian, &n); err != nil {
				return cut, nil
			}
			if _, err := reader.Discard(int(n)); err != nil {
				return cut, nil
			}
			frame = streamDataFrameOverhead + int64(n)
		case streamFrameTypeSummary:
			frame = streamSummaryFrameSize
			if _, err := reader.Discard(int(frame - 1)); err != nil {
				return cut, nil
			}
		default:
			return cut, nil
		}
		if offset+frame > head {
			return cut, nil
		}
		offset += frame
		cut = offset
	}
}

// openSpool opens a spool file a row names, reporting whether the budget cut
// it. A row that names a file no longer there is ErrSpoolReclaimed.
func openSpool(dir, relativePath, label string) (*OpenedSpool, error) {
	file, err := openRelativeSpoolFile(dir, relativePath, label)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s %s: %w", label, relativePath, ErrSpoolReclaimed)
	}
	if err != nil {
		return nil, err
	}
	opened := &OpenedSpool{File: file}
	if marker, err := openRelativeSpoolFile(dir, relativePath+truncatedSuffix, label); err == nil {
		data, _ := io.ReadAll(io.LimitReader(marker, 32))
		_ = marker.Close()
		opened.TruncatedFrom, _ = strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	}
	return opened, nil
}

// spoolDirs names the spool trees, for sizing the filesystem they are on.
func (r *Recorder) spoolDirs() []string {
	var dirs []string
	for _, dir := range []string{r.bodyDir, r.streamDir} {
		if dir != "" {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// filesystemTotal is the total size of the filesystem holding the first of
// dirs that resolves. A spool tree is created on its first write, so a missing
// one is sized by its nearest existing ancestor.
func filesystemTotal(dirs ...string) (int64, error) {
	var errs []error
	for _, dir := range dirs {
		path, err := filepath.Abs(dir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for {
			if _, err := os.Stat(path); err == nil {
				break
			}
			parent := filepath.Dir(path)
			if parent == path {
				break
			}
			path = parent
		}
		total, err := filesystemSize(path)
		if err == nil {
			return total, nil
		}
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return 0, errors.New("no spool directory configured")
	}
	return 0, errors.Join(errs...)
}
