package wslc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/transport"
	"github.com/obot-platform/discobox/server/providers/dockerworker"
	"github.com/obot-platform/discobox/server/providers/wslc/internal/wslcsession"
)

const (
	guestDockerSocket = "/var/run/docker.sock"
	bootTimeout       = 2 * time.Minute
)

var validPoolID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// DriverConfig configures the wslc VM driver.
type DriverConfig struct {
	// StorageDir is the root under which each pool gets a persistent
	// /var/lib/docker VHD at <StorageDir>/<poolID>. Empty means ephemeral.
	StorageDir    string
	CPUCount      int
	MemoryMiB     int
	MaxStorageMiB int64
	AgentPort     int
	// RelayStagingDir holds the extracted guest relay binary, mounted into every
	// guest. It is shared across pools because the binary is identical.
	RelayStagingDir string
	// ControlPlaneStreams receives control-plane connections opened by guests.
	// When nil the relay still runs, but the agent cannot register.
	ControlPlaneStreams StreamSink
}

// Driver owns one wslc VM session per pool. Sessions are held for the lifetime
// of the discobox-server process and torn down on StopVM/DeleteVM or Close;
// they cannot be re-adopted across a server restart, which is intended on
// Windows (the VM is meant to die with the server).
type Driver struct {
	storageDir    string
	cpuCount      int
	memoryMiB     int
	maxStorageMiB int64
	agentPort     int

	relayStagingDir string
	streams         StreamSink

	mu       sync.Mutex
	sessions map[string]*wslcsession.Session
	relays   map[string]*relaySession
}

// NewDriver validates configuration and creates a wslc driver. It does not
// start any VM; EnsureVM does that on demand.
func NewDriver(cfg DriverConfig) (*Driver, error) {
	if cfg.CPUCount < 0 || cfg.MemoryMiB < 0 || cfg.MaxStorageMiB < 0 {
		return nil, fmt.Errorf("wslc VM sizing values must not be negative")
	}
	storageDir := strings.TrimSpace(cfg.StorageDir)
	if storageDir != "" {
		if !filepath.IsAbs(storageDir) {
			return nil, fmt.Errorf("wslc storage directory must be an absolute path")
		}
		storageDir = filepath.Clean(storageDir)
	}
	return &Driver{
		storageDir:      storageDir,
		cpuCount:        effectiveInt(cfg.CPUCount, defaultCPUCount),
		memoryMiB:       effectiveInt(cfg.MemoryMiB, defaultMemoryMiB),
		maxStorageMiB:   effectiveInt64(cfg.MaxStorageMiB, defaultMaxStgMiB),
		agentPort:       effectiveInt(cfg.AgentPort, defaultAgentPort),
		relayStagingDir: defaultRelayStagingDir(cfg.RelayStagingDir),
		streams:         cfg.ControlPlaneStreams,
		sessions:        map[string]*wslcsession.Session{},
		relays:          map[string]*relaySession{},
	}, nil
}

// Close tears down every live session. Unlike libkrun, wslc VMs cannot outlive
// the process, so leaving them running for re-adoption is not an option.
func (d *Driver) Close() error {
	d.mu.Lock()
	sessions := d.sessions
	relays := d.relays
	d.sessions = map[string]*wslcsession.Session{}
	d.relays = map[string]*relaySession{}
	d.mu.Unlock()
	// Relays first: each one owns a guest process reachable only through its
	// session, so tearing the VM down first would strand it.
	for _, r := range relays {
		r.close()
	}
	for _, s := range sessions {
		_ = s.Close()
	}
	return nil
}

func (d *Driver) EnsureVM(ctx context.Context, poolID string, _ dockerworker.VMSpec) (*dockerworker.VMInfo, error) {
	if err := validatePoolID(poolID); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.sessions[poolID]; ok {
		return runningVM(poolID), nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	opts := wslcsession.Options{
		DisplayName: "discobox-" + poolID,
		CPUCount:    uint32(d.cpuCount),
		MemoryMB:    uint32(d.memoryMiB),
		BootTimeout: bootTimeout,
	}
	if d.storageDir != "" {
		storagePath := filepath.Join(d.storageDir, poolID)
		if err := os.MkdirAll(storagePath, 0o700); err != nil {
			return nil, fmt.Errorf("create wslc storage dir for %s: %w", poolID, err)
		}
		opts.StoragePath = storagePath
		opts.MaxStorageSizeMB = uint64(d.maxStorageMiB)
	}

	session, err := wslcsession.NewSession(opts)
	if err != nil {
		return nil, fmt.Errorf("start wslc VM for %s: %w", poolID, err)
	}

	// The relay carries every control-plane byte, in both directions, so a VM
	// without one is useless: fail here rather than hand back a pool whose agent
	// can never register.
	relay, err := startRelay(ctx, session, poolID, d.relayStagingDir, d.streams)
	if err != nil {
		// The VM has to actually go here, not just be dropped: it holds the
		// name, and a VM left running under it makes every retry of this
		// function fail with ErrSessionExists for the life of the process,
		// with no handle left to close it by. Close ending the VM is what
		// makes the next attempt a fresh one rather than a wedge.
		if closeErr := session.Close(); closeErr != nil {
			return nil, fmt.Errorf("start control-plane relay for %s: %w (the VM could not be torn down either: %w)",
				poolID, err, closeErr)
		}
		return nil, fmt.Errorf("start control-plane relay for %s: %w", poolID, err)
	}

	d.sessions[poolID] = session
	d.relays[poolID] = relay
	return runningVM(poolID), nil
}

// StopVM tears the VM down but preserves its /var/lib/docker VHD under
// StorageDir so a later EnsureVM keeps the pool's images and volumes.
func (d *Driver) StopVM(_ context.Context, poolID string) error {
	if err := validatePoolID(poolID); err != nil {
		return err
	}
	d.mu.Lock()
	session := d.sessions[poolID]
	relay := d.relays[poolID]
	delete(d.sessions, poolID)
	delete(d.relays, poolID)
	d.mu.Unlock()
	if relay != nil {
		relay.close()
	}
	if session == nil {
		// Nothing here holds this pool's VM. Saying so is not pedantry: a bare
		// nil reads as "stopped it", and a repair that believes that goes on to
		// create a VM whose name may still be taken by one this process cannot
		// see. ErrNotFound is what RepairPool already tolerates, so the caller
		// keeps its behavior and stops being told something untrue.
		return fmt.Errorf("wslc VM for %s: %w", poolID, sandbox.ErrNotFound)
	}
	if err := session.Close(); err != nil {
		// The handle goes back: a session that would not close is the one thing
		// that must not be forgotten, since it is holding the pool's name and
		// this is the only reference to it left.
		d.mu.Lock()
		if _, taken := d.sessions[poolID]; !taken {
			d.sessions[poolID] = session
		}
		d.mu.Unlock()
		return err
	}
	return nil
}

// DeleteVM stops the VM and removes its persistent storage directory.
//
// A VM this process never held is not an obstacle to deleting the storage --
// the usual way to reach here is a pool being removed after a restart, when
// nothing is running for it at all. StopVM reports that as ErrNotFound, the
// same thing RepairPool passes over.
func (d *Driver) DeleteVM(ctx context.Context, poolID string) error {
	if err := d.StopVM(ctx, poolID); err != nil && !errors.Is(err, sandbox.ErrNotFound) {
		return err
	}
	if d.storageDir == "" {
		return nil
	}
	storagePath := filepath.Join(d.storageDir, poolID)
	if err := os.RemoveAll(storagePath); err != nil {
		return fmt.Errorf("remove wslc storage %s: %w", storagePath, err)
	}
	return nil
}

func (d *Driver) InspectVM(_ context.Context, poolID string) (*dockerworker.VMInfo, error) {
	if err := validatePoolID(poolID); err != nil {
		return nil, err
	}
	d.mu.Lock()
	_, ok := d.sessions[poolID]
	relay := d.relays[poolID]
	d.mu.Unlock()
	if !ok {
		return nil, sandbox.ErrNotFound
	}
	if !relay.healthy() {
		// Report the VM unhealthy rather than running: the engine replaces it,
		// which is the only way to get a working control-plane channel back.
		return &dockerworker.VMInfo{ID: "wslc-" + poolID, Status: sandbox.StatusStopped}, nil
	}
	return runningVM(poolID), nil
}

func (d *Driver) AcquireDockerClient(ctx context.Context, poolID string) (*dockerworker.DockerClientLease, error) {
	session, err := d.session(poolID)
	if err != nil {
		return nil, err
	}
	cli, err := dockerworker.NewDockerClientForDialer(func(_ context.Context, _, _ string) (net.Conn, error) {
		return session.DialGuestUnix(guestDockerSocket)
	})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = cli.Close()
		return nil, err
	}
	return dockerworker.NewDockerClientLease(cli, func() { _ = cli.Close() }), nil
}

func (d *Driver) AcquirePoolAgentClient(_ context.Context, poolID string) (*transport.HTTPClientLease, error) {
	relay, err := d.relay(poolID)
	if err != nil {
		return nil, err
	}
	// Control-plane traffic rides the multiplexed session rather than spawning a
	// guest process per connection. Docker traffic deliberately does not: it
	// carries image loads and build contexts, which would head-of-line block the
	// agent behind a single session.
	guestTarget := "tcp:" + net.JoinHostPort("127.0.0.1", strconv.Itoa(d.agentPort))
	httpTransport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return relay.dial(ctx, guestTarget)
		},
	}
	client := &http.Client{Transport: httpTransport}
	return transport.NewHTTPClientLeaseWithBaseURL(client, "http://pool.local", httpTransport.CloseIdleConnections), nil
}

func (d *Driver) session(poolID string) (*wslcsession.Session, error) {
	if err := validatePoolID(poolID); err != nil {
		return nil, err
	}
	d.mu.Lock()
	session := d.sessions[poolID]
	d.mu.Unlock()
	if session == nil {
		return nil, fmt.Errorf("wslc VM %s: %w", poolID, sandbox.ErrNotFound)
	}
	return session, nil
}

// defaultRelayStagingDir keeps a directly constructed driver usable: the relay
// is mandatory for every VM, so an unset path would fail every EnsureVM.
func defaultRelayStagingDir(configured string) string {
	if value := strings.TrimSpace(configured); value != "" {
		return value
	}
	return filepath.Join(os.TempDir(), "discobox-relay")
}

func (d *Driver) relay(poolID string) (*relaySession, error) {
	if err := validatePoolID(poolID); err != nil {
		return nil, err
	}
	d.mu.Lock()
	relay := d.relays[poolID]
	d.mu.Unlock()
	if !relay.healthy() {
		return nil, fmt.Errorf("wslc pool %s control-plane relay: %w", poolID, sandbox.ErrNotFound)
	}
	return relay, nil
}

func runningVM(poolID string) *dockerworker.VMInfo {
	return &dockerworker.VMInfo{ID: "wslc-" + poolID, Status: sandbox.StatusRunning}
}

func validatePoolID(poolID string) error {
	if !validPoolID.MatchString(poolID) || poolID == "." || poolID == ".." || strings.Contains(poolID, "..") {
		return fmt.Errorf("invalid pool ID %q", poolID)
	}
	return nil
}
