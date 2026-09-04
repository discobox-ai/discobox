package poolruntime

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager)

	before := runtimeProvider.acquireCalls
	err := provider.syncKnownPools(context.Background(), manager,
		&model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"},
		activePool("pool-1"))
	if err == nil {
		t.Fatal("sync reported success against an agent that refused every request")
	}

	attempts := len(poolSyncRetryBackoff) + 1
	if got := runtimeProvider.acquireCalls - before; got != attempts {
		t.Fatalf("AcquirePoolAgentClient calls = %d, want %d: every attempt needs its own lease", got, attempts)
	}
}
