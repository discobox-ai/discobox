package proxy

import (
	"io"
	"testing"
)

// countingSource reports what the client-to-server counter held at the moment
// the bytes had been handed on but Write had not yet returned. That instant is
// the whole question: the origin can answer immediately, and the answer travels
// the other direction, whose goroutine may reach finish() and snapshot the
// counters right there.
type countingSource struct {
	io.ReadWriteCloser
	stream   *upgradedResponseStream
	short    int // bytes to accept, 0 meaning all of them
	observed int64
}

func (c *countingSource) Write(p []byte) (int, error) {
	n := len(p)
	if c.short > 0 {
		n = c.short
	}
	c.observed = c.stream.c2sBytes.Load()
	return n, nil
}

// A byte the origin has already seen must be counted, even if the peer
// direction records the audit event before this goroutine runs again. Counting
// after the write left a window in which it did not — an upgrade was audited
// c2s=0 despite the origin having answered what the client sent.
func TestUpgradeC2SBytesAreCountedBeforeTheyAreHandedOn(t *testing.T) {
	source := &countingSource{}
	stream := &upgradedResponseStream{source: source}
	source.stream = stream

	n, err := stream.Write([]byte("ping"))
	if err != nil || n != 4 {
		t.Fatalf("Write() = %d, %v", n, err)
	}
	if source.observed != 4 {
		t.Errorf("c2s bytes visible to the origin's peer = %d, want 4", source.observed)
	}
	if got := stream.c2sBytes.Load(); got != 4 {
		t.Errorf("c2s bytes after Write = %d, want 4", got)
	}
}

// A short write gives back what the origin never saw, so counting first does
// not leave the total permanently high.
func TestUpgradeC2SBytesGiveBackAShortWrite(t *testing.T) {
	source := &countingSource{short: 1}
	stream := &upgradedResponseStream{source: source}
	source.stream = stream

	if _, err := stream.Write([]byte("ping")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if got := stream.c2sBytes.Load(); got != 1 {
		t.Errorf("c2s bytes after a short write = %d, want 1", got)
	}
}
