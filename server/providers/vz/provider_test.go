package vz

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pool-agent/wire"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/vz/internal/vzvm"
)

// The whole backend is expressed in the two transport URLs. VSOCK in both
// directions is what lets a Mac run pools with no TCP listener and therefore no
// firewall prompt, so a change that turns either into an IP endpoint is a
// behavior change, not a refactor.
func TestEngineConfigUsesVSOCKInBothDirections(t *testing.T) {
	cfg := engineConfig(Config{}, nil, nil)

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
	if image := engineConfig(Config{}, nil, nil).Image; strings.TrimSpace(image) == "" {
		t.Fatal("engine config resolved no pool image")
	}
	const configured = "example.com/pool:custom"
	if image := engineConfig(Config{WorkerImage: configured}, nil, nil).Image; image != configured {
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
// hostGuestDir spells the guest directory the way the machine running the test
// does. Validate requires an absolute path, and a POSIX one is not absolute on
// Windows — where vz never runs, but where the package still compiles and its
// tests still execute.
func hostGuestDir() string {
	if runtime.GOOS == "windows" {
		return `C:\opt\discobox\guest`
	}
	return "/opt/discobox/guest"
}

func TestValidateAcceptsLocalGuestArtifacts(t *testing.T) {
	raw, err := json.Marshal(Config{GuestImageDir: hostGuestDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(raw); err != nil {
		t.Fatalf("Validate = %v", err)
	}
	resolver, err := guestResolver(Config{GuestImageDir: hostGuestDir()})
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

// The share, the pool container's host mount, and the published source roots
// are three views of one decision. If they disagree, a pool claims it can clone
// a path its guest or its pool agent cannot reach, and the failure surfaces as a
// clone error inside the guest with nothing pointing back here.
func TestHostShareIsOneDecision(t *testing.T) {
	shares := hostShares()
	if runtime.GOOS != "darwin" {
		// /Users is a macOS directory; elsewhere there is nothing to share and
		// nothing to claim.
		return
	}
	if len(shares) != 1 {
		t.Fatalf("host shares = %d, want the single /Users share", len(shares))
	}
	share := shares[0]
	if share.HostPath != hostShareDir {
		t.Errorf("share host path = %q, want %q", share.HostPath, hostShareDir)
	}
	if !share.ReadOnly {
		t.Error("the host share is writable; a sandbox must never write to files on the Mac")
	}

	mounts := engineConfig(Config{}, nil, nil).HostMounts
	if len(mounts) != len(shares) {
		t.Fatalf("engine host mounts = %d, want one per share (%d)", len(mounts), len(shares))
	}
	if mounts[0].Source != share.HostPath || !mounts[0].ReadOnly {
		t.Errorf("engine host mount = %+v, want a read-only bind of %q", mounts[0], share.HostPath)
	}

	roots := localSourceRoots()
	if len(roots) != len(shares) || roots[0] != share.HostPath {
		t.Errorf("local source roots = %v, want %v", roots, []string{share.HostPath})
	}
}

// The tag is a contract with a separately released guest image, and the
// framework caps it at 36 bytes. A tag this side rejects is a pool that never
// starts; one the framework rejects is the same failure with a worse message.
func TestHostShareTagIsAcceptable(t *testing.T) {
	opts := vzvm.Options{
		CPUCount:       1,
		MemoryBytes:    512 * 1024 * 1024,
		KernelPath:     "/guest/vmlinux",
		RootImagePath:  "/guest/root.ext4",
		DataImagePath:  "/pool/data.raw",
		CacheImagePath: "/pool/cache.raw",
		SharedDirectories: []vzvm.SharedDirectory{
			{Tag: hostShareTag, HostPath: hostShareDir, ReadOnly: true},
		},
	}
	if err := opts.Validate(); err != nil {
		t.Fatalf("the configured host share is not a valid VM option: %v", err)
	}
}

// The guest build closes the macOS bootstrap: the running guest builds its
// successor. The spec is what the engine builds from, so a wrong Dockerfile
// path or a destination the resolver does not read would produce a build that
// appears to succeed and changes nothing.
func TestGuestImageBuildSpecPointsAtTheGuestThisDriverBoots(t *testing.T) {
	local := filepath.Join(t.TempDir(), "guest", "local")
	guest, err := guestResolver(Config{GuestImageLocalDir: local})
	if err != nil {
		t.Fatalf("guestResolver: %v", err)
	}
	driver := &Driver{guest: guest}

	spec, err := driver.GuestImageBuildSpec()
	if err != nil {
		t.Fatalf("GuestImageBuildSpec: %v", err)
	}
	if spec.Dockerfile != guestImageDockerfile {
		t.Errorf("Dockerfile = %q, want %q", spec.Dockerfile, guestImageDockerfile)
	}
	// The Dockerfile path is resolved inside a checkout, so it has to name the
	// file that is actually in this repository.
	if _, err := os.Stat(filepath.Join(repoRoot(t), filepath.FromSlash(spec.Dockerfile))); err != nil {
		t.Errorf("the guest image Dockerfile is not at %s: %v", spec.Dockerfile, err)
	}
	if spec.Destination != local {
		t.Errorf("Destination = %q, want the resolver's local build directory %q", spec.Destination, local)
	}
	if !strings.HasPrefix(spec.Platform, "linux/") {
		t.Errorf("Platform = %q, want a linux guest", spec.Platform)
	}
	if spec.Adopt == nil {
		t.Error("no Adopt: the resolver memoizes for the life of the process, so a build would not be picked up")
	}
}

// An override directory names the artifacts outright, so there is nothing for a
// build to be adopted into and the operation is declined rather than writing
// somewhere nobody reads.
func TestGuestImageBuildSpecDeclinesWithoutALocalDirectory(t *testing.T) {
	override := t.TempDir()
	for _, name := range []string{kernelArtifact, rootArtifact} {
		if err := os.WriteFile(filepath.Join(override, name), []byte("artifact"), 0o600); err != nil {
			t.Fatalf("seed override: %v", err)
		}
	}
	guest, err := guestResolver(Config{GuestImageDir: override})
	if err != nil {
		t.Fatalf("guestResolver: %v", err)
	}
	driver := &Driver{guest: guest}
	if _, err := driver.GuestImageBuildSpec(); !errors.Is(err, sandbox.ErrGuestImageBuildUnsupported) {
		t.Fatalf("GuestImageBuildSpec with an override = %v, want ErrGuestImageBuildUnsupported", err)
	}
}

// repoRoot walks up from this package to the checkout it lives in.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "CLAUDE.md")); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no repository root above this package")
		}
		dir = parent
	}
}
