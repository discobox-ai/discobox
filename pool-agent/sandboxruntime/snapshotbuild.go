package sandboxruntime

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/fsdurable"
	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

// What an admin sees of a build while it runs, and of one that failed (ADR
// 26-11-23-004): the progress its snapshot command streams, which the report
// carries (§§1-2); its feature logs, read through the guest the build holds,
// or the tails a failure kept (§§4, 6); and a command run in its box, which
// keeps the build from being committed (§5).

const (
	// snapshotFailureLogsSuffix names, beside a plan's failure record, the
	// directory of what its build's feature logs ended with (§6).
	snapshotFailureLogsSuffix = ".logs"
	// snapshotKeptLogLimit bounds what a failure keeps of each feature's
	// log: its end, where a failing step says why.
	snapshotKeptLogLimit = 64 << 10
	// snapshotKeptLogFiles bounds how many feature logs a failure keeps.
	snapshotKeptLogFiles = 64
	// snapshotKeepLogsTimeout bounds the copy of a failed build's logs out
	// of its box, which its removal waits on.
	snapshotKeepLogsTimeout = 2 * time.Minute
	// MaxSnapshotLogLines is the most lines of each log a read with a count
	// shows.
	MaxSnapshotLogLines = 1_000_000
)

var (
	// ErrSnapshotBuildNotFound is a key that names no build under way and
	// no failed build with kept logs.
	ErrSnapshotBuildNotFound = errors.New("no snapshot build of that key is under way on this pool")
	// ErrSnapshotBuildQueued is a build with no box yet: it waits for
	// another build to end, or its box is still coming up.
	ErrSnapshotBuildQueued = errors.New("the snapshot build is queued and has no box yet")
	// ErrSnapshotBuildProvisioned is a build whose provisioning has ended,
	// while its box is stopped and committed: it takes no more reads or
	// commands.
	ErrSnapshotBuildProvisioned = errors.New("the snapshot build has finished provisioning and is stopping its box; read its logs again once it is listed as failed, if it fails")
	// ErrSnapshotBuildNoFeatures is a build that has reported no feature,
	// so there is no log to read yet.
	ErrSnapshotBuildNoFeatures = errors.New("the snapshot build has reported no features yet")
	// ErrSnapshotFeatureUnknown is a feature the build does not provision.
	ErrSnapshotFeatureUnknown = errors.New("the snapshot build provisions no feature of that name")
)

// SnapshotLogsRequest is what an admin reads of a build's feature logs.
type SnapshotLogsRequest struct {
	// Feature is one feature's log; empty is every feature's.
	Feature string
	// Lines is how many lines of each log, from its end: zero is the whole
	// log, and more than MaxSnapshotLogLines is that.
	Lines int
	// Follow keeps reading as the logs grow, until the build ends or the
	// caller does. A failed build's kept logs do not grow.
	Follow bool
}

// snapshotProgressWriter reads the snapshot command's stdout as it comes,
// line by line, and hands each progress line to report (ADR 26-11-23-004
// §1). A line that is not one, or is longer than
// agentlayer.SnapshotProgressLimit, is dropped.
type snapshotProgressWriter struct {
	report func(SnapshotProgress)
	line   []byte
	// skipping drops the rest of a line that grew past the limit.
	skipping bool
}

func (w *snapshotProgressWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		chunk := p
		if end >= 0 {
			chunk = p[:end]
		}
		if !w.skipping {
			if len(w.line)+len(chunk) >= agentlayer.SnapshotProgressLimit {
				w.line, w.skipping = w.line[:0], true
			} else {
				w.line = append(w.line, chunk...)
			}
		}
		if end < 0 {
			break
		}
		w.flush()
		p = p[end+1:]
	}
	return n, nil
}

// flush ends the line held, as a newline does.
func (w *snapshotProgressWriter) flush() {
	if !w.skipping && len(w.line) > 0 {
		if status, ok := agentlayer.ParseSnapshotProgress(w.line); ok {
			w.report(snapshotProgressOf(status))
		}
	}
	w.line, w.skipping = w.line[:0], false
}

// snapshotProgressOf is what the pool keeps of a build's status: each
// feature's state, step and error, the errors bounded as a failure's is.
func snapshotProgressOf(status provisionspec.Status) SnapshotProgress {
	out := SnapshotProgress{State: string(status.State), Error: boundSnapshotError(status.Error)}
	if len(status.Features) > 0 {
		out.Features = make(map[string]SnapshotFeatureProgress, len(status.Features))
		for name, feature := range status.Features {
			out.Features[name] = SnapshotFeatureProgress{State: string(feature.State), Step: feature.Step, Error: boundSnapshotError(feature.Error)}
		}
	}
	return out
}

// snapshotFailureLogsPath holds what the failed build of the plan keyed key
// kept of its feature logs, one file each.
func (r *sandboxCore) snapshotFailureLogsPath(key string) string {
	return r.snapshotFailurePath(key) + snapshotFailureLogsSuffix
}

// keepSnapshotLogs copies the end of each feature log out of a failed
// build's box, through guest, before its removal takes them (ADR
// 26-11-23-004 §6). It replaces what an earlier failure kept.
func (r *sandboxCore) keepSnapshotLogs(ctx context.Context, key string, guest machine.Guest) error {
	ctx, cancel := context.WithTimeout(ctx, snapshotKeepLogsTimeout)
	defer cancel()
	dir := r.snapshotFailureLogsPath(key)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	stream, err := guest.CopyFrom(ctx, provisionspec.LogDir)
	if err != nil {
		return fmt.Errorf("copy %s out of the build's box: %w", provisionspec.LogDir, err)
	}
	defer stream.Close()
	kept := map[string][]byte{}
	archive := tar.NewReader(stream)
	for len(kept) < snapshotKeptLogFiles {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read the build's logs: %w", err)
		}
		feature, ok := strings.CutSuffix(path.Base(header.Name), ".log")
		if header.Typeflag != tar.TypeReg || !ok || !provisionspec.ValidName(feature) {
			continue
		}
		tail := tailBuffer{limit: snapshotKeptLogLimit}
		if _, err := io.CopyN(&tail, archive, header.Size); err != nil {
			return fmt.Errorf("read the build's %s log: %w", feature, err)
		}
		kept[feature] = tail.data
	}
	if len(kept) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for feature, data := range kept {
		if err := fsdurable.WriteFile(filepath.Join(dir, feature+".log"), data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// SnapshotBuildLogs writes to w the feature logs of the build keyed key,
// read through its box's guest by a fixed tail of fixed paths — never a
// command the caller names (ADR 26-11-23-004 §4). A key with no build under
// way reads what its failure kept, if it did. A read that follows ends when
// the build does.
func (r *sandboxCore) SnapshotBuildLogs(ctx context.Context, key string, req SnapshotLogsRequest, w io.Writer) error {
	if !validSnapshotKey(key) {
		return ErrSnapshotBuildNotFound
	}
	if req.Feature != "" && !provisionspec.ValidName(req.Feature) {
		return ErrSnapshotFeatureUnknown
	}
	lines := min(max(req.Lines, 0), MaxSnapshotLogLines)
	b := &r.snapshots
	b.mu.Lock()
	build := b.active[key]
	var guest machine.Guest
	var features []string
	var ended context.Context
	provisioned := false
	if build != nil {
		guest, ended, provisioned = build.guest, build.ended, build.provisioned
		if build.progress != nil {
			features = slices.Sorted(maps.Keys(build.progress.Features))
		}
	}
	b.mu.Unlock()
	switch {
	case build == nil:
		return r.keptSnapshotLogs(key, req.Feature, lines, w)
	case provisioned:
		return ErrSnapshotBuildProvisioned
	case guest == nil:
		return ErrSnapshotBuildQueued
	case len(features) == 0:
		return ErrSnapshotBuildNoFeatures
	}
	if req.Feature != "" {
		if !slices.Contains(features, req.Feature) {
			return ErrSnapshotFeatureUnknown
		}
		features = []string{req.Feature}
	}
	// The names are the build's word: only one a feature may be named is
	// made a path.
	from := "+1" // the whole log
	if lines > 0 {
		from = strconv.Itoa(lines)
	}
	argv := []string{"tail", "-n", from}
	if req.Follow {
		argv = append(argv, "-F")
	}
	argv = append(argv, "--")
	for _, feature := range features {
		if provisionspec.ValidName(feature) {
			argv = append(argv, path.Join(provisionspec.LogDir, feature+".log"))
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ended, cancel)
	defer stop()
	out := &lockedWriter{w: w}
	// Its exit status is tail's own, which a log not written yet fails. What
	// tail says of one feature's log is on the stream; of every feature's,
	// it would be a line for each feature not reached yet, so it is not.
	var stderr io.Writer = out
	if req.Feature == "" {
		stderr = io.Discard
	}
	if _, err := machine.Run(ctx, guest, machine.ExecRequest{Argv: argv}, nil, out, stderr); err != nil && ctx.Err() == nil {
		return fmt.Errorf("read the build's logs: %w", err)
	}
	return nil
}

// keptSnapshotLogs writes the last lines of each log the failed build keyed
// key kept, or of feature's alone, headed as tail heads them.
func (r *sandboxCore) keptSnapshotLogs(key, feature string, lines int, w io.Writer) error {
	dir := r.snapshotFailureLogsPath(key)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrSnapshotBuildNotFound
	}
	if err != nil {
		return err
	}
	found := false
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".log")
		if !ok || entry.IsDir() || (feature != "" && name != feature) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "==> %s <==\n%s", path.Join(provisionspec.LogDir, entry.Name()), lastLines(data, lines)); err != nil {
			return err
		}
		found = true
	}
	if !found {
		if feature != "" {
			return ErrSnapshotFeatureUnknown
		}
		return ErrSnapshotBuildNotFound
	}
	return nil
}

// lastLines is the last n lines of data, or all of them for n zero, ending
// in a newline.
func lastLines(data []byte, n int) []byte {
	if n <= 0 {
		n = len(data) + 1
	}
	data = bytes.TrimSuffix(data, []byte("\n"))
	start := len(data)
	for i := 0; i < n && start > 0; i++ {
		start = bytes.LastIndexByte(data[:start], '\n')
		if start < 0 {
			start = 0
			break
		}
	}
	out := bytes.TrimPrefix(data[start:], []byte("\n"))
	if len(out) == 0 {
		return nil
	}
	return append(slices.Clip(out), '\n')
}

// SnapshotBuildExec starts req in the box of the build keyed key, through
// the guest it holds, and marks the build opened: its result is then never
// committed (ADR 26-11-23-004 §5). The returned context ends when the build
// does, and the caller closes the process then: the build removes its box.
func (r *sandboxCore) SnapshotBuildExec(ctx context.Context, key string, req machine.ExecRequest) (machine.Process, context.Context, error) {
	if !validSnapshotKey(key) {
		return nil, nil, ErrSnapshotBuildNotFound
	}
	b := &r.snapshots
	b.mu.Lock()
	build := b.active[key]
	if build == nil {
		b.mu.Unlock()
		return nil, nil, ErrSnapshotBuildNotFound
	}
	guest, ended := build.guest, build.ended
	switch {
	case build.provisioned:
		b.mu.Unlock()
		return nil, nil, ErrSnapshotBuildProvisioned
	case guest == nil:
		b.mu.Unlock()
		return nil, nil, ErrSnapshotBuildQueued
	}
	// Marked before the command starts, under the lock the build's end
	// takes: a build that ends after this sees it, and one that ended first
	// has no guest to start it in.
	build.opened = true
	b.mu.Unlock()
	process, err := guest.Exec(ctx, req)
	if err != nil {
		return nil, nil, fmt.Errorf("run a command in the build's box: %w", err)
	}
	return process, ended, nil
}

// validSnapshotKey reports whether key is a plan's key, the hex of a
// SHA-256: anything else names no build, and is never made a path.
func validSnapshotKey(key string) bool {
	if len(key) != 2*sha256.Size {
		return false
	}
	_, err := hex.DecodeString(key)
	return err == nil && strings.ToLower(key) == key
}

// lockedWriter serializes the writes of a process's two streams to one
// writer.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
