package sandboxruntime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// Tier 2 of the attach wait (ADR 0039) waits for a container to come back only
// for a sandbox this pool is actually holding. The tree is what says so: it is
// the durable half of a sandbox and outlives the container a rebuild replaces,
// so a sandbox mid-rebuild is worth waiting for while an id this pool has never
// seen is not — waiting on that one would stall every sandbox-directed route
// for the full budget before failing.
func TestHostsSandboxTellsARebuildFromAnUnknownID(t *testing.T) {
	withTestRoot(t)
	runtime := &DockerSandboxRuntime{projectID: "proj_a", poolID: "pool_a"}

	if runtime.hostsSandbox("sbx_elsewhere") {
		t.Fatal("a sandbox with no tree on this pool reads as hosted here")
	}

	if err := os.MkdirAll(runtime.sandboxRoot("sbx_rebuilding"), 0o755); err != nil {
		t.Fatalf("create sandbox tree: %v", err)
	}
	if !runtime.hostsSandbox("sbx_rebuilding") {
		t.Fatal("a sandbox whose tree is retained here does not read as hosted")
	}
}

// runningDaemon is a Docker daemon holding one sandbox container that it
// reports as running.
func runningDaemon(t *testing.T, sandboxID string) *DockerSandboxRuntime {
	t.Helper()
	return sandboxDaemon(t, sandboxID, func() bool { return true })
}

// sandboxDaemon is a Docker daemon holding one sandbox container, which it
// reports as running whenever running says so, asked once per inspect. The
// container has no address, so a wait for its sandbox agent never succeeds; it
// ends when the container exits.
func sandboxDaemon(t *testing.T, sandboxID string, running func() bool) *DockerSandboxRuntime {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Api-Version", "1.45")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			_, _ = w.Write([]byte(`[{"Id":"ctr-1"}]`))
		case strings.HasSuffix(r.URL.Path, "/containers/ctr-1/json"):
			state := `{"Status":"running","Running":true}`
			if !running() {
				state = `{"Status":"exited","Running":false}`
			}
			_, _ = w.Write([]byte(`{"Id":"ctr-1","Config":{"Labels":{"` + sandboxLabelSandbox + `":"` + sandboxID + `"}},"State":` + state + `}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(server.URL, "http://")), client.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return &DockerSandboxRuntime{client: cli, projectID: "proj_a", poolID: "pool_a"}
}

// Docker reports a container running as soon as it starts, before the sandbox
// agent inside it answers. A request arriving while another start is still
// waiting for that agent must wait for the boot too, not be proxied to an
// agent that is not listening yet and come back a 502.
func TestEnsureSandboxRunningWaitsForABootUnderWay(t *testing.T) {
	runtime := runningDaemon(t, "sbx_1")

	boot := runtime.beginBoot("sbx_1")
	if !runtime.SandboxBooting("sbx_1") {
		t.Fatal("a sandbox mid-boot does not read as booting")
	}
	done := make(chan error, 1)
	go func() { done <- runtime.EnsureSandboxRunning(context.Background(), "sbx_1", true) }()

	select {
	case err := <-done:
		t.Fatalf("EnsureSandboxRunning returned %v while a start was still under way", err)
	case <-time.After(200 * time.Millisecond):
	}
	runtime.endBoot("sbx_1", boot, nil)
	if runtime.SandboxBooting("sbx_1") {
		t.Fatal("a sandbox whose boot ended still reads as booting")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("EnsureSandboxRunning = %v, want nil once the start finished", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("EnsureSandboxRunning did not return after the start finished")
	}
}

// A running sandbox nobody is booting is the healthy path, and it must not
// queue behind anything: another request's look at the same sandbox, or a
// power operation holding its lock for some other reason.
func TestEnsureSandboxRunningDoesNotWaitOnARunningSandbox(t *testing.T) {
	runtime := runningDaemon(t, "sbx_1")
	lock := runtime.sandboxLock("sbx_1")
	lock.Lock()
	defer lock.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runtime.EnsureSandboxRunning(ctx, "sbx_1", true); err != nil {
		t.Fatalf("EnsureSandboxRunning = %v, want nil at once", err)
	}
}

// A boot belongs to the sandbox, not to the request that began it. An attach
// canceled mid-start must not end the mark while the agent is still not
// listening, or every request behind it would be proxied into a 502; the mark
// lasts until the wait for the agent itself ends.
func TestABootOutlivesTheRequestThatBeganIt(t *testing.T) {
	exited := &atomic.Bool{}
	runtime := sandboxDaemon(t, "sbx_1", func() bool { return !exited.Load() })

	ctx, cancel := context.WithCancel(context.Background())
	boot := runtime.beginBoot("sbx_1")
	finished := make(chan error, 1)
	go func() { finished <- runtime.finishBoot(ctx, "sbx_1", boot) }()
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("finishBoot = %v, want the caller's cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("finishBoot did not return when its caller went away")
	}
	if !runtime.SandboxBooting("sbx_1") {
		t.Fatal("the boot ended with its requester, while the agent was still not answering")
	}

	exited.Store(true)
	select {
	case <-boot.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the boot did not end when its container exited")
	}
	if runtime.SandboxBooting("sbx_1") {
		t.Fatal("a sandbox whose boot ended still reads as booting")
	}
	if boot.err == nil {
		t.Fatal("a boot whose container exited ended without an error")
	}
}

// A request that first sees the sandbox down, and then finds it running under
// the lock on a boot an earlier request began and left, waits that boot out
// before it is proxied — with the gate and the lock released, so a stop or a
// clear is not held up behind it.
func TestEnsureSandboxRunningWaitsForABootFoundUnderTheLock(t *testing.T) {
	var inspects atomic.Int32
	// Down on the first look, running from the one taken under the lock.
	runtime := sandboxDaemon(t, "sbx_1", func() bool { return inspects.Add(1) > 1 })
	boot := runtime.beginBoot("sbx_1")

	done := make(chan error, 1)
	go func() { done <- runtime.EnsureSandboxRunning(context.Background(), "sbx_1", true) }()
	select {
	case err := <-done:
		t.Fatalf("EnsureSandboxRunning returned %v while the boot was still under way", err)
	case <-time.After(200 * time.Millisecond):
	}
	lock := runtime.sandboxLock("sbx_1")
	if !lock.TryLock() {
		t.Fatal("the power lock is held while a boot is waited out")
	}
	lock.Unlock()

	runtime.endBoot("sbx_1", boot, nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("EnsureSandboxRunning = %v, want nil once the boot ended", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("EnsureSandboxRunning did not return after the boot ended")
	}
}
