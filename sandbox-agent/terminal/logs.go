package terminal

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const logBucketDuration = 15 * time.Second

type LogStream string

const (
	LogStreamInput  LogStream = "input"
	LogStreamOutput LogStream = "output"
)

type LogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Stream    LogStream `json:"stream"`
	Data      []byte    `json:"data"`
}

type logFileEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Stream    LogStream `json:"stream"`
	Data      string    `json:"data"`
}

type AsyncLogger struct {
	terminalID string
	dir        string
	mu         sync.Mutex
	cond       *sync.Cond
	flushCond  *sync.Cond
	queue      []LogEntry
	closed     bool
	// flushedOutput is the number of output-stream bytes durably written to the
	// log files. Replay attachers wait on it via WaitForFlush so they can read a
	// cutover offset off disk without racing the async writer.
	flushedOutput int64
	wg            sync.WaitGroup
}

func NewAsyncLogger(dir, terminalID string) (*AsyncLogger, error) {
	dir = strings.TrimSpace(dir)
	terminalID = strings.TrimSpace(terminalID)
	if dir == "" || terminalID == "" {
		return nil, nil
	}
	l := &AsyncLogger{
		terminalID: terminalID,
		dir:        filepath.Join(dir, safeName(terminalID)),
	}
	l.cond = sync.NewCond(&l.mu)
	l.flushCond = sync.NewCond(&l.mu)
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return nil, err
	}
	l.wg.Add(1)
	go l.run()
	return l, nil
}

func (l *AsyncLogger) Record(stream LogStream, data []byte) {
	if l == nil || len(data) == 0 {
		return
	}
	entry := LogEntry{
		Timestamp: time.Now().UTC(),
		Stream:    stream,
		Data:      append([]byte(nil), data...),
	}
	l.mu.Lock()
	if !l.closed {
		l.queue = append(l.queue, entry)
		l.cond.Signal()
	}
	l.mu.Unlock()
}

func (l *AsyncLogger) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		l.cond.Signal()
		l.flushCond.Broadcast()
	}
	l.mu.Unlock()
	l.wg.Wait()
}

func (l *AsyncLogger) run() {
	defer l.wg.Done()
	var currentBucket time.Time
	var currentFile *os.File
	defer func() {
		if currentFile != nil {
			_ = currentFile.Close()
		}
	}()
	for {
		entries, done := l.nextBatch()
		if len(entries) == 0 && done {
			return
		}
		for _, entry := range entries {
			bucket := logBucket(entry.Timestamp)
			if currentFile == nil || !bucket.Equal(currentBucket) {
				if currentFile != nil {
					_ = currentFile.Close()
				}
				file, err := os.OpenFile(logBucketPath(l.dir, bucket), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
				if err != nil {
					currentFile = nil
					continue
				}
				currentBucket = bucket
				currentFile = file
			}
			row := logFileEntry{
				Timestamp: entry.Timestamp.UTC(),
				Stream:    entry.Stream,
				Data:      base64.StdEncoding.EncodeToString(entry.Data),
			}
			if data, err := json.Marshal(row); err == nil {
				if _, werr := currentFile.Write(append(data, '\n')); werr == nil && entry.Stream == LogStreamOutput {
					l.mu.Lock()
					l.flushedOutput += int64(len(entry.Data))
					l.flushCond.Broadcast()
					l.mu.Unlock()
				}
			}
		}
	}
}

// WaitForFlush blocks until at least n output-stream bytes have been written to
// the log files, the logger is closed, or ctx is cancelled. It lets a replay
// attacher read the historical output up to a cutover offset without racing the
// async writer that may not yet have persisted the tail of the stream.
func (l *AsyncLogger) WaitForFlush(ctx context.Context, n int64) {
	if l == nil || n <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.flushedOutput >= n {
		return
	}
	stop := context.AfterFunc(ctx, func() {
		l.mu.Lock()
		l.flushCond.Broadcast()
		l.mu.Unlock()
	})
	defer stop()
	for l.flushedOutput < n && !l.closed && ctx.Err() == nil {
		l.flushCond.Wait()
	}
}

func (l *AsyncLogger) nextBatch() ([]LogEntry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for len(l.queue) == 0 && !l.closed {
		l.cond.Wait()
	}
	entries := l.queue
	l.queue = nil
	return entries, l.closed
}

func ReadLogs(ctx context.Context, logRoot, terminalID string) ([]LogEntry, error) {
	if strings.TrimSpace(logRoot) == "" || strings.TrimSpace(terminalID) == "" {
		return nil, nil
	}
	matches, err := filepath.Glob(filepath.Join(logRoot, safeName(terminalID), "*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	var out []LogEntry
	for _, path := range matches {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, err := readLogFile(path)
		if err != nil {
			return nil, err
		}
		out = append(out, entries...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Timestamp.Before(out[j].Timestamp)
	})
	return out, nil
}

// StreamOutput replays saved output-stream payloads in the order they were
// produced, invoking fn for each chunk, until limit output bytes have been
// emitted. A limit <= 0 streams the entire recorded output. The chunk that
// straddles the limit boundary is truncated so exactly limit bytes are emitted.
// Input entries are skipped: the PTY echoes input into the output stream, so the
// output alone reconstructs the terminal screen.
//
// Files are read in bucket (chronological) order without re-sorting by
// timestamp, preserving the exact broadcast order so a replay lines up byte for
// byte with the live cutover offset captured by the shim.
func StreamOutput(ctx context.Context, logRoot, terminalID string, limit int64, fn func([]byte) error) error {
	if strings.TrimSpace(logRoot) == "" || strings.TrimSpace(terminalID) == "" {
		return nil
	}
	if limit == 0 {
		return nil
	}
	matches, err := filepath.Glob(filepath.Join(logRoot, safeName(terminalID), "*.jsonl"))
	if err != nil {
		return err
	}
	sort.Strings(matches)
	var emitted int64
	for _, path := range matches {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := readLogFile(path)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.Stream != LogStreamOutput {
				continue
			}
			data := entry.Data
			if limit > 0 {
				remaining := limit - emitted
				if remaining <= 0 {
					return nil
				}
				if int64(len(data)) > remaining {
					data = data[:remaining]
				}
			}
			if len(data) == 0 {
				continue
			}
			if err := fn(data); err != nil {
				return err
			}
			emitted += int64(len(data))
			if limit > 0 && emitted >= limit {
				return nil
			}
		}
	}
	return nil
}

func readLogFile(path string) ([]LogEntry, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	var out []LogEntry
	for scanner.Scan() {
		var row logFileEntry
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return nil, fmt.Errorf("read terminal log %s: %w", path, err)
		}
		data, err := base64.StdEncoding.DecodeString(row.Data)
		if err != nil {
			return nil, fmt.Errorf("read terminal log %s: %w", path, err)
		}
		out = append(out, LogEntry{
			Timestamp: row.Timestamp.UTC(),
			Stream:    row.Stream,
			Data:      data,
		})
	}
	return out, scanner.Err()
}

func logBucket(t time.Time) time.Time {
	return t.UTC().Round(logBucketDuration)
}

func logBucketPath(dir string, bucket time.Time) string {
	return filepath.Join(dir, fmt.Sprintf("%d.jsonl", bucket.Unix()))
}
