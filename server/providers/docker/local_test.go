package docker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// A pool container that is not running, or whose agent has not answered its
// healthcheck yet, is a host on its way up: the driver must say so rather than
// hand out a lease to nothing, or report the port it cannot publish yet as a
// missing harness URL.
func TestLocalDriverPoolAgentClientReportsContainerState(t *testing.T) {
	port, ok := network.PortFrom(defaultAgentPort, network.TCP)
	if !ok {
		t.Fatal("invalid agent port")
	}
	published := network.PortMap{port: []network.PortBinding{{HostPort: "43210"}}}
	tests := []struct {
		name    string
		state   *container.State
		ports   network.PortMap
		wantErr string
		wantURL string
	}{
		{name: "created", state: &container.State{Status: container.StateCreated}, wantErr: `pool "pool-1": container abc is created`},
		{name: "exited", state: &container.State{Status: container.StateExited}, wantErr: `pool "pool-1": container abc is exited`},
		{name: "agent starting", state: &container.State{Status: container.StateRunning, Running: true, Health: &container.Health{Status: container.Starting}}, ports: published, wantErr: `pool "pool-1": container abc health check is starting`},
		{name: "unhealthy still answers", state: &container.State{Status: container.StateRunning, Running: true, Health: &container.Health{Status: container.Unhealthy}}, ports: published, wantURL: "http://127.0.0.1:43210"},
		{name: "running without a published port", state: &container.State{Status: container.StateRunning, Running: true}, wantErr: `pool "pool-1" does not expose a harness URL`},
		{name: "healthy", state: &container.State{Status: container.StateRunning, Running: true, Health: &container.Health{Status: container.Healthy}}, ports: published, wantURL: "http://127.0.0.1:43210"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			driver := fakeDaemonDriver(t, container.InspectResponse{
				ID:              "abc",
				State:           tt.state,
				NetworkSettings: &container.NetworkSettings{Ports: tt.ports},
			})
			lease, err := driver.AcquirePoolAgentClient(t.Context(), "pool-1")
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("AcquirePoolAgentClient error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("AcquirePoolAgentClient: %v", err)
			}
			defer lease.Release()
			if lease.BaseURL != tt.wantURL {
				t.Fatalf("lease base URL = %q, want %q", lease.BaseURL, tt.wantURL)
			}
		})
	}
}

// fakeDaemonDriver is a local driver whose daemon answers every container
// inspect with inspect.
func fakeDaemonDriver(t *testing.T, inspect container.InspectResponse) *LocalDriver {
	t.Helper()
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/json") || !strings.Contains(r.URL.Path, "/containers/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(inspect)
	}))
	t.Cleanup(daemon.Close)
	cli, err := client.New(client.WithHost("tcp://" + strings.TrimPrefix(daemon.URL, "http://")))
	if err != nil {
		t.Fatalf("new docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return &LocalDriver{client: cli, agentPort: defaultAgentPort}
}
