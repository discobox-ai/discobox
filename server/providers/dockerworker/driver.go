// Package dockerworker implements the shared worker runtime engine. Every
// worker backend ends the same way: run the pool-agent container in some
// Docker daemon. The engine owns that container management uniformly; a
// Driver owns only VM lifecycle and how to reach the VM's Docker daemon and
// pool-agent API.
package dockerworker

import (
	"context"
	"net"
	"net/http"
	"sync"

	"github.com/moby/moby/client"

	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/transport"
)

// Driver is the VM abstraction beneath the docker worker engine.
//
// Implementations target local Docker (the host is the "VM"), cloud VMs such
// as DigitalOcean or EC2, or local hypervisors. Adding a backend means
// implementing VM lifecycle plus the two connection methods; the engine
// handles everything Docker.
type Driver interface {
	Close() error

	// EnsureVM idempotently creates and starts the VM for a worker. The local
	// driver is a no-op that resolves every worker to the host. Instance
	// sizing, region, and image come from driver configuration, not the spec.
	EnsureVM(ctx context.Context, poolID string, spec VMSpec) (*VMInfo, error)
	// StopVM stops the worker's VM while preserving any driver-owned persistent
	// state needed by a later EnsureVM. Drivers without separately attached
	// state may implement this by deleting the replaceable VM instance.
	StopVM(ctx context.Context, poolID string) error
	// DeleteVM removes the worker's VM and its local resources. It must
	// succeed when the VM is already gone.
	DeleteVM(ctx context.Context, poolID string) error
	// InspectVM reports the worker's VM state. It returns sandbox.ErrNotFound
	// when no VM exists for the worker.
	InspectVM(ctx context.Context, poolID string) (*VMInfo, error)

	// AcquireDockerClient returns a Docker API client for the daemon that
	// hosts this worker's containers: the host daemon for the local driver, or
	// the in-VM daemon (for example dialed over SSH or vsock) for VM drivers.
	AcquireDockerClient(ctx context.Context, poolID string) (*DockerClientLease, error)
	// AcquirePoolAgentClient returns an HTTP client lease that reaches the
	// pool-agent API inside the worker container.
	AcquirePoolAgentClient(ctx context.Context, poolID string) (*transport.HTTPClientLease, error)
}

// VMSpec is the driver-neutral VM launch request for one worker.
type VMSpec struct {
	// Name is the suggested instance name.
	Name string
	// Metadata carries labels/tags, including the worker identity labels.
	Metadata map[string]string
}

// VMInfo is the driver-neutral VM runtime state.
type VMInfo struct {
	// ID identifies the instance at the backend (droplet ID, EC2 instance ID).
	// The local driver reports a static host identity.
	ID string
	// Status uses the shared runtime status vocabulary; the engine treats
	// anything other than StatusRunning as an unhealthy VM.
	Status sandbox.Status
	// Address is the host that reaches the VM's published ports, when the
	// backend has one.
	Address string
}

// DockerClientLease holds a Docker API client until Release is called.
type DockerClientLease struct {
	Client  *client.Client
	release func()
	once    sync.Once
}

// NewDockerClientLease creates a lease around a Docker client and release callback.
func NewDockerClientLease(cli *client.Client, release func()) *DockerClientLease {
	return &DockerClientLease{Client: cli, release: release}
}

// Release returns the leased client and tears down its transport resources.
func (l *DockerClientLease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.release != nil {
			l.release()
		}
	})
}

// NewDockerClientForDialer builds a Docker API client whose connections use
// the given dialer, for drivers that reach the in-VM daemon through a tunnel
// such as SSH or vsock. The logical host stays the in-VM Unix socket.
func NewDockerClientForDialer(dial func(ctx context.Context, network, addr string) (net.Conn, error)) (*client.Client, error) {
	// Deliberately no WithHTTPClient: the client must build its own base
	// transport so DialHijack — used to reach the daemon's embedded BuildKit at
	// /grpc (development image build-mode) and to attach/exec — dials through
	// this dialer. WithDialContext injects it. Passing a pre-built http.Client
	// leaves the client's baseTransport unset, and DialHijack then falls back to
	// net.Dial("unix", "/var/run/docker.sock"), which cannot reach a VM daemon.
	return client.New(
		client.WithHost("unix:///var/run/docker.sock"),
		client.WithDialContext(dial),
	)
}

// NewDockerClientForHost builds a Docker API client for a directly reachable
// daemon endpoint such as unix:///path or tcp://host:port.
func NewDockerClientForHost(host string) (*client.Client, error) {
	return client.New(
		client.WithHTTPClient(&http.Client{Transport: &http.Transport{}}),
		client.WithHost(host),
	)
}
