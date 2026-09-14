// Package vmsize decides how big a local pool VM is.
//
// Every provider that runs one VM per pool on the server's own machine - vz on
// macOS, libkrun on Linux, wslc on Windows - sizes it the same way, so the
// rule lives here rather than as a constant in each. A pool VM is the
// developer's whole Linux environment, so by default it is sized from the host
// rather than from a fixed guess: every logical CPU, and half the physical
// memory. Neither is a reservation. vCPUs are scheduled against the host's own
// work, and each backend's guest balloons memory back to the host, so an idle
// pool does not hold its ceiling.
//
// That default can be narrowed twice. A provider instance's configuration sets
// a size for every pool it hosts, and a pool's own size (Pool.CPUVCPUs,
// Pool.MemoryBytes) sets a size for itself. The most specific setting wins,
// field by field: a pool that names only its memory still takes its vCPUs from
// the provider, or from the host.
package vmsize

import (
	"math"
	"runtime"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// Size is a local pool VM's vCPUs and memory. A zero field is unset at
// whatever level the Size describes, and is filled from the next level down by
// Resolve.
type Size struct {
	VCPUs     int
	MemoryMiB int
}

// fallbackMemoryMiB stands in for half of a host whose physical memory cannot be
// read. It is the fixed default the VM providers used before they sized from the
// host, so a platform that cannot answer gets what it always got.
const fallbackMemoryMiB = 4096

// Host is the size every local VM provider defaults to: every logical CPU and
// half of the physical memory. Backends clamp it to what they will accept.
func Host() Size {
	size := Size{VCPUs: runtime.NumCPU(), MemoryMiB: fallbackMemoryMiB}
	if total, ok := physicalMemoryBytes(); ok && total > 0 {
		size.MemoryMiB = int(total / 2 / mib)
	}
	return size
}

const mib = 1024 * 1024

// PoolSizeFields are the pool size fields a provider that sizes its VMs with this
// package acts on, for its ProviderDefinition: the fields FromPool reads.
// Declaring it from here keeps the declaration and the code that honors it in
// one place, so the pool service can never accept a field no VM is sized from.
func PoolSizeFields() []sandbox.PoolSizeField {
	return []sandbox.PoolSizeField{sandbox.PoolSizeCPU, sandbox.PoolSizeMemory}
}

// FromPool converts a pool's size into VM units.
//
// A pool's size is fractional vCPUs and exact bytes; a VM is whole vCPUs and
// whole MiB. Both round up, so the VM is never smaller than what the pool asked
// for. Zero or negative fields stay unset.
func FromPool(cpuVCPUs float64, memoryBytes int64) Size {
	var size Size
	if cpuVCPUs > 0 {
		size.VCPUs = int(math.Ceil(cpuVCPUs))
	}
	if memoryBytes > 0 {
		size.MemoryMiB = int((memoryBytes + mib - 1) / mib)
	}
	return size
}

// Resolve returns, for each field, the first positive value among sizes,
// which are given most specific first. A field no size sets comes from Host.
//
//	vmsize.Resolve(vmsize.FromPool(pool.CPUVCPUs, pool.MemoryBytes), providerSize)
func Resolve(sizes ...Size) Size {
	var resolved Size
	for _, size := range sizes {
		if resolved.VCPUs <= 0 && size.VCPUs > 0 {
			resolved.VCPUs = size.VCPUs
		}
		if resolved.MemoryMiB <= 0 && size.MemoryMiB > 0 {
			resolved.MemoryMiB = size.MemoryMiB
		}
	}
	if resolved.VCPUs <= 0 || resolved.MemoryMiB <= 0 {
		host := Host()
		if resolved.VCPUs <= 0 {
			resolved.VCPUs = host.VCPUs
		}
		if resolved.MemoryMiB <= 0 {
			resolved.MemoryMiB = host.MemoryMiB
		}
	}
	return resolved
}

// Clamp bounds a size to what a backend accepts. A maximum of zero or less is
// no maximum.
//
// It clamps rather than refusing, because the value being bounded is usually
// not one anybody typed for that backend: the host default on a machine bigger
// than the backend supports, or a pool size set without the backend's floor
// in mind.
func (s Size) Clamp(minVCPUs, maxVCPUs, minMemoryMiB, maxMemoryMiB int) Size {
	s.VCPUs = clamp(s.VCPUs, minVCPUs, maxVCPUs)
	s.MemoryMiB = clamp(s.MemoryMiB, minMemoryMiB, maxMemoryMiB)
	return s
}

func clamp(value, minimum, maximum int) int {
	if value < minimum {
		value = minimum
	}
	if maximum > 0 && value > maximum {
		value = maximum
	}
	return value
}
