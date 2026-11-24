package sandboxruntime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// slowExitDocker is a Docker daemon that answers a stop the way dockerd does
// when a killed container's exit event is late: it fails the stop, after
// stopDelay, and the container exits exitAfter later. A negative exitAfter
// never exits. It counts the waits it is asked for.
func slowExitDocker(t *testing.T, stopDelay, exitAfter time.Duration) (*DockerSandboxRuntime, *atomic.Int32) {
	t.Helper()
	var waits atomic.Int32
	var stopped atomic.Bool
	var exitAt atomic.Int64
	exited := func() bool {
		return stopped.Load() && exitAfter >= 0 && time.Now().UnixNano() >= exitAt.Load()
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Api-Version", "1.45")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			_, _ = w.Write([]byte(`[{"Id":"ctr-1"}]`))
		case strings.HasSuffix(r.URL.Path, "/containers/ctr-1/stop"):
			time.Sleep(stopDelay)
			stopped.Store(true)
			exitAt.Store(time.Now().Add(exitAfter).UnixNano())
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"cannot stop container: ctr-1: tried to kill container, but did not receive an exit event"}`))
		case strings.HasSuffix(r.URL.Path, "/containers/ctr-1/wait"):
			waits.Add(1)
			for !exited() {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
			_, _ = w.Write([]byte(`{"StatusCode":137}`))
		case strings.HasSuffix(r.URL.Path, "/containers/ctr-1/json"):
			state := `{"Status":"running","Running":true}`
			if exited() {
				state = `{"Status":"exited","Running":false}`
			}
			_, _ = w.Write([]byte(`{"Id":"ctr-1","Config":{"Labels":{"` + sandboxLabelSandbox + `":"sbx_1"}},"State":` + state + `}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(server.URL, "http://")), client.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return testDockerRuntime(&DockerSandboxRuntime{client: cli, root: withTestRoot(t), projectID: "proj_a", poolID: "pool_a"}), &waits
}

// recordStates records every state the runtime publishes, in order.
func recordStates(r *DockerSandboxRuntime) func() []string {
	var mu sync.Mutex
	var states []string
	r.statePublisher.Store(func(_ context.Context, batch SandboxStateBatch) error {
		mu.Lock()
		defer mu.Unlock()
		for _, state := range batch.States {
			states = append(states, state.State)
		}
		return nil
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), states...)
	}
}

// A sandbox running nested containers (docker compose inside the box) can take
// longer to exit after SIGKILL than Docker waits for its exit event, and Docker
// then fails the stop with "tried to kill container, but did not receive an
// exit event" although the container is on its way out and exits moments
// later. A stop must wait that exit out, not fail and leave the sandbox
// reading as stopping.
func TestStopWaitsOutAContainerSlowToExitAfterItsKill(t *testing.T) {
	runtime, waits := slowExitDocker(t, 0, 300*time.Millisecond)
	states := recordStates(runtime)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runtime.StopSandbox(ctx, "sbx_1", nil); err != nil {
		t.Fatalf("StopSandbox = %v, want nil once the killed container exits", err)
	}
	if got := waits.Load(); got != 1 {
		t.Fatalf("waits = %d, want the stop to wait for the container's exit once", got)
	}
	if got := states(); len(got) != 1 || got[0] != StateStopping {
		t.Fatalf("published states = %v, want only %q", got, StateStopping)
	}
}

// A caller that goes away mid-stop leaves the stop to Docker. Reading the
// container then, in its grace period, would report it running over the
// stopping just published.
func TestStopAbandonedByItsCallerReportsNothingMore(t *testing.T) {
	runtime, waits := slowExitDocker(t, 300*time.Millisecond, -1)
	states := recordStates(runtime)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := runtime.StopSandbox(ctx, "sbx_1", nil); err == nil {
		t.Fatal("StopSandbox = nil, want the abandoned stop's error")
	}
	if got := waits.Load(); got != 0 {
		t.Fatalf("waits = %d, want none for a caller that went away", got)
	}
	if got := states(); len(got) != 1 || got[0] != StateStopping {
		t.Fatalf("published states = %v, want only %q", got, StateStopping)
	}
}

// A container that does not exit in the wait fails the stop with a reason, and
// the sandbox is reported as Docker reports it, not left reading as stopping.
func TestStopOfAContainerThatNeverExitsReportsItRunning(t *testing.T) {
	runtime, _ := slowExitDocker(t, 0, -1)
	states := recordStates(runtime)
	wait := sandboxStopExitWait
	sandboxStopExitWait = 300 * time.Millisecond
	t.Cleanup(func() { sandboxStopExitWait = wait })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := runtime.StopSandbox(ctx, "sbx_1", nil)
	if err == nil || !strings.Contains(err.Error(), "did not exit after it was stopped") {
		t.Fatalf("StopSandbox = %v, want a stop that says the container did not exit", err)
	}
	got := states()
	if len(got) == 0 || got[len(got)-1] != StateRunning {
		t.Fatalf("published states = %v, want the last to be %q", got, StateRunning)
	}
}
