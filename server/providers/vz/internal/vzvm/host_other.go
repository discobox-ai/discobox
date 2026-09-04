//go:build !darwin || !cgo

package vzvm

import "runtime"

// DefaultHostResources reports sizing that keeps the provider's configuration
// and tests meaningful in a build with no bindings, where no VM is ever
// started.
func DefaultHostResources() HostResources {
	return HostResources{CPUCount: uint(runtime.NumCPU()), MemoryBytes: fallbackMemoryBytes}
}
