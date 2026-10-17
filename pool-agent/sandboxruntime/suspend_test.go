package sandboxruntime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"
)

// pausingDaemon is a Docker daemon holding one sandbox container, which it
// runs, pauses and unpauses as asked, and records every power call it takes.
type pausingDaemon struct {
	mu      sync.Mutex
	status  string // "running", "paused" or "exited"
	actions []string
}

func (d *pausingDaemon) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Api-Version", "1.45")
	d.mu.Lock()
	defer d.mu.Unlock()
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/_ping"):
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/containers/json"):
		_, _ = w.Write([]byte(`[{"Id":"ctr_1","Labels":{}}]`))
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/containers/ctr_1/json"):
		running, paused := d.status != "exited", d.status == "paused"
		_, _ = w.Write([]byte(`{"Id":"ctr_1","Image":"sha256:img","State":{"Status":"` + d.status + `","Running":` + boolJSON(running) + `,"Paused":` + boolJSON(paused) + `},"Config":{"Labels":{"` + sandboxLabelSandbox + `":"sbx_1"}}}`))
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/containers/ctr_1/pause"):
		d.actions = append(d.actions, "pause")
		d.status = "paused"
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/containers/ctr_1/unpause"):
		d.actions = append(d.actions, "unpause")
		d.status = "running"
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && (strings.HasSuffix(path, "/containers/ctr_1/start") || strings.HasSuffix(path, "/containers/ctr_1/stop")):
		d.actions = append(d.actions, path[strings.LastIndex(path, "/")+1:])
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (d *pausingDaemon) took() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.actions...)
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func newPausingRuntime(t *testing.T, status string) (*DockerSandboxRuntime, *pausingDaemon) {
	t.Helper()
	state := withTestRoot(t)
	daemon := &pausingDaemon{status: status}
	server := httptest.NewServer(http.HandlerFunc(daemon.serve))
	t.Cleanup(server.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(server.URL, "http://")), client.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return &DockerSandboxRuntime{root: state, client: cli, projectID: "proj_1", poolID: "pool_1"}, daemon
}

// A suspend pauses a running container and nothing else; one already paused
// is left as it is, and one that is not running is refused with nothing done
// (ADR 26-10-16-420 §3).
func TestSuspendPausesOnlyARunningSandbox(t *testing.T) {
	for _, tc := range []struct {
		status  string
		wantErr error
		want    []string
	}{
		{status: "running", want: []string{"pause"}},
		{status: "paused"},
		{status: "exited", wantErr: ErrNotRunning},
	} {
		t.Run(tc.status, func(t *testing.T) {
			r, daemon := newPausingRuntime(t, tc.status)
			err := r.SuspendSandbox(context.Background(), "sbx_1")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("SuspendSandbox = %v, want %v", err, tc.wantErr)
			}
			if got := daemon.took(); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("daemon took %v, want %v", got, tc.want)
			}
		})
	}
}

// A paused container reads as suspended, though Docker calls it running:
// nothing in it answers, and everything that reaches into a running sandbox
// would hang on it.
func TestAPausedContainerIsSuspended(t *testing.T) {
	r, _ := newPausingRuntime(t, "paused")
	sb, err := r.GetSandbox(context.Background(), "sbx_1")
	if err != nil {
		t.Fatal(err)
	}
	if sb.Status != StatusSuspended {
		t.Fatalf("status = %q, want %q", sb.Status, StatusSuspended)
	}
}

// Every start thaws a suspended sandbox rather than booting it: an explicit
// start and the start a use makes alike (ADR 26-10-16-420 §4).
func TestAStartThawsASuspendedSandbox(t *testing.T) {
	for name, start := range map[string]func(*DockerSandboxRuntime) error{
		"start": func(r *DockerSandboxRuntime) error { return r.StartSandbox(context.Background(), "sbx_1", nil) },
		"use":   func(r *DockerSandboxRuntime) error { return r.EnsureSandboxRunning(context.Background(), "sbx_1") },
	} {
		t.Run(name, func(t *testing.T) {
			r, daemon := newPausingRuntime(t, "paused")
			if err := start(r); err != nil {
				t.Fatal(err)
			}
			if got := daemon.took(); strings.Join(got, ",") != "unpause" {
				t.Fatalf("daemon took %v, want only an unpause", got)
			}
		})
	}
}

func TestPauseAndUnpauseAreReported(t *testing.T) {
	for action, want := range map[events.Action]string{
		events.ActionPause:   StateSuspended,
		events.ActionUnPause: StateRunning,
	} {
		got, ok := observationFromEvent(events.Message{Action: action, Actor: events.Actor{Attributes: map[string]string{sandboxLabelSandbox: "sbx_1"}}})
		if !ok || got.State != want {
			t.Errorf("%s reported as %q (%v), want %q", action, got.State, ok, want)
		}
	}
	if got := stateFromStatus(StatusSuspended); got != StateSuspended {
		t.Errorf("a suspended sandbox syncs as %q, want %q", got, StateSuspended)
	}
}
