package handover

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// One holder of a lock at a time: a second waits until the first lets go.
func TestALockIsHeldByOneAtATime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locks", "active")
	first, err := Acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, held, err := TryAcquire(path); err != nil || held {
		t.Fatalf("TryAcquire of a held lock = %v, %v; want not held", held, err)
	}
	if held, err := Held(path); err != nil || !held {
		t.Fatalf("Held = %v, %v; want held", held, err)
	}
	got := make(chan *Lock, 1)
	go func() {
		lock, err := Acquire(context.Background(), path)
		if err != nil {
			t.Error(err)
		}
		got <- lock
	}()
	select {
	case <-got:
		t.Fatal("a second holder took the lock while the first held it")
	case <-time.After(150 * time.Millisecond):
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case second := <-got:
		_ = second.Release()
	case <-time.After(5 * time.Second):
		t.Fatal("the second holder never took the lock the first let go")
	}
	if held, err := Held(path); err != nil || held {
		t.Fatalf("Held after both let go = %v, %v; want free", held, err)
	}
}

// A wait for a lock ends with its context.
func TestAWaitForALockEndsWithItsContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "active")
	lock, err := Acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := Acquire(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire of a held lock = %v, want the context's deadline", err)
	}
}

// Marks share a lock, and it is held until the last lets go.
func TestMarksShareALockUntilTheLastLetsGo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boot")
	if held, err := Held(path); err != nil || held {
		t.Fatalf("Held of a lock never taken = %v, %v; want free", held, err)
	}
	first, err := Mark(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Mark(context.Background(), path)
	if err != nil {
		t.Fatalf("a second mark waited on the first: %v", err)
	}
	if _, held, _ := TryAcquire(path); held {
		t.Fatal("a lock was taken alone over two marks")
	}
	_ = first.Release()
	if held, _ := Held(path); !held {
		t.Fatal("the mark ended with the first of two")
	}
	freed := make(chan error, 1)
	go func() { freed <- AwaitFree(context.Background(), path) }()
	select {
	case err := <-freed:
		t.Fatalf("AwaitFree returned with a mark held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	_ = second.Release()
	select {
	case err := <-freed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AwaitFree did not return once the last mark let go")
	}
}

// InFlight counts a handler for as long as it runs, an upgraded stream's
// included, and Wait returns once none is.
func TestInFlightCountsAHandlerUntilItReturns(t *testing.T) {
	var inFlight InFlight
	release := make(chan struct{})
	entered := make(chan struct{})
	server := httptest.NewServer(inFlight.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	})))
	defer server.Close()
	go func() {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
		if err != nil {
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := inFlight.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait with a handler running = %v, want the deadline", err)
	}
	close(release)
	if err := inFlight.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}
