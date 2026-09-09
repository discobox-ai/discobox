package server

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// Services are declared inside the working tree, so a push-delivered sandbox
// has nothing to discover until its source lands. Starting before the wait
// found an empty directory, which is not an error — most repositories declare
// no services — so the boot said nothing and never looked again.
func TestDeclaredServicesWaitForTheSources(t *testing.T) {
	released := make(chan struct{})
	var started atomic.Bool

	done := make(chan struct{})
	go func() {
		defer close(done)
		startDeclaredServices(context.Background(), quietLogger(),
			func(context.Context) error {
				<-released
				return nil
			},
			func(context.Context, *slog.Logger) error {
				started.Store(true)
				return nil
			})
	}()

	// Long enough that a start racing the gate would have happened by now.
	time.Sleep(50 * time.Millisecond)
	if started.Load() {
		t.Fatal("services started before the sandbox's sources were delivered")
	}
	close(released)
	<-done
	if !started.Load() {
		t.Fatal("services never started after the sources were delivered")
	}
}

// A sandbox whose sources were in place before its container existed has no
// gate at all, and must not acquire a wait it would never be released from.
func TestDeclaredServicesStartWithNoGate(t *testing.T) {
	var started atomic.Bool
	startDeclaredServices(context.Background(), quietLogger(), nil,
		func(context.Context, *slog.Logger) error {
			started.Store(true)
			return nil
		})
	if !started.Load() {
		t.Fatal("services did not start when there was nothing to wait for")
	}
}

// A wait that ends because the agent is shutting down is not a start.
func TestDeclaredServicesDoNotStartWhenTheWaitFails(t *testing.T) {
	for _, waitErr := range []error{context.Canceled, errors.New("watch failed")} {
		var started atomic.Bool
		startDeclaredServices(context.Background(), quietLogger(),
			func(context.Context) error { return waitErr },
			func(context.Context, *slog.Logger) error {
				started.Store(true)
				return nil
			})
		if started.Load() {
			t.Fatalf("services started after the wait returned %v", waitErr)
		}
	}
}
