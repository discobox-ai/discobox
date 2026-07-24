package docker

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/moby/moby/client"

	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/transport"
	"github.com/obot-platform/discobox/server/providers/dockerworker"
)

// localVMID is the static instance identity the local driver reports: every
// worker resolves to the host Docker daemon, so there is no per-worker VM.
const localVMID = "local"

// LocalDriver is the local Docker VM driver. VM CRUD is a no-op because all
// workers share the host daemon; connectivity resolves to the host socket and
// the worker container's published loopback port.
type LocalDriver struct {
	client    *client.Client
	agentPort int

	watcherMu     sync.Mutex
	watcherCancel context.CancelFunc
}

// NewLocalDriver creates a local Docker driver and verifies API connectivity.
func NewLocalDriver(ctx context.Context, host string, agentPort int) (*LocalDriver, error) {
	opts := []client.Opt{client.FromEnv}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}
	cli, err := client.New(opts...)
	if err != nil {
		return nil, err
	}
	if _, err := cli.Ping(ctx, client.PingOptions{}); err != nil {
		_ = cli.Close()
		return nil, err
	}
	return &LocalDriver{client: cli, agentPort: agentPort}, nil
}

// DaemonHost returns the resolved Docker daemon endpoint. It is the configured
// host when one was given, and otherwise whatever the environment selected, so
// callers reasoning about where containers actually run must consult this
// rather than the provider's configuration.
func (d *LocalDriver) DaemonHost() string {
	if d == nil || d.client == nil {
		return ""
	}
	return d.client.DaemonHost()
}

func (d *LocalDriver) Close() error {
	if d == nil {
		return nil
	}
	d.watcherMu.Lock()
	cancel := d.watcherCancel
	d.watcherCancel = nil
	d.watcherMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if d.client == nil {
		return nil
	}
	return d.client.Close()
}

func (d *LocalDriver) EnsureVM(context.Context, string, dockerworker.VMSpec) (*dockerworker.VMInfo, error) {
	return d.hostVM(), nil
}

func (d *LocalDriver) StopVM(context.Context, string) error {
	return nil
}

func (d *LocalDriver) DeleteVM(context.Context, string) error {
	return nil
}

func (d *LocalDriver) InspectVM(context.Context, string) (*dockerworker.VMInfo, error) {
	return d.hostVM(), nil
}

func (d *LocalDriver) hostVM() *dockerworker.VMInfo {
	return &dockerworker.VMInfo{ID: localVMID, Status: sandbox.StatusRunning, Address: "127.0.0.1"}
}

func (d *LocalDriver) AcquireDockerClient(context.Context, string) (*dockerworker.DockerClientLease, error) {
	// The shared client outlives leases; releasing a lease is a no-op.
	return dockerworker.NewDockerClientLease(d.client, nil), nil
}

func (d *LocalDriver) AcquirePoolAgentClient(ctx context.Context, poolID string) (*transport.HTTPClientLease, error) {
	if strings.TrimSpace(poolID) == "" {
		return nil, fmt.Errorf("worker ID is required")
	}
	inspect, err := d.client.ContainerInspect(ctx, dockerworker.ContainerName(poolID), client.ContainerInspectOptions{})
	if err != nil {
		return nil, mapDockerNotFound(err)
	}
	host, port := dockerworker.AssignedAgentEndpoint(inspect.Container.NetworkSettings.Ports, d.agentPort)
	if host == "" || port <= 0 {
		return nil, fmt.Errorf("pool %q does not expose a harness URL", poolID)
	}
	baseURL := "http://" + net.JoinHostPort(host, strconv.Itoa(port))
	return transport.NewHTTPClientLeaseWithBaseURL(http.DefaultClient, baseURL, nil), nil
}
