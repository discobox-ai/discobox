package poolruntime

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm/logger"

	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	poolagentserver "github.com/discobox-ai/discobox/pool-agent/server"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/x/gormdb"
)

// newHangingRuntimeProvider is a pool whose agent takes every write to route
// and never answers it, as an agent does whose teardown waits on a sandbox's
// power lock behind a start that never finishes. Each request's context ending
// is sent on the returned channel: that is the agent seeing the server stop
// waiting.
func newHangingRuntimeProvider(t *testing.T, projectID, poolID, route string) (*testRuntimeProvider, <-chan struct{}) {
	t.Helper()
	runtime := sandboxruntime.NewMemorySandboxRuntime()
	controlPlaneKey, poolToken := newPoolAgentTestAuth(t, projectID, poolID, poolagentserver.ScopeSandboxRead, poolagentserver.ScopeSandboxWrite)
	router, _ := poolagentserver.NewRouter(poolagentserver.Config{
		Identity:              poolagentserver.Identity{ProjectID: projectID, PoolID: poolID},
		Runtime:               runtime,
		ControlPlanePublicKey: controlPlaneKey,
	})
	abandoned := make(chan struct{}, 8)
	agent := newCreateProtocolTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && strings.HasSuffix(r.URL.Path, route) {
			<-r.Context().Done()
			abandoned <- struct{}{}
			return
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(agent.Close)
	return &testRuntimeProvider{baseURL: agent.URL, client: agent.Client(), token: poolToken, runtime: runtime}, abandoned
}

func shortTeardownDeadline(t *testing.T) {
	t.Helper()
	old := poolAgentTeardownDeadline
	poolAgentTeardownDeadline = 100 * time.Millisecond
	t.Cleanup(func() { poolAgentTeardownDeadline = old })
}

// An archive or a remove the pool's agent takes and never answers ends at the
// teardown deadline as the pool not being reachable, which the sandbox
// reconciler retries with backoff, instead of holding the reconcile, and its
// slot on the node, until the server stops (#1728). The agent sees the request
// end, so its wait for the sandbox's lock is abandoned with it.
func TestPoolProviderTeardownThePoolDoesNotAnswerInTimeIsThePoolNotReachable(t *testing.T) {
	shortTeardownDeadline(t)
	state := poolRuntimeState(t, &sandbox.Sandbox{SandboxID: "sandbox-1", Metadata: map[string]string{"pool_id": "pool-1"}})
	ref := sandbox.SandboxRef{ProjectID: "project-1", PoolID: "pool-1", SandboxID: "sandbox-1"}

	for name, tc := range map[string]struct {
		route    string
		state    []byte
		teardown func(*Provider, context.Context, sandbox.SandboxRef, []byte) ([]byte, error)
	}{
		"archive": {"/archive", state, (*Provider).Archive},
		"remove":  {"/sandbox-1", state, (*Provider).Remove},
		// Not not-found, as an agent that went away is (#1095): this one is
		// there, and the create it is queued behind may still finish.
		"remove without its state": {"/sandbox-1", nil, (*Provider).Remove},
	} {
		t.Run(name, func(t *testing.T) {
			runtimeProvider, abandoned := newHangingRuntimeProvider(t, "project-1", "pool-1", tc.route)
			manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
			provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := tc.teardown(provider, ctx, ref, tc.state)
			if !errors.Is(err, sandbox.ErrPoolNotReachable) || errors.Is(err, sandbox.ErrNotFound) {
				t.Fatalf("%s the pool never answers = %v, want the pool not reachable", name, err)
			}
			if want := "pool pool-1 did not answer the " + strings.Fields(name)[0]; !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "within 100ms") {
				t.Fatalf("%s error = %q, want it to say %q", name, err, want)
			}
			select {
			case <-abandoned:
			case <-time.After(5 * time.Second):
				t.Fatalf("the agent's %s request did not end with the server's deadline", name)
			}
		})
	}
}

// A caller that stops waiting is not the pool failing to answer: the
// reconcile's own context ending is reported as itself.
func TestPoolProviderTeardownCanceledByItsCallerIsNotTheDeadline(t *testing.T) {
	runtimeProvider, _ := newHangingRuntimeProvider(t, "project-1", "pool-1", "/archive")
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)
	ref := sandbox.SandboxRef{ProjectID: "project-1", PoolID: "pool-1", SandboxID: "sandbox-1"}
	state := poolRuntimeState(t, &sandbox.Sandbox{SandboxID: "sandbox-1", Metadata: map[string]string{"pool_id": "pool-1"}})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := provider.Archive(ctx, ref, state)
	if err == nil || strings.Contains(err.Error(), "did not answer the archive") {
		t.Fatalf("archive canceled by its caller = %v, want the caller's own end", err)
	}
	if ctx.Err() == nil {
		t.Fatal("archive returned before its caller's context ended")
	}
}

// archivingReconciler stands in for the sandbox reconciler: an id starting
// "archived-" is archived through the pool provider, and any other converges
// at once, as a create the pool answers does.
type archivingReconciler struct {
	provider *Provider
	state    []byte
	archives atomic.Int32
	created  atomic.Bool
}

func (r *archivingReconciler) Reconcile(ctx context.Context, id string) (reconcile.Result, error) {
	if !strings.HasPrefix(id, "archived-") {
		r.created.Store(true)
		return reconcile.Result{}, nil
	}
	r.archives.Add(1)
	_, err := r.provider.Archive(ctx, sandbox.SandboxRef{ProjectID: "project-1", PoolID: "pool-1", SandboxID: id}, r.state)
	return reconcile.Result{}, err
}

// A new box's reconcile is not held up behind archives of other boxes that
// their pool takes and never answers (#1728): every sandbox reconcile on a
// node shares one cap, and each of those archives used to hold its slot until
// the server stopped. Bounded, they give their slots back, and the create runs.
func TestUnansweredArchivesDoNotStarveANewSandbox(t *testing.T) {
	shortTeardownDeadline(t)
	runtimeProvider, _ := newHangingRuntimeProvider(t, "project-1", "pool-1", "/archive")
	manager := &fakePoolManager{pool: activePool("pool-1"), schedulable: true}
	provider := New(runtimeProvider, sandbox.ProviderDefinition{Name: "test"}, manager, nil)
	r := &archivingReconciler{
		provider: provider,
		state:    poolRuntimeState(t, &sandbox.Sandbox{SandboxID: "archived", Metadata: map[string]string{"pool_id": "pool-1"}}),
	}

	pools, err := gormdb.Open(gormdb.Config{
		Driver: gormdb.DriverSQLite,
		DSN:    filepath.Join(t.TempDir(), "test.db"),
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	engine, err := reconcile.New(pools.Write, reconcile.Options{
		SingleNode:   true,
		PollInterval: 20 * time.Millisecond,
		Lease:        2 * time.Second,
		BackoffBase:  time.Hour, // an archive retried would take its slot again
	})
	if err != nil {
		t.Fatalf("reconcile.New: %v", err)
	}
	if err := engine.Register("sandbox", r, reconcile.WithConcurrency(4)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := engine.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = engine.Stop(ctx)
	})

	ctx := context.Background()
	for _, id := range []string{"archived-1", "archived-2", "archived-3", "archived-4"} {
		if err := engine.MarkDirty(ctx, "sandbox", id); err != nil {
			t.Fatalf("mark %s: %v", id, err)
		}
	}
	waitUntil(t, "every archive to be in flight", func() bool { return r.archives.Load() == 4 })
	if err := engine.MarkDirty(ctx, "sandbox", "new"); err != nil {
		t.Fatalf("mark new: %v", err)
	}
	waitUntil(t, "the new sandbox's reconcile to run while four archives go unanswered", r.created.Load)
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
