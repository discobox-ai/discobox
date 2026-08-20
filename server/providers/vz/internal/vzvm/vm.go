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
	"strings"
)

// ErrUnsupported reports that this build cannot run Virtualization.framework.
var ErrUnsupported = errors.New("vzvm: Apple Virtualization.framework is available on macOS only")

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
	return nil
}
