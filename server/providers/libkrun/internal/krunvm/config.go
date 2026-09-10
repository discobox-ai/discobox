// Package krunvm is the whole of the libkrun surface: the manifest a pool VM is
// described by, and the code that turns one into a running microVM.
//
// It is isolated the way wslc/internal/wslcsession and vz/internal/vzvm are, so
// the driver, its configuration, and its tests compile and run on every
// platform. Off linux/amd64 every entry point returns ErrUnsupported.
//
// Unlike those two, the code here does not run in the server process. libkrun's
// krun_start_enter consumes its calling process, so a VM needs a process of its
// own; the server re-executes itself into the hidden launcher subcommand and
// that child is where Run happens (ADR 0062 §9). The manifest is the boundary
// between the two halves, which is why it is a file format with a version
// rather than a Go struct passed by memory.
package krunvm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ConfigVersion is the manifest format the launcher accepts. The launcher is
// this repository's own binary re-executed, so the two halves never disagree in
// practice; the version is what makes a stale manifest left in a runtime
// directory fail loudly instead of being read with the wrong meaning.
const ConfigVersion = 3

// VSOCKDirection says which side opens a connection on a VSOCK port.
type VSOCKDirection string

const (
	// GuestConnects terminates the guest's outbound connections at a Unix
	// socket the host already listens on.
	GuestConnects VSOCKDirection = "guestConnects"
	// HostConnects makes libkrun listen on a Unix socket and forward each
	// connection to the guest's port.
	HostConnects VSOCKDirection = "hostConnects"
)

// VSOCKMapping is one guest VSOCK port terminated at a host Unix socket.
type VSOCKMapping struct {
	Name      string         `json:"name"`
	Port      uint32         `json:"port"`
	Socket    string         `json:"socket"`
	Direction VSOCKDirection `json:"direction"`
}

// Config is one pool VM, as the driver writes it and the launcher reads it.
type Config struct {
	Version    int    `json:"version"`
	PoolID     string `json:"poolId"`
	RuntimeDir string `json:"runtimeDir"`

	// KernelImage is the ELF vmlinux libkrun boots. It is not the kernel the
	// shared guest image carries: libkrun needs libkrunfw's patches, so its
	// kernel is a separate artifact with its own release line.
	KernelImage string `json:"kernelImage"`
	// RootDisk is the read-only raw ext4 root from the guest image, shared by
	// every pool on the host. DataDisk and CacheDisk are this pool's own.
	RootDisk  string `json:"rootDisk"`
	DataDisk  string `json:"dataDisk"`
	CacheDisk string `json:"cacheDisk"`

	// PasstSocket is where passt and libkrun meet. PasstPath overrides the
	// executable, which is otherwise resolved from PATH.
	PasstSocket string `json:"passtSocket"`
	PasstPath   string `json:"passtPath,omitempty"`
	// LibraryPath overrides the libkrun shared object, which is otherwise
	// dlopened by soname from the loader's search path.
	LibraryPath string `json:"libraryPath,omitempty"`

	ConsoleLog string `json:"consoleLog"`
	VCPUs      int    `json:"vcpus"`
	MemoryMiB  int    `json:"memoryMiB"`
	MACAddress string `json:"macAddress"`

	VSOCK []VSOCKMapping `json:"vsock"`
}

// Validate reports whether a manifest describes a VM that can be started. It
// runs on both sides of the boundary: in the driver, so a bad configuration is
// a provider error rather than a child that exits into a log file, and in the
// launcher, so a manifest is never trusted merely because it was on disk.
func (c Config) Validate() error {
	if c.Version != ConfigVersion {
		return fmt.Errorf("unsupported manifest version %d, want %d", c.Version, ConfigVersion)
	}
	if strings.TrimSpace(c.PoolID) == "" {
		return errors.New("poolId is required")
	}
	if c.VCPUs < 1 || c.VCPUs > 255 {
		return fmt.Errorf("vcpus must be between 1 and 255, got %d", c.VCPUs)
	}
	if c.MemoryMiB < 256 {
		return fmt.Errorf("memoryMiB must be at least 256, got %d", c.MemoryMiB)
	}
	for name, path := range map[string]string{
		"runtimeDir":  c.RuntimeDir,
		"kernelImage": c.KernelImage,
		"rootDisk":    c.RootDisk,
		"dataDisk":    c.DataDisk,
		"cacheDisk":   c.CacheDisk,
		"passtSocket": c.PasstSocket,
		"consoleLog":  c.ConsoleLog,
	} {
		if err := validateAbsolutePath(name, path); err != nil {
			return err
		}
	}
	// The console log and passt's socket are the launcher's own files. Keeping
	// them inside the runtime directory is what makes deleting that directory
	// the whole of cleaning up after a VM.
	for name, path := range map[string]string{
		"passtSocket": c.PasstSocket,
		"consoleLog":  c.ConsoleLog,
	} {
		if !isBeneath(c.RuntimeDir, path) {
			return fmt.Errorf("%s %s must be beneath runtimeDir %s", name, path, c.RuntimeDir)
		}
	}
	if _, err := c.MAC(); err != nil {
		return err
	}
	return c.validateVSOCK()
}

func (c Config) validateVSOCK() error {
	names := map[string]struct{}{}
	ports := map[uint32]struct{}{}
	sockets := map[string]struct{}{}
	for _, mapping := range c.VSOCK {
		if strings.TrimSpace(mapping.Name) == "" {
			return errors.New("vsock mapping name is required")
		}
		if _, ok := names[mapping.Name]; ok {
			return fmt.Errorf("duplicate vsock mapping name %q", mapping.Name)
		}
		names[mapping.Name] = struct{}{}
		if mapping.Port < 1024 {
			return fmt.Errorf("vsock mapping %q uses privileged port %d", mapping.Name, mapping.Port)
		}
		if _, ok := ports[mapping.Port]; ok {
			return fmt.Errorf("duplicate vsock port %d", mapping.Port)
		}
		ports[mapping.Port] = struct{}{}
		if err := validateAbsolutePath("vsock socket", mapping.Socket); err != nil {
			return err
		}
		if _, ok := sockets[mapping.Socket]; ok {
			return fmt.Errorf("duplicate vsock socket %s", mapping.Socket)
		}
		sockets[mapping.Socket] = struct{}{}
		switch mapping.Direction {
		case GuestConnects:
			// The guest's outbound target is the server's own listening socket,
			// which is deliberately outside the runtime directory.
		case HostConnects:
			// libkrun creates this one. It is private to the pool, so a second
			// server process cannot find it and talk to somebody else's VM.
			if !isBeneath(c.RuntimeDir, mapping.Socket) {
				return fmt.Errorf("host-listening vsock socket %s must be beneath runtimeDir %s", mapping.Socket, c.RuntimeDir)
			}
		default:
			return fmt.Errorf("vsock mapping %q has unknown direction %q", mapping.Name, mapping.Direction)
		}
	}
	return nil
}

// MAC parses the guest's hardware address, which must be locally administered
// and unicast: it is invented per pool, so it may not claim a vendor prefix and
// may not be a multicast address the guest would refuse to bring an interface
// up with.
func (c Config) MAC() ([6]byte, error) {
	var mac [6]byte
	parts := strings.Split(c.MACAddress, ":")
	if len(parts) != 6 {
		return mac, fmt.Errorf("invalid macAddress %q", c.MACAddress)
	}
	for i, part := range parts {
		if len(part) != 2 {
			return mac, fmt.Errorf("invalid macAddress %q", c.MACAddress)
		}
		value, err := strconv.ParseUint(part, 16, 8)
		if err != nil {
			return mac, fmt.Errorf("invalid macAddress %q", c.MACAddress)
		}
		mac[i] = byte(value)
	}
	if mac[0]&0x01 != 0 {
		return mac, fmt.Errorf("macAddress %q must be unicast", c.MACAddress)
	}
	if mac[0]&0x02 == 0 {
		return mac, fmt.Errorf("macAddress %q must be locally administered", c.MACAddress)
	}
	return mac, nil
}

// OwnedSockets are the Unix sockets starting this VM creates: passt's, and one
// for every port libkrun listens on for the host.
//
// It deliberately excludes a guest-initiated port's socket. That one is the
// server's own listening socket, which already exists and belongs to something
// else — removing it, or waiting for a launcher to create it, would be a
// category error in opposite directions.
//
// One list, because two callers have to agree about it exactly: the driver
// deletes these before starting a launcher and then treats their reappearance
// as the VM being up, so a name in one set and not the other is either a
// readiness check a dead VM can satisfy or one nothing ever will.
func (c Config) OwnedSockets() []string {
	sockets := []string{c.PasstSocket}
	for _, mapping := range c.VSOCK {
		if mapping.Direction == HostConnects {
			sockets = append(sockets, mapping.Socket)
		}
	}
	return sockets
}

// RemoveOwnedSockets clears the sockets a previous VM for this pool left
// behind. libkrun binds them and does not unlink them when it is killed, which
// is how every launcher dies, so after any unclean exit these are exactly the
// paths that exist and mean nothing.
//
// A path that is not a socket is refused rather than removed: it is not this
// pool's to delete, and a manifest that names one is a bug worth failing on.
func RemoveOwnedSockets(cfg Config) error {
	for _, path := range cfg.OwnedSockets() {
		if err := removeStaleSocket(path); err != nil {
			return err
		}
	}
	return nil
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect socket %s: %w", path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to replace non-socket path %s", path)
	}
	return os.Remove(path)
}

// PasstExecutable is the passt binary to run, resolved from PATH when the
// manifest does not name one.
func (c Config) PasstExecutable() string {
	if value := strings.TrimSpace(c.PasstPath); value != "" {
		return value
	}
	return "passt"
}

// ValidateDiskFiles reports whether the artifacts the manifest names are
// present. It is separate from Validate because the manifest can be checked
// anywhere while the files exist only on the host that will boot them.
func (c Config) ValidateDiskFiles() error {
	for name, path := range map[string]string{
		"kernelImage": c.KernelImage,
		"rootDisk":    c.RootDisk,
		"dataDisk":    c.DataDisk,
		"cacheDisk":   c.CacheDisk,
	} {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("inspect %s %s: %w", name, path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s %s must be a regular file", name, path)
		}
	}
	return nil
}

func validateAbsolutePath(name, path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s %q must be an absolute path", name, path)
	}
	if path != filepath.Clean(path) {
		return fmt.Errorf("%s %q must be a clean path", name, path)
	}
	return nil
}

func isBeneath(parent, child string) bool {
	if parent == child {
		return false
	}
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
