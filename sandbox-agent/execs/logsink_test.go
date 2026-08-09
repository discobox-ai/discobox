package execs

import (
	"context"
	"sync"
	"time"
)

// fakeLogSink is an in-memory LogSink test double, standing in for the
// sqlite-backed store.Store the real exec-shim process opens (see
// docs/adr/0028). It's shared by tests in this package that exercise
// AsyncLogger/ReadExecLog through a real shim run.
type fakeLogSink struct {
	mu     sync.Mutex
	chunks map[string][]LogChunk
	// failAppend, if non-nil, is returned by AppendExecLogChunk instead of
	// recording the chunk — used to exercise AsyncLogger's onFlushErr path.
	failAppend error
}

func newFakeLogSink() *fakeLogSink {
	return &fakeLogSink{chunks: map[string][]LogChunk{}}
}

func (s *fakeLogSink) AppendExecLogChunk(_ context.Context, execID string, bucketStart time.Time, codec string, data []byte, rawSize int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAppend != nil {
		return s.failAppend
	}
	s.chunks[execID] = append(s.chunks[execID], LogChunk{
		BucketStart: bucketStart,
		Codec:       codec,
		Data:        append([]byte{}, data...),
		RawSize:     rawSize,
	})
	return nil
}

func (s *fakeLogSink) ListExecLogChunks(_ context.Context, execID string) ([]LogChunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]LogChunk{}, s.chunks[execID]...), nil
}

func (s *fakeLogSink) DeleteExecLog(_ context.Context, execID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.chunks, execID)
	return nil
}
