//go:build !darwin || !cgo

package vzvm

// Clamp has no framework to ask in a build with no bindings, where no VM is
// ever started; it only keeps both fields at least one unit, so the provider's
// configuration and tests stay meaningful.
func Clamp(resources HostResources) HostResources {
	if resources.CPUCount < 1 {
		resources.CPUCount = 1
	}
	if resources.MemoryBytes < mib {
		resources.MemoryBytes = mib
	}
	return resources
}
