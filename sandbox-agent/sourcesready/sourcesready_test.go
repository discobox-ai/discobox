package sourcesready

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/sandboxconfig"
)

// Every sandbox whose sources were materialized before its container existed
// must not acquire a wait — including every sandbox created before the field,
// whose config names no source that awaits delivery.
func TestGateIsAbsentWhenNothingAwaitsDelivery(t *testing.T) {
	if gate := Gate(nil, "", nil); gate != nil {
		t.Fatal("a sandbox with no sources acquired a wait")
	}
	clone := []sandboxconfig.Source{{Slug: "primary", Target: "/workspace"}}
	if gate := Gate(clone, "", nil); gate != nil {
		t.Fatal("a clone-delivered source acquired a wait")
	}
	pushed := []sandboxconfig.Source{
		{Slug: "primary", Target: "/workspace"},
		{Slug: "foo", Target: "/src/foo", AwaitsDelivery: true},
	}
	if gate := Gate(pushed, "", nil); gate == nil {
		t.Fatal("a source awaiting delivery did not acquire a wait")
	}
}

// The overwhelmingly common case is a launch that happens long after delivery:
// it must cost one stat and no waiting at all.
func TestWaitReturnsImmediatelyWhenAlreadySettled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, sandboxconfig.SourcesReadyFileName)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	if err := Wait(ctx, path, nil); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed > backstopInterval {
		t.Fatalf("wait took %s on an already-settled sandbox, want no waiting at all", elapsed)
	}
}

// A launch that genuinely races delivery wakes on the signal appearing, not on
// the backstop tick: the file is created well inside one interval and the wait
// must end long before that interval elapses.
func TestWaitWakesOnTheSignalRatherThanTheBackstop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, sandboxconfig.SourcesReadyFileName)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- Wait(ctx, path, nil) }()

	// Long enough that the watch is armed, short enough that a wait ending on
	// the backstop tick instead would be visible in the elapsed time.
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wait: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("wait did not end after the sandbox was settled")
	}
	if elapsed := time.Since(start); elapsed >= backstopInterval {
		t.Fatalf("wait took %s, want it to wake on the event rather than the %s backstop", elapsed, backstopInterval)
	}
}

// A sandbox torn down while it waits must not hold its launch open: the wait
// ends with the context, and the caller reports it rather than launching.
func TestWaitEndsWithTheContext(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Wait(ctx, filepath.Join(dir, sandboxconfig.SourcesReadyFileName), nil) }()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("wait returned success after its context was canceled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait outlived its context")
	}
}
