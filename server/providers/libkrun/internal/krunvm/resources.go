package krunvm

import "github.com/discobox-ai/discobox/server/providers/vmsize"

// HostResources are a pool VM's vCPUs and memory in the units the launcher
// takes.
//
// A pool VM hosts every sandbox on that pool, so it is sized from the host
// rather than from a fixed guess: every vCPU, and half the memory - the rule
// every local VM provider shares, in vmsize. Neither number is a reservation.
// vCPUs are shared with the host by the scheduler, and the guest has a memory
// balloon, so an idle pool hands memory back instead of holding its ceiling.
type HostResources struct {
	CPUCount    uint
	MemoryBytes uint64
}

const mib = 1024 * 1024

// maxVCPUs is libkrun's ceiling: krun_set_vm_config takes the vCPU count as a
// uint8, so a machine with more cores than this is clamped rather than
// wrapping around to a VM with two.
const maxVCPUs = 255

// minMemoryMiB is the floor a guest can boot systemd, Docker, and a pool agent
// under. A machine small enough to fall below half of it is given the floor
// and left to fail honestly on memory pressure rather than on a kernel panic
// during boot.
const minMemoryMiB = 1024

// DefaultHostResources sizes a pool VM from this machine, clamped to what
// libkrun accepts.
func DefaultHostResources() HostResources {
	return ClampSize(vmsize.Host())
}

// ClampSize converts a resolved size to launcher units and clamps it to what
// libkrun accepts. It clamps rather than refusing: the value is rarely one
// anybody typed for libkrun - a host with more cores than it takes, or a pool
// size set without its floor in mind.
func ClampSize(size vmsize.Size) HostResources {
	clamped := size.Clamp(1, maxVCPUs, minMemoryMiB, 0)
	return HostResources{CPUCount: uint(clamped.VCPUs), MemoryBytes: uint64(clamped.MemoryMiB) * mib}
}

// Size is resources back in vmsize units, for comparing against a resolved size.
func (r HostResources) Size() vmsize.Size {
	return vmsize.Size{VCPUs: int(r.CPUCount), MemoryMiB: int(r.MemoryBytes / mib)}
}
