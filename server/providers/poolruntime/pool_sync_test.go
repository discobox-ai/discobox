package poolruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
)

// A pool-agent client carries a single-use transport lease, so the retry needs
// a fresh one each time round. Acquiring it once outside the loop left every
// attempt after the first with no transport, dialing the literal `https://pool`
// and failing to resolve it -- a sync that could only ever land on its first
// try, reporting a DNS error whenever it did not.
func TestSyncKnownPoolsAcquiresAClientPerAttempt(t *testing.T) {
	// Fails every attempt, so the sync runs its whole backoff.
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(failing.Close)

	runtimeProvider := newTestRuntimeProvider(t, "project-1", "pool-1")
	runtimeProvider.baseURL = failing.URL
	runtimeProvider.client = failing.Client()
	runtimeProvider.staticToken = true

	restore := poolSyncRetryBackoff
	poolSyncRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { poolSyncRetryBackoff = restore })

	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)

	before := runtimeProvider.acquireCalls
	made, err := provider.syncKnownPools(context.Background(), manager,
		&model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"},
		activePool("pool-1"))
	if err == nil {
		t.Fatal("sync reported success against an agent that refused every request")
	}

	attempts := len(poolSyncRetryBackoff) + 1
	if got := runtimeProvider.acquireCalls - before; got != attempts {
		t.Fatalf("AcquirePoolAgentClient calls = %d, want %d: every attempt needs its own lease", got, attempts)
	}
	if made != attempts {
		t.Fatalf("sync reported %d attempts, want %d: the log says how many it made", made, attempts)
	}
}

// Pool-sync runs inside the pool's own reconcile, so an agent it cannot reach
// must come back as that failure. Falling back to scheduling the pool's
// reconcile, as the sandbox paths do, marks the in-flight resource dirty: the
// engine rejects that, and the rejection replaces the real cause in the log on
// every reconcile of a pool whose agent is gone.
func TestSyncKnownPoolsDoesNotScheduleItsOwnPool(t *testing.T) {
	unreachable := errors.New("pool agent unreachable")
	runtimeProvider := newTestRuntimeProvider(t, "project-1", "pool-1")
	runtimeProvider.acquireErrs = []error{unreachable, unreachable, unreachable}

	restore := poolSyncRetryBackoff
	poolSyncRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { poolSyncRetryBackoff = restore })

	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)

	_, err := provider.syncKnownPools(context.Background(), manager,
		&model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"},
		activePool("pool-1"))
	if !errors.Is(err, unreachable) {
		t.Fatalf("sync error = %v, want the agent's own failure", err)
	}
	if manager.scheduledReconciles != 0 {
		t.Fatalf("sync scheduled %d reconciles of the pool it runs inside", manager.scheduledReconciles)
	}
}

// What a reconcile's pool-sync logs, given the agent leases its attempts get
// (nil for one that is handed a client).
func poolSyncLog(t *testing.T, acquireErrs []error, unreachable error) string {
	t.Helper()
	// An agent that takes every pool-sync.
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(agent.Close)
	runtimeProvider := newTestRuntimeProvider(t, "project-1", "pool-1")
	runtimeProvider.baseURL, runtimeProvider.client, runtimeProvider.staticToken = agent.URL, agent.Client(), true
	runtimeProvider.acquireErrs = acquireErrs
	runtimeProvider.unreachable = unreachable

	restore := poolSyncRetryBackoff
	poolSyncRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { poolSyncRetryBackoff = restore })

	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	pool := activePool("pool-1")
	manager := &fakePoolManager{pool: pool, schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)
	if err := provider.ReconcilePool(context.Background(), manager, &model.Project{ID: "project-1"},
		&model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"}, pool,
		func(context.Context) error { return nil }); err != nil {
		t.Fatalf("reconcile pool: %v", err)
	}
	return logged.String()
}

// A pool-sync's retries can be the longest part of a pass, so a sync that
// retried says how long it took and how many attempts it made, whether it then
// landed or failed; one that lands at once is a routine re-check, and says
// nothing at info.
func TestReconcilePoolLogsTheTimeASyncSpentRetrying(t *testing.T) {
	refused := errors.New("connection refused")

	if logged := poolSyncLog(t, []error{refused}, nil); !strings.Contains(logged,
		`level=INFO msg="synced the pool's agent after retrying" pool=pool-1 duration=`) || !strings.Contains(logged, "attempts=2") {
		t.Errorf("a sync that landed on its second attempt did not log its duration and attempts; log:\n%s", logged)
	}

	attempts := fmt.Sprintf("attempts=%d", len(poolSyncRetryBackoff)+1)
	if logged := poolSyncLog(t, nil, refused); !strings.Contains(logged,
		`level=WARN msg="pool-sync failed" pool=pool-1 duration=`) || !strings.Contains(logged, attempts) {
		t.Errorf("a sync that failed did not log its duration and %s; log:\n%s", attempts, logged)
	}

	if logged := poolSyncLog(t, nil, nil); strings.Contains(logged, "pool-sync") || strings.Contains(logged, "synced the pool's agent") {
		t.Errorf("a sync that landed at once was logged; log:\n%s", logged)
	}
}
