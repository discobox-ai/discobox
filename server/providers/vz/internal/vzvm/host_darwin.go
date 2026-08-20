//go:build darwin

package vzvm

import (
	"runtime"

	"github.com/Code-Hex/vz/v3"
	"golang.org/x/sys/unix"
)

// DefaultHostResources sizes a pool VM from this Mac, clamped to what
// Virtualization.framework will actually accept.
//
// The clamps come from the framework rather than from constants here: the
// allowed range depends on the host and the OS version, and a configuration
// outside it is rejected at VM creation with an error that does not say which
// field was wrong.
func DefaultHostResources() HostResources {
	resources := HostResources{
		CPUCount:    uint(runtime.NumCPU()),
		MemoryBytes: hostMemoryBytes() / 2,
	}

	if minimum := vz.VirtualMachineConfigurationMinimumAllowedCPUCount(); resources.CPUCount < minimum {
		resources.CPUCount = minimum
	}
	if maximum := vz.VirtualMachineConfigurationMaximumAllowedCPUCount(); maximum > 0 && resources.CPUCount > maximum {
		resources.CPUCount = maximum
	}
	if minimum := vz.VirtualMachineConfigurationMinimumAllowedMemorySize(); resources.MemoryBytes < minimum {
		resources.MemoryBytes = minimum
	}
	if maximum := vz.VirtualMachineConfigurationMaximumAllowedMemorySize(); maximum > 0 && resources.MemoryBytes > maximum {
		resources.MemoryBytes = maximum
	}
	return resources
}

func hostMemoryBytes() uint64 {
	// hw.memsize is the machine's physical memory. There is no portable Go way
	// to ask, and shelling out to sysctl(8) to learn a constant would be worse.
	memory, err := unix.SysctlUint64("hw.memsize")
	if err != nil || memory == 0 {
		return fallbackMemoryBytes
	}
	return memory
}
