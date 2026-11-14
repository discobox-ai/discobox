//go:build darwin && cgo

package vzvm

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/Code-Hex/vz/v3"
)

// VM is one running guest.
type VM struct {
	name string

	mu        sync.Mutex
	machine   *vz.VirtualMachine
	socket    *vz.VirtioSocketDevice
	listeners []*vz.VirtioSocketListener
	closed    bool
	// closing is closed by Close, which starts the clock on how long the
	// keeper waits for the machine to stop before it says it has not.
	closing chan struct{}
}

// Supported reports whether this build can run VMs.
func Supported() error { return nil }

// entitlementHint decorates a framework failure with the one cause that is
// almost always responsible during development and gives no useful message of
// its own: an unsigned binary. Creating a VM without
// com.apple.security.virtualization fails at the framework boundary with an
// opaque internal error.
func entitlementHint(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w (a server binary without the com.apple.security.virtualization entitlement cannot start a VM; run `go tool task build:server` to build and sign one)", err)
}

// CreateDiskImage creates a sparse raw disk image. Virtualization.framework
// accepts raw images only — there is no QCOW2 path — so a pool's durable and
// disposable disks are plain files that APFS keeps sparse until written.
func CreateDiskImage(path string, size int64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("vzvm: create disk directory: %w", err)
	}
	if err := vz.CreateDiskImage(path, size); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return fmt.Errorf("vzvm: create disk image %s: %w", path, err)
	}
	return nil
}

// Start builds the machine configuration and boots it.
func Start(opts Options) (*VM, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}

	bootOptions := []vz.LinuxBootLoaderOption{vz.WithCommandLine(opts.KernelCmdline)}
	if opts.InitrdPath != "" {
		bootOptions = append(bootOptions, vz.WithInitrd(opts.InitrdPath))
	}
	bootLoader, err := vz.NewLinuxBootLoader(opts.KernelPath, bootOptions...)
	if err != nil {
		return nil, fmt.Errorf("vzvm: boot loader: %w", err)
	}

	config, err := vz.NewVirtualMachineConfiguration(bootLoader, opts.CPUCount, opts.MemoryBytes)
	if err != nil {
		return nil, entitlementHint(fmt.Errorf("vzvm: machine configuration: %w", err))
	}

	storage, err := storageDevices(opts)
	if err != nil {
		return nil, err
	}
	config.SetStorageDevicesVirtualMachineConfiguration(storage)

	network, err := natNetworkDevice()
	if err != nil {
		return nil, err
	}
	config.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{network})

	// Every byte of Discobox control traffic crosses this device, in both
	// directions; the IP network exists only for the guest's own outbound
	// traffic (image pulls, package installs) and reaches no host listener.
	socketConfig, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("vzvm: socket device: %w", err)
	}
	config.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{socketConfig})

	if opts.ConsoleLogPath != "" {
		console, err := consoleDevice(opts.ConsoleLogPath)
		if err != nil {
			return nil, err
		}
		config.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{console})
	}

	shares, err := directorySharingDevices(opts.SharedDirectories)
	if err != nil {
		return nil, err
	}
	if len(shares) > 0 {
		config.SetDirectorySharingDevicesVirtualMachineConfiguration(shares)
	}

	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("vzvm: entropy device: %w", err)
	}
	config.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})

	// A pool VM's memory is a ceiling, not a reservation. The balloon lets a
	// guest that is doing nothing hand memory back to macOS instead of holding
	// all of it for the life of the pool.
	balloon, err := vz.NewVirtioTraditionalMemoryBalloonDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("vzvm: memory balloon device: %w", err)
	}
	config.SetMemoryBalloonDevicesVirtualMachineConfiguration([]vz.MemoryBalloonDeviceConfiguration{balloon})

	if valid, err := config.Validate(); err != nil || !valid {
		if err == nil {
			err = errors.New("configuration rejected")
		}
		return nil, fmt.Errorf("vzvm: validate machine configuration: %w", err)
	}

	machine, err := vz.NewVirtualMachine(config)
	if err != nil {
		return nil, entitlementHint(fmt.Errorf("vzvm: create virtual machine: %w", err))
	}
	startErr := machine.Start()
	// From here the framework may change the machine's state at any time, so
	// it is kept reachable until it settles, whatever this function returns.
	closing := make(chan struct{})
	go keepUntilSettled(opts.Name, machine, closing, stopGrace)
	if startErr != nil {
		close(closing)
		return nil, entitlementHint(fmt.Errorf("vzvm: start virtual machine: %w", startErr))
	}

	devices := machine.SocketDevices()
	if len(devices) == 0 {
		_ = machine.Stop()
		close(closing)
		return nil, errors.New("vzvm: virtual machine started without a socket device")
	}

	vm := &VM{name: opts.Name, machine: machine, socket: devices[0], closing: closing}
	if err := vm.waitRunning(30 * time.Second); err != nil {
		_ = vm.Close()
		return nil, err
	}
	return vm, nil
}

func storageDevices(opts Options) ([]vz.StorageDeviceConfiguration, error) {
	// Order is the guest's device naming: root, data, cache become vda, vdb, vdc.
	disks := []struct {
		path     string
		readOnly bool
	}{
		{opts.RootImagePath, true},
		{opts.DataImagePath, false},
		{opts.CacheImagePath, false},
	}
	devices := make([]vz.StorageDeviceConfiguration, 0, len(disks))
	for _, disk := range disks {
		// Cached with fsync sync, as Lima, UTM and vfkit attach theirs. The
		// framework's default (automatic caching, full sync) corrupts ext4 on
		// Apple silicon (lima-vm/lima#1957), and the closest match to the CPU 0
		// hard lockups of #1101 was traced to it (NixOS/nixpkgs#557576). A
		// guest flush still reaches the disk: fsync sync forwards it as
		// fsync(2) on the image file. Needs macOS 12, as virtiofs already does.
		attachment, err := vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(
			disk.path, disk.readOnly, vz.DiskImageCachingModeCached, vz.DiskImageSynchronizationModeFsync)
		if err != nil {
			return nil, fmt.Errorf("vzvm: attach disk %s: %w", disk.path, err)
		}
		device, err := vz.NewVirtioBlockDeviceConfiguration(attachment)
		if err != nil {
			return nil, fmt.Errorf("vzvm: configure disk %s: %w", disk.path, err)
		}
		devices = append(devices, device)
	}
	return devices, nil
}

// directorySharingDevices exports host directories to the guest over virtiofs,
// one device per share so each carries its own tag and its own read-only bit.
//
// This is what lets a sandbox clone a developer's checkout out of the guest
// instead of the client pushing it over the wire: the tag mounts at the same
// absolute path the Mac uses, so a host path and the guest's spelling of it are
// the same string, all the way down to the bind the pool agent asks its Docker
// daemon for.
//
// A share the framework rejects fails the pool rather than being dropped. The
// caller decided a directory is exported, and a guest silently missing it looks
// like a source that will not clone, diagnosed nowhere near here.
func directorySharingDevices(shares []SharedDirectory) ([]vz.DirectorySharingDeviceConfiguration, error) {
	devices := make([]vz.DirectorySharingDeviceConfiguration, 0, len(shares))
	for _, share := range shares {
		directory, err := vz.NewSharedDirectory(share.HostPath, share.ReadOnly)
		if err != nil {
			return nil, fmt.Errorf("vzvm: share %s: %w", share.HostPath, err)
		}
		single, err := vz.NewSingleDirectoryShare(directory)
		if err != nil {
			return nil, fmt.Errorf("vzvm: share %s: %w", share.HostPath, err)
		}
		device, err := vz.NewVirtioFileSystemDeviceConfiguration(share.Tag)
		if err != nil {
			return nil, fmt.Errorf("vzvm: file system device %s: %w", share.Tag, err)
		}
		device.SetDirectoryShare(single)
		devices = append(devices, device)
	}
	return devices, nil
}

// natNetworkDevice gives the guest outbound IP connectivity through the
// framework's own NAT, which also serves DHCP and DNS. Bridged networking is
// deliberately not used: it needs the com.apple.vm.networking entitlement,
// which Apple grants by request, and it would put pool guests on the user's LAN.
func natNetworkDevice() (*vz.VirtioNetworkDeviceConfiguration, error) {
	attachment, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return nil, fmt.Errorf("vzvm: network attachment: %w", err)
	}
	device, err := vz.NewVirtioNetworkDeviceConfiguration(attachment)
	if err != nil {
		return nil, fmt.Errorf("vzvm: network device: %w", err)
	}
	address, err := vz.NewRandomLocallyAdministeredMACAddress()
	if err != nil {
		return nil, fmt.Errorf("vzvm: network address: %w", err)
	}
	device.SetMACAddress(address)
	return device, nil
}

func consoleDevice(path string) (*vz.VirtioConsoleDeviceSerialPortConfiguration, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("vzvm: create console log directory: %w", err)
	}
	// Appended rather than truncated: a pool that is being repaired has a boot
	// history, and the interesting one is usually not the last.
	attachment, err := vz.NewFileSerialPortAttachment(path, true)
	if err != nil {
		return nil, fmt.Errorf("vzvm: console attachment %s: %w", path, err)
	}
	device, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(attachment)
	if err != nil {
		return nil, fmt.Errorf("vzvm: console device: %w", err)
	}
	return device, nil
}

// Connect opens a connection to a guest VSOCK port.
//
// The returned connection deliberately does not implement CloseWrite. The
// framework hands back a socket whose write side cannot be shut down
// independently through this binding, and a no-op CloseWrite would be worse
// than none: a caller that finds the method believes the guest saw EOF. Callers
// that check for it take their full-close fallback instead, which is correct.
func (v *VM) Connect(port uint32) (net.Conn, error) {
	v.mu.Lock()
	socket, closed := v.socket, v.closed
	v.mu.Unlock()
	if closed || socket == nil {
		return nil, fmt.Errorf("vzvm: VM %s is not running", v.name)
	}
	conn, err := socket.Connect(port)
	if err != nil {
		return nil, fmt.Errorf("vzvm: connect to %s guest port %d: %w", v.name, port, socketError("dial", "connect", err))
	}
	return conn, nil
}

// Listen accepts connections the guest opens to the host on a VSOCK port. This
// is the control-plane direction: the guest dials, the host serves, and macOS
// never opens a TCP listener.
func (v *VM) Listen(port uint32) (net.Listener, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed || v.socket == nil {
		return nil, fmt.Errorf("vzvm: VM %s is not running", v.name)
	}
	listener, err := v.socket.Listen(port)
	if err != nil {
		return nil, fmt.Errorf("vzvm: listen on %s guest port %d: %w", v.name, port, socketError("listen", "listen", err))
	}
	v.listeners = append(v.listeners, listener)
	return listener, nil
}

// Running reports whether the VM is still up. A guest that powered itself off
// reports false, which is what makes the engine replace it. A guest that
// panicked does not: panic= reboots it inside the same VM, which stays running.
func (v *VM) Running() bool {
	v.mu.Lock()
	machine, closed := v.machine, v.closed
	v.mu.Unlock()
	if closed || machine == nil {
		return false
	}
	return machine.State() == vz.VirtualMachineStateRunning
}

// RequestStop asks the guest to power itself off. It is the fallback for a
// guest whose lifecycle service is not answering; the driver prefers that
// service, which shuts systemd down in order.
func (v *VM) RequestStop() error {
	v.mu.Lock()
	machine, closed := v.machine, v.closed
	v.mu.Unlock()
	if closed || machine == nil {
		return nil
	}
	if !machine.CanRequestStop() {
		return nil
	}
	if _, err := machine.RequestStop(); err != nil {
		return fmt.Errorf("vzvm: request stop of %s: %w", v.name, err)
	}
	return nil
}

// WaitStopped waits for the guest to leave the running state.
func (v *VM) WaitStopped(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !v.Running() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return !v.Running()
}

// Close force-stops the guest and releases its resources. It is idempotent.
//
// Dropping the machine here does not free it: the keeper Start began holds it
// until the framework reports it stopped, so a machine Close could not stop
// (one that cannot be stopped in its current state, or whose stop failed) is
// never left to the garbage collector while it can still change state.
func (v *VM) Close() error {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return nil
	}
	v.closed = true
	machine, listeners := v.machine, v.listeners
	v.machine, v.socket, v.listeners = nil, nil, nil
	close(v.closing)
	v.mu.Unlock()

	for _, listener := range listeners {
		_ = listener.Close()
	}
	if machine == nil {
		return nil
	}
	if !machine.CanStop() {
		return nil
	}
	if err := machine.Stop(); err != nil {
		return fmt.Errorf("vzvm: stop %s: %w", v.name, err)
	}
	return nil
}

// stopGrace is how long after Close a machine may take to reach a settled
// state before the keeper reports it. A forced stop settles in well under a
// second; one that has not after this long is a hung guest or framework.
const stopGrace = 30 * time.Second

// keepUntilSettled holds machine reachable until the framework reports it
// Stopped or Error, the states it leaves only when started again, which
// nothing here does.
//
// The binding deletes the cgo handle its state observer is called with when
// the *vz.VirtualMachine is finalized, but the observer itself is removed
// only when the framework deallocates the native machine — which it does not
// while the machine is still running, stopping, or hung. A state change after
// that finalizer runs looks the deleted handle up and panics the whole server
// ("runtime/cgo: misuse of an invalid Handle"). This goroutine's reference is
// what keeps the finalizer from running while a state change can still
// arrive, and it stays whatever the binding does with the handle: a machine
// that can still change state is the server's to keep.
//
// A machine that never settles is held for the life of the process: a leak of
// one hung VM is better than a crash of every pool. Once closing is closed,
// grace bounds how long that may take before it is logged.
func keepUntilSettled(name string, machine *vz.VirtualMachine, closing <-chan struct{}, grace time.Duration) {
	notify := machine.StateChangedNotify()
	var overdue <-chan time.Time
	reported := false
	for {
		if settled(machine) {
			if reported {
				slog.Info("vz VM settled after it was closed; releasing it", "vm", name, "state", machine.State().String())
			}
			return
		}
		select {
		case <-notify:
		case <-closing:
			closing = nil
			overdue = time.After(grace)
		case <-overdue:
			overdue = nil
			reported = true
			slog.Warn("vz VM did not stop after it was closed; keeping it in memory until it does",
				"vm", name, "state", machine.State().String(), "waited", grace)
		}
	}
}

// settled reports whether machine has reached Stopped or Error and every state
// change the framework made before now has been delivered. The binding's state
// is a copy its observer updates, so a settled copy is confirmed after a
// synchronous call onto the machine's dispatch queue: the framework changes the
// state and notifies observers on that queue, so whatever it queued before the
// call has been delivered by the time the call returns. That also covers a
// machine that never started, whose copy reads Stopped before any change.
func settled(machine *vz.VirtualMachine) bool {
	if !settledState(machine.State()) {
		return false
	}
	_ = machine.CanStart()
	return settledState(machine.State())
}

func settledState(state vz.VirtualMachineState) bool {
	return state == vz.VirtualMachineStateStopped || state == vz.VirtualMachineStateError
}

func (v *VM) waitRunning(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		switch v.machine.State() {
		case vz.VirtualMachineStateRunning:
			return nil
		case vz.VirtualMachineStateError:
			return fmt.Errorf("vzvm: VM %s entered the error state while starting", v.name)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("vzvm: VM %s did not reach the running state within %s", v.name, timeout)
}

// socketError exposes POSIX failures from the framework as Go network errors.
// Other NSError domains use unrelated code spaces and must remain opaque.
func socketError(op, call string, err error) error {
	var nsErr *vz.NSError
	if errors.As(err, &nsErr) && nsErr.Domain == "NSPOSIXErrorDomain" {
		return &net.OpError{Op: op, Net: "vsock", Err: os.NewSyscallError(call, errors.Join(err, syscall.Errno(nsErr.Code)))}
	}
	return err
}
