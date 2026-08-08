package nestedbridge

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPublishAndRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "forwarder.json")
	if err := Publish(path, "172.19.0.1:17008"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := PublishedAddress(path); got != "172.19.0.1:17008" {
		t.Fatalf("PublishedAddress = %q", got)
	}
	// Re-publishing a different address (dockerd chose a different subnet on a
	// later boot) must replace, not append.
	if err := Publish(path, "172.22.0.1:17008"); err != nil {
		t.Fatalf("re-Publish: %v", err)
	}
	if got := PublishedAddress(path); got != "172.22.0.1:17008" {
		t.Fatalf("PublishedAddress after rewrite = %q", got)
	}
}

// An absent or unreadable file means "no forwarder yet". Callers must be able
// to tell that apart from an address, so they degrade instead of guessing one.
func TestPublishedAddressAbsentIsEmpty(t *testing.T) {
	if got := PublishedAddress(filepath.Join(t.TempDir(), "missing.json")); got != "" {
		t.Fatalf("expected empty for a missing file, got %q", got)
	}
	garbage := filepath.Join(t.TempDir(), "garbage.json")
	if err := os.WriteFile(garbage, []byte("not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := PublishedAddress(garbage); got != "" {
		t.Fatalf("expected empty for unparseable content, got %q", got)
	}
}

// Loopback always exists, so discovery should resolve it immediately and
// return a host:port suitable for binding.
func TestWaitForAddressResolvesExistingInterface(t *testing.T) {
	addr, err := WaitForAddress(context.Background(), "lo", 5*time.Second)
	if err != nil {
		t.Skipf("no usable loopback in this environment: %v", err)
	}
	if addr != "127.0.0.1:17008" {
		t.Fatalf("WaitForAddress(lo) = %q, want 127.0.0.1:17008", addr)
	}
}

// dockerd creates docker0 asynchronously, so a missing interface is a wait,
// not an error -- but it must give up rather than hang a unit forever.
func TestWaitForAddressTimesOut(t *testing.T) {
	start := time.Now()
	_, err := WaitForAddress(context.Background(), "discobox-nope0", 600*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout for an interface that does not exist")
	}
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Fatalf("returned after %s, so it did not actually wait", elapsed)
	}
}

// A canceled context (unit stopping) must unblock the wait promptly.
func TestWaitForAddressHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	if _, err := WaitForAddress(ctx, "discobox-nope0", time.Minute); err == nil {
		t.Fatal("expected cancellation to end the wait")
	}
}
