package vzvm

import "github.com/discobox-ai/discobox/server/providers/vmsize"

// HostResources are a pool VM's vCPUs and memory in the units
// Virtualization.framework takes.
//
// The defaults come from vmsize, the rule every local VM provider shares: every
// vCPU and half the memory. Neither is a reservation. vCPUs are shared with
// macOS by the scheduler, and the guest has a memory balloon, so an idle pool
// hands memory back instead of holding its ceiling.
type HostResources struct {
	CPUCount    uint
	MemoryBytes uint64
}

const mib = 1024 * 1024

// DefaultHostResources sizes a pool VM from this machine, clamped to what the
// framework accepts.
func DefaultHostResources() HostResources {
	return ClampSize(vmsize.Host())
}

// ClampSize converts a resolved size to framework units and clamps it to what
// Virtualization.framework will actually accept.
func ClampSize(size vmsize.Size) HostResources {
	return Clamp(HostResources{CPUCount: uint(max(size.VCPUs, 0)), MemoryBytes: uint64(max(size.MemoryMiB, 0)) * mib})
}

// Size is resources back in vmsize units, for comparing against a resolved size.
func (r HostResources) Size() vmsize.Size {
	return vmsize.Size{VCPUs: int(r.CPUCount), MemoryMiB: int(r.MemoryBytes / mib)}
}
