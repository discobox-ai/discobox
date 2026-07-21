package execs

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

// Log streams, matching the attach frames they are recorded alongside. A TTY
// exec records its single merged stream as stdout and never records stderr,
// exactly as it does on the wire: a reader should not be able to tell a TTY exec
// from a pipe exec that wrote nothing to stderr.
const (
	LogStreamInput  LogStream = "input"
	LogStreamStdout LogStream = "stdout"
	LogStreamStderr LogStream = "stderr"
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
	dir    string
	mu     sync.Mutex
	cond   *sync.Cond
	queue  []LogEntry
	closed bool
	wg     sync.WaitGroup
}

func NewAsyncLogger(dir, execID string) (*AsyncLogger, error) {
	dir = strings.TrimSpace(dir)
	execID = strings.TrimSpace(execID)
	if dir == "" || execID == "" {
		return nil, nil
	}
	l := &AsyncLogger{dir: filepath.Join(dir, safeName(execID))}
	l.cond = sync.NewCond(&l.mu)
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
				_, _ = currentFile.Write(append(data, '\n'))
			}
		}
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

func ReadLogs(ctx context.Context, logRoot, execID string) ([]LogEntry, error) {
	if strings.TrimSpace(logRoot) == "" || strings.TrimSpace(execID) == "" {
		return nil, nil
	}
	matches, err := filepath.Glob(filepath.Join(logRoot, safeName(execID), "*.jsonl"))
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
			return nil, fmt.Errorf("read exec log %s: %w", path, err)
		}
		data, err := base64.StdEncoding.DecodeString(row.Data)
		if err != nil {
			return nil, fmt.Errorf("read exec log %s: %w", path, err)
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
