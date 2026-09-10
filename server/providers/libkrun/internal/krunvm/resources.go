package krunvm

// HostResources are the VM sizing defaults derived from the machine the server
// runs on.
//
// A pool VM hosts every sandbox on that pool, so it is sized from the host
// rather than from a fixed guess: every vCPU, and half the memory. This is the
// same rule vzvm applies, and for the same reason — neither number is a
// reservation. vCPUs are shared with the host by the scheduler, and the guest
// has a memory balloon, so an idle pool hands memory back instead of holding
// its ceiling.
type HostResources struct {
	CPUCount    uint
	MemoryBytes uint64
}

// fallbackMemoryBytes is used where the host's memory cannot be read, which is
// every platform the provider does not run on.
const fallbackMemoryBytes = 4096 * 1024 * 1024

// maxVCPUs is libkrun's ceiling: krun_set_vm_config takes the vCPU count as a
// uint8, so a machine with more cores than this is clamped rather than
// wrapping around to a VM with two.
const maxVCPUs = 255

// minMemoryMiB is the floor a guest can boot systemd, Docker, and a pool agent
// under. A machine small enough to fall below half of it is given the floor
// and left to fail honestly on memory pressure rather than on a kernel panic
// during boot.
const minMemoryMiB = 1024
