package vz

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/obot-platform/discobox/pool-agent/wire"
	"github.com/obot-platform/discobox/server/providers/vz/internal/vzvm"
)

// The whole backend is expressed in the two transport URLs. VSOCK in both
// directions is what lets a Mac run pools with no TCP listener and therefore no
// firewall prompt, so a change that turns either into an IP endpoint is a
// behavior change, not a refactor.
func TestEngineConfigUsesVSOCKInBothDirections(t *testing.T) {
	cfg := engineConfig(Config{}, nil)

	controlPlane, err := wire.Parse(cfg.ControlPlaneURL)
	if err != nil {
		t.Fatalf("parse control plane URL %q: %v", cfg.ControlPlaneURL, err)
	}
	if controlPlane.Scheme != "vsock" {
		t.Errorf("control plane scheme = %q, want vsock", controlPlane.Scheme)
	}
	if controlPlane.Port != controlPlaneVSOCKPort {
		t.Errorf("control plane port = %d, want %d", controlPlane.Port, controlPlaneVSOCKPort)
	}

	agent, err := wire.Parse(cfg.AgentListenURL)
	if err != nil {
		t.Fatalf("parse agent listen URL %q: %v", cfg.AgentListenURL, err)
	}
	if agent.Scheme != "vsock" {
		t.Errorf("agent listen scheme = %q, want vsock", agent.Scheme)
	}
	if agent.Port != agentVSOCKPort {
		t.Errorf("agent listen port = %d, want %d", agent.Port, agentVSOCKPort)
	}
}

// A pool image is required on every backend; leaving it unset would start a
// container from whatever the daemon happened to have.
func TestEngineConfigResolvesAPoolImage(t *testing.T) {
	if image := engineConfig(Config{}, nil).Image; strings.TrimSpace(image) == "" {
		t.Fatal("engine config resolved no pool image")
	}
	const configured = "example.com/pool:custom"
	if image := engineConfig(Config{WorkerImage: configured}, nil).Image; image != configured {
		t.Fatalf("pool image = %q, want the configured value", image)
	}
}

// Defaults have to be absolute and Discobox-scoped: they name directories that
// hold multi-gigabyte disk images, and a relative one would land wherever the
// server happened to be started from.
func TestStorageDefaultsAreAbsoluteAndScoped(t *testing.T) {
	for name, got := range map[string]string{
		"state dir":       effectiveStateDir(""),
		"guest cache dir": effectiveGuestCacheDir(""),
	} {
		if !filepath.IsAbs(got) {
			t.Errorf("%s default %q is not absolute", name, got)
		}
		if !strings.Contains(strings.ToLower(got), "discobox") {
			t.Errorf("%s default %q is not discobox-scoped", name, got)
		}
	}
	if effectiveStateDir("") == effectiveGuestCacheDir("") {
		t.Error("pool disks and the guest image cache share a directory")
	}
}

func TestStorageDefaultsHonorConfiguredValues(t *testing.T) {
	const configured = "/var/discobox/pools"
	if got := effectiveStateDir("  " + configured + "  "); got != configured {
		t.Fatalf("effectiveStateDir = %q, want the trimmed configured value", got)
	}
	if got := effectiveGuestCacheDir(configured); got != configured {
		t.Fatalf("effectiveGuestCacheDir = %q, want the configured value", got)
	}
}

// The root filesystem is shared read-only by every pool on the host, so the
// kernel command line must never make it writable.
func TestKernelCmdlineMountsTheRootReadOnly(t *testing.T) {
	fields := strings.Fields(kernelCmdline)
	var sawReadOnly bool
	for _, field := range fields {
		if field == "ro" {
			sawReadOnly = true
		}
		if field == "rw" {
			t.Fatalf("kernel command line %q mounts the shared root writable", kernelCmdline)
		}
	}
	if !sawReadOnly {
		t.Fatalf("kernel command line %q does not mount the root read-only", kernelCmdline)
	}
	if !strings.Contains(kernelCmdline, "root=/dev/vda") {
		t.Fatalf("kernel command line %q does not boot the first disk", kernelCmdline)
	}
}

// Every VSOCK port is distinct, and matches the guest image's units. Two
// services sharing a port fails at guest boot with a bind error and no hint
// about which one lost.
func TestGuestPortsAreDistinct(t *testing.T) {
	seen := map[uint32]string{}
	for name, port := range map[string]uint32{
		"control plane": controlPlaneVSOCKPort,
		"pool agent":    agentVSOCKPort,
		"lifecycle":     lifecycleVSOCKPort,
		"docker":        dockerVSOCKPort,
	} {
		if other, ok := seen[port]; ok {
			t.Fatalf("%s and %s share VSOCK port %d", name, other, port)
		}
		seen[port] = name
	}
}

func TestValidateRejectsBadConfigurations(t *testing.T) {
	for name, cfg := range map[string]Config{
		"relative state dir": {StateDir: "pools"},
		"relative guest dir": {GuestImageDir: "guest"},
		"relative cache dir": {GuestImageCacheDir: "cache"},
		"negative vcpus":     {VCPUs: -1},
		"absurd vcpus":       {VCPUs: 4096},
		"tiny memory":        {MemoryMiB: 64},
		"absurd data disk":   {DataDiskGiB: 100000},
		"unparseable image":  {GuestImage: "NOT A REFERENCE"},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := Validate(raw); err == nil {
				t.Fatal("Validate accepted the configuration")
			}
		})
	}
}

// The default configuration must be usable: a Mac with nothing configured is
// the case this backend exists for.
func TestValidateAcceptsTheDefaultConfiguration(t *testing.T) {
	if err := Validate(json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Validate({}) = %v", err)
	}
}

// A local guest artifact directory is how a guest image built inside a pool VM
// is booted, so it must be accepted alongside — and take precedence over — the
// published image.
func TestValidateAcceptsLocalGuestArtifacts(t *testing.T) {
	raw, err := json.Marshal(Config{GuestImageDir: "/opt/discobox/guest"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(raw); err != nil {
		t.Fatalf("Validate = %v", err)
	}
	resolver, err := guestResolver(Config{GuestImageDir: "/opt/discobox/guest"})
	if err != nil {
		t.Fatalf("guestResolver: %v", err)
	}
	if resolver.Reference() != "" {
		t.Errorf("Reference() = %q, want empty while local artifacts are configured", resolver.Reference())
	}
}

// A pool VM is the developer's whole Linux environment on a Mac, so its sizing
// comes from the host rather than from a fixed guess.
func TestDefaultSizingComesFromTheHost(t *testing.T) {
	cpus := defaultVCPUs()
	if cpus < 1 {
		t.Fatalf("default vCPUs = %d", cpus)
	}
	if cpus != runtime.NumCPU() {
		// Only the framework's own clamp may narrow this; anything else means
		// the pool silently stopped using the machine it was given.
		t.Logf("default vCPUs = %d, host has %d (expected only if clamped by the framework)", cpus, runtime.NumCPU())
	}

	memory := defaultMemoryMiB()
	if memory < 512 {
		t.Fatalf("default memory = %d MiB, below the configured minimum", memory)
	}
	host := vzvm.DefaultHostResources()
	if got, want := memory, int(host.MemoryBytes/(1024*1024)); got != want {
		t.Errorf("default memory = %d MiB, want %d", got, want)
	}
}

// The disks are ceilings on sparse images, so the data disk is sized for a real
// workload. Shrinking it is a behavior change, not a tuning detail: it is where
// every image, layer, volume, and container in the pool lives.
func TestDefaultDataDiskIsSizedForRealWork(t *testing.T) {
	if defaultDataDiskGiB < 100 {
		t.Errorf("default data disk = %d GiB, want at least 100", defaultDataDiskGiB)
	}
	if defaultCacheDiskGiB <= 0 {
		t.Errorf("default cache disk = %d GiB", defaultCacheDiskGiB)
	}
}
