package providers

import (
	"runtime"
	"slices"
	"testing"

	"github.com/discobox-ai/discobox/releasemanifest"
)

// A release build on Linux installs libkrun, so a fresh install gets a VM
// boundary; a development build keeps the host's Docker; a configured choice
// decides either way (ADR 26-09-23-473 §1, §4).
func TestLinuxDefaultProviderFollowsTheBuildAndTheConfiguration(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the default provider is chosen only on Linux")
	}
	for _, tc := range []struct {
		name       string
		configured string
		released   bool
		want       string
	}{
		{name: "release", released: true, want: "libkrun"},
		{name: "development", released: false, want: "docker"},
		{name: "release told docker", configured: "docker", released: true, want: "docker"},
		{name: "development told libkrun", configured: "libkrun", released: false, want: "libkrun"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DefaultProviderType(tc.configured, tc.released); got != tc.want {
				t.Fatalf("DefaultProviderType(%q, %v) = %q, want %q", tc.configured, tc.released, got, tc.want)
			}
		})
	}
}

// The images a first start stages follow the provider it installs: a host
// that will install Docker downloads no libkrun image, at a CLI's head start
// or the server's own (ADR 26-09-23-473 §6).
func TestReleaseImagesFollowTheProviderAFirstStartInstalls(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("only amd64 Linux boots the libkrun image")
	}
	images := releasemanifest.Images{
		PoolAgent:    "example.com/pool-agent:v1",
		SandboxAgent: "example.com/sandbox-agent:v1",
		VM:           "example.com/vm:v1",
		Libkrun:      "example.com/vm-krun:v1",
		Harnesses:    map[string]string{"shell": "example.com/shell:v1"},
	}
	if refs := ReleaseImages(images, "libkrun"); !slices.Contains(refs, images.Libkrun) {
		t.Fatalf("a libkrun first start stages %v, without the libkrun image", refs)
	}
	refs := ReleaseImages(images, "docker")
	if slices.Contains(refs, images.Libkrun) {
		t.Fatalf("a Docker first start stages %v, with the libkrun image", refs)
	}
	if !slices.Contains(refs, images.PoolAgent) {
		t.Fatalf("a Docker first start stages %v, without the pool agent", refs)
	}
}
