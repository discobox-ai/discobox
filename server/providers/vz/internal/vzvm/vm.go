// Package vzvm is the Virtualization.framework boundary for the vz provider.
//
// It exists for the same reason wslc has internal/wslcsession: the framework
// binding is cgo and darwin-only, and confining it here keeps the driver, its
// configuration, and its tests buildable on every platform.
//
// A VM is an in-process object with no representation on disk, which is what
// makes "the VM dies with the server" a property rather than a policy
// (ADR 0062 §1). Nothing here is re-adoptable across a server restart, and
// nothing tries to be.
package vzvm

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// ErrUnsupported reports that this build cannot run Virtualization.framework.
// What is wrong differs by build, so the stub wraps it with the reason: off
// macOS the framework does not exist, and on macOS without cgo the bindings
// were not compiled in.
var ErrUnsupported = errors.New("vzvm: this build cannot run Apple Virtualization.framework")

// Options describes one guest. Disk order is fixed and load-bearing: the guest
// addresses the root filesystem as /dev/vda, durable data as /dev/vdb, and
// disposable cache as /dev/vdc.
type Options struct {
	// Name identifies the VM in logs. It is not visible to the guest.
	Name string
	// CPUCount and MemoryBytes are the guest's envelope.
	CPUCount    uint
	MemoryBytes uint64
	// KernelPath is an uncompressed kernel image. Virtualization.framework
	// rejects a compressed vmlinuz, which is why the guest image publishes the
	// decompressed artifact (ADR 0062 §8).
	KernelPath string
	// InitrdPath brings up the virtio drivers the distribution builds as
	// modules, so the root filesystem is reachable at all.
	InitrdPath    string
	KernelCmdline string
	// RootImagePath is attached read-only and shared by every pool on this host.
	RootImagePath string
	// DataImagePath and CacheImagePath are this pool's private raw disks.
	DataImagePath  string
	CacheImagePath string
	// ConsoleLogPath receives the guest's serial console, appended across
	// restarts. It is the only diagnostic available for a guest that fails
	// before its Docker daemon answers, which is exactly when a pool host
	// console cannot help.
	ConsoleLogPath string
	// SharedDirectories are host directories the guest sees over virtiofs.
	// Each is one device, addressed by its tag, and the guest decides where a
	// tag mounts.
	SharedDirectories []SharedDirectory
}

// SharedDirectory is one host directory exported to the guest over virtiofs.
//
// The tag is the whole of the contract with the guest image: the host attaches
// a device carrying it, and the guest's fstab mounts that tag somewhere. Since
// the guest image is released on its own line, a tag is as load-bearing as a
// VSOCK port number — renaming one is a coordinated release, not a rename.
type SharedDirectory struct {
	// Tag names the device inside the guest. Virtualization.framework limits
	// it to 36 bytes and rejects "." and "..".
	Tag string
	// HostPath is the directory on the Mac. It is a macOS path whatever the
	// platform this package is compiled for, so it is checked with path, not
	// filepath: on Windows filepath.IsAbs rejects "/Users".
	HostPath string
	// ReadOnly is enforced by the host, not by how the guest mounts it.
	ReadOnly bool
}

// maxSharedDirectoryTagLen is Virtualization.framework's limit on a virtiofs
// tag, checked here so an over-long one is a configuration error rather than an
// opaque framework rejection at pool start.
const maxSharedDirectoryTagLen = 36

func (s SharedDirectory) validate() error {
	tag := strings.TrimSpace(s.Tag)
	switch {
	case tag == "":
		return errors.New("vzvm: shared directory tag is required")
	case len(tag) > maxSharedDirectoryTagLen:
		return fmt.Errorf("vzvm: shared directory tag %q exceeds %d bytes", tag, maxSharedDirectoryTagLen)
	case tag == "." || tag == "..":
		return fmt.Errorf("vzvm: shared directory tag %q is reserved", tag)
	}
	if host := strings.TrimSpace(s.HostPath); host == "" || !path.IsAbs(host) {
		return fmt.Errorf("vzvm: shared directory %q needs an absolute host path, got %q", tag, s.HostPath)
	}
	return nil
}

// Validate checks what can be checked without a hypervisor, so a
// misconfiguration is reported before a pool tries to start.
func (o Options) Validate() error {
	if o.CPUCount == 0 {
		return errors.New("vzvm: cpu count must be at least 1")
	}
	if o.MemoryBytes == 0 {
		return errors.New("vzvm: memory must be greater than zero")
	}
	for field, value := range map[string]string{
		"kernel":      o.KernelPath,
		"root image":  o.RootImagePath,
		"data image":  o.DataImagePath,
		"cache image": o.CacheImagePath,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("vzvm: %s path is required", field)
		}
	}
	tags := map[string]struct{}{}
	for _, share := range o.SharedDirectories {
		if err := share.validate(); err != nil {
			return err
		}
		tag := strings.TrimSpace(share.Tag)
		if _, ok := tags[tag]; ok {
			return fmt.Errorf("vzvm: shared directory tag %q is attached twice", tag)
		}
		tags[tag] = struct{}{}
	}
	return nil
}
