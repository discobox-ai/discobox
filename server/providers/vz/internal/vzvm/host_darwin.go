//go:build darwin && cgo

package vzvm

import "github.com/Code-Hex/vz/v3"

// Clamp bounds resources to the range Virtualization.framework accepts.
//
// The bounds come from the framework rather than from constants here: the
// allowed range depends on the host and the OS version, and a configuration
// outside it is rejected at VM creation with an error that does not say which
// field was wrong.
func Clamp(resources HostResources) HostResources {
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
