package poolagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

func statusWaitTestPoller(t *testing.T, servers map[string]*httptest.Server) *sandboxAgentStatusPoller {
	t.Helper()
	return &sandboxAgentStatusPoller{
		logger:    slog.New(slog.DiscardHandler),
		bootstrap: Bootstrap{ProjectID: "project-1", PoolID: "pool-1"},
		identity:  statusPollTestIdentity(t),
		runtime: &statusPollTestRuntime{
			MemorySandboxRuntime: sandboxruntime.NewMemorySandboxRuntime(),
			servers:              servers,
		},
		client:  &fakeSandboxAgentStatusClient{},
		tokens:  map[string]cachedSandboxAgentToken{},
		wake:    make(chan string, sandboxAgentStatusWakeQueue),
		waiters: map[string]context.CancelFunc{},
	}
}

func receiveWake(t *testing.T, wake <-chan string, what string) {
	t.Helper()
	select {
	case sandboxID := <-wake:
		if sandboxID != "sandbox-1" {
			t.Fatalf("woke for %q, want sandbox-1", sandboxID)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no wake for %s", what)
	}
}

// The first wait names no version and is answered at once; each after names
// the last one seen and is held. A changed version wakes the loop, and so does
// the first answer, since a port may have been bound before the waiter was up.
func TestTheWaiterWakesTheLoopWhenThePortsChange(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var afters []string
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the wait is this test's: another test's leftover dial can land
		// on a port this server reused.
		if r.URL.Path != "/api/projects/project-1/sandboxes/sandbox-1/status/wait" {
			http.NotFound(w, r)
			return
		}
		after := r.URL.Query().Get("after")
		mu.Lock()
		afters = append(afters, after)
		mu.Unlock()
		version := "v1"
		switch after {
		case "v1":
			select {
			case <-release:
				version = "v2"
			case <-r.Context().Done():
				return
			}
		case "v2":
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"portsVersion":"` + version + `"}`))
	}))
	defer agent.Close()

	poller := statusWaitTestPoller(t, map[string]*httptest.Server{"sandbox-1": agent})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go poller.waitForPorts(ctx, "sandbox-1")

	receiveWake(t, poller.wake, "the first answer")
	select {
	case id := <-poller.wake:
		t.Fatalf("woke for %s with nothing changed", id)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	receiveWake(t, poller.wake, "the changed ports")

	mu.Lock()
	defer mu.Unlock()
	if len(afters) < 2 || afters[0] != "" || afters[1] != "v1" {
		t.Fatalf("afters = %q, want the first wait to name nothing and the second v1", afters)
	}
}

// An agent older than the route answers 404, which the waiter takes as the
// route being absent rather than as a failure to retry at once.
func TestAnAgentWithoutTheWaitIsToldApart(t *testing.T) {
	agent := httptest.NewServer(http.NotFoundHandler())
	defer agent.Close()
	poller := statusWaitTestPoller(t, map[string]*httptest.Server{"sandbox-1": agent})
	if _, err := poller.waitOne(t.Context(), "sandbox-1", "token", ""); !errors.Is(err, errStatusWaitUnsupported) {
		t.Fatalf("err = %v, want errStatusWaitUnsupported", err)
	}
}

// A sandbox seen running is waited on from then; one seen leaving running, or
// missing from a tick's running set, is not. A nil poller — a pool with no
// status client — ignores state changes.
func TestWaitersFollowTheSandboxesThatAreRunning(t *testing.T) {
	poller := statusWaitTestPoller(t, nil)
	ctx := t.Context()
	count := func() int {
		poller.mu.Lock()
		defer poller.mu.Unlock()
		return len(poller.waiters)
	}

	poller.sandboxStateChanged(ctx, "sandbox-1", sandboxruntime.StateRunning)
	poller.sandboxStateChanged(ctx, "sandbox-1", sandboxruntime.StateRunning)
	if got := count(); got != 1 {
		t.Fatalf("waiters = %d after sandbox-1 started twice, want 1", got)
	}
	poller.sandboxStateChanged(ctx, "sandbox-1", sandboxruntime.StateStopping)
	if got := count(); got != 0 {
		t.Fatalf("waiters = %d after sandbox-1 stopped, want 0", got)
	}

	poller.waitOn(ctx, []string{"sandbox-1", "sandbox-2"}, time.Now())
	poller.waitOn(ctx, []string{"sandbox-2"}, time.Now())
	poller.mu.Lock()
	_, one := poller.waiters["sandbox-1"]
	_, two := poller.waiters["sandbox-2"]
	poller.mu.Unlock()
	if one || !two {
		t.Fatalf("waiters on sandbox-1 %v and sandbox-2 %v, want only sandbox-2", one, two)
	}

	var none *sandboxAgentStatusPoller
	none.sandboxStateChanged(ctx, "sandbox-1", sandboxruntime.StateRunning)
}

// A burst of changes is checked in as one batch, each sandbox once.
func TestQueuedWakesAreCheckedInTogether(t *testing.T) {
	poller := statusWaitTestPoller(t, nil)
	for _, id := range []string{"sandbox-2", "sandbox-1", "sandbox-2"} {
		poller.wake <- id
	}
	got := poller.drainWakes("sandbox-1")
	if len(got) != 2 || got[0] != "sandbox-1" || got[1] != "sandbox-2" {
		t.Fatalf("drained %q, want sandbox-1 then sandbox-2", got)
	}
}

// An agent that answers with no version has nothing to wait on, and is told
// apart the same way: the waiter must not ask it again at once.
func TestAnAgentWithNoVersionIsToldApart(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"portsVersion":""}`))
	}))
	defer agent.Close()
	poller := statusWaitTestPoller(t, map[string]*httptest.Server{"sandbox-1": agent})
	if _, err := poller.waitOne(t.Context(), "sandbox-1", "token", ""); !errors.Is(err, errStatusWaitUnsupported) {
		t.Fatalf("err = %v, want errStatusWaitUnsupported", err)
	}
}

// An agent that answers a wait naming its version at once, with that same
// version, is not holding; the waiter backs off rather than asking again in a
// loop.
func TestTheWaiterDoesNotSpinOnAnAgentThatDoesNotHold(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		_, _ = w.Write([]byte(`{"portsVersion":"v1"}`))
	}))
	defer agent.Close()
	poller := statusWaitTestPoller(t, map[string]*httptest.Server{"sandbox-1": agent})
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	poller.waitForPorts(ctx, "sandbox-1")
	mu.Lock()
	defer mu.Unlock()
	if calls > 3 {
		t.Fatalf("asked %d times in half a second, want a back-off", calls)
	}
}

// A tick's list is read before it is acted on, and a sandbox can start or stop
// in between. The list is older than that change and does not undo it: a box
// that started while the tick was listing keeps the waiter its start gave it,
// and one that stopped is not given one back.
func TestATicksOlderListDoesNotUndoAStartOrStop(t *testing.T) {
	poller := statusWaitTestPoller(t, nil)
	ctx := t.Context()
	poller.sandboxStateChanged(ctx, "sandbox-2", sandboxruntime.StateRunning)
	listedAt := time.Now()
	poller.sandboxStateChanged(ctx, "sandbox-1", sandboxruntime.StateRunning)
	poller.sandboxStateChanged(ctx, "sandbox-2", sandboxruntime.StateStopping)

	// Listed before sandbox-1 started and before sandbox-2 stopped.
	poller.waitOn(ctx, []string{"sandbox-2"}, listedAt)
	poller.mu.Lock()
	_, one := poller.waiters["sandbox-1"]
	_, two := poller.waiters["sandbox-2"]
	poller.mu.Unlock()
	if !one || two {
		t.Fatalf("waiters on sandbox-1 %v and sandbox-2 %v, want sandbox-1's kept and none on the stopped sandbox-2", one, two)
	}

	// A list taken after both changes is the truth again.
	poller.waitOn(ctx, []string{"sandbox-2"}, time.Now())
	poller.mu.Lock()
	_, one = poller.waiters["sandbox-1"]
	_, two = poller.waiters["sandbox-2"]
	poller.mu.Unlock()
	if one || !two {
		t.Fatalf("waiters on sandbox-1 %v and sandbox-2 %v, want the newer list followed", one, two)
	}
}

// What moves the version is the box's, and a box that moves it on every
// answer — a hostile agent, or a test run starting servers — does not get a
// check-in per answer: wakes for one sandbox are spaced by the pool.
func TestWakesForOneSandboxAreSpaced(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/status/wait") {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"portsVersion":"v%d"}`, n)
	}))
	defer agent.Close()
	poller := statusWaitTestPoller(t, map[string]*httptest.Server{"sandbox-1": agent})
	ctx, cancel := context.WithTimeout(t.Context(), sandboxAgentStatusWakeSpacing/2)
	defer cancel()
	poller.waitForPorts(ctx, "sandbox-1")
	mu.Lock()
	defer mu.Unlock()
	if wakes := len(poller.wake); wakes != 1 || calls != 1 {
		t.Fatalf("%d wakes from %d answers inside one spacing, want one of each", wakes, calls)
	}
}
