package service

import (
	"runtime"
	"testing"
)

// A release build on Linux installs libkrun, so a fresh install gets a VM
// boundary; a development build keeps the host's Docker; a configured choice
// decides either way (ADR 0148 §1, §4).
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
			if got := defaultProviderType(tc.configured, tc.released); got != tc.want {
				t.Fatalf("defaultProviderType(%q, %v) = %q, want %q", tc.configured, tc.released, got, tc.want)
			}
		})
	}
}

// The libkrun instance a first start installs carries no configuration: the
// image it boots brings its own runtime, so nothing is installed or named on
// the host (ADR 0148 §5).
func TestDefaultLibkrunInstanceNeedsNoConfiguration(t *testing.T) {
	provider := defaultSandboxProvider("proj_1", "prov_1", "libkrun")
	if provider.Type != "libkrun" || provider.Disabled {
		t.Fatalf("provider = %+v, want an enabled libkrun instance", provider)
	}
	if len(provider.Config) != 0 {
		t.Fatalf("config = %s, want none", provider.Config)
	}
}
