//go:build !darwin

package vzvm

import "runtime"

// DefaultHostResources reports sizing that keeps the provider's configuration
// and tests meaningful off darwin, where no VM is ever started.
func DefaultHostResources() HostResources {
	return HostResources{CPUCount: uint(runtime.NumCPU()), MemoryBytes: fallbackMemoryBytes}
}
