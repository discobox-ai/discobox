package sandboxruntime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// A container that is still running is a sandbox still coming up, however
// slowly: on a host under heavy load its agent can take minutes to answer.
// The wait for that agent must not give up on it and fail the create while the
// container runs, at the thirty seconds it once did, or anywhere near them: its
// bound is a backstop for an agent that will never answer, not a measure of a
// slow boot.
func TestWaitForSandboxAgentOutlastsASlowBootWhileTheContainerRuns(t *testing.T) {
	if bound := (&sandboxCore{}).agentReadyBound(); bound < 5*time.Minute {
		t.Fatalf("a boot gives up on its agent after %s, want at least five minutes", bound)
	}

	runtime := runningDaemon(t, "sbx_1")
	runtime.agentReadyTimeout = time.Second
	done := make(chan error, 1)
	go func() { done <- runtime.waitForSandboxAgent(context.Background(), "sbx_1") }()

	select {
	case err := <-done:
		t.Fatalf("waitForSandboxAgent gave up on a container that is still running, before its bound: %v", err)
	case <-time.After(800 * time.Millisecond):
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a wait on an agent that never answered ended without an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitForSandboxAgent did not end at its bound")
	}
}

// A container removed under its boot — a delete, an archive, a rebuild — has
// no agent coming. The wait ends at once rather than spinning on it until its
// bound, which holds the sandbox's boot mark for minutes.
func TestWaitForSandboxAgentEndsWhenTheContainerIsRemoved(t *testing.T) {
	removed := &atomic.Bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Api-Version", "1.45")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			_, _ = w.Write([]byte(`[{"Id":"ctr-1"}]`))
		case strings.HasSuffix(r.URL.Path, "/containers/ctr-1/json") && !removed.Load():
			_, _ = w.Write([]byte(`{"Id":"ctr-1","Config":{"Labels":{"` + sandboxLabelSandbox + `":"sbx_1"}},"State":{"Status":"running","Running":true}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"No such container: ctr-1"}`))
		}
	}))
	t.Cleanup(server.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(server.URL, "http://")), client.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	runtime := testDockerRuntime(&DockerSandboxRuntime{client: cli, projectID: "proj_a", poolID: "pool_a"})

	done := make(chan error, 1)
	go func() { done <- runtime.waitForSandboxAgent(context.Background(), "sbx_1") }()
	select {
	case err := <-done:
		t.Fatalf("waitForSandboxAgent returned %v while the container was still running", err)
	case <-time.After(200 * time.Millisecond):
	}
	removed.Store(true)
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("waitForSandboxAgent = %v, want ErrNotFound for a container removed under it", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitForSandboxAgent kept waiting on a container that is gone")
	}
}

// Nothing else about a sandbox moves while its agent comes up, and both the
// attach wait's stall budget and a client's freshness for a recorded phase give
// up on one that goes unrestated. The boot restates it for as long as it runs,
// and nothing once it has ended: what is reported next must not be
// overwritten by a late restatement.
func TestABootRestatesItsPhaseUntilItEnds(t *testing.T) {
	interval := agentWaitRestateInterval
	agentWaitRestateInterval = 20 * time.Millisecond
	t.Cleanup(func() { agentWaitRestateInterval = interval })

	exited := &atomic.Bool{}
	runtime := sandboxDaemon(t, "sbx_1", func() bool { return !exited.Load() })
	observed, stopWatching := watchProgress(t, runtime)
	defer stopWatching()

	boot := runtime.beginBoot("sbx_1")
	go func() { _ = runtime.finishBoot(context.Background(), "sbx_1", boot) }()
	for i := range 3 {
		select {
		case got := <-observed:
			if got.SandboxID != "sbx_1" || got.Phase != PhaseWaitingForAgent {
				t.Fatalf("report %d = %+v, want sbx_1 waiting for its agent", i, got)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("report %d of the wait for the agent never came", i)
		}
	}
	exited.Store(true)
	select {
	case <-boot.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the boot did not end when its container exited")
	}
	// Drain what was reported before the boot ended; nothing may follow.
	for drained := false; !drained; {
		select {
		case <-observed:
		default:
			drained = true
		}
	}
	select {
	case got := <-observed:
		t.Fatalf("restated after the boot ended: %+v", got)
	case <-time.After(10 * agentWaitRestateInterval):
	}
}

// One pass of the wait can block for most of a minute: a starved agent that has
// bound its port answers /healthz as slowly as it does everything else, and
// takes its runtime-config document as slowly after that. The phase is
// restated beside the boot, so a step blocked that long does not leave it to
// age out.
func TestABootRestatesThroughAPassThatBlocks(t *testing.T) {
	interval := agentWaitRestateInterval
	agentWaitRestateInterval = 20 * time.Millisecond
	t.Cleanup(func() { agentWaitRestateInterval = interval })

	exited := &atomic.Bool{}
	release := make(chan struct{})
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	// Every inspect blocks until the test lets it go: one pass, held.
	runtime := sandboxDaemon(t, "sbx_1", func() bool { <-release; return !exited.Load() })
	// Registered after the daemon, so it runs before the daemon's Close, which
	// would otherwise wait forever on the held inspect of a test that failed.
	t.Cleanup(func() { exited.Store(true); letGo() })
	observed, stopWatching := watchProgress(t, runtime)
	defer stopWatching()

	boot := runtime.beginBoot("sbx_1")
	go func() { _ = runtime.finishBoot(context.Background(), "sbx_1", boot) }()
	for i := range 3 {
		select {
		case got := <-observed:
			if got.Phase != PhaseWaitingForAgent {
				t.Fatalf("report %d = %+v, want waiting for its agent", i, got)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("report %d never came while a pass was blocked", i)
		}
	}
	exited.Store(true)
	letGo()
	select {
	case <-boot.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the boot did not end when its container exited")
	}
}

// A restatement is a report to the control plane, over a client with no
// timeout of its own. One the control plane never answers is given up on, so
// the next is still made and the phase keeps being restated, and stopping the
// restatements does not wait on it.
func TestARestatementTheControlPlaneNeverAnswersIsGivenUpOn(t *testing.T) {
	interval := agentWaitRestateInterval
	agentWaitRestateInterval = 20 * time.Millisecond
	t.Cleanup(func() { agentWaitRestateInterval = interval })

	runtime := testDockerRuntime(&DockerSandboxRuntime{})
	ctx, stopSink := context.WithCancel(t.Context())
	defer stopSink()
	reports := make(chan struct{}, 8)
	go runtime.WatchSandboxProgress(ctx, func(reportCtx context.Context, _ SandboxProgressObservation) error {
		reports <- struct{}{}
		<-reportCtx.Done()
		return reportCtx.Err()
	})
	for {
		if publish, _ := runtime.progressPublisher.Load().(func(context.Context, SandboxProgressObservation) error); publish != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}

	stop := runtime.restateWaitingForAgent(context.Background(), "sbx_1")
	for i := range 2 {
		select {
		case <-reports:
		case <-time.After(agentWaitRestateTimeout + 5*time.Second):
			t.Fatalf("restatement %d never came after one the control plane did not answer", i)
		}
	}
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("stopping the restatements waited on a report the control plane never answered")
	}
}
