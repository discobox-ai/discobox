package poolruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

	manager := &fakePoolManager{pool: reportedPool("pool-1"), schedulable: true}
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

	manager := &fakePoolManager{pool: reportedPool("pool-1"), schedulable: true}
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

	pool := reportedPool("pool-1")
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

// poolSyncBodies runs one pool-sync, against an agent that refuses the
// first attempt, of a pool whose agent did or did not declare it takes the
// list's age, and answers the knownPoolsAgeMs each attempt carried, nil for
// a body that did not name the field.
func poolSyncBodies(t *testing.T, declared bool, wait time.Duration) []*int64 {
	t.Helper()
	var mu sync.Mutex
	var ages []*int64
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var age *int64
		if raw, ok := body["knownPoolsAgeMs"]; ok {
			age = new(int64)
			if err := json.Unmarshal(raw, age); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		mu.Lock()
		defer mu.Unlock()
		ages = append(ages, age)
		if len(ages) == 1 {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(agent.Close)
	runtimeProvider := newTestRuntimeProvider(t, "project-1", "pool-1")
	runtimeProvider.baseURL, runtimeProvider.client, runtimeProvider.staticToken = agent.URL, agent.Client(), true

	restore := poolSyncRetryBackoff
	poolSyncRetryBackoff = []time.Duration{wait}
	t.Cleanup(func() { poolSyncRetryBackoff = restore })

	pool := reportedPool("pool-1")
	pool.PoolSyncListAge = declared
	manager := &fakePoolManager{pool: pool, schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)
	if _, err := provider.syncKnownPools(context.Background(), manager,
		&model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"}, pool); err != nil {
		t.Fatalf("sync: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return ages
}

// The pool set goes with its age, taken as each attempt is sent: a retry holds
// the set while a sibling pool's row commits, and the agent spares what was
// made after the read only if it knows how long ago that was.
func TestSyncKnownPoolsSendsTheListsAgeWithEachAttempt(t *testing.T) {
	const wait = 200 * time.Millisecond
	ages := poolSyncBodies(t, true, wait)
	if len(ages) != 2 || ages[0] == nil || ages[1] == nil {
		t.Fatalf("agent saw syncs carrying ages %v, want two", ages)
	}
	if *ages[1]-*ages[0] < wait.Milliseconds() {
		t.Fatalf("list ages %d, %d: the retry, %v later, sent the same set as no older", *ages[0], *ages[1], wait)
	}
}

// The pool API's request bodies are strict, so an agent that has not declared
// it takes the age would refuse every pool-sync that carried one, and never
// reap an orphan pool again. It is sent the bare set, as before.
func TestSyncKnownPoolsSendsNoAgeToAnAgentThatHasNotDeclaredIt(t *testing.T) {
	ages := poolSyncBodies(t, false, time.Millisecond)
	if len(ages) != 2 {
		t.Fatalf("agent saw %d syncs, want 2", len(ages))
	}
	for i, age := range ages {
		if age != nil {
			t.Fatalf("attempt %d carried knownPoolsAgeMs %d to an agent that has not declared it takes one", i+1, *age)
		}
	}
}

// reportedPool is an active pool whose agent has sent a status report, so a
// pool-sync knows what it takes.
func reportedPool(id string) *model.Pool {
	pool := activePool(id)
	reported := time.Now()
	pool.StatusReportedAt = &reported
	return pool
}

// firstReportManager is a pool manager whose pool's agent sends its first
// status report, declaring it takes a list age, only by the reads-th read of
// the pool: before that it has reported nothing, as the agent of a pool its own
// reconcile has just started.
type firstReportManager struct {
	*fakePoolManager
	reads int
}

func (m *firstReportManager) GetPool(ctx context.Context, projectID, poolID string) (*model.Pool, error) {
	m.mu.Lock()
	m.reads--
	if m.reads <= 0 {
		reported := time.Now()
		m.pool.StatusReportedAt, m.pool.PoolSyncListAge = &reported, true
	}
	m.mu.Unlock()
	return m.fakePoolManager.GetPool(ctx, projectID, poolID)
}

// A new pool's reconcile loaded its row before EnsurePool started its agent,
// so that row declares nothing. Its first sync is when the set is likeliest to
// miss a sibling made meanwhile, and it must carry the age: it waits for the
// agent's first report, and then reads what the agent declared there.
func TestSyncKnownPoolsWaitsForANewAgentsFirstReport(t *testing.T) {
	var mu sync.Mutex
	var ages []*int64
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		var age *int64
		if raw, ok := body["knownPoolsAgeMs"]; ok {
			age = new(int64)
			_ = json.Unmarshal(raw, age)
		}
		mu.Lock()
		ages = append(ages, age)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(agent.Close)
	runtimeProvider := newTestRuntimeProvider(t, "project-1", "pool-1")
	runtimeProvider.baseURL, runtimeProvider.client, runtimeProvider.staticToken = agent.URL, agent.Client(), true

	restore := poolSyncRetryBackoff
	poolSyncRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { poolSyncRetryBackoff = restore })

	// The row as the reconcile loaded it: never reported, declaring nothing.
	loaded := activePool("pool-1")
	manager := &firstReportManager{fakePoolManager: &fakePoolManager{pool: activePool("pool-1"), schedulable: true}, reads: 2}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)
	attempts, err := provider.syncKnownPools(context.Background(), manager,
		&model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"}, loaded)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if attempts != 2 || len(ages) != 1 {
		t.Fatalf("sync made %d attempts and reached the agent %d times, want 2 and 1: an agent that has not reported is not synced", attempts, len(ages))
	}
	if ages[0] == nil {
		t.Fatal("the new pool's first sync went out with no age, although its agent declared it takes one")
	}
}
