package vz

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/transport"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
	"github.com/discobox-ai/discobox/server/providers/guestimage"
	"github.com/discobox-ai/discobox/server/providers/vz/internal/vzvm"
)

const (
	// Guest VSOCK ports, shared with the libkrun guest so one guest-side helper
	// serves both backends.
	controlPlaneVSOCKPort = 3001
	agentVSOCKPort        = 3002
	lifecycleVSOCKPort    = 3003
	dockerVSOCKPort       = 3004

	// kernelCmdline mounts the shared read-only root; everything writable is on
	// the per-pool data and cache disks the guest mounts itself.
	kernelCmdline = "console=hvc0 root=/dev/vda ro rootfstype=ext4"

	gracefulStopTimeout = 30 * time.Second
	forcedStopTimeout   = 15 * time.Second

	// consoleLogName is where the guest's serial console is appended, in the
	// pool's state directory. It survives the VM, which is the point: the boot
	// worth reading is the one that did not finish.
	consoleLogName = "console.log"
)

var validPoolID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// StreamSink receives control-plane connections opened by a guest. The driver
// hands each one to the server, which serves the ordinary control-plane handler
// over it; see server/internal/transport/carrierhub.
type StreamSink interface {
	Push(conn net.Conn, cancel <-chan struct{}) error
}

// DriverConfig configures the Virtualization.framework VM driver.
type DriverConfig struct {
	// Guest resolves the boot artifacts, from the registry or a local build.
	Guest *guestimage.Resolver
	// StateDir roots each pool's durable disks at <StateDir>/<poolID>.
	StateDir     string
	VCPUs        int
	MemoryMiB    int
	DataDiskGiB  int64
	CacheDiskGiB int64
	// ControlPlaneStreams receives connections the guests open. When nil the VM
	// still runs, but its agent can never register.
	ControlPlaneStreams StreamSink
	// ProgressReporter says what bringing a VM up is doing. The engine reports
	// the coarse phases around this driver; what only this driver knows is that
	// the first pool on a machine downloads and extracts a multi-hundred-megabyte
	// disk image before there is a VM to start at all.
	ProgressReporter sandbox.PoolProgressReporter
}

// Driver owns one Virtualization.framework VM per pool.
//
// VMs are in-process objects, so they die with the discobox-server process and
// cannot be re-adopted by the next one. Only the disks under StateDir survive,
// which is sufficient: the guest keeps all image, container, and volume state
// on them.
type Driver struct {
	guest        *guestimage.Resolver
	stateDir     string
	vcpus        int
	memoryMiB    int
	dataDiskGiB  int64
	cacheDiskGiB int64
	streams      StreamSink
	progress     sandbox.PoolProgressReporter

	mu  sync.Mutex
	vms map[string]*guestVM
}

// guestVM is one running VM plus the control-plane channel that makes it
// useful. The two are created and torn down together: a VM whose guests cannot
// reach the control plane can never register a pool agent.
type guestVM struct {
	vm       *vzvm.VM
	listener net.Listener

	stopOnce sync.Once
	stopped  chan struct{}
}

// NewDriver validates configuration without starting anything.
func NewDriver(cfg DriverConfig) (*Driver, error) {
	if err := vzvm.Supported(); err != nil {
		return nil, err
	}
	if cfg.Guest == nil {
		return nil, errors.New("vz: a guest image resolver is required")
	}
	if strings.TrimSpace(cfg.StateDir) == "" || !filepath.IsAbs(cfg.StateDir) {
		return nil, fmt.Errorf("vz: state directory must be an absolute path")
	}
	if cfg.VCPUs < 0 || cfg.MemoryMiB < 0 || cfg.DataDiskGiB < 0 || cfg.CacheDiskGiB < 0 {
		return nil, errors.New("vz: sizing values must not be negative")
	}
	return &Driver{
		guest:        cfg.Guest,
		stateDir:     filepath.Clean(cfg.StateDir),
		vcpus:        effectiveInt(cfg.VCPUs, defaultVCPUs()),
		memoryMiB:    effectiveInt(cfg.MemoryMiB, defaultMemoryMiB()),
		dataDiskGiB:  effectiveInt64(cfg.DataDiskGiB, defaultDataDiskGiB),
		cacheDiskGiB: effectiveInt64(cfg.CacheDiskGiB, defaultCacheDiskGiB),
		streams:      cfg.ControlPlaneStreams,
		progress:     cfg.ProgressReporter,
		vms:          map[string]*guestVM{},
	}, nil
}

// Close tears down every VM. Unlike libkrun's launcher there is nothing to
// leave running for the next process to adopt: the VMs belong to this one.
func (d *Driver) Close() error {
	d.mu.Lock()
	running := d.vms
	d.vms = map[string]*guestVM{}
	d.mu.Unlock()
	for _, guest := range running {
		guest.close()
	}
	return nil
}

func (d *Driver) EnsureVM(ctx context.Context, poolID string, _ dockerworker.VMSpec) (*dockerworker.VMInfo, error) {
	if err := validatePoolID(poolID); err != nil {
		return nil, err
	}
	if existing := d.runningVM(poolID); existing != nil {
		return runningInfo(poolID), nil
	}

	// Resolved outside the lock: the first pool on a machine pulls the guest
	// image, and no other pool should block on that.
	//
	// Reported unconditionally rather than only when it turns out to be a miss:
	// whether the image is cached is known only by asking, and a phase that
	// lasts milliseconds on a warm machine costs a status line one frame.
	releaseFetch := d.progress.Hold(ctx, poolID, sandbox.PoolPhaseFetchingVMImage)
	bundle, err := d.guest.Resolve(ctx)
	releaseFetch()
	if err != nil {
		return nil, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	// A VM that died while this call was resolving is replaced, not reused.
	if existing, ok := d.vms[poolID]; ok {
		if existing.vm.Running() {
			return runningInfo(poolID), nil
		}
		delete(d.vms, poolID)
		existing.close()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	stateDir := d.poolStateDir(poolID)
	dataDisk, cacheDisk, err := d.ensureDisks(stateDir)
	if err != nil {
		return nil, err
	}

	vm, err := vzvm.Start(vzvm.Options{
		Name:           poolID,
		CPUCount:       uint(d.vcpus),
		MemoryBytes:    uint64(d.memoryMiB) * 1024 * 1024,
		KernelPath:     bundle.Path(kernelArtifact),
		InitrdPath:     bundle.Path(initrdArtifact),
		KernelCmdline:  kernelCmdline,
		RootImagePath:  bundle.Path(rootArtifact),
		DataImagePath:  dataDisk,
		CacheImagePath: cacheDisk,
		ConsoleLogPath: filepath.Join(stateDir, consoleLogName),
	})
	if err != nil {
		return nil, fmt.Errorf("start vz VM for pool %s: %w", poolID, err)
	}

	guest := &guestVM{vm: vm, stopped: make(chan struct{})}
	listener, err := vm.Listen(controlPlaneVSOCKPort)
	if err != nil {
		guest.close()
		return nil, fmt.Errorf("serve control plane for pool %s: %w", poolID, err)
	}
	guest.listener = listener
	go guest.serveControlPlane(ctx, poolID, d.streams)

	d.vms[poolID] = guest
	slog.InfoContext(ctx, "started vz pool VM",
		"pool_id", poolID, "guest_image", bundle.Source, "vcpus", d.vcpus, "memory_mib", d.memoryMiB)
	return runningInfo(poolID), nil
}

// StopVM powers the guest down but keeps its data and cache disks, so a repair
// or a later start finds the pool's images, volumes, and containers intact.
func (d *Driver) StopVM(ctx context.Context, poolID string) error {
	if err := validatePoolID(poolID); err != nil {
		return err
	}
	d.mu.Lock()
	guest := d.vms[poolID]
	delete(d.vms, poolID)
	d.mu.Unlock()
	if guest == nil {
		return nil
	}

	// Ask systemd to shut down in order first: the guest owns filesystems that
	// Docker is writing to, and a hard stop is a dirty unmount of both disks.
	d.requestGuestShutdown(ctx, guest)
	if guest.vm.WaitStopped(gracefulStopTimeout) {
		guest.close()
		return nil
	}
	if err := guest.vm.RequestStop(); err == nil && guest.vm.WaitStopped(forcedStopTimeout) {
		guest.close()
		return nil
	}
	guest.close()
	return nil
}

// DeleteVM stops the VM and removes the pool's disks. It is reserved for an
// authorized pool deletion; StopVM is what repair uses.
func (d *Driver) DeleteVM(ctx context.Context, poolID string) error {
	if err := d.StopVM(ctx, poolID); err != nil {
		return err
	}
	if err := validatePoolID(poolID); err != nil {
		return err
	}
	stateDir := d.poolStateDir(poolID)
	if err := os.RemoveAll(stateDir); err != nil {
		return fmt.Errorf("remove vz pool state %s: %w", stateDir, err)
	}
	return nil
}

func (d *Driver) InspectVM(_ context.Context, poolID string) (*dockerworker.VMInfo, error) {
	if err := validatePoolID(poolID); err != nil {
		return nil, err
	}
	d.mu.Lock()
	guest, ok := d.vms[poolID]
	d.mu.Unlock()
	if !ok {
		return nil, sandbox.ErrNotFound
	}
	if !guest.vm.Running() {
		// Reported unhealthy rather than absent so the engine replaces the VM
		// in place and the pool keeps its disks.
		return &dockerworker.VMInfo{ID: vmID(poolID), Status: sandbox.StatusStopped}, nil
	}
	return runningInfo(poolID), nil
}

func (d *Driver) AcquireDockerClient(ctx context.Context, poolID string) (*dockerworker.DockerClientLease, error) {
	guest, err := d.guestVM(poolID)
	if err != nil {
		return nil, err
	}
	cli, err := dockerworker.NewDockerClientForDialer(func(_ context.Context, _, _ string) (net.Conn, error) {
		return guest.vm.Connect(dockerVSOCKPort)
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
	guest, err := d.guestVM(poolID)
	if err != nil {
		return nil, err
	}
	httpTransport := &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return guest.vm.Connect(agentVSOCKPort)
		},
	}
	client := &http.Client{Transport: httpTransport}
	return transport.NewHTTPClientLeaseWithBaseURL(client, "http://pool.local", httpTransport.CloseIdleConnections), nil
}

// PoolLogs reads the guest's serial console, which the VM appends to a file in
// the pool's state directory across every boot.
//
// It is deliberately a file rather than the live console device: the log an
// operator needs is the one from the boot that failed, and a guest that panics
// before its Docker daemon starts leaves nothing else behind at all.
func (d *Driver) PoolLogs(ctx context.Context, poolID string, opts sandbox.PoolLogOptions) (*sandbox.PoolLogStream, error) {
	if err := validatePoolID(poolID); err != nil {
		return nil, err
	}
	path := filepath.Join(d.poolStateDir(poolID), consoleLogName)
	stream, err := dockerworker.TailFile(ctx, path, opts)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("pool %s has no vz console log yet: its VM has not been started on this host", poolID)
		}
		return nil, err
	}
	return &sandbox.PoolLogStream{Source: "vz guest serial console", ReadCloser: stream}, nil
}

// ensureDisks creates the pool's durable and disposable disks if they are
// absent. They are sparse: the sizes are ceilings the guest grows into, not
// space taken from the developer's disk up front.
func (d *Driver) ensureDisks(stateDir string) (string, string, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", "", fmt.Errorf("create vz pool state directory: %w", err)
	}
	dataDisk := filepath.Join(stateDir, "data.raw")
	cacheDisk := filepath.Join(stateDir, "cache.raw")
	for _, disk := range []struct {
		path string
		size int64
	}{
		{dataDisk, d.dataDiskGiB * 1024 * 1024 * 1024},
		{cacheDisk, d.cacheDiskGiB * 1024 * 1024 * 1024},
	} {
		if err := vzvm.CreateDiskImage(disk.path, disk.size); err != nil {
			return "", "", err
		}
		if err := growDiskImage(disk.path, disk.size); err != nil {
			return "", "", err
		}
	}
	return dataDisk, cacheDisk, nil
}

// growDiskImage raises an existing disk to a larger configured size, so the
// size is a ceiling the pool can be given more of rather than whatever it
// happened to be created with. The guest grows its filesystem to match on the
// next boot.
//
// It never shrinks: the image is sparse, so a smaller number costs nothing to
// leave alone, while truncating one would discard the pool's data.
func growDiskImage(path string, size int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat vz disk %s: %w", path, err)
	}
	if info.Size() >= size {
		return nil
	}
	if err := os.Truncate(path, size); err != nil {
		return fmt.Errorf("grow vz disk %s: %w", path, err)
	}
	return nil
}

// requestGuestShutdown asks the guest's lifecycle service to power off. The
// same service serves libkrun, so the guest side is one implementation.
func (d *Driver) requestGuestShutdown(ctx context.Context, guest *guestVM) {
	httpTransport := &http.Transport{DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
		return guest.vm.Connect(lifecycleVSOCKPort)
	}}
	defer httpTransport.CloseIdleConnections()
	client := &http.Client{Transport: httpTransport, Timeout: 5 * time.Second}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://lifecycle.local/shutdown", nil)
	if err != nil {
		return
	}
	response, err := client.Do(request)
	if err == nil {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
}

func (d *Driver) guestVM(poolID string) (*guestVM, error) {
	if err := validatePoolID(poolID); err != nil {
		return nil, err
	}
	d.mu.Lock()
	guest := d.vms[poolID]
	d.mu.Unlock()
	if guest == nil || !guest.vm.Running() {
		return nil, fmt.Errorf("vz VM %s: %w", poolID, sandbox.ErrNotFound)
	}
	return guest, nil
}

func (d *Driver) runningVM(poolID string) *guestVM {
	d.mu.Lock()
	defer d.mu.Unlock()
	guest := d.vms[poolID]
	if guest == nil || !guest.vm.Running() {
		return nil
	}
	return guest
}

func (d *Driver) poolStateDir(poolID string) string {
	return filepath.Join(d.stateDir, poolID)
}

// serveControlPlane hands every connection the guest opens to the control
// plane. The guest can only mean one thing by dialing this port.
func (g *guestVM) serveControlPlane(ctx context.Context, poolID string, sink StreamSink) {
	for {
		conn, err := g.listener.Accept()
		if err != nil {
			select {
			case <-g.stopped:
			default:
				slog.DebugContext(ctx, "vz control-plane listener ended", "pool_id", poolID, "error", err)
			}
			return
		}
		if sink == nil {
			_ = conn.Close()
			continue
		}
		if err := sink.Push(conn, g.stopped); err != nil {
			_ = conn.Close()
		}
	}
}

func (g *guestVM) close() {
	g.stopOnce.Do(func() {
		close(g.stopped)
		if g.listener != nil {
			_ = g.listener.Close()
		}
		_ = g.vm.Close()
	})
}

func runningInfo(poolID string) *dockerworker.VMInfo {
	return &dockerworker.VMInfo{ID: vmID(poolID), Status: sandbox.StatusRunning}
}

func vmID(poolID string) string { return "vz-" + poolID }

func validatePoolID(poolID string) error {
	if !validPoolID.MatchString(poolID) || poolID == "." || poolID == ".." || strings.Contains(poolID, "..") {
		return fmt.Errorf("invalid pool ID %q", poolID)
	}
	return nil
}
