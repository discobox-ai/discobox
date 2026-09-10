package libkrun

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/transport"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
	"github.com/discobox-ai/discobox/server/providers/guestimage"
	"github.com/discobox-ai/discobox/server/providers/libkrun/internal/krunvm"
)

const (
	launcherStartTimeout = 30 * time.Second
	gracefulStopTimeout  = 30 * time.Second
	forcedStopTimeout    = 15 * time.Second

	// maxUnixSocketPath is sun_path minus its NUL. A runtime directory deep
	// enough to overflow it produces a VM whose sockets silently never appear,
	// so it is checked where the paths are built.
	maxUnixSocketPath = 103

	// consoleLogName is where the launcher appends the guest's serial console,
	// in the pool's runtime directory.
	consoleLogName  = "console.log"
	launcherLogName = "launcher.log"
	manifestName    = "config.json"

	passtSocketName     = "passt.sock"
	agentSocketName     = "pool-agent.sock"
	lifecycleSocketName = "lifecycle.sock"
	dockerSocketName    = "docker.sock"
)

var validPoolID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// DriverConfig configures the local libkrun VM driver.
type DriverConfig struct {
	// Guest resolves the shared VM guest image; Kernel resolves the
	// libkrunfw-patched kernel, which is a separate artifact on its own release
	// line because it is the one thing libkrun cannot take from a guest image
	// every backend shares.
	Guest  *guestimage.Resolver
	Kernel *guestimage.Resolver

	// StateDir roots each pool's durable disks at <StateDir>/<poolID>.
	// RuntimeDir roots the sockets, manifest, and logs of a running VM, and is
	// expected to be tmpfs: nothing under it outlives a reboot, and nothing
	// under it needs to.
	StateDir   string
	RuntimeDir string
	// ControlPlaneSocket is the server's own listening socket, which libkrun
	// terminates the guest's outbound control-plane port at.
	ControlPlaneSocket string
	// PasstPath and LibraryPath override the two host binaries this backend
	// needs. Both are resolved by the launcher, not here, because the launcher
	// is the process that uses them.
	PasstPath   string
	LibraryPath string

	VCPUs        int
	MemoryMiB    int
	DataDiskGiB  int64
	CacheDiskGiB int64

	// ProgressReporter says what bringing a VM up is doing. What only this
	// driver knows is that the first pool on a machine downloads and extracts a
	// guest image before there is a VM to start at all.
	ProgressReporter sandbox.PoolProgressReporter
}

// Driver owns one libkrun microVM per pool.
//
// Each VM is a child process of this server, kept alive by nothing else: the
// launcher dies with its parent (see watchServerProcess), so the VMs belong to
// this process exactly as vz's in-process VMs belong to theirs (ADR 0062 §9).
// Only the disks under StateDir survive, which is sufficient — the guest keeps
// all image, container, and volume state on them.
type Driver struct {
	guest              *guestimage.Resolver
	kernel             *guestimage.Resolver
	stateDir           string
	runtimeDir         string
	controlPlaneSocket string
	passtPath          string
	libraryPath        string
	vcpus              int
	memoryMiB          int
	dataDiskBytes      int64
	cacheDiskBytes     int64
	progress           sandbox.PoolProgressReporter

	mu  sync.Mutex
	vms map[string]*guestVM
}

// guestVM is one launcher process plus the pipe that binds its life to this
// one. The pipe's write end is held here and nowhere else, so it is closed
// exactly when this process exits or this driver lets the VM go.
type guestVM struct {
	cmd        *exec.Cmd
	watchdog   *os.File
	runtimeDir string
	// sockets are the Unix sockets this launcher creates. The driver deletes
	// them before starting it, so their presence means this process bound them.
	sockets []string

	exited   chan struct{}
	stopOnce sync.Once
}

// NewDriver validates configuration without starting anything.
func NewDriver(cfg DriverConfig) (*Driver, error) {
	if err := krunvm.Supported(); err != nil {
		return nil, err
	}
	if cfg.Guest == nil || cfg.Kernel == nil {
		return nil, errors.New("libkrun: a guest image and a kernel image resolver are required")
	}
	if cfg.VCPUs < 0 || cfg.MemoryMiB < 0 || cfg.DataDiskGiB < 0 || cfg.CacheDiskGiB < 0 {
		return nil, errors.New("libkrun: sizing values must not be negative")
	}
	stateDir, err := absoluteDir(cfg.StateDir, "state directory")
	if err != nil {
		return nil, err
	}
	runtimeDir, err := absoluteDir(cfg.RuntimeDir, "runtime directory")
	if err != nil {
		return nil, err
	}
	controlPlaneSocket := strings.TrimSpace(cfg.ControlPlaneSocket)
	if !filepath.IsAbs(controlPlaneSocket) {
		return nil, errors.New("libkrun: control plane socket must be an absolute path")
	}
	vcpus := effectiveInt(cfg.VCPUs, defaultVCPUs())
	memoryMiB := effectiveInt(cfg.MemoryMiB, defaultMemoryMiB())
	dataBytes, err := gibibytes(effectiveInt64(cfg.DataDiskGiB, defaultDataDiskGiB))
	if err != nil {
		return nil, fmt.Errorf("libkrun data disk size: %w", err)
	}
	cacheBytes, err := gibibytes(effectiveInt64(cfg.CacheDiskGiB, defaultCacheDiskGiB))
	if err != nil {
		return nil, fmt.Errorf("libkrun cache disk size: %w", err)
	}
	return &Driver{
		guest:              cfg.Guest,
		kernel:             cfg.Kernel,
		stateDir:           stateDir,
		runtimeDir:         runtimeDir,
		controlPlaneSocket: filepath.Clean(controlPlaneSocket),
		passtPath:          strings.TrimSpace(cfg.PasstPath),
		libraryPath:        strings.TrimSpace(cfg.LibraryPath),
		vcpus:              vcpus,
		memoryMiB:          memoryMiB,
		dataDiskBytes:      dataBytes,
		cacheDiskBytes:     cacheBytes,
		progress:           cfg.ProgressReporter,
		vms:                map[string]*guestVM{},
	}, nil
}

// Close tears down every VM this driver started. There is nothing to leave
// running for the next process to adopt: a launcher whose server has exited
// kills itself, so leaving one behind would only produce a VM nothing can
// reach.
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
	if guest := d.runningVM(poolID); guest != nil {
		return runningInfo(poolID), nil
	}

	// Resolved outside the lock: the first pool on a machine pulls the guest
	// image and the kernel, and no other pool should block on that.
	//
	// The phase is reported and not held. A fetch that moves bytes restates
	// itself twice a second with its byte counts, and a held phase would blank
	// those every time its heartbeat fired.
	d.progress.Report(ctx, poolID, sandbox.PoolPhaseFetchingVMImage)
	root, kernel, err := d.resolveArtifacts(ctx, poolID)
	if err != nil {
		return nil, err
	}

	guest, started, err := d.launch(ctx, poolID, root, kernel)
	if err != nil {
		return nil, err
	}
	if !started {
		return runningInfo(poolID), nil
	}

	// Waited on outside the lock, for the reason the resolve is: this is up to
	// launcherStartTimeout, it happens on every pool start rather than only the
	// first one on a machine, and every other operation on this driver — an
	// InspectVM, another pool's AcquireDockerClient — would queue behind it.
	if err := d.waitForLauncher(ctx, guest); err != nil {
		d.forget(poolID, guest)
		return nil, err
	}
	slog.InfoContext(ctx, "started libkrun pool VM",
		"pool_id", poolID, "guest_image", root.Source, "kernel_image", kernel.Source,
		"vcpus", d.vcpus, "memory_mib", d.memoryMiB, "pid", guest.cmd.Process.Pid)
	return runningInfo(poolID), nil
}

// launch creates the pool's disks and starts its launcher, and is the whole of
// what needs the driver's lock: it reserves the map entry so a second EnsureVM
// for this pool joins the first rather than starting a second VM.
//
// started is false when the VM was already running, which is the only case
// where the caller has nothing to wait for.
func (d *Driver) launch(ctx context.Context, poolID string, root, kernel *guestimage.Bundle) (*guestVM, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// A VM that died while this call was resolving is replaced, not reused.
	if existing, ok := d.vms[poolID]; ok {
		if existing.running() {
			return existing, false, nil
		}
		delete(d.vms, poolID)
		existing.close()
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	stateDir := d.poolStateDir(poolID)
	runtimeDir := d.poolRuntimeDir(poolID)
	if err := makePrivateDir(stateDir); err != nil {
		return nil, false, err
	}
	if err := makePrivateDir(runtimeDir); err != nil {
		return nil, false, err
	}
	dataDisk, cacheDisk, err := d.ensureDisks(stateDir)
	if err != nil {
		return nil, false, err
	}

	manifest := d.manifest(poolID, root, kernel, dataDisk, cacheDisk)
	if err := manifest.Validate(); err != nil {
		return nil, false, fmt.Errorf("libkrun VM %s: %w", poolID, err)
	}
	for _, path := range manifestSocketPaths(manifest) {
		if len(path) > maxUnixSocketPath {
			return nil, false, fmt.Errorf("libkrun VM socket path %s is too long; shorten runtimeDir", path)
		}
	}
	// Before the launcher, not after: the runtime directory outlives a server
	// process, libkrun does not unlink its sockets when it is killed — which is
	// how every launcher dies — and the readiness check below is their
	// reappearance. Left in place, a previous VM's sockets make the next start
	// look finished the instant it began, and a launcher that then fails takes
	// its reason to a log nobody is sent to.
	if err := krunvm.RemoveOwnedSockets(manifest); err != nil {
		return nil, false, fmt.Errorf("libkrun VM %s: %w", poolID, err)
	}
	manifestPath := filepath.Join(runtimeDir, manifestName)
	if err := writeJSONAtomic(manifestPath, manifest, 0o600); err != nil {
		return nil, false, err
	}

	guest, err := d.startLauncher(manifest, manifestPath)
	if err != nil {
		return nil, false, err
	}
	d.vms[poolID] = guest
	return guest, true, nil
}

// forget drops a VM this driver started and could not bring up. It is a no-op
// when something else already replaced the entry, so a slow failing start
// cannot remove a newer VM's registration.
func (d *Driver) forget(poolID string, guest *guestVM) {
	d.mu.Lock()
	if d.vms[poolID] == guest {
		delete(d.vms, poolID)
	}
	d.mu.Unlock()
	guest.close()
}

// resolveArtifacts fetches the root filesystem and the kernel. They are two
// images because they change on unrelated clocks: the guest moves when Debian
// or Docker does, the kernel when libkrunfw or upstream Linux does.
func (d *Driver) resolveArtifacts(ctx context.Context, poolID string) (*guestimage.Bundle, *guestimage.Bundle, error) {
	report := func(fetch guestimage.Progress) {
		d.progress.ReportProgress(ctx, poolID, sandbox.PoolProvisionProgress{
			Phase: sandbox.PoolPhaseFetchingVMImage,
			Pull: &sandbox.PoolPullProgress{
				Image:          fetch.Reference,
				Current:        fetch.Current,
				Total:          fetch.Total,
				Layers:         fetch.Layers,
				LayersComplete: fetch.LayersComplete,
				Done:           fetch.Done,
			},
		})
	}
	// Both failures name the local build that answers them. Neither artifact is
	// published for this backend yet, and a resolver error otherwise reports a
	// registry problem to someone whose actual next step is a build.
	root, err := d.guest.Resolve(ctx, report)
	if err != nil {
		return nil, nil, fmt.Errorf("%w (build one from this checkout with `task build:vm-guest`)", err)
	}
	kernel, err := d.kernel.Resolve(ctx, report)
	if err != nil {
		return nil, nil, fmt.Errorf("%w (build one from this checkout with `task build:vm-kernel`)", err)
	}
	return root, kernel, nil
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
	if guest.waitExited(gracefulStopTimeout) {
		guest.close()
		return nil
	}
	guest.signal(syscall.SIGTERM)
	if guest.waitExited(forcedStopTimeout) {
		guest.close()
		return nil
	}
	guest.close()
	if !guest.waitExited(forcedStopTimeout) {
		return fmt.Errorf("libkrun VM %s did not stop after SIGKILL", poolID)
	}
	return nil
}

// DeleteVM stops the VM and removes its disks and runtime state. It is reserved
// for an authorized pool deletion; StopVM is what repair uses.
func (d *Driver) DeleteVM(ctx context.Context, poolID string) error {
	if err := d.StopVM(ctx, poolID); err != nil {
		return err
	}
	if err := validatePoolID(poolID); err != nil {
		return err
	}
	for _, path := range []string{d.poolRuntimeDir(poolID), d.poolStateDir(poolID)} {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove libkrun VM path %s: %w", path, err)
		}
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
	if ok && guest.running() {
		return runningInfo(poolID), nil
	}
	if ok || regularFileExists(filepath.Join(d.poolStateDir(poolID), "data.raw")) {
		// Reported unhealthy rather than absent so the engine replaces the VM
		// in place and the pool keeps its disks.
		return &dockerworker.VMInfo{ID: vmID(poolID), Status: sandbox.StatusStopped}, nil
	}
	return nil, sandbox.ErrNotFound
}

func (d *Driver) AcquireDockerClient(ctx context.Context, poolID string) (*dockerworker.DockerClientLease, error) {
	socket, err := d.runningSocket(poolID, dockerSocketName)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{}
	cli, err := dockerworker.NewDockerClientForDialer(func(dialCtx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(dialCtx, "unix", socket)
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
	socket, err := d.runningSocket(poolID, agentSocketName)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{}
	httpTransport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		},
	}
	client := &http.Client{Transport: httpTransport}
	return transport.NewHTTPClientLeaseWithBaseURL(client, "http://pool.local", httpTransport.CloseIdleConnections), nil
}

// PoolLogs reads the guest's serial console, which the launcher appends to a
// file in the pool's runtime directory.
//
// The console is what a microVM has instead of a place to log in: a guest that
// never brings its Docker daemon up has no socket to reach and no agent to ask,
// and the kernel messages here are the only account of why.
func (d *Driver) PoolLogs(ctx context.Context, poolID string, opts sandbox.PoolLogOptions) (*sandbox.PoolLogStream, error) {
	if err := validatePoolID(poolID); err != nil {
		return nil, err
	}
	path := filepath.Join(d.poolRuntimeDir(poolID), consoleLogName)
	stream, err := dockerworker.TailFile(ctx, path, opts)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("pool %s has no libkrun console log yet: its VM has not been started by this server, which on a cold start means the guest image is still being fetched; --follow waits for the boot instead of failing here", poolID)
		}
		return nil, err
	}
	return &sandbox.PoolLogStream{Source: "libkrun guest serial console", ReadCloser: stream}, nil
}

// GuestImageBuildSpec points the engine at the shared guest image Dockerfile
// and at the directory this driver's resolver prefers.
//
// A Linux developer usually has a Docker daemon of their own and can run
// `task build:vm-guest` instead. This exists for the case that made it
// necessary on macOS (ADR 0062 §7) and is not macOS-specific at all: a host
// with no daemon, or one whose only useful builder is the one inside a pool VM.
// It is reached through the driver, so it answers on a pool whose agent never
// started — which is the pool a broken guest image produces, and therefore the
// only pool anybody would be rebuilding a guest from.
//
// The kernel is not buildable this way. It has its own image, and it changes on
// its own clock; a guest rebuild is the loop worth closing.
func (d *Driver) GuestImageBuildSpec() (dockerworker.GuestImageBuildSpec, error) {
	destination := d.guest.LocalDir()
	if destination == "" {
		return dockerworker.GuestImageBuildSpec{}, fmt.Errorf("this libkrun provider instance has no local guest image directory configured: %w", sandbox.ErrGuestImageBuildUnsupported)
	}
	return dockerworker.GuestImageBuildSpec{
		Dockerfile:  guestImageDockerfile,
		Platform:    guestImagePlatform,
		Destination: destination,
		Adopt:       d.guest.Invalidate,
	}, nil
}

// manifest renders one VM for the launcher.
func (d *Driver) manifest(poolID string, root, kernel *guestimage.Bundle, dataDisk, cacheDisk string) krunvm.Config {
	runtimeDir := d.poolRuntimeDir(poolID)
	return krunvm.Config{
		Version:     krunvm.ConfigVersion,
		PoolID:      poolID,
		RuntimeDir:  runtimeDir,
		KernelImage: kernel.Path(kernelArtifact),
		RootDisk:    root.Path(rootArtifact),
		DataDisk:    dataDisk,
		CacheDisk:   cacheDisk,
		PasstSocket: filepath.Join(runtimeDir, passtSocketName),
		PasstPath:   d.passtPath,
		LibraryPath: d.libraryPath,
		ConsoleLog:  filepath.Join(runtimeDir, consoleLogName),
		VCPUs:       d.vcpus,
		MemoryMiB:   d.memoryMiB,
		MACAddress:  macAddress(poolID),
		VSOCK: []krunvm.VSOCKMapping{
			{Name: "control-plane", Port: controlPlaneVSOCKPort, Socket: d.controlPlaneSocket, Direction: krunvm.GuestConnects},
			{Name: "pool-agent", Port: agentVSOCKPort, Socket: filepath.Join(runtimeDir, agentSocketName), Direction: krunvm.HostConnects},
			{Name: "lifecycle", Port: lifecycleVSOCKPort, Socket: filepath.Join(runtimeDir, lifecycleSocketName), Direction: krunvm.HostConnects},
			{Name: "docker", Port: dockerVSOCKPort, Socket: filepath.Join(runtimeDir, dockerSocketName), Direction: krunvm.HostConnects},
		},
	}
}

// startLauncher re-executes this binary as a pool VM.
//
// Two mechanisms bind the child's life to this process, and both are needed.
// Pdeathsig is armed by the kernel and survives anything this process does
// afterwards, including being killed with SIGKILL; it cannot cover the window
// between fork and the prctl call. The pipe covers exactly that window and
// closes with the process rather than with a thread. Between them there is no
// way to leave a VM behind (ADR 0062 §9).
func (d *Driver) startLauncher(manifest krunvm.Config, manifestPath string) (*guestVM, error) {
	runtimeDir := manifest.RuntimeDir
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate this server binary: %w", err)
	}
	logPath := filepath.Join(runtimeDir, launcherLogName)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // Path is derived from validated provider configuration.
	if err != nil {
		return nil, fmt.Errorf("open launcher log: %w", err)
	}
	defer func() { _ = logFile.Close() }()

	watchdogRead, watchdogWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create launcher watchdog: %w", err)
	}
	defer func() { _ = watchdogRead.Close() }()

	// context.Background, deliberately: the VM outlives the request that created
	// it and is bound to this *process* instead, by the two mechanisms below. A
	// request context here would tear the pool's VM down the moment its create
	// call returned.
	//nolint:gosec // The command is this binary and the manifest is one it wrote.
	cmd := exec.CommandContext(context.Background(), self, launcherCommand, "--config", manifestPath)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.ExtraFiles = []*os.File{watchdogRead}
	cmd.SysProcAttr = launcherSysProcAttr()
	if err := cmd.Start(); err != nil {
		_ = watchdogWrite.Close()
		return nil, fmt.Errorf("start the pool VM launcher: %w", err)
	}

	guest := &guestVM{
		cmd:        cmd,
		watchdog:   watchdogWrite,
		runtimeDir: runtimeDir,
		// The sockets this launcher must create, taken from the manifest it was
		// handed rather than from a list spelled again here: readiness is
		// exactly "the paths launch cleared have come back".
		sockets: manifest.OwnedSockets(),
		exited:  make(chan struct{}),
	}
	go func() {
		_ = cmd.Wait()
		close(guest.exited)
	}()
	return guest, nil
}

// waitForLauncher waits for the VM's sockets to appear, which is the only
// evidence from out here that libkrun configured the guest and started it.
//
// It is evidence only because launch deleted them first. They live in a runtime
// directory that outlives a server process, and libkrun leaves them behind when
// it is killed.
func (d *Driver) waitForLauncher(ctx context.Context, guest *guestVM) error {
	deadline := time.Now().Add(launcherStartTimeout)
	for {
		if guest.socketsReady() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-guest.exited:
			// Any exit here is a failure, including a clean one:
			// krun_start_enter returns only on error, and a guest that powered
			// itself off before its sockets were usable did not start. The
			// sockets may exist by now — libkrun binds them before the guest
			// boots — which is exactly why their presence is not the answer.
			return fmt.Errorf("the pool VM launcher exited during startup: %s", guest.launcherLogTail())
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the pool VM launcher did not create its sockets within %s: %s", launcherStartTimeout, guest.launcherLogTail())
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// requestGuestShutdown asks the guest's lifecycle service to power off. The
// same service serves vz, so the guest side is one implementation.
func (d *Driver) requestGuestShutdown(ctx context.Context, guest *guestVM) {
	socket := filepath.Join(guest.runtimeDir, lifecycleSocketName)
	dialer := &net.Dialer{Timeout: time.Second}
	httpTransport := &http.Transport{DialContext: func(dialCtx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(dialCtx, "unix", socket)
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

// ensureDisks creates the pool's durable and disposable disks if they are
// absent, and grows them when the configured size is raised.
//
// They are created empty and sparse: the guest formats them on first boot and
// runs resize2fs on every mount, so the sizes are ceilings the guest grows into
// rather than space taken from the developer's disk up front — and no mkfs.ext4
// is needed on the host. Shrinking is never attempted; a smaller number costs
// nothing to leave alone, while truncating would discard the pool's data.
func (d *Driver) ensureDisks(stateDir string) (string, string, error) {
	dataDisk := filepath.Join(stateDir, "data.raw")
	cacheDisk := filepath.Join(stateDir, "cache.raw")
	for _, disk := range []struct {
		path string
		size int64
	}{
		{dataDisk, d.dataDiskBytes},
		{cacheDisk, d.cacheDiskBytes},
	} {
		if err := ensureSparseImage(disk.path, disk.size); err != nil {
			return "", "", err
		}
	}
	return dataDisk, cacheDisk, nil
}

func (d *Driver) runningSocket(poolID, name string) (string, error) {
	if err := validatePoolID(poolID); err != nil {
		return "", err
	}
	guest := d.runningVM(poolID)
	if guest == nil {
		return "", fmt.Errorf("libkrun VM %s: %w", poolID, sandbox.ErrNotFound)
	}
	path := filepath.Join(guest.runtimeDir, name)
	if !isUnixSocket(path) {
		return "", fmt.Errorf("libkrun VM %s socket %s is unavailable", poolID, name)
	}
	return path, nil
}

func (d *Driver) runningVM(poolID string) *guestVM {
	d.mu.Lock()
	defer d.mu.Unlock()
	guest := d.vms[poolID]
	if guest == nil || !guest.running() {
		return nil
	}
	return guest
}

func (d *Driver) poolStateDir(poolID string) string {
	return filepath.Join(d.stateDir, poolID)
}

func (d *Driver) poolRuntimeDir(poolID string) string {
	return filepath.Join(d.runtimeDir, poolID)
}

func (g *guestVM) running() bool {
	select {
	case <-g.exited:
		return false
	default:
		return true
	}
}

func (g *guestVM) socketsReady() bool {
	for _, path := range g.sockets {
		if !isUnixSocket(path) {
			return false
		}
	}
	return len(g.sockets) > 0
}

func (g *guestVM) signal(signal syscall.Signal) {
	if g.cmd.Process == nil {
		return
	}
	_ = g.cmd.Process.Signal(signal)
}

func (g *guestVM) waitExited(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-g.exited:
		return true
	case <-timer.C:
		return false
	}
}

// close kills the VM and releases the watchdog. Closing the pipe is not merely
// tidying: it is the second of the two things that tell the launcher to go, and
// the one that works when a signal does not arrive.
func (g *guestVM) close() {
	g.stopOnce.Do(func() {
		if g.cmd.Process != nil {
			_ = g.cmd.Process.Kill()
		}
		_ = g.watchdog.Close()
	})
}

// launcherLogTail is the last of what the launcher said before it stopped. A
// failure here is almost always one line — no KVM, no libkrun, no passt — and
// naming the log file instead would send an operator to a path that is under
// XDG_RUNTIME_DIR and gone after a reboot.
func (g *guestVM) launcherLogTail() string {
	const maxTail = 2048
	path := filepath.Join(g.runtimeDir, launcherLogName)
	data, err := os.ReadFile(path) //nolint:gosec // Path is derived from validated provider configuration.
	if err != nil {
		return fmt.Sprintf("no launcher log at %s: %v", path, err)
	}
	if len(data) > maxTail {
		data = data[len(data)-maxTail:]
	}
	tail := strings.TrimSpace(string(data))
	if tail == "" {
		return fmt.Sprintf("the launcher log %s is empty", path)
	}
	return tail
}

func manifestSocketPaths(manifest krunvm.Config) []string {
	paths := []string{manifest.PasstSocket}
	for _, mapping := range manifest.VSOCK {
		paths = append(paths, mapping.Socket)
	}
	return paths
}

// ensureSparseImage creates a disk image of the configured size, or grows an
// existing one. The file is sparse, so its length is a ceiling rather than an
// allocation.
func ensureSparseImage(path string, size int64) error {
	info, err := os.Lstat(path)
	switch {
	case err == nil && !info.Mode().IsRegular():
		return fmt.Errorf("disk %s must be a regular file", path)
	case err == nil:
		if info.Size() >= size {
			return nil
		}
		if err := os.Truncate(path, size); err != nil {
			return fmt.Errorf("grow disk %s: %w", path, err)
		}
		return nil
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // Path is derived from validated provider configuration.
	if err != nil {
		return fmt.Errorf("create disk %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	if err := file.Truncate(size); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("size disk %s: %w", path, err)
	}
	return nil
}

func writeJSONAtomic(path string, value any, mode os.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	success := false
	defer func() {
		_ = tmp.Close()
		if !success {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	success = true
	return nil
}

func makePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must be a real directory", path)
	}
	return os.Chmod(path, 0o700)
}

func absoluteDir(path, name string) (string, error) {
	path = strings.TrimSpace(path)
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("libkrun %s must be an absolute path", name)
	}
	return filepath.Clean(path), nil
}

func gibibytes(value int64) (int64, error) {
	if value <= 0 || value > 4096 {
		return 0, errors.New("must be between 1 and 4096 GiB")
	}
	return value << 30, nil
}

func validatePoolID(poolID string) error {
	if !validPoolID.MatchString(poolID) || poolID == "." || poolID == ".." || strings.Contains(poolID, "..") {
		return fmt.Errorf("invalid pool ID %q", poolID)
	}
	return nil
}

// macAddress invents a stable locally administered unicast address per pool.
// Stability matters because the guest's DHCP lease is keyed on it.
func macAddress(poolID string) string {
	sum := sha256.Sum256([]byte(poolID))
	sum[0] = (sum[0] & 0xfc) | 0x02
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3], sum[4], sum[5])
}

func regularFileExists(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

func isUnixSocket(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSocket != 0
}

func runningInfo(poolID string) *dockerworker.VMInfo {
	return &dockerworker.VMInfo{ID: vmID(poolID), Status: sandbox.StatusRunning}
}

func vmID(poolID string) string { return "libkrun-" + poolID }
