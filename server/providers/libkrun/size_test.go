package libkrun

import (
	"testing"

	"github.com/discobox-ai/discobox/server/providers/dockerworker"
	"github.com/discobox-ai/discobox/server/providers/libkrun/internal/krunvm"
	"github.com/discobox-ai/discobox/server/providers/vmsize"
)

// A pool VM takes its size from the pool's own size first, then the provider's
// configuration, then the host, each clamped to what libkrun accepts - the same
// order every local VM provider uses.
func TestSizeForPrefersPoolThenProviderThenHost(t *testing.T) {
	host := krunvm.DefaultHostResources().Size()
	gib := int64(1024 * 1024 * 1024)
	for _, tc := range []struct {
		name     string
		spec     dockerworker.VMSpec
		provider vmsize.Size
		want     vmsize.Size
	}{
		{"host default", dockerworker.VMSpec{}, vmsize.Size{}, host},
		{"provider size", dockerworker.VMSpec{}, vmsize.Size{VCPUs: 6, MemoryMiB: 8192}, vmsize.Size{VCPUs: 6, MemoryMiB: 8192}},
		{"pool size wins", dockerworker.VMSpec{CPUVCPUs: 2.5, MemoryBytes: 3 * gib}, vmsize.Size{VCPUs: 6, MemoryMiB: 8192}, vmsize.Size{VCPUs: 3, MemoryMiB: 3072}},
		{"pool memory only", dockerworker.VMSpec{MemoryBytes: 3 * gib}, vmsize.Size{VCPUs: 6}, vmsize.Size{VCPUs: 6, MemoryMiB: 3072}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			driver := &Driver{size: tc.provider}
			if got := driver.sizeFor(tc.spec); got != tc.want {
				t.Fatalf("sizeFor = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A host bigger than libkrun takes, or a pool size set without libkrun's floor
// in mind, still gets a VM that boots rather than a launcher that refuses it.
func TestSizeForClampsToLibkrunLimits(t *testing.T) {
	got := (&Driver{}).sizeFor(dockerworker.VMSpec{CPUVCPUs: 1000, MemoryBytes: 64 * 1024 * 1024})
	if got.VCPUs != 255 || got.MemoryMiB != 1024 {
		t.Fatalf("sizeFor = %+v, want libkrun's 255 vCPUs and 1024 MiB floor", got)
	}
}
