package libkrun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/endpoint"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
	"github.com/discobox-ai/discobox/server/providers/guestimage"
	"github.com/discobox-ai/discobox/server/providers/libkrun/internal/krunvm"
)

func TestProviderIdentity(t *testing.T) {
	if ProviderType != "libkrun" {
		t.Fatalf("ProviderType = %q, want libkrun", ProviderType)
	}
	if got := Definition().Name; got != "libkrun" {
		t.Fatalf("provider name = %q, want libkrun", got)
	}
}

// The guest image is shared with vz, so this backend must not carry a pin of
// its own: one publish has to be one edit, or a backend quietly keeps booting
// the release before last.
func TestGuestImageIsTheSharedPin(t *testing.T) {
	guest, err := guestResolver(Config{})
	if err != nil {
		t.Fatalf("build guest resolver: %v", err)
	}
	if got := guest.Reference(); got != guestimage.DefaultVMImage {
		t.Fatalf("guest image = %q, want the shared %q", got, guestimage.DefaultVMImage)
	}
}

// The kernel is the one artifact libkrun does not take from the shared guest
// image: it needs libkrunfw's patches, and a distribution kernel does not boot
// under libkrun at all.
func TestKernelIsResolvedFromItsOwnImage(t *testing.T) {
	kernel, err := kernelResolver(Config{})
	if err != nil {
		t.Fatalf("build kernel resolver: %v", err)
	}
	if kernel.Reference() == guestimage.DefaultVMImage {
		t.Fatal("the kernel resolves from the shared guest image")
	}
	if got := kernel.Reference(); got != DefaultKernelImage {
		t.Fatalf("kernel image = %q, want %q", got, DefaultKernelImage)
	}
}

// A local build of either image wins over the published one with nothing
// configured, and the two land in different directories: both publish a file
// called vmlinux, so one directory would have them overwrite each other.
func TestLocalBuildDirectoriesAreDistinctAndDefaulted(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	guest, err := guestResolver(Config{})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := kernelResolver(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if guest.LocalDir() == "" || kernel.LocalDir() == "" {
		t.Fatalf("local directories = %q, %q; both must be defaulted", guest.LocalDir(), kernel.LocalDir())
	}
	if guest.LocalDir() == kernel.LocalDir() {
		t.Fatalf("guest and kernel share the local build directory %q", guest.LocalDir())
	}
}

// Pool disks live at <stateDir>/<poolID>, so the image cache has to be a name
// no pool can take. A pool ID must start with a letter or a digit; a dot is how
// that is guaranteed rather than merely likely.
func TestImageCacheCannotCollideWithAPool(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	name := filepath.Base(defaultImageRoot())
	if filepath.Dir(defaultImageRoot()) != defaultStateDir() {
		t.Fatalf("image root %q is not inside the pool disk root %q", defaultImageRoot(), defaultStateDir())
	}
	if err := validatePoolID(name); err == nil {
		t.Fatalf("a pool could be named %q and take the image cache directory", name)
	}
}

func TestStorageDirectoriesUseLibkrunNamespaceAndAdoptLegacyState(t *testing.T) {
	dataHome := filepath.Join(t.TempDir(), "data")
	runtimeHome := filepath.Join(t.TempDir(), "run")
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeHome)

	if got, want := defaultStateDir(), filepath.Join(dataHome, "discobox", "libkrun"); got != want {
		t.Fatalf("default state directory = %q, want %q", got, want)
	}
	if got, want := defaultRuntimeDir(), filepath.Join(runtimeHome, "discobox", "libkrun"); got != want {
		t.Fatalf("default runtime directory = %q, want %q", got, want)
	}
	for _, path := range []string{legacyStateDir(), legacyRuntimeDir()} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if got := effectiveStateDir(""); got != legacyStateDir() {
		t.Fatalf("effective state directory = %q, want legacy %q", got, legacyStateDir())
	}
	if got := effectiveRuntimeDir(""); got != legacyRuntimeDir() {
		t.Fatalf("effective runtime directory = %q, want legacy %q", got, legacyRuntimeDir())
	}
}

// Nothing has to be configured to start a libkrun pool: the images are pinned,
// the disks are sized from the host, and the control-plane socket is the
// server's own. That is the whole difference from the pre-0062 provider, which
// could not start without two locally built artifacts named by hand.
func TestValidateAcceptsAnEmptyConfiguration(t *testing.T) {
	requireLinuxHost(t)
	if err := Validate(json.RawMessage(`{}`)); err != nil {
		t.Fatalf("validate an unconfigured provider: %v", err)
	}
}

func TestValidateRejectsRelativePaths(t *testing.T) {
	requireLinuxHost(t)
	for field, value := range map[string]string{
		"guestImageDir":  "images",
		"kernelImageDir": "kernels",
		"stateDir":       "state",
		"runtimeDir":     "run",
		"passtPath":      "passt",
		"libkrunPath":    "libkrun.so.1",
	} {
		data, err := json.Marshal(map[string]string{field: value})
		if err != nil {
			t.Fatal(err)
		}
		if err := Validate(data); err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Fatalf("Validate(%s) error = %v, want an absolute path requirement", data, err)
		}
	}
}

func TestValidateRejectsAnUnparseableImageReference(t *testing.T) {
	requireLinuxHost(t)
	if err := Validate(json.RawMessage(`{"kernelImage":"NOT A REFERENCE"}`)); err == nil {
		t.Fatal("Validate accepted an unparseable kernel image reference")
	}
}

func TestValidateAcceptsUnixControlPlaneSocket(t *testing.T) {
	requireLinuxHost(t)
	data, err := json.Marshal(Config{ControlPlaneSocket: "unix://" + filepath.Join(t.TempDir(), "server.sock")})
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(data); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestValidateRejectsInvalidSizing(t *testing.T) {
	requireLinuxHost(t)
	for _, config := range []Config{
		{VCPUs: 256},
		{MemoryMiB: 128},
		{DataDiskGiB: 4097},
		{CacheDiskGiB: 4097},
	} {
		data, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		if err := Validate(data); err == nil {
			t.Fatalf("Validate(%s) succeeded", data)
		}
	}
}

func TestDriverConfigUsesVSOCKAndHostSizingDefaults(t *testing.T) {
	cfg := driverConfig(Config{}, nil, nil, nil)
	if cfg.ControlPlaneSocket == "" {
		t.Fatal("control plane socket default is empty")
	}
	if parsed, err := endpoint.Parse(endpoint.DefaultEndpoint()); err != nil || cfg.ControlPlaneSocket != parsed.Value {
		t.Fatalf("control plane socket = %q, parsed default = %#v, err = %v", cfg.ControlPlaneSocket, parsed, err)
	}
	// Sizing is left unset here and resolved by NewDriver from the host, the
	// same way vz does it: a pool VM is the machine's, not a fixed guess.
	if defaultVCPUs() < 1 || defaultMemoryMiB() < 1024 {
		t.Fatalf("host sizing defaults = %d vCPUs, %d MiB", defaultVCPUs(), defaultMemoryMiB())
	}
}

// The engine's transport is the whole of what makes this backend VSOCK-only in
// both directions, and nothing about it depends on a VM being startable.
func TestEngineConfigIsVSOCKInBothDirections(t *testing.T) {
	cfg := engineConfig(Config{}, nil, nil, dockerworker.ServerDefaults{})
	if !strings.HasPrefix(cfg.ControlPlaneURL, "vsock://") {
		t.Fatalf("control plane URL = %q, want a vsock:// scheme", cfg.ControlPlaneURL)
	}
	if !strings.Contains(cfg.AgentListenURL, strconv.Itoa(agentVSOCKPort)) {
		t.Fatalf("agent listen URL = %q, want port %d", cfg.AgentListenURL, agentVSOCKPort)
	}
	if cfg.Labels[labelProviderType] != ProviderType {
		t.Fatalf("labels = %v", cfg.Labels)
	}
}

// The manifest is the contract between the server and the launcher child, and
// it is the only place the guest's port map is written down on the host side.
func TestManifestPlacesEveryPortAndSocket(t *testing.T) {
	driver := &Driver{
		runtimeDir:         "/run/user/1000/discobox/libkrun",
		controlPlaneSocket: "/run/user/1000/discobox/server.sock",
		vcpus:              2,
		memoryMiB:          2048,
	}
	guest := bundleAt(t, rootArtifact)
	kernel := bundleAt(t, kernelArtifact)
	manifest := driver.manifest("pool_1", guest, kernel, "/state/pool_1/data.raw", "/state/pool_1/cache.raw")
	if err := manifest.Validate(); err != nil {
		t.Fatalf("the driver rendered a manifest its own launcher rejects: %v", err)
	}
	directions := map[string]krunvm.VSOCKDirection{}
	ports := map[string]uint32{}
	for _, mapping := range manifest.VSOCK {
		directions[mapping.Name] = mapping.Direction
		ports[mapping.Name] = mapping.Port
	}
	if directions["control-plane"] != krunvm.GuestConnects {
		t.Fatalf("the control plane is not guest-initiated: %v", directions)
	}
	for _, name := range []string{"pool-agent", "lifecycle", "docker"} {
		if directions[name] != krunvm.HostConnects {
			t.Fatalf("%s is not host-initiated: %v", name, directions)
		}
	}
	if ports["control-plane"] != controlPlaneVSOCKPort || ports["pool-agent"] != agentVSOCKPort ||
		ports["lifecycle"] != lifecycleVSOCKPort || ports["docker"] != dockerVSOCKPort {
		t.Fatalf("port map = %v", ports)
	}
	if manifest.KernelImage != kernel.Path(kernelArtifact) {
		t.Fatalf("kernel = %q, want the kernel image's artifact", manifest.KernelImage)
	}
	if manifest.RootDisk != guest.Path(rootArtifact) {
		t.Fatalf("root disk = %q, want the guest image's artifact", manifest.RootDisk)
	}
}

// Disks are created empty and sparse for the guest to format, and grown when
// the configured ceiling is raised. Shrinking would discard a pool's data, so
// it never happens.
func TestSparseDisksAreCreatedGrownAndNeverShrunk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.raw")
	if err := ensureSparseImage(path, 4<<20); err != nil {
		t.Fatalf("create: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 4<<20 {
		t.Fatalf("size = %d, want %d", info.Size(), 4<<20)
	}
	// Sparse: the length is a ceiling, not space taken from the disk. This is
	// what makes a 100 GiB default a number rather than a demand on a laptop.
	if blocks, ok := allocatedBlocks(path); ok && blocks != 0 {
		t.Fatalf("a freshly created disk occupies %d blocks, want 0", blocks)
	}
	if err := ensureSparseImage(path, 8<<20); err != nil {
		t.Fatalf("grow: %v", err)
	}
	if info, err = os.Stat(path); err != nil || info.Size() != 8<<20 {
		t.Fatalf("grown size = %v, %v", info, err)
	}
	if err := ensureSparseImage(path, 1<<20); err != nil {
		t.Fatalf("shrink request: %v", err)
	}
	if info, err = os.Stat(path); err != nil || info.Size() != 8<<20 {
		t.Fatalf("a smaller configured size truncated the disk: %v, %v", info, err)
	}
}

func TestMACAddressIsStableLocalUnicast(t *testing.T) {
	first := macAddress("pool_123")
	if first != macAddress("pool_123") {
		t.Fatal("MAC address is not stable")
	}
	octet, err := strconv.ParseUint(strings.Split(first, ":")[0], 16, 8)
	if err != nil || octet&0x03 != 0x02 {
		t.Fatalf("MAC address %q is not locally administered unicast", first)
	}
}

func TestValidatePoolIDRejectsPaths(t *testing.T) {
	for _, value := range []string{"", "..", "pool/other", "pool..other"} {
		if err := validatePoolID(value); err == nil {
			t.Fatalf("validatePoolID(%q) succeeded", value)
		}
	}
	if err := validatePoolID("pool_9ade63td40g87ddm"); err != nil {
		t.Fatalf("valid pool ID rejected: %v", err)
	}
}

// The launcher argv has to be recognized as a launcher, not merely ignored: a
// server that fell through to its normal startup here would open a database and
// bind a listener in what is supposed to be a VM.
func TestLauncherRejectsAMalformedInvocation(t *testing.T) {
	for _, args := range [][]string{nil, {"--config"}, {"--config", "relative.json"}, {"run", "--config", "/tmp/x.json"}} {
		if err := runLauncher(args); err == nil {
			t.Fatalf("runLauncher(%v) succeeded", args)
		}
	}
}

// bundleAt resolves an artifact set out of a directory holding one file. The
// resolver is exercised by its own package's tests; what matters here is which
// file the driver reaches for.
func bundleAt(t *testing.T, artifact string) *guestimage.Bundle {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, artifact), []byte("artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := guestimage.New(guestimage.Config{
		OverrideDir: dir,
		Artifacts:   []guestimage.Artifact{{Name: artifact}},
	})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := resolver.Resolve(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

// requireLinuxHost skips a test whose subject is Linux-only. This provider's
// configuration is validated as POSIX absolute paths -- "/images" is not
// absolute to a Windows filepath, and a Windows path cannot be spelled inside a
// unix:// URL at all.
func requireLinuxHost(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("libkrun is x86-64 Linux only")
	}
}
