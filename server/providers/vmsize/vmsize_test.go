package vmsize

import (
	"runtime"
	"testing"
)

// The default the user sees is "every CPU and half the memory"; a host that
// reports its memory has to get exactly that, not a rounded guess.
func TestHostIsEveryCPUAndHalfTheMemory(t *testing.T) {
	host := Host()
	if host.VCPUs != runtime.NumCPU() {
		t.Errorf("host vCPUs = %d, want every logical CPU (%d)", host.VCPUs, runtime.NumCPU())
	}
	total, ok := physicalMemoryBytes()
	if !ok {
		if host.MemoryMiB != fallbackMemoryMiB {
			t.Errorf("host memory = %d MiB with no readable host memory, want the %d MiB fallback", host.MemoryMiB, fallbackMemoryMiB)
		}
		return
	}
	if want := int(total / 2 / mib); host.MemoryMiB != want {
		t.Errorf("host memory = %d MiB, want half of %d bytes (%d MiB)", host.MemoryMiB, total, want)
	}
}

// Each field falls through on its own: a pool that sets only memory still gets
// the provider's vCPUs, and a provider that sets nothing hands both to the host.
func TestResolveTakesTheMostSpecificValueFieldByField(t *testing.T) {
	host := Host()
	for _, tc := range []struct {
		name     string
		pool     Size
		provider Size
		want     Size
	}{
		{"nothing set", Size{}, Size{}, host},
		{"provider only", Size{}, Size{VCPUs: 6, MemoryMiB: 8192}, Size{VCPUs: 6, MemoryMiB: 8192}},
		{"pool overrides provider", Size{VCPUs: 3, MemoryMiB: 2048}, Size{VCPUs: 6, MemoryMiB: 8192}, Size{VCPUs: 3, MemoryMiB: 2048}},
		{"pool memory, provider vCPUs", Size{MemoryMiB: 2048}, Size{VCPUs: 6}, Size{VCPUs: 6, MemoryMiB: 2048}},
		{"pool vCPUs, host memory", Size{VCPUs: 3}, Size{}, Size{VCPUs: 3, MemoryMiB: host.MemoryMiB}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Resolve(tc.pool, tc.provider); got != tc.want {
				t.Fatalf("Resolve(%+v, %+v) = %+v, want %+v", tc.pool, tc.provider, got, tc.want)
			}
		})
	}
}

// A VM is whole vCPUs and MiB, and must never come out smaller than the pool
// asked for, so both conversions round up.
func TestFromPoolNeverUndersizesTheVM(t *testing.T) {
	for _, tc := range []struct {
		cpu    float64
		memory int64
		want   Size
	}{
		{0, 0, Size{}},
		{-1, -1, Size{}},
		{1.5, 0, Size{VCPUs: 2}},
		{4, 0, Size{VCPUs: 4}},
		{0.1, 1, Size{VCPUs: 1, MemoryMiB: 1}},
		{0, 8 * mib, Size{MemoryMiB: 8}},
		{0, 8*mib + 1, Size{MemoryMiB: 9}},
	} {
		if got := FromPool(tc.cpu, tc.memory); got != tc.want {
			t.Errorf("FromPool(%v, %v) = %+v, want %+v", tc.cpu, tc.memory, got, tc.want)
		}
	}
}

func TestClampBoundsBothFields(t *testing.T) {
	got := Size{VCPUs: 300, MemoryMiB: 100}.Clamp(1, 255, 256, 0)
	if want := (Size{VCPUs: 255, MemoryMiB: 256}); got != want {
		t.Fatalf("Clamp = %+v, want %+v", got, want)
	}
	got = Size{VCPUs: 0, MemoryMiB: 1 << 20}.Clamp(1, 0, 256, 65536)
	if want := (Size{VCPUs: 1, MemoryMiB: 65536}); got != want {
		t.Fatalf("Clamp = %+v, want %+v", got, want)
	}
}
