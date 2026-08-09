package execs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// logBucketDuration is how often a live logger flushes to its LogSink on a
// timer. A bucket also flushes early if it crosses flushSizeThreshold, and
// always flushes (partial or not) when Close is called — see AsyncLogger.run.
const logBucketDuration = 15 * time.Second

// flushSizeThreshold flushes a bucket early once its buffered raw bytes cross
// this size, bounding both how stale a read of a still-running exec's log can
// be and how much output an unclean kill (OOM, SIGKILL, forced container
// stop) can lose, independent of the time-based trigger.
const flushSizeThreshold = 256 * 1024

const zstdCodec = "zstd"

// flushTimeout bounds each individual flush write to the LogSink. It is
// deliberately its own fresh context rather than one derived from the
// exec-shim's lifetime context: the final drain-on-close flush (see Close)
// must not be aborted by the same SIGTERM that triggered shutdown.
const flushTimeout = 10 * time.Second

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

// AsyncLogger batches an exec's transcript into a LogSink. Entries are
// buffered in memory and flushed as one compressed row per bucket, not one
// write per chunk, so a verbose or long-running exec does not turn into a
// stream of tiny sqlite transactions (see docs/adr/0028).
type AsyncLogger struct {
	sink       LogSink
	execID     string
	encoder    *zstd.Encoder
	onFlushErr func(error)
	mu         sync.Mutex
	cond       *sync.Cond
	queue      []LogEntry
	closed     bool
	wg         sync.WaitGroup
}

// NewAsyncLogger starts a logger that batches execID's transcript into sink.
// onFlushErr, if non-nil, is called (from the logger's background goroutine)
// whenever a flush to sink fails — for example because sqlite's busy_timeout
// was exceeded under multi-process write contention (see docs/adr/0028). A
// failed flush's bucket is otherwise dropped silently, so a caller that wants
// flush failures to be observable rather than silently lost data must supply
// this. It follows the same optional-callback shape as secretswatch.Watch's
// onError, keeping this package free of a logging dependency.
func NewAsyncLogger(sink LogSink, execID string, onFlushErr func(error)) (*AsyncLogger, error) {
	execID = strings.TrimSpace(execID)
	if sink == nil || execID == "" {
		return nil, nil
	}
	// A nil-writer encoder is only usable via EncodeAll, which is safe to call
	// repeatedly on one instance (klauspost/compress/zstd docs) — one encoder
	// is reused across every flush this logger does, rather than paying setup
	// cost per flush.
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, err
	}
	l := &AsyncLogger{sink: sink, execID: execID, encoder: encoder, onFlushErr: onFlushErr}
	l.cond = sync.NewCond(&l.mu)
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
	l.encoder.Close()
}

func (l *AsyncLogger) run() {
	defer l.wg.Done()
	var bucket []LogEntry
	var bucketStart time.Time
	var bucketRawSize int
	flush := func() {
		if len(bucket) == 0 {
			return
		}
		l.flushBucket(bucketStart, bucket)
		bucket = nil
		bucketRawSize = 0
	}
	for {
		entries, done := l.nextBatch(bucketStart, len(bucket) > 0)
		for _, entry := range entries {
			if len(bucket) == 0 {
				bucketStart = entry.Timestamp
			}
			bucket = append(bucket, entry)
			bucketRawSize += len(entry.Data)
			if bucketRawSize >= flushSizeThreshold {
				flush()
			}
		}
		if len(bucket) > 0 && time.Since(bucketStart) >= logBucketDuration {
			flush()
		}
		if done {
			flush()
			return
		}
	}
}

// nextBatch blocks for new entries, waking early once logBucketDuration has
// elapsed since bucketStart if a bucket is already open, so a slow trickle of
// output still flushes on the timer instead of waiting indefinitely for the
// next chunk to arrive.
func (l *AsyncLogger) nextBatch(bucketStart time.Time, haveBucket bool) ([]LogEntry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for len(l.queue) == 0 && !l.closed {
		if haveBucket {
			remaining := logBucketDuration - time.Since(bucketStart)
			if remaining <= 0 {
				return nil, false
			}
			timer := time.AfterFunc(remaining, l.cond.Signal)
			l.cond.Wait()
			timer.Stop()
			continue
		}
		l.cond.Wait()
	}
	entries := l.queue
	l.queue = nil
	return entries, l.closed
}

func (l *AsyncLogger) flushBucket(bucketStart time.Time, entries []LogEntry) {
	var buf bytes.Buffer
	rawSize := 0
	for _, entry := range entries {
		row := logFileEntry{
			Timestamp: entry.Timestamp.UTC(),
			Stream:    entry.Stream,
			Data:      base64.StdEncoding.EncodeToString(entry.Data),
		}
		data, err := json.Marshal(row)
		if err != nil {
			continue
		}
		buf.Write(data)
		buf.WriteByte('\n')
		rawSize += len(entry.Data)
	}
	if buf.Len() == 0 {
		return
	}
	encoded := l.encoder.EncodeAll(buf.Bytes(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), flushTimeout)
	defer cancel()
	if err := l.sink.AppendExecLogChunk(ctx, l.execID, bucketStart, zstdCodec, encoded, rawSize); err != nil && l.onFlushErr != nil {
		l.onFlushErr(fmt.Errorf("flush exec log chunk: %w", err))
	}
}

// ReadExecLog returns an exec's full transcript, decompressing and
// concatenating every chunk a LogSink holds for it, oldest first.
func ReadExecLog(ctx context.Context, sink LogSink, execID string) ([]LogEntry, error) {
	if sink == nil || strings.TrimSpace(execID) == "" {
		return nil, nil
	}
	chunks, err := sink.ListExecLogChunks(ctx, execID)
	if err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		return nil, nil
	}
	// One decoder reused across every chunk (DecodeAll is safe to call
	// repeatedly on one instance), rather than paying setup cost per chunk.
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer decoder.Close()
	var out []LogEntry
	for _, chunk := range chunks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, err := decodeLogChunk(decoder, chunk)
		if err != nil {
			return nil, fmt.Errorf("read exec log %s: %w", execID, err)
		}
		out = append(out, entries...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Timestamp.Before(out[j].Timestamp)
	})
	return out, nil
}

func decodeLogChunk(decoder *zstd.Decoder, chunk LogChunk) ([]LogEntry, error) {
	if chunk.Codec != zstdCodec {
		return nil, fmt.Errorf("unsupported log chunk codec %q", chunk.Codec)
	}
	raw, err := decoder.DecodeAll(chunk.Data, nil)
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	var out []LogEntry
	for scanner.Scan() {
		var row logFileEntry
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return nil, err
		}
		data, err := base64.StdEncoding.DecodeString(row.Data)
		if err != nil {
			return nil, err
		}
		out = append(out, LogEntry{
			Timestamp: row.Timestamp.UTC(),
			Stream:    row.Stream,
			Data:      data,
		})
	}
	return out, scanner.Err()
}
