package vzvm

// HostResources are the VM sizing defaults derived from the machine the server
// runs on.
//
// A pool VM is the developer's whole Linux environment on a Mac, so it is sized
// from the host rather than from a fixed guess: every vCPU, and half the memory.
// Neither is a reservation. vCPUs are shared with macOS by the scheduler, and
// the guest has a memory balloon, so an idle pool hands memory back instead of
// holding its ceiling.
type HostResources struct {
	CPUCount    uint
	MemoryBytes uint64
}

// fallbackMemoryBytes is used only where the host's memory cannot be read,
// which off darwin is always — the provider does not run there.
const fallbackMemoryBytes = 4096 * 1024 * 1024
