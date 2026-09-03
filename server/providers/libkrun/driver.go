package libkrun

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/transport"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
)

const (
	launcherConfigVersion = 2
	defaultLauncherPath   = "discobox-krun"
	defaultMkfsPath       = "mkfs.ext4"

	launcherStartTimeout = 10 * time.Second
	gracefulStopTimeout  = 10 * time.Second
	forcedStopTimeout    = 5 * time.Second
	maxUnixSocketPath    = 103

	// consoleLogName is where the launcher appends the guest's serial console,
	// in the pool's runtime directory.
	consoleLogName = "console.log"
)

var validPoolID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// DriverConfig configures the local libkrun VM driver.
type DriverConfig struct {
	RootImage          string
	KernelImage        string
	StateDir           string
	RuntimeDir         string
	ControlPlaneSocket string
	LauncherPath       string
	MkfsPath           string
	VCPUs              int
	MemoryMiB          int
	DataDiskGiB        int64
	CacheDiskGiB       int64
}

// Driver manages one libkrun process and two writable disks per pool.
type Driver struct {
	rootImage          string
	kernelImage        string
	stateDir           string
	runtimeDir         string
	controlPlaneSocket string
	launcherPath       string
	mkfsPath           string
	vcpus              int
	memoryMiB          int
	dataDiskBytes      int64
	cacheDiskBytes     int64

	mu sync.Mutex
}

// NewDriver validates host dependencies and creates a libkrun driver. It does
// not start or stop existing VMs; InspectVM re-adopts them from runtime state.
func NewDriver(cfg DriverConfig) (*Driver, error) {
	if err := validateHostPlatform(); err != nil { //nolint:staticcheck // SA4023: validateHostPlatform is a stub that always errors off linux/amd64; the check is meaningful on linux/amd64.
		return nil, err
	}
	if cfg.VCPUs < 0 || cfg.MemoryMiB < 0 || cfg.DataDiskGiB < 0 || cfg.CacheDiskGiB < 0 {
		return nil, errors.New("libkrun VM sizing values must not be negative")
	}
	rootImage, err := absoluteRegularFile(cfg.RootImage, "root image")
	if err != nil {
		return nil, err
	}
	kernelImage, err := absoluteRegularFile(cfg.KernelImage, "kernel image")
	if err != nil {
		return nil, err
	}
	launcherPath, err := executablePath(defaultString(cfg.LauncherPath, defaultLauncherPath))
	if err != nil {
		return nil, fmt.Errorf("resolve discobox-krun: %w", err)
	}
	mkfsPath, err := executablePath(defaultString(cfg.MkfsPath, defaultMkfsPath))
	if err != nil {
		return nil, fmt.Errorf("resolve mkfs.ext4: %w", err)
	}
	stateDir, err := absoluteDirPath(defaultString(cfg.StateDir, defaultStateDir()), "state directory")
	if err != nil {
		return nil, err
	}
	runtimeDir, err := absoluteDirPath(defaultString(cfg.RuntimeDir, defaultRuntimeDir()), "runtime directory")
	if err != nil {
		return nil, err
	}
	controlPlaneSocket := strings.TrimSpace(cfg.ControlPlaneSocket)
	if !filepath.IsAbs(controlPlaneSocket) {
		return nil, errors.New("control plane socket must be an absolute path")
	}
	vcpus := effectiveInt(cfg.VCPUs, defaultVCPUs)
	memoryMiB := effectiveInt(cfg.MemoryMiB, defaultMemoryMiB)
	dataDiskGiB := effectiveInt64(cfg.DataDiskGiB, defaultDataDiskGiB)
	cacheDiskGiB := effectiveInt64(cfg.CacheDiskGiB, defaultCacheDiskGiB)
	if vcpus > 255 {
		return nil, errors.New("vcpus must not exceed 255")
	}
	if memoryMiB < 256 {
		return nil, errors.New("memoryMiB must be at least 256")
	}
	dataBytes, err := gibibytes(dataDiskGiB)
	if err != nil {
		return nil, fmt.Errorf("data disk size: %w", err)
	}
	cacheBytes, err := gibibytes(cacheDiskGiB)
	if err != nil {
		return nil, fmt.Errorf("cache disk size: %w", err)
	}
	return &Driver{
		rootImage:          rootImage,
		kernelImage:        kernelImage,
		stateDir:           stateDir,
		runtimeDir:         runtimeDir,
		controlPlaneSocket: filepath.Clean(controlPlaneSocket),
		launcherPath:       launcherPath,
		mkfsPath:           mkfsPath,
		vcpus:              vcpus,
		memoryMiB:          memoryMiB,
		dataDiskBytes:      dataBytes,
		cacheDiskBytes:     cacheBytes,
	}, nil
}

func (d *Driver) Close() error {
	// VMs deliberately outlive the server process and are re-adopted after a
	// restart. Pool lifecycle methods are the only operations that stop them.
	return nil
}

func (d *Driver) EnsureVM(ctx context.Context, poolID string, _ dockerworker.VMSpec) (*dockerworker.VMInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := validatePoolID(poolID); err != nil {
		return nil, err
	}
	if info, err := d.inspectVM(poolID); err == nil && info.Status == sandbox.StatusRunning {
		return info, nil
	} else if err != nil && !errors.Is(err, sandbox.ErrNotFound) {
		return nil, err
	}

	stateDir := d.poolStateDir(poolID)
	runtimeDir := d.poolRuntimeDir(poolID)
	if err := makePrivateDir(stateDir); err != nil {
		return nil, err
	}
	if err := makePrivateDir(runtimeDir); err != nil {
		return nil, err
	}
	dataDisk := filepath.Join(stateDir, "data.raw")
	cacheDisk := filepath.Join(stateDir, "cache.raw")
	if err := ensureExt4Disk(ctx, d.mkfsPath, dataDisk, d.dataDiskBytes, "discobox-data"); err != nil {
		return nil, err
	}
	if err := ensureExt4Disk(ctx, d.mkfsPath, cacheDisk, d.cacheDiskBytes, "discobox-cache"); err != nil {
		return nil, err
	}

	manifestPath := filepath.Join(runtimeDir, "config.json")
	manifest := d.launcherManifest(poolID, dataDisk, cacheDisk)
	socketPaths := []string{manifest.PasstSocket}
	for _, mapping := range manifest.VSOCK {
		socketPaths = append(socketPaths, mapping.Socket)
	}
	for _, path := range socketPaths {
		if len(path) > maxUnixSocketPath {
			return nil, fmt.Errorf("libkrun VM socket path %s is too long; shorten runtimeDir", path)
		}
	}
	if err := writeJSONAtomic(manifestPath, manifest, 0o600); err != nil {
		return nil, err
	}
	if err := d.validateLauncher(ctx, manifestPath); err != nil {
		return nil, err
	}
	launcherExit, err := d.startLauncher(manifestPath, runtimeDir)
	if err != nil {
		return nil, err
	}
	if err := d.waitForLauncher(ctx, poolID, launcherStartTimeout, launcherExit); err != nil {
		return nil, err
	}
	return d.inspectVM(poolID)
}

// StopVM stops the launcher but preserves both writable disks.
func (d *Driver) StopVM(ctx context.Context, poolID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := validatePoolID(poolID); err != nil {
		return err
	}
	info, err := d.inspectVM(poolID)
	if errors.Is(err, sandbox.ErrNotFound) || (err == nil && info.Status == sandbox.StatusStopped) {
		return nil
	}
	if err != nil {
		return err
	}

	d.requestGuestShutdown(ctx, poolID)
	if d.waitForStop(ctx, poolID, gracefulStopTimeout) {
		return nil
	}
	if err := d.signalLauncher(poolID, syscall.SIGTERM); err != nil {
		return err
	}
	if d.waitForStop(ctx, poolID, forcedStopTimeout) {
		return nil
	}
	if err := d.signalLauncher(poolID, syscall.SIGKILL); err != nil {
		return err
	}
	if !d.waitForStop(ctx, poolID, forcedStopTimeout) {
		return fmt.Errorf("libkrun VM %s did not stop after SIGKILL", poolID)
	}
	return nil
}

// DeleteVM stops the VM and removes its data, cache, and runtime directories.
func (d *Driver) DeleteVM(ctx context.Context, poolID string) error {
	if err := d.StopVM(ctx, poolID); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
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
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := validatePoolID(poolID); err != nil {
		return nil, err
	}
	return d.inspectVM(poolID)
}

func (d *Driver) AcquireDockerClient(ctx context.Context, poolID string) (*dockerworker.DockerClientLease, error) {
	socket, err := d.runningSocket(poolID, "docker.sock")
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
	socket, err := d.runningSocket(poolID, "pool-agent.sock")
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
			return nil, fmt.Errorf("pool %s has no libkrun console log yet: its VM has not been started on this host", poolID)
		}
		return nil, err
	}
	return &sandbox.PoolLogStream{Source: "libkrun guest serial console", ReadCloser: stream}, nil
}

func (d *Driver) inspectVM(poolID string) (*dockerworker.VMInfo, error) {
	stateExists := regularFileExists(filepath.Join(d.poolStateDir(poolID), "data.raw"))
	identity, identityErr := readProcessIdentity(filepath.Join(d.poolRuntimeDir(poolID), "launcher.json"))
	if identityErr == nil && processMatches(identity) && lockHeld(filepath.Join(d.poolRuntimeDir(poolID), "launcher.lock")) {
		status := sandbox.StatusCreated
		if d.runtimeSocketsReady(poolID) {
			status = sandbox.StatusRunning
		}
		return &dockerworker.VMInfo{ID: "libkrun-" + poolID, Status: status}, nil
	}
	if stateExists {
		return &dockerworker.VMInfo{ID: "libkrun-" + poolID, Status: sandbox.StatusStopped}, nil
	}
	return nil, sandbox.ErrNotFound
}

func (d *Driver) runningSocket(poolID, name string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := validatePoolID(poolID); err != nil {
		return "", err
	}
	info, err := d.inspectVM(poolID)
	if err != nil {
		return "", err
	}
	if info.Status != sandbox.StatusRunning {
		return "", fmt.Errorf("libkrun VM %s is %s", poolID, info.Status)
	}
	path := filepath.Join(d.poolRuntimeDir(poolID), name)
	if !isUnixSocket(path) {
		return "", fmt.Errorf("libkrun VM %s socket %s is unavailable", poolID, name)
	}
	return path, nil
}

func (d *Driver) launcherManifest(poolID, dataDisk, cacheDisk string) launcherConfig {
	runtimeDir := d.poolRuntimeDir(poolID)
	return launcherConfig{
		Version:     launcherConfigVersion,
		PoolID:      poolID,
		RuntimeDir:  runtimeDir,
		KernelImage: d.kernelImage,
		RootDisk:    d.rootImage,
		DataDisk:    dataDisk,
		CacheDisk:   cacheDisk,
		PasstSocket: filepath.Join(runtimeDir, "passt.sock"),
		ConsoleLog:  filepath.Join(runtimeDir, consoleLogName),
		VCPUs:       d.vcpus,
		MemoryMiB:   d.memoryMiB,
		MACAddress:  macAddress(poolID),
		VSOCK: []launcherVSOCK{
			{Name: "control-plane", Port: controlPlaneVSOCKPort, Socket: d.controlPlaneSocket, Direction: "guestConnects"},
			{Name: "pool-agent", Port: agentVSOCKPort, Socket: filepath.Join(runtimeDir, "pool-agent.sock"), Direction: "hostConnects"},
			{Name: "lifecycle", Port: lifecycleVSOCKPort, Socket: filepath.Join(runtimeDir, "lifecycle.sock"), Direction: "hostConnects"},
			{Name: "docker", Port: dockerVSOCKPort, Socket: filepath.Join(runtimeDir, "docker.sock"), Direction: "hostConnects"},
		},
	}
}

func (d *Driver) validateLauncher(ctx context.Context, manifestPath string) error {
	cmd := exec.CommandContext(ctx, d.launcherPath, "validate", "--config", manifestPath) //nolint:gosec // Paths come from validated provider configuration.
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("validate libkrun launcher config: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (d *Driver) startLauncher(manifestPath, runtimeDir string) (<-chan error, error) {
	logPath := filepath.Join(runtimeDir, "launcher.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open launcher log: %w", err)
	}
	// The launcher deliberately outlives request and server contexts so a
	// replacement server can re-adopt the VM from its runtime identity.
	cmd := exec.CommandContext(context.Background(), d.launcherPath, "run", "--config", manifestPath) //nolint:gosec // Paths come from validated provider configuration.
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = detachedSysProcAttr()
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("start discobox-krun: %w", err)
	}
	_ = logFile.Close()
	exit := make(chan error, 1)
	go func() {
		exit <- cmd.Wait()
		close(exit)
	}()
	return exit, nil
}

func (d *Driver) waitForLauncher(ctx context.Context, poolID string, timeout time.Duration, launcherExit <-chan error) error {
	deadline := time.Now().Add(timeout)
	launcherSeen := false
	for {
		info, err := d.inspectVM(poolID)
		if err == nil && info.Status == sandbox.StatusRunning {
			return nil
		}
		if err == nil && info.Status == sandbox.StatusCreated {
			launcherSeen = true
		}
		if launcherSeen && err == nil && info.Status == sandbox.StatusStopped {
			return fmt.Errorf("discobox-krun exited during startup; see %s", filepath.Join(d.poolRuntimeDir(poolID), "launcher.log"))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("discobox-krun did not create its runtime sockets within %s", timeout)
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case err := <-launcherExit:
			timer.Stop()
			// Another server may have won the launcher lock concurrently. If
			// the lock is still held, keep reconciling that launcher instead
			// of treating this launcher's lock failure as a VM failure.
			if lockHeld(filepath.Join(d.poolRuntimeDir(poolID), "launcher.lock")) {
				launcherExit = nil
				continue
			}
			if err == nil {
				err = errors.New("launcher exited successfully before creating runtime sockets")
			}
			return fmt.Errorf("discobox-krun exited during startup: %w; see %s", err, filepath.Join(d.poolRuntimeDir(poolID), "launcher.log"))
		case <-timer.C:
		}
	}
}

func (d *Driver) runtimeSocketsReady(poolID string) bool {
	runtimeDir := d.poolRuntimeDir(poolID)
	for _, name := range []string{"passt.sock", "pool-agent.sock", "lifecycle.sock", "docker.sock"} {
		if !isUnixSocket(filepath.Join(runtimeDir, name)) {
			return false
		}
	}
	return true
}

func (d *Driver) requestGuestShutdown(ctx context.Context, poolID string) {
	socket := filepath.Join(d.poolRuntimeDir(poolID), "lifecycle.sock")
	dialer := &net.Dialer{Timeout: time.Second}
	httpTransport := &http.Transport{DialContext: func(dialCtx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(dialCtx, "unix", socket)
	}}
	defer httpTransport.CloseIdleConnections()
	client := &http.Client{Transport: httpTransport, Timeout: 2 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://lifecycle.local/shutdown", nil)
	if err != nil {
		return
	}
	resp, err := client.Do(req)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

func (d *Driver) waitForStop(ctx context.Context, poolID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		info, err := d.inspectVM(poolID)
		if errors.Is(err, sandbox.ErrNotFound) || (err == nil && info.Status == sandbox.StatusStopped) {
			return true
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
	return false
}

func (d *Driver) signalLauncher(poolID string, signal syscall.Signal) error {
	identity, err := readProcessIdentity(filepath.Join(d.poolRuntimeDir(poolID), "launcher.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read discobox-krun identity for %s: %w", poolID, err)
	}
	if !processMatches(identity) {
		return nil
	}
	//nolint:staticcheck // SA4023: signalProcess is a stub that always errors off linux/amd64; the check is meaningful on linux/amd64.
	if err := signalProcess(identity.PID, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal discobox-krun for %s: %w", poolID, err)
	}
	return nil
}

func (d *Driver) poolStateDir(poolID string) string {
	return filepath.Join(d.stateDir, poolID)
}

func (d *Driver) poolRuntimeDir(poolID string) string {
	return filepath.Join(d.runtimeDir, poolID)
}

type launcherConfig struct {
	Version     int             `json:"version"`
	PoolID      string          `json:"poolId"`
	RuntimeDir  string          `json:"runtimeDir"`
	KernelImage string          `json:"kernelImage"`
	RootDisk    string          `json:"rootDisk"`
	DataDisk    string          `json:"dataDisk"`
	CacheDisk   string          `json:"cacheDisk"`
	PasstSocket string          `json:"passtSocket"`
	ConsoleLog  string          `json:"consoleLog"`
	VCPUs       int             `json:"vcpus"`
	MemoryMiB   int             `json:"memoryMiB"`
	MACAddress  string          `json:"macAddress"`
	VSOCK       []launcherVSOCK `json:"vsock"`
}

type launcherVSOCK struct {
	Name      string `json:"name"`
	Port      uint32 `json:"port"`
	Socket    string `json:"socket"`
	Direction string `json:"direction"`
}

type processIdentity struct {
	PID            int    `json:"pid"`
	StartTimeTicks uint64 `json:"startTimeTicks"`
}

func readProcessIdentity(path string) (processIdentity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return processIdentity{}, err
	}
	var identity processIdentity
	if err := json.Unmarshal(data, &identity); err != nil {
		return processIdentity{}, err
	}
	if identity.PID <= 0 || identity.StartTimeTicks == 0 {
		return processIdentity{}, errors.New("invalid launcher identity")
	}
	return identity, nil
}

func processMatches(identity processIdentity) bool {
	start, err := processStartTimeTicks(identity.PID)
	return err == nil && start == identity.StartTimeTicks
}

func processStartTimeTicks(pid int) (uint64, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	stat := string(data)
	commandEnd := strings.LastIndexByte(stat, ')')
	if commandEnd < 0 {
		return 0, errors.New("invalid process stat")
	}
	fields := strings.Fields(stat[commandEnd+1:])
	if len(fields) <= 19 {
		return 0, errors.New("process stat has no start time")
	}
	return strconv.ParseUint(fields[19], 10, 64)
}

func ensureExt4Disk(ctx context.Context, mkfsPath, path string, size int64, label string) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("disk %s must be a regular file", path)
		}
		if info.Size() != size {
			return fmt.Errorf("disk %s is %d bytes, configured size is %d bytes; resizing existing libkrun disks is not supported", path, info.Size(), size)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create disk %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	success := false
	defer func() {
		_ = tmp.Close()
		if !success {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Truncate(size); err != nil {
		return fmt.Errorf("size disk %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	output, err := exec.CommandContext(ctx, mkfsPath, "-F", "-q", "-L", label, tmpPath).CombinedOutput() //nolint:gosec // Executable and path are trusted local provider state.
	if err != nil {
		return fmt.Errorf("format disk %s: %w: %s", path, err, strings.TrimSpace(string(output)))
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("install disk %s: %w", path, err)
	}
	success = true
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

func absoluteRegularFile(path, name string) (string, error) {
	path = strings.TrimSpace(path)
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s must be an absolute path", name)
	}
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("inspect %s %s: %w", name, path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s %s must be a regular file", name, path)
	}
	return path, nil
}

func absoluteDirPath(path, name string) (string, error) {
	path = strings.TrimSpace(path)
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s must be an absolute path", name)
	}
	return filepath.Clean(path), nil
}

func executablePath(path string) (string, error) {
	if filepath.IsAbs(path) {
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return "", fmt.Errorf("%s is not executable", path)
		}
		return filepath.Clean(path), nil
	}
	return exec.LookPath(path)
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

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

// GuestImageBuildSpec reports that there is nothing to build here yet.
//
// libkrun runs on Linux only, where a developer has a Docker daemon of their
// own and builds the guest with it — the bootstrap this exists to break is
// macOS's, where the only reachable daemon is inside the VM being replaced
// (ADR 0062 §7). Its guest also does not go through the shared resolver yet, so
// there is no local build directory for an export to land in.
func (d *Driver) GuestImageBuildSpec() (dockerworker.GuestImageBuildSpec, error) {
	return dockerworker.GuestImageBuildSpec{}, fmt.Errorf("the libkrun backend builds its guest image on the host's own Docker: %w", sandbox.ErrGuestImageBuildUnsupported)
}
