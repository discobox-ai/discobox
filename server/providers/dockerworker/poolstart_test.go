package dockerworker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
)

// stoppedPoolDaemon is a Docker daemon holding one pool container, stopped,
// that answers the requests starting it, and rebuilding it, make. What the
// container does once started is the test's to say.
type stoppedPoolDaemon struct {
	t *testing.T

	mu        sync.Mutex
	inspect   container.InspectResponse
	requests  []string
	refuse    bool // refuse the start
	onStarted func(*container.State)
	// agentStates is what the agent's unit reads in a started container, one
	// per `systemctl is-active` asked; the last is kept.
	agentStates []string
	lastExit    int
	// wedged makes an exec's start accept the connection and never answer,
	// as a guest wedged in its kernel does.
	wedged chan struct{}
}

func (d *stoppedPoolDaemon) serve() *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		path := stripDockerAPIVersion(r.URL.Path)
		d.requests = append(d.requests, r.Method+" "+path)
		switch {
		case path == "/info":
			writeDockerJSON(w, map[string]string{"OSType": "linux", "Architecture": "x86_64"})
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
			writeDockerJSON(w, d.inspect)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/exec"):
			if d.inspect.State == nil || !d.inspect.State.Running {
				http.Error(w, `{"message":"container is not running"}`, http.StatusConflict)
				return
			}
			writeDockerJSON(w, map[string]string{"Id": "exec-1"})
		case r.Method == http.MethodPost && path == "/exec/exec-1/start" && d.wedged != nil:
			d.mu.Unlock()
			<-d.wedged
			d.mu.Lock()
		case r.Method == http.MethodPost && path == "/exec/exec-1/start":
			state := d.agentStates[0]
			if len(d.agentStates) > 1 {
				d.agentStates = d.agentStates[1:]
			}
			d.lastExit = 3
			if state == "active" {
				d.lastExit = 0
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && path == "/exec/exec-1/json":
			writeDockerJSON(w, map[string]any{"Running": false, "ExitCode": d.lastExit})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/start"):
			if d.refuse {
				http.Error(w, `{"message":"cannot start container: network not found"}`, http.StatusInternalServerError)
				return
			}
			d.inspect.State = &container.State{Status: container.StateRunning, Running: true}
			if d.onStarted != nil {
				d.onStarted(d.inspect.State)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "/containers/"):
			w.WriteHeader(http.StatusNoContent)
		default:
			// A rebuild goes on to make a network and a container; refusing
			// there ends the pass once the rebuild has begun.
			http.Error(w, `{"message":"refused by the test daemon"}`, http.StatusInternalServerError)
		}
	}))
	d.t.Cleanup(server.Close)
	return server
}

func (d *stoppedPoolDaemon) made(request string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range d.requests {
		if strings.HasPrefix(r, request) {
			return true
		}
	}
	return false
}

type stoppedPoolFixture struct {
	engine   *Engine
	daemon   *stoppedPoolDaemon
	lease    *DockerClientLease
	provider *model.SandboxProviderInstance
	pool     *model.Pool
	maker    *fakeMaker
	recorded poolruntime.BoxRecord
}

// newStoppedPoolFixture is a pool whose container is stopped and has not
// drifted: the container its runtime state names, on the engine's image and
// labels, with a box recorded as provisioned in it whose placement nothing
// has moved since.
func newStoppedPoolFixture(t *testing.T) *stoppedPoolFixture {
	t.Helper()
	engine := newTestEngine(t, Config{})
	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1"}
	pool := &model.Pool{ID: "pool-1", ProjectID: "project-1"}
	pool.SetState(model.PoolStateActive)
	pool.Ready, pool.Schedulable = true, true
	maker := &fakeMaker{box: sandbox.PoolBox{Bundles: []sandbox.BundlePin{{Name: "discobox-pool-agent-bundle", Digest: "sha256:b"}}}}
	bootstrap, err := engine.poolBootstrap(context.Background(), pool, maker)
	if err != nil {
		t.Fatal(err)
	}
	recorded := poolruntime.BoxRecord{Agent: "discobox-agent@sha256:a", Bundles: []string{"discobox-pool-agent-bundle@sha256:b"}}
	recorded.Placement, err = poolruntime.PlacementFingerprint(poolruntime.BoxPlacement{Bootstrap: bootstrap, Box: maker.box, Env: engine.poolPolicyEnv()})
	if err != nil {
		t.Fatal(err)
	}
	pool.RuntimeState, err = encodeRuntimeState(RuntimeState{ContainerID: "container-1", Box: recorded})
	if err != nil {
		t.Fatal(err)
	}
	daemon := &stoppedPoolDaemon{t: t, inspect: container.InspectResponse{
		ID: "container-1",
		Config: &container.Config{
			Image:       engine.Image(),
			Labels:      engine.containerLabels(provider, pool),
			Healthcheck: &container.HealthConfig{Test: []string{"CMD-SHELL", "curl -fsS http://127.0.0.1:8080/healthz"}},
		},
		State: &container.State{Status: container.StateExited, ExitCode: 130},
	}}
	daemon.onStarted = func(state *container.State) { state.Health = &container.Health{Status: container.Healthy} }
	cli, err := testDockerClient(daemon.serve().URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return &stoppedPoolFixture{engine: engine, daemon: daemon, lease: &DockerClientLease{Client: cli}, provider: provider, pool: pool, maker: maker, recorded: recorded}
}

// A plain restart of a VM pool's server leaves its container stopped. It is
// started on its own disk, not rebuilt (ADR 26-11-17-503 §§1, 2): placement
// closes, the box's start is recorded before Docker starts it, nothing is
// provisioned or enrolled, and the pool waits for its agent's report.
func TestEnsurePoolContainerStartsAStoppedContainerInPlace(t *testing.T) {
	for _, status := range []container.ContainerState{container.StateExited, container.StateCreated} {
		t.Run(string(status), func(t *testing.T) {
			f := newStoppedPoolFixture(t)
			f.daemon.inspect.State.Status = status
			var began bool
			var starts []sandbox.AgentStartKind
			ctx := sandbox.WithAgentStartReporter(t.Context(), func(_ time.Time, kind sandbox.AgentStartKind) {
				if !began {
					t.Errorf("the box's start was recorded before placement closed")
				}
				if f.daemon.made("POST /containers/container-1/start") {
					t.Errorf("the box's start was recorded after Docker started it")
				}
				starts = append(starts, kind)
			})
			begin := func(context.Context) error { began = true; return nil }

			inst, record, recreated, err := f.engine.ensurePoolContainer(ctx, f.lease, f.provider, f.pool, f.maker, false, begin, poolruntime.NewHostSteps(f.pool.ID))
			if err != nil {
				t.Fatal(err)
			}
			if inst.ID != "container-1" || !recreated || record.Placement != f.recorded.Placement || record.Agent != f.recorded.Agent {
				t.Fatalf("started: container %s, recreated %v, record %+v; want the same container, started, its record kept", inst.ID, recreated, record)
			}
			if len(starts) != 1 || starts[0] != sandbox.AgentStartBox {
				t.Fatalf("agent starts %v; want one box start", starts)
			}
			if f.daemon.made("DELETE ") || f.daemon.made("POST /containers/create") || f.daemon.made("PUT /containers/container-1/archive") || f.daemon.made("POST /containers/container-1/exec") || f.maker.enrolled {
				t.Fatalf("a start rebuilt or provisioned the box: requests %v, enrolled %v", f.daemon.requests, f.maker.enrolled)
			}
			if f.pool.Ready || f.pool.Schedulable || f.pool.State != model.PoolStateRegistering {
				t.Fatalf("pool after the start: ready %v, schedulable %v, state %s; want it waiting on its agent's report", f.pool.Ready, f.pool.Schedulable, f.pool.State)
			}
		})
	}
}

// A stopped container is rebuilt, as before, when a start cannot be trusted
// to boot a complete box, or when the start fails (ADR 26-11-17-503 §§1, 4).
func TestEnsurePoolContainerRebuildsAStoppedContainerItCannotStart(t *testing.T) {
	for name, tc := range map[string]struct {
		arrange func(*stoppedPoolFixture)
		started bool // whether the start was tried before the rebuild
	}{
		"not the container the pool names": {arrange: func(f *stoppedPoolFixture) {
			f.pool.RuntimeState, _ = encodeRuntimeState(RuntimeState{ContainerID: "container-0", Box: f.recorded})
		}},
		"no box recorded": {arrange: func(f *stoppedPoolFixture) {
			f.pool.RuntimeState, _ = encodeRuntimeState(RuntimeState{ContainerID: "container-1"})
		}},
		"dead": {arrange: func(f *stoppedPoolFixture) {
			f.daemon.inspect.State.Status = container.StateDead
		}},
		"start refused": {started: true, arrange: func(f *stoppedPoolFixture) {
			f.daemon.refuse = true
		}},
		"exits on start": {started: true, arrange: func(f *stoppedPoolFixture) {
			f.daemon.onStarted = func(state *container.State) {
				*state = container.State{Status: container.StateExited, ExitCode: 255}
			}
		}},
		"unhealthy": {started: true, arrange: func(f *stoppedPoolFixture) {
			f.daemon.onStarted = func(state *container.State) {
				state.Health = &container.Health{Status: container.Unhealthy}
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newStoppedPoolFixture(t)
			tc.arrange(f)
			_, _, _, err := f.engine.ensurePoolContainer(t.Context(), f.lease, f.provider, f.pool, f.maker, false, func(context.Context) error { return nil }, poolruntime.NewHostSteps(f.pool.ID))
			if err == nil || !strings.Contains(err.Error(), "refused by the test daemon") {
				t.Fatalf("err %v; want the rebuild's", err)
			}
			if !f.daemon.made("DELETE /containers/container-1") {
				t.Fatalf("requests %v; want the container removed for a rebuild", f.daemon.requests)
			}
			if got := f.daemon.made("POST /containers/container-1/start"); got != tc.started {
				t.Fatalf("start tried %v; want %v", got, tc.started)
			}
		})
	}
}

// A repair rebuilds a stopped container that would otherwise start.
func TestEnsurePoolContainerRepairRebuildsAStoppedContainer(t *testing.T) {
	f := newStoppedPoolFixture(t)
	_, _, _, err := f.engine.ensurePoolContainer(t.Context(), f.lease, f.provider, f.pool, f.maker, true, func(context.Context) error { return nil }, poolruntime.NewHostSteps(f.pool.ID))
	if err == nil || f.daemon.made("POST /containers/container-1/start") || !f.daemon.made("DELETE /containers/container-1") {
		t.Fatalf("repair of a stopped container: err %v, requests %v; want it rebuilt, never started", err, f.daemon.requests)
	}
}

// Newer intent, through begin, stops a start before anything is started.
func TestEnsurePoolContainerStartStopsOnNewerIntent(t *testing.T) {
	f := newStoppedPoolFixture(t)
	superseded := errors.New("pool generation changed")
	_, _, _, err := f.engine.ensurePoolContainer(t.Context(), f.lease, f.provider, f.pool, f.maker, false, func(context.Context) error { return superseded }, poolruntime.NewHostSteps(f.pool.ID))
	if !errors.Is(err, superseded) || f.daemon.made("POST /containers/container-1/start") || f.daemon.made("DELETE ") {
		t.Fatalf("err %v, requests %v; want begin's error and nothing started or removed", err, f.daemon.requests)
	}
}

// A pool whose agent listens on no IP (VSOCK on libkrun and vz) has no
// healthcheck, and a running container says only that systemd is up. Its
// start waits for the agent's unit to be active instead, and a container that
// stops before it is rebuilt (ADR 26-11-17-503 §§2, 4).
func TestEnsurePoolContainerStartWithoutAHealthcheckWaitsForTheAgentUnit(t *testing.T) {
	t.Run("active", func(t *testing.T) {
		f := newStoppedPoolFixture(t)
		f.daemon.inspect.Config.Healthcheck = &container.HealthConfig{Test: []string{"NONE"}}
		f.daemon.onStarted = nil
		f.daemon.agentStates = []string{"activating", "activating", "active"}
		inst, _, started, err := f.engine.ensurePoolContainer(t.Context(), f.lease, f.provider, f.pool, f.maker, false, func(context.Context) error { return nil }, poolruntime.NewHostSteps(f.pool.ID))
		if err != nil || inst.ID != "container-1" || !started {
			t.Fatalf("container %v, started %v, err %v; want it started in place", inst, started, err)
		}
		if len(f.daemon.agentStates) != 1 || f.daemon.made("DELETE ") {
			t.Fatalf("agent states left %v, requests %v; want the start to wait until the unit was active, and nothing rebuilt", f.daemon.agentStates, f.daemon.requests)
		}
	})
	t.Run("stops", func(t *testing.T) {
		f := newStoppedPoolFixture(t)
		f.daemon.inspect.Config.Healthcheck = &container.HealthConfig{Test: []string{"NONE"}}
		f.daemon.agentStates = []string{"activating"}
		f.daemon.onStarted = func(state *container.State) {
			*state = container.State{Status: container.StateExited, ExitCode: 255}
		}
		_, _, _, err := f.engine.ensurePoolContainer(t.Context(), f.lease, f.provider, f.pool, f.maker, false, func(context.Context) error { return nil }, poolruntime.NewHostSteps(f.pool.ID))
		if err == nil || !f.daemon.made("POST /containers/container-1/start") || !f.daemon.made("DELETE /containers/container-1") {
			t.Fatalf("err %v, requests %v; want the start tried, then the container rebuilt", err, f.daemon.requests)
		}
	})
}

// A guest whose Docker accepts an exec and never answers it does not hold the
// start's wait past its bound: every attempt is bounded, not only the pauses
// between them, since the pass's own context never ends (#1101).
func TestWaitPoolAgentActiveBoundsEveryAttempt(t *testing.T) {
	f := newStoppedPoolFixture(t)
	f.daemon.inspect.State = &container.State{Status: container.StateRunning, Running: true}
	f.daemon.wedged = make(chan struct{})
	t.Cleanup(func() { close(f.daemon.wedged) })
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	began := time.Now()
	err := f.engine.waitPoolAgentActive(ctx, f.lease.Client, "container-1")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(began) > 5*time.Second {
		t.Fatalf("a wedged exec: err %v after %s; want the wait's deadline, promptly", err, time.Since(began))
	}
}
