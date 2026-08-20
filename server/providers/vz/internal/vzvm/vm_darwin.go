//go:build darwin

package vzvm

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
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

	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("vzvm: entropy device: %w", err)
	}
	config.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})

	// A pool's memory envelope is a ceiling, not a reservation. The balloon lets
	// a guest that is doing nothing hand memory back to macOS instead of holding
	// the whole envelope for the life of the pool.
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
	if err := machine.Start(); err != nil {
		return nil, entitlementHint(fmt.Errorf("vzvm: start virtual machine: %w", err))
	}

	devices := machine.SocketDevices()
	if len(devices) == 0 {
		_ = machine.Stop()
		return nil, errors.New("vzvm: virtual machine started without a socket device")
	}

	vm := &VM{name: opts.Name, machine: machine, socket: devices[0]}
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
		attachment, err := vz.NewDiskImageStorageDeviceAttachment(disk.path, disk.readOnly)
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
		return nil, fmt.Errorf("vzvm: connect to %s guest port %d: %w", v.name, port, err)
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
		return nil, fmt.Errorf("vzvm: listen on %s guest port %d: %w", v.name, port, err)
	}
	v.listeners = append(v.listeners, listener)
	return listener, nil
}

// Running reports whether the guest is still up. A guest that panicked or
// powered itself off reports false, which is what makes the engine replace it.
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
func (v *VM) Close() error {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return nil
	}
	v.closed = true
	machine, listeners := v.machine, v.listeners
	v.machine, v.socket, v.listeners = nil, nil, nil
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
