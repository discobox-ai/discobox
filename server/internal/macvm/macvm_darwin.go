//go:build darwin && cgo

package macvm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Code-Hex/vz/v3"
	"golang.org/x/sys/unix"
)

// Supported reports whether this build can run VMs.
func Supported() error { return nil }

// entitlementHint decorates a framework failure with the one cause that is
// almost always responsible during development and gives no useful message of
// its own: an unsigned binary. Creating a VM, and loading a restore image,
// without com.apple.security.virtualization fails at the framework boundary
// with an opaque internal error.
func entitlementHint(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w (a binary without the com.apple.security.virtualization entitlement cannot start a VM or read a restore image; run `go tool task build:macvm` to build and sign one)", err)
}

// LatestRestoreImageURL asks Apple which macOS this host can install. The
// answer tracks the host: a Mac on 26.6.2 is offered 26.6.2, never something
// newer.
func LatestRestoreImageURL() (string, error) {
	url, err := vz.GetLatestSupportedMacOSRestoreImageURL()
	if err != nil {
		return "", fmt.Errorf("macvm: fetch latest supported restore image: %w", err)
	}
	return url, nil
}

// FetchRestoreImage downloads the latest supported IPSW to destPath, resuming a
// partial file rather than starting over — it is roughly 20 GB, and the
// framework installs only from a local file.
//
// Progress is called with the fraction downloaded and the bytes on disk, which
// includes whatever a previous run left.
func FetchRestoreImage(ctx context.Context, destPath string, progress func(fraction float64, current int64)) error {
	reader, err := vz.FetchLatestSupportedMacOSRestoreImage(ctx, destPath)
	if err != nil {
		return fmt.Errorf("macvm: download restore image: %w", err)
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-reader.Finished():
			if err := reader.Err(); err != nil {
				return fmt.Errorf("macvm: download restore image: %w", err)
			}
			if progress != nil {
				progress(1, reader.Current())
			}
			return nil
		case <-ticker.C:
			if progress != nil {
				progress(reader.FractionCompleted(), reader.Current())
			}
		}
	}
}

// Install creates a guest's four files and installs macOS onto it.
//
// The hardware model is the restore image's, not a choice: it decides how the
// auxiliary storage is laid out and which macOS will boot, and it is written
// into the bundle so every later boot uses the same one.
func Install(ctx context.Context, opts InstallOptions) (retErr error) {
	if err := opts.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(opts.Bundle.Dir, 0o700); err != nil {
		return fmt.Errorf("macvm: create bundle directory: %w", err)
	}

	restoreImage, err := vz.LoadMacOSRestoreImageFromPath(opts.RestoreImagePath)
	if err != nil {
		return entitlementHint(fmt.Errorf("macvm: load restore image %s: %w", opts.RestoreImagePath, err))
	}
	requirements := restoreImage.MostFeaturefulSupportedConfiguration()
	if requirements == nil {
		return fmt.Errorf("macvm: this host supports no configuration in %s", opts.RestoreImagePath)
	}
	hardwareModel := requirements.HardwareModel()
	if !hardwareModel.Supported() {
		return fmt.Errorf("macvm: this host does not support the hardware model in %s", opts.RestoreImagePath)
	}

	// The three identity files first, then the disk, so an interrupted install
	// leaves a bundle that Installed() reports as incomplete rather than one
	// that boots into nothing.
	if err := os.WriteFile(opts.Bundle.HardwareModelPath(), hardwareModel.DataRepresentation(), 0o600); err != nil {
		return fmt.Errorf("macvm: write hardware model: %w", err)
	}
	machineIdentifier, err := vz.NewMacMachineIdentifier()
	if err != nil {
		return fmt.Errorf("macvm: create machine identifier: %w", err)
	}
	if err := os.WriteFile(opts.Bundle.MachineIdentifierPath(), machineIdentifier.DataRepresentation(), 0o600); err != nil {
		return fmt.Errorf("macvm: write machine identifier: %w", err)
	}
	auxiliaryStorage, err := vz.NewMacAuxiliaryStorage(
		opts.Bundle.AuxiliaryPath(),
		vz.WithCreatingMacAuxiliaryStorage(hardwareModel),
	)
	if err != nil {
		return fmt.Errorf("macvm: create auxiliary storage: %w", err)
	}
	if err := createDiskImage(ctx, opts.Bundle.DiskPath(), opts.DiskBytes, opts.Sparse); err != nil {
		return err
	}

	platform, err := vz.NewMacPlatformConfiguration(
		vz.WithMacHardwareModel(hardwareModel),
		vz.WithMacMachineIdentifier(machineIdentifier),
		vz.WithMacAuxiliaryStorage(auxiliaryStorage),
	)
	if err != nil {
		return fmt.Errorf("macvm: platform configuration: %w", err)
	}

	cpuCount, memoryBytes := clamp(
		max(opts.CPUCount, uint(requirements.MinimumSupportedCPUCount())),
		max(opts.MemoryBytes, requirements.MinimumSupportedMemorySize()),
	)
	address, err := bundleMACAddress(opts.Bundle)
	if err != nil {
		return err
	}
	config, err := buildConfiguration(platform, configOptions{
		CPUCount:      cpuCount,
		MemoryBytes:   memoryBytes,
		DiskPath:      opts.Bundle.DiskPath(),
		MACAddress:    address,
		DisplayWidth:  defaultDisplayWidth,
		DisplayHeight: defaultDisplayHeight,
		DisplayPPI:    defaultDisplayPPI,
	})
	if err != nil {
		return err
	}

	machine, err := vz.NewVirtualMachine(config)
	if err != nil {
		return entitlementHint(fmt.Errorf("macvm: create virtual machine: %w", err))
	}
	installer, err := vz.NewMacOSInstaller(machine, opts.RestoreImagePath)
	if err != nil {
		return fmt.Errorf("macvm: create installer: %w", err)
	}

	if opts.Progress != nil {
		reported, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-reported.Done():
					return
				case <-installer.Done():
					return
				case <-ticker.C:
					opts.Progress(installer.FractionCompleted())
				}
			}
		}()
	}

	if err := installer.Install(ctx); err != nil {
		return fmt.Errorf("macvm: install macOS: %w", err)
	}

	version := restoreImage.OperatingSystemVersion()
	return opts.Bundle.writeMetadata(Metadata{
		MacOSVersion: fmt.Sprintf("%d.%d.%d", version.MajorVersion, version.MinorVersion, version.PatchVersion),
		BuildVersion: restoreImage.BuildVersion(),
		CPUCount:     cpuCount,
		MemoryBytes:  memoryBytes,
		DiskBytes:    opts.DiskBytes,
		InstalledAt:  time.Now().UTC(),
	})
}

// Run boots an installed bundle and blocks until the guest stops, the context
// is canceled, or — in GUI mode — the window closes. It then asks the guest to
// shut down before stopping it, because a hard stop is a dirty unmount.
//
// In GUI mode the caller must be on the main thread with runtime.LockOSThread
// held: the framework's window runs an NSApplication event loop, which owns
// that thread until it returns.
func Run(ctx context.Context, opts RunOptions) error {
	if err := opts.Validate(); err != nil {
		return err
	}

	hardwareModel, err := vz.NewMacHardwareModelWithDataPath(opts.Bundle.HardwareModelPath())
	if err != nil {
		return fmt.Errorf("macvm: read hardware model: %w", err)
	}
	machineIdentifier, err := vz.NewMacMachineIdentifierWithDataPath(opts.Bundle.MachineIdentifierPath())
	if err != nil {
		return fmt.Errorf("macvm: read machine identifier: %w", err)
	}
	auxiliaryStorage, err := vz.NewMacAuxiliaryStorage(opts.Bundle.AuxiliaryPath())
	if err != nil {
		return fmt.Errorf("macvm: open auxiliary storage: %w", err)
	}
	platform, err := vz.NewMacPlatformConfiguration(
		vz.WithMacHardwareModel(hardwareModel),
		vz.WithMacMachineIdentifier(machineIdentifier),
		vz.WithMacAuxiliaryStorage(auxiliaryStorage),
	)
	if err != nil {
		return fmt.Errorf("macvm: platform configuration: %w", err)
	}

	cpuCount, memoryBytes := clamp(opts.CPUCount, opts.MemoryBytes)
	address, err := bundleMACAddress(opts.Bundle)
	if err != nil {
		return err
	}
	config, err := buildConfiguration(platform, configOptions{
		CPUCount:      cpuCount,
		MemoryBytes:   memoryBytes,
		DiskPath:      opts.Bundle.DiskPath(),
		MACAddress:    address,
		DisplayWidth:  opts.DisplayWidth,
		DisplayHeight: opts.DisplayHeight,
		DisplayPPI:    opts.DisplayPPI,
		Shares:        opts.Shares,
	})
	if err != nil {
		return err
	}

	machine, err := vz.NewVirtualMachine(config)
	if err != nil {
		return entitlementHint(fmt.Errorf("macvm: create virtual machine: %w", err))
	}

	// Asked before anything is started, because the framework will not say why
	// a save failed after the guest has been running for an hour.
	if opts.Restore || opts.SaveOnExit {
		if savable, err := config.ValidateSaveRestoreSupport(); err != nil || !savable {
			if err == nil {
				err = errors.New("configuration rejected")
			}
			return fmt.Errorf("macvm: this configuration cannot be saved or restored: %w", err)
		}
	}

	if opts.Restore {
		// A restore takes a stopped VM to the paused state, holding the memory
		// the guest had when it was saved; resuming is what makes it run.
		if err := machine.RestoreMachineStateFromURL(opts.Bundle.StatePath()); err != nil {
			return fmt.Errorf("macvm: restore %s (\"permission denied\" means the Mac's screen is locked, which a restore needs unlocked; \"invalid argument\" means the configuration or memory size differs from the one saved; a host OS update can invalidate saved state too; boot without --restore): %w", opts.Bundle.StatePath(), err)
		}
		if err := machine.Resume(); err != nil {
			return fmt.Errorf("macvm: resume restored guest: %w", err)
		}
	} else {
		var startOptions []vz.VirtualMachineStartOption
		if opts.Recovery {
			startOptions = append(startOptions, vz.WithStartUpFromMacOSRecovery(true))
		}
		if p := opts.Provision; p != nil {
			startOptions = append(startOptions, vz.WithMacGuestProvisioning(vz.MacGuestProvisioningOptions{
				FullName:            p.FullName,
				Username:            p.Username,
				Password:            p.Password,
				LogsInAutomatically: p.AutoLogin,
				EnablesRemoteLogin:  p.RemoteLogin,
			}))
		}
		if err := machine.Start(startOptions...); err != nil {
			return entitlementHint(fmt.Errorf("macvm: start virtual machine: %w", err))
		}
		if opts.Provision != nil {
			if err := recordProvisionedUser(opts.Bundle, opts.Provision.Username); err != nil {
				return err
			}
		}
	}

	// However this run ends — a closed window, a canceled context, a guest
	// that powered itself off — it ends exactly once, and the same way.
	finish := sync.OnceValue(func() error {
		if opts.SaveOnExit {
			return saveAndStop(machine, opts.Bundle.StatePath())
		}
		return shutdown(machine)
	})

	if opts.GUI {
		// Canceling is the only way out of the event loop below other than
		// the window itself: it owns this thread, and signal.NotifyContext has
		// already taken SIGINT's default termination away. Stopping the guest
		// is what ends it, because the binding terminates the application when
		// the VM reaches the stopped state.
		go func() {
			<-ctx.Done()
			_ = finish()
		}()
		// Blocks in the framework's event loop. Closing the window, or the
		// guest stopping, ends it.
		if err := machine.StartGraphicApplication(
			opts.WindowWidth,
			opts.WindowHeight,
			vz.WithWindowTitle("discobox macOS"),
			vz.WithController(true),
		); err != nil {
			return fmt.Errorf("macvm: open graphics window: %w", err)
		}
	} else {
		waitUntilStopped(ctx, machine)
	}
	return finish()
}

// recordProvisionedUser notes which account a provisioning boot asked for, so
// info can say so and a later run can tell a second request will be ignored.
func recordProvisionedUser(bundle Bundle, username string) error {
	metadata, err := bundle.ReadMetadata()
	if err != nil {
		return err
	}
	metadata.ProvisionedUser = username
	return bundle.writeMetadata(metadata)
}

// saveAndStop takes the snapshot: pause the guest, write its memory and device
// state, then stop it.
//
// The state is written to a temporary name and renamed, because a half-written
// file is indistinguishable from a good one until the framework rejects it, and
// it would have replaced a snapshot that worked.
func saveAndStop(machine *vz.VirtualMachine, path string) error {
	if !machine.CanPause() {
		// Already stopped or stopping: there is nothing to capture, and the
		// snapshot that exists is still the one that matches the disk.
		return shutdown(machine)
	}
	if err := machine.Pause(); err != nil {
		return fmt.Errorf("macvm: pause before saving: %w", err)
	}
	temporary := path + ".tmp"
	if err := machine.SaveMachineStateToPath(temporary); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("macvm: save machine state: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("macvm: publish saved state: %w", err)
	}
	if machine.CanStop() {
		if err := machine.Stop(); err != nil {
			return fmt.Errorf("macvm: stop after saving: %w", err)
		}
	}
	return nil
}

// Clone copies a guest by cloning its files rather than reading them: on APFS
// every one of them is a copy-on-write clone, so a 100 GiB disk costs no time
// and no space until the clone diverges from it.
//
// What is not cloned is the identity. A cold clone gets a new machine
// identifier, because two running guests that share one is undefined behavior.
// A snapshot clone cannot: the saved memory belongs to that machine, so it
// keeps the identifier and the source is not expected to run again.
func Clone(opts CloneOptions) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	if entries, err := os.ReadDir(opts.Dest.Dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("macvm: %s already exists", opts.Dest.Dir)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("macvm: read %s: %w", opts.Dest.Dir, err)
	}
	if err := os.MkdirAll(opts.Dest.Dir, 0o700); err != nil {
		return fmt.Errorf("macvm: create clone directory: %w", err)
	}

	clones := []struct{ from, to string }{
		{opts.Source.DiskPath(), opts.Dest.DiskPath()},
		{opts.Source.AuxiliaryPath(), opts.Dest.AuxiliaryPath()},
		{opts.Source.HardwareModelPath(), opts.Dest.HardwareModelPath()},
		{opts.Source.MetadataPath(), opts.Dest.MetadataPath()},
	}
	if opts.Snapshot {
		clones = append(clones,
			struct{ from, to string }{opts.Source.StatePath(), opts.Dest.StatePath()},
			struct{ from, to string }{opts.Source.MachineIdentifierPath(), opts.Dest.MachineIdentifierPath()},
		)
	}
	if opts.Snapshot {
		// The saved memory holds a network stack configured for this address
		// and leased on it, so it travels with the snapshot. A cold clone does
		// not copy it and gets a new one on its first boot.
		if _, err := os.Stat(opts.Source.MACAddressPath()); err == nil {
			clones = append(clones, struct{ from, to string }{opts.Source.MACAddressPath(), opts.Dest.MACAddressPath()})
		}
	}
	for _, clone := range clones {
		if err := clonefile(clone.from, clone.to); err != nil {
			return err
		}
	}
	if opts.Snapshot {
		return nil
	}

	identifier, err := vz.NewMacMachineIdentifier()
	if err != nil {
		return fmt.Errorf("macvm: create machine identifier: %w", err)
	}
	if err := os.WriteFile(opts.Dest.MachineIdentifierPath(), identifier.DataRepresentation(), 0o600); err != nil {
		return fmt.Errorf("macvm: write machine identifier: %w", err)
	}
	return nil
}

// clonefile is APFS's copy-on-write copy. A source and destination on different
// volumes cannot share blocks, and the caller asked for a clone rather than a
// copy, so that is an error naming the reason rather than a silent 100 GiB read.
func clonefile(from, to string) error {
	if _, err := os.Stat(from); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("macvm: %s does not exist", from)
		}
		return fmt.Errorf("macvm: stat %s: %w", from, err)
	}
	if err := unix.Clonefile(from, to, 0); err != nil {
		if errors.Is(err, unix.EXDEV) {
			return fmt.Errorf("macvm: %s and %s are on different volumes, so they cannot share blocks", from, to)
		}
		if errors.Is(err, unix.ENOTSUP) {
			return fmt.Errorf("macvm: %s is not on a filesystem that clones (APFS)", from)
		}
		return fmt.Errorf("macvm: clone %s to %s: %w", from, to, err)
	}
	return nil
}

// createDiskImage makes the disk the installer formats.
//
// ASIF is Apple's sparse image format and what it recommends for VM storage
// from macOS 26; diskutil is how it is created, which is what the binding's own
// helper does too. Raw stays the default because it needs nothing but the
// framework, and on APFS it is sparse as well.
func createDiskImage(ctx context.Context, path string, size int64, sparse bool) error {
	if !sparse {
		if err := vz.CreateDiskImage(path, size); err != nil {
			return fmt.Errorf("macvm: create disk image %s: %w", path, err)
		}
		return nil
	}
	// --fs None leaves the image blank. Without it diskutil puts an APFS
	// volume inside, which the installer would only throw away.
	//nolint:gosec // The command is fixed; the size is an integer and the path is the bundle's own, built from a validated guest name.
	command := exec.CommandContext(ctx, "diskutil", "image", "create", "blank",
		"--format", "ASIF", "--fs", "None", "--size", strconv.FormatInt(size, 10), path)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("macvm: create ASIF disk image %s: %w: %s", path, err, output)
	}
	return nil
}

// waitUntilStopped returns when the guest leaves the running state or the
// context is canceled.
func waitUntilStopped(ctx context.Context, machine *vz.VirtualMachine) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			switch machine.State() {
			case vz.VirtualMachineStateRunning, vz.VirtualMachineStateStarting, vz.VirtualMachineStatePausing, vz.VirtualMachineStatePaused, vz.VirtualMachineStateResuming:
			default:
				return
			}
		}
	}
}

// shutdown asks the guest to power itself off and force-stops it only if it
// will not, because a hard stop is a dirty unmount of the guest's disk.
//
// The request goes to the guest's lifecycle service over VSOCK — the same port
// and protocol the Linux guest serves — and not through the framework's own
// stop request. That one reaches a macOS guest as a power-button press, which
// macOS answers by sleeping (and a logged-in guest with a sleep assertion by
// doing nothing), so waiting on it only delayed the hard stop by a minute.
func shutdown(machine *vz.VirtualMachine) error {
	if machine.State() == vz.VirtualMachineStateRunning {
		started := time.Now()
		switch err := requestGuestShutdown(machine); {
		case err != nil:
			fmt.Fprintf(os.Stderr, "shutdown: the guest's lifecycle service did not answer (%v); forcing it off\n", err)
		case waitStopped(machine, shutdownTimeout):
			fmt.Fprintf(os.Stderr, "shutdown: the guest powered itself off in %s\n", time.Since(started).Round(100*time.Millisecond))
			return nil
		default:
			fmt.Fprintf(os.Stderr, "shutdown: the guest did not power off within %s; forcing it off\n", shutdownTimeout)
		}
	}
	if !machine.CanStop() {
		return nil
	}
	if err := machine.Stop(); err != nil {
		return fmt.Errorf("macvm: stop virtual machine: %w", err)
	}
	return nil
}

// lifecycleVSOCKPort is the guest's orderly-shutdown service,
// discobox-vsock-guest's lifecycle subcommand: the port the vz provider's Linux
// guest uses for the same thing.
const lifecycleVSOCKPort = 3003

// requestGuestShutdown asks the guest to shut down in order. It returns once the
// guest has accepted; the power-off itself is watched by the caller.
func requestGuestShutdown(machine *vz.VirtualMachine) error {
	devices := machine.SocketDevices()
	if len(devices) == 0 {
		return errors.New("the VM has no socket device")
	}
	device := devices[0]
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				return device.Connect(lifecycleVSOCKPort)
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://guest/shutdown", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		return fmt.Errorf("lifecycle service answered %s", response.Status)
	}
	return nil
}

func waitStopped(machine *vz.VirtualMachine, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if machine.State() == vz.VirtualMachineStateStopped {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

const shutdownTimeout = 60 * time.Second

const (
	defaultDisplayWidth  = 1920
	defaultDisplayHeight = 1200
	defaultDisplayPPI    = 80
)

// configOptions is what buildConfiguration needs, which is the same set for an
// install and a boot: the same devices, so the guest does not see its hardware
// change between the two.
type configOptions struct {
	CPUCount                                uint
	MemoryBytes                             uint64
	DiskPath                                string
	MACAddress                              *vz.MACAddress
	DisplayWidth, DisplayHeight, DisplayPPI int64
	Shares                                  []SharedDirectory
}

func buildConfiguration(platform vz.PlatformConfiguration, opts configOptions) (*vz.VirtualMachineConfiguration, error) {
	// No kernel, no initrd, no command line: a Mac's boot loader is the
	// platform's own, and everything it needs is in the auxiliary storage.
	bootLoader, err := vz.NewMacOSBootLoader()
	if err != nil {
		return nil, fmt.Errorf("macvm: boot loader: %w", err)
	}
	config, err := vz.NewVirtualMachineConfiguration(bootLoader, opts.CPUCount, opts.MemoryBytes)
	if err != nil {
		return nil, entitlementHint(fmt.Errorf("macvm: machine configuration: %w", err))
	}
	config.SetPlatformVirtualMachineConfiguration(platform)

	attachment, err := vz.NewDiskImageStorageDeviceAttachment(opts.DiskPath, false)
	if err != nil {
		return nil, fmt.Errorf("macvm: attach disk %s: %w", opts.DiskPath, err)
	}
	disk, err := vz.NewVirtioBlockDeviceConfiguration(attachment)
	if err != nil {
		return nil, fmt.Errorf("macvm: configure disk %s: %w", opts.DiskPath, err)
	}
	config.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{disk})

	network, err := natNetworkDevice(opts.MACAddress)
	if err != nil {
		return nil, err
	}
	config.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{network})

	graphics, err := graphicsDevice(opts.DisplayWidth, opts.DisplayHeight, opts.DisplayPPI)
	if err != nil {
		return nil, err
	}
	config.SetGraphicsDevicesVirtualMachineConfiguration([]vz.GraphicsDeviceConfiguration{graphics})

	keyboard, pointing, err := inputDevices()
	if err != nil {
		return nil, err
	}
	config.SetKeyboardsVirtualMachineConfiguration(keyboard)
	config.SetPointingDevicesVirtualMachineConfiguration(pointing)

	// The guest end of this is AF_VSOCK, which a macOS 13 or newer guest has
	// too, so the transport the Linux guest uses for everything is available
	// here without an IP listener on the Mac.
	socket, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("macvm: socket device: %w", err)
	}
	config.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{socket})

	// No memory balloon. A macOS guest matches AppleVirtIOBalloon to the device
	// but does not act on a host target, so it would return nothing, and every
	// device is part of the configuration a saved state must match: adding or
	// removing one strands every snapshot taken before.

	shares, err := directorySharingDevices(opts.Shares)
	if err != nil {
		return nil, err
	}
	if len(shares) > 0 {
		config.SetDirectorySharingDevicesVirtualMachineConfiguration(shares)
	}

	if valid, err := config.Validate(); err != nil || !valid {
		if err == nil {
			err = errors.New("configuration rejected")
		}
		return nil, fmt.Errorf("macvm: validate machine configuration: %w", err)
	}
	return config, nil
}

func natNetworkDevice(address *vz.MACAddress) (*vz.VirtioNetworkDeviceConfiguration, error) {
	attachment, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return nil, fmt.Errorf("macvm: network attachment: %w", err)
	}
	device, err := vz.NewVirtioNetworkDeviceConfiguration(attachment)
	if err != nil {
		return nil, fmt.Errorf("macvm: network device: %w", err)
	}
	device.SetMACAddress(address)
	return device, nil
}

// bundleMACAddress is the guest's persisted network address, created on first
// use. A bundle from before addresses were persisted gets one the first time it
// boots, which is the same as it would have had then: a new one.
func bundleMACAddress(bundle Bundle) (*vz.MACAddress, error) {
	data, err := os.ReadFile(bundle.MACAddressPath())
	if err == nil {
		hardware, err := net.ParseMAC(strings.TrimSpace(string(data)))
		if err != nil {
			return nil, fmt.Errorf("macvm: parse %s: %w", bundle.MACAddressPath(), err)
		}
		address, err := vz.NewMACAddress(hardware)
		if err != nil {
			return nil, fmt.Errorf("macvm: network address: %w", err)
		}
		return address, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("macvm: read %s: %w", bundle.MACAddressPath(), err)
	}
	address, err := vz.NewRandomLocallyAdministeredMACAddress()
	if err != nil {
		return nil, fmt.Errorf("macvm: network address: %w", err)
	}
	if err := os.WriteFile(bundle.MACAddressPath(), []byte(address.String()+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("macvm: write %s: %w", bundle.MACAddressPath(), err)
	}
	return address, nil
}

// graphicsDevice is the Mac framebuffer, not the virtio one a Linux guest gets.
// It is attached whether or not a window is opened, so that turning the window
// on and off does not change the hardware the guest sees.
func graphicsDevice(width, height, ppi int64) (*vz.MacGraphicsDeviceConfiguration, error) {
	device, err := vz.NewMacGraphicsDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("macvm: graphics device: %w", err)
	}
	display, err := vz.NewMacGraphicsDisplayConfiguration(width, height, ppi)
	if err != nil {
		return nil, fmt.Errorf("macvm: graphics display: %w", err)
	}
	device.SetDisplays(display)
	return device, nil
}

// inputDevices are what makes the window interactive. Without them it renders
// the guest and swallows every key, which during Setup Assistant looks like a
// hung VM.
func inputDevices() ([]vz.KeyboardConfiguration, []vz.PointingDeviceConfiguration, error) {
	keyboard, err := vz.NewMacKeyboardConfiguration()
	if err != nil {
		return nil, nil, fmt.Errorf("macvm: keyboard device: %w", err)
	}
	pointer, err := vz.NewUSBScreenCoordinatePointingDeviceConfiguration()
	if err != nil {
		return nil, nil, fmt.Errorf("macvm: pointing device: %w", err)
	}
	pointing := []vz.PointingDeviceConfiguration{pointer}
	if trackpad, err := vz.NewMacTrackpadConfiguration(); err == nil {
		pointing = append(pointing, trackpad)
	}
	return []vz.KeyboardConfiguration{keyboard}, pointing, nil
}

// directorySharingDevices exports host directories over virtiofs. A share with
// no tag gets the automount tag, which a macOS 13 or newer guest mounts by
// itself at /Volumes/My Shared Files; any other tag waits for a
// mount_virtiofs in the guest.
func directorySharingDevices(shares []SharedDirectory) ([]vz.DirectorySharingDeviceConfiguration, error) {
	devices := make([]vz.DirectorySharingDeviceConfiguration, 0, len(shares))
	for _, share := range shares {
		tag := share.Tag
		if tag == "" {
			automount, err := vz.MacOSGuestAutomountTag()
			if err != nil {
				return nil, fmt.Errorf("macvm: automount tag: %w", err)
			}
			tag = automount
		}
		directory, err := vz.NewSharedDirectory(share.HostPath, share.ReadOnly)
		if err != nil {
			return nil, fmt.Errorf("macvm: share %s: %w", share.HostPath, err)
		}
		single, err := vz.NewSingleDirectoryShare(directory)
		if err != nil {
			return nil, fmt.Errorf("macvm: share %s: %w", share.HostPath, err)
		}
		device, err := vz.NewVirtioFileSystemDeviceConfiguration(tag)
		if err != nil {
			return nil, fmt.Errorf("macvm: file system device %s: %w", tag, err)
		}
		device.SetDirectoryShare(single)
		devices = append(devices, device)
	}
	return devices, nil
}

// clamp bounds a size to the range Virtualization.framework accepts, asking the
// framework rather than assuming: the range depends on the host and the OS
// version, and a configuration outside it is rejected with an error that does
// not say which field was wrong.
func clamp(cpuCount uint, memoryBytes uint64) (uint, uint64) {
	cpuCount = min(max(cpuCount, vz.VirtualMachineConfigurationMinimumAllowedCPUCount()), vz.VirtualMachineConfigurationMaximumAllowedCPUCount())
	memoryBytes = min(max(memoryBytes, vz.VirtualMachineConfigurationMinimumAllowedMemorySize()), vz.VirtualMachineConfigurationMaximumAllowedMemorySize())
	return cpuCount, memoryBytes
}
