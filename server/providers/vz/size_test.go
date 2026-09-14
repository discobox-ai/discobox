package vz

import (
	"testing"

	"github.com/discobox-ai/discobox/server/providers/dockerworker"
	"github.com/discobox-ai/discobox/server/providers/vmsize"
	"github.com/discobox-ai/discobox/server/providers/vz/internal/vzvm"
)

// A pool VM takes its size from the pool's own size first, then the
// provider's configuration, then the host, each clamped to what the framework
// accepts - the same order every local VM provider uses.
func TestSizeForPrefersPoolThenProviderThenHost(t *testing.T) {
	host := vzvm.DefaultHostResources().Size()
	gib := int64(1024 * 1024 * 1024)
	for _, tc := range []struct {
		name     string
		spec     dockerworker.VMSpec
		provider vmsize.Size
		want     vmsize.Size
	}{
		{"host default", dockerworker.VMSpec{}, vmsize.Size{}, host},
		{"provider size", dockerworker.VMSpec{}, vmsize.Size{VCPUs: 6, MemoryMiB: 8192}, vmsize.Size{VCPUs: 6, MemoryMiB: 8192}},
		{"pool size wins", dockerworker.VMSpec{CPUVCPUs: 3, MemoryBytes: 4 * gib}, vmsize.Size{VCPUs: 6, MemoryMiB: 8192}, vmsize.Size{VCPUs: 3, MemoryMiB: 4096}},
		{"pool vCPUs only", dockerworker.VMSpec{CPUVCPUs: 1.25}, vmsize.Size{MemoryMiB: 8192}, vmsize.Size{VCPUs: 2, MemoryMiB: 8192}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			driver := &Driver{size: tc.provider}
			if got := driver.sizeFor(tc.spec); got != tc.want {
				t.Fatalf("sizeFor = %+v, want %+v", got, tc.want)
			}
		})
	}
}
