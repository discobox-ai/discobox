//go:build !linux

package krunvm

import "runtime"

// DefaultHostResources reports sizing that keeps the provider's configuration
// and tests meaningful off Linux, where no VM is ever started.
func DefaultHostResources() HostResources {
	cpus := uint(runtime.NumCPU())
	if cpus > maxVCPUs {
		cpus = maxVCPUs
	}
	return HostResources{CPUCount: cpus, MemoryBytes: fallbackMemoryBytes}
}
