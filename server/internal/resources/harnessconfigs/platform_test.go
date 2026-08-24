package harnessconfigs

import (
	"runtime"
	"testing"
)

// A multi-arch image has one config digest per architecture, so inspecting it
// without naming one asks the wrong question — and go-containerregistry answers
// linux/amd64. That is how an Apple Silicon Mac pinned the amd64 digest of a
// harness image its arm64 pool would never hold, and every sandbox on it
// refused to launch.
func TestPoolPlatformIsThisMachinesLinux(t *testing.T) {
	platform := poolPlatform()
	// Linux whatever the control plane runs: the pool is a Linux machine even
	// when the server is macOS or Windows.
	if platform.OS != "linux" {
		t.Fatalf("OS = %q, want linux", platform.OS)
	}
	if platform.Architecture != runtime.GOARCH {
		t.Fatalf("Architecture = %q, want %q", platform.Architecture, runtime.GOARCH)
	}
	// The default this exists to override.
	if runtime.GOARCH != "amd64" && platform.Architecture == "amd64" {
		t.Fatal("fell back to the amd64 default on a non-amd64 machine")
	}
}
