package wslc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/providers/wslc/internal/wslcsession"
)

// sessionAttempts stands in for wslcsession.NewSession: it collides failures
// times, then answers with final.
type sessionAttempts struct {
	failures int
	final    error
	calls    int
}

func (a *sessionAttempts) newSession(wslcsession.Options) (*wslcsession.Session, error) {
	a.calls++
	if a.calls <= a.failures {
		return nil, fmt.Errorf("start: %w", wslcsession.ErrSessionExists)
	}
	return nil, a.final
}

// A development restart kills the old server, whose VM lingers for a moment
// before WSL ends it. A collision that clears inside the grace period is that
// cleanup, and has to start the VM rather than fail the reconcile.
func TestStaleSessionThatClearsIsNotAnError(t *testing.T) {
	attempts := &sessionAttempts{failures: 3}
	if _, err := newSessionAfterStale(context.Background(), attempts.newSession,
		wslcsession.Options{DisplayName: "discobox-pool_1"}, time.Second, time.Millisecond); err != nil {
		t.Fatalf("newSessionAfterStale = %v, want the VM started once the old one went away", err)
	}
	if attempts.calls != 4 {
		t.Fatalf("attempts = %d, want the three collisions retried and then one success", attempts.calls)
	}
}

// A VM that outlasts the grace period is no longer the cleanup, and the error is
// the one that says so: still matchable as a collision, and carrying the
// diagnosis for an abandoned VM that the collision itself no longer gives.
func TestStaleSessionThatPersistsExplainsItself(t *testing.T) {
	attempts := &sessionAttempts{failures: 1 << 30}
	_, err := newSessionAfterStale(context.Background(), attempts.newSession,
		wslcsession.Options{DisplayName: "discobox-pool_1"}, 20*time.Millisecond, time.Millisecond)
	if !errors.Is(err, wslcsession.ErrSessionExists) {
		t.Fatalf("error = %v, want it still to match ErrSessionExists", err)
	}
	for _, want := range []string{"has not gone away", "hcsdiag list", "hcsdiag kill"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
	if attempts.calls < 2 {
		t.Fatalf("attempts = %d, want the collision retried before giving up", attempts.calls)
	}
}

// Anything other than a collision is a real failure, and waiting on it would
// only delay the report.
func TestStaleSessionWaitDoesNotRetryOtherFailures(t *testing.T) {
	boom := errors.New("HRESULT 0x80004005")
	attempts := &sessionAttempts{final: boom}
	_, err := newSessionAfterStale(context.Background(), attempts.newSession,
		wslcsession.Options{}, time.Second, time.Millisecond)
	if !errors.Is(err, boom) || attempts.calls != 1 {
		t.Fatalf("error = %v after %d attempts, want the failure returned at once", err, attempts.calls)
	}
}

// The wait holds the driver mutex, so a canceled reconcile has to stop it
// rather than sit out the rest of the grace period.
func TestStaleSessionWaitStopsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	attempts := &sessionAttempts{failures: 1 << 30}
	start := time.Now()
	_, err := newSessionAfterStale(ctx, attempts.newSession, wslcsession.Options{}, time.Minute, time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want the context's", err)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("waited %v on a canceled context", waited)
	}
}
