package execs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// A verbose exec must not wait out the full logBucketDuration before its
// output becomes durable: crossing flushSizeThreshold flushes early, bounding
// both read staleness and how much an unclean kill can lose (see
// docs/adr/0028).
func TestAsyncLoggerFlushesOnSizeThresholdBeforeClose(t *testing.T) {
	sink := newFakeLogSink()
	logger, err := NewAsyncLogger(sink, "exec_size", nil)
	if err != nil {
		t.Fatalf("new async logger: %v", err)
	}
	defer logger.Close()

	chunk := make([]byte, 32*1024)
	for i := range chunk {
		chunk[i] = byte(i)
	}
	written := 0
	for written < flushSizeThreshold {
		logger.Record(LogStreamStdout, chunk)
		written += len(chunk)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		chunks, err := sink.ListExecLogChunks(context.Background(), "exec_size")
		if err != nil {
			t.Fatalf("list chunks: %v", err)
		}
		if len(chunks) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no chunk flushed within %s of crossing the size threshold, without Close ever being called", 5*time.Second)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Close must drain a still-open, sub-threshold, sub-timer bucket rather than
// dropping it: a session almost never ends exactly on a flush boundary.
func TestAsyncLoggerDrainsPartialBucketOnClose(t *testing.T) {
	sink := newFakeLogSink()
	logger, err := NewAsyncLogger(sink, "exec_partial", nil)
	if err != nil {
		t.Fatalf("new async logger: %v", err)
	}
	logger.Record(LogStreamStdout, []byte("partial"))
	logger.Close()

	entries, err := ReadExecLog(context.Background(), sink, "exec_partial")
	if err != nil {
		t.Fatalf("read exec log: %v", err)
	}
	if len(entries) != 1 || string(entries[0].Data) != "partial" {
		t.Fatalf("entries = %#v, want one entry \"partial\"", entries)
	}
}

// A failed flush (e.g. sqlite's busy_timeout exceeded under multi-process
// write contention, see docs/adr/0028) must not vanish silently: the caller's
// onFlushErr callback is the only signal that a bucket's data was dropped.
func TestAsyncLoggerReportsFlushFailures(t *testing.T) {
	sink := newFakeLogSink()
	wantErr := errors.New("database is locked")
	sink.failAppend = wantErr

	var mu sync.Mutex
	var got error
	reported := make(chan struct{}, 1)
	logger, err := NewAsyncLogger(sink, "exec_fail", func(err error) {
		mu.Lock()
		got = err
		mu.Unlock()
		select {
		case reported <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatalf("new async logger: %v", err)
	}
	logger.Record(LogStreamStdout, []byte("hi"))
	logger.Close()

	select {
	case <-reported:
	default:
		t.Fatal("onFlushErr was never called")
	}
	mu.Lock()
	defer mu.Unlock()
	if got == nil || !errors.Is(got, wantErr) {
		t.Fatalf("onFlushErr error = %v, want wrapping %v", got, wantErr)
	}
}
