package wslc

import (
	"testing"

	"github.com/discobox-ai/discobox/server/providers/dockerworker"
	"github.com/discobox-ai/discobox/server/providers/vmsize"
)

// A wslc VM used to be a fixed 2 vCPUs and 4 GiB whatever the machine. It now
// defaults to the host like every local VM provider, and the pool's own
// size outranks the provider's configuration, which outranks the host.
func TestSizeForPrefersPoolThenProviderThenHost(t *testing.T) {
	host := vmsize.Host()
	gib := int64(1024 * 1024 * 1024)
	for _, tc := range []struct {
		name   string
		config DriverConfig
		spec   dockerworker.VMSpec
		want   vmsize.Size
	}{
		{"host default", DriverConfig{}, dockerworker.VMSpec{}, host},
		{"provider size", DriverConfig{CPUCount: 6, MemoryMiB: 8192}, dockerworker.VMSpec{}, vmsize.Size{VCPUs: 6, MemoryMiB: 8192}},
		{"pool size wins", DriverConfig{CPUCount: 6, MemoryMiB: 8192}, dockerworker.VMSpec{CPUVCPUs: 3, MemoryBytes: 3 * gib}, vmsize.Size{VCPUs: 3, MemoryMiB: 3072}},
		{"pool memory, host vCPUs", DriverConfig{}, dockerworker.VMSpec{MemoryBytes: 3 * gib}, vmsize.Size{VCPUs: host.VCPUs, MemoryMiB: 3072}},
		// A pool size can be smaller than any VM that boots a dockerd; the VM is
		// clamped up to the floor rather than failing to start.
		{"tiny pool size clamps up", DriverConfig{}, dockerworker.VMSpec{CPUVCPUs: 0.5, MemoryBytes: 64 * 1024 * 1024}, vmsize.Size{VCPUs: 1, MemoryMiB: minMemoryMiB}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			driver, err := NewDriver(tc.config)
			if err != nil {
				t.Fatalf("NewDriver: %v", err)
			}
			if got := driver.sizeFor(tc.spec); got != tc.want {
				t.Fatalf("sizeFor = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Unset sizing has to reach the driver unset. A provider layer that filled in a
// number would outrank the host default it was only standing in for, which is
// how every wslc VM ended up with 2 vCPUs.
func TestDriverConfigLeavesUnsetSizingToTheDriver(t *testing.T) {
	cfg := driverConfig(Config{}, nil)
	if cfg.CPUCount != 0 || cfg.MemoryMiB != 0 {
		t.Fatalf("driver config sizing = %d vCPUs, %d MiB; want both unset", cfg.CPUCount, cfg.MemoryMiB)
	}
}
