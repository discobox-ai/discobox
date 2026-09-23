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
	"github.com/discobox-ai/discobox/releasemanifest"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
	"github.com/discobox-ai/discobox/server/providers/guestimage"
	"github.com/discobox-ai/discobox/server/providers/libkrun/internal/krunvm"
	"github.com/discobox-ai/discobox/server/providers/vmsize"
)

func TestProviderIdentity(t *testing.T) {
	if ProviderType != "libkrun" {
		t.Fatalf("ProviderType = %q, want libkrun", ProviderType)
	}
	if got := Definition().Name; got != "libkrun" {
		t.Fatalf("provider name = %q, want libkrun", got)
	}
}

// The pool boots one image, pinned by this backend: the guest's root disk and
// the kernel and runtime built for it are one pin because a host can use none
// of the four without the other three (ADR 0148 §5). It is not the shared vz
// guest image, whose kernel is a distribution one libkrun cannot boot.
func TestImageIsOnePinCarryingTheRuntime(t *testing.T) {
	image, err := imageResolver(Config{}, dockerworker.ServerDefaults{})
	if err != nil {
		t.Fatalf("build image resolver: %v", err)
	}
	if got := image.Reference(); got != DefaultImage {
		t.Fatalf("image = %q, want %q", got, DefaultImage)
	}
	if image.Reference() == guestimage.DefaultVMImage {
		t.Fatal("libkrun resolves the shared vz guest image")
	}
	// Every artifact is required, so an image missing the runtime fails to
	// resolve instead of booting with whatever the host has.
	dir := t.TempDir()
	for _, name := range []string{rootArtifact, kernelArtifact, libraryArtifact} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("artifact"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	partial, err := imageResolver(Config{VMImageDir: dir}, dockerworker.ServerDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := partial.Resolve(t.Context(), nil); err == nil {
		t.Fatal("resolved an image directory with no passt")
	}
	if err := os.WriteFile(filepath.Join(dir, passtArtifact), []byte("artifact"), 0o700); err != nil {
		t.Fatal(err)
	}
	whole, err := imageResolver(Config{VMImageDir: dir}, dockerworker.ServerDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := whole.Resolve(t.Context(), nil)
	if err != nil {
		t.Fatalf("resolve a whole image directory: %v", err)
	}
	for _, name := range []string{rootArtifact, kernelArtifact, libraryArtifact, passtArtifact} {
		if bundle.Path(name) != filepath.Join(dir, name) {
			t.Fatalf("%s = %q, want it from %s", name, bundle.Path(name), dir)
		}
	}
}

// A local build wins over the published image with nothing configured, and it
// lands where `task build:vm-krun` writes: under the image cache, which pulled
// images share, in a directory of its own.
func TestLocalBuildDirectoryIsDefaulted(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	image, err := imageResolver(Config{}, dockerworker.ServerDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(defaultStateDir(), ".images", "vm", "local")
	if got := image.LocalDir(); got != want {
		t.Fatalf("local directory = %q, want %q", got, want)
	}
	if got := effectiveImageLocalDir(""); got != want {
		t.Fatalf("effectiveImageLocalDir = %q, want %q", got, want)
	}
	if got, want := effectiveImageCacheDir(""), filepath.Join(defaultImageRoot(), "vm"); got != want {
		t.Fatalf("image cache = %q, want %q", got, want)
	}
	configured := t.TempDir()
	if got, want := effectiveImageCacheDir(configured), filepath.Join(configured, "vm"); got != want {
		t.Fatalf("configured image cache = %q, want %q", got, want)
	}
	local := t.TempDir()
	image, err = imageResolver(Config{VMImageLocalDir: local}, dockerworker.ServerDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	if image.LocalDir() != local {
		t.Fatalf("configured local directory = %q, want %q", image.LocalDir(), local)
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
		"vmImageDir":      "images",
		"vmImageLocalDir": "local",
		"imageCacheDir":   "cache",
		"stateDir":        "state",
		"runtimeDir":      "run",
		"passtPath":       "passt",
		"libkrunPath":     "libkrun.so.1",
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
	if err := Validate(json.RawMessage(`{"vmImage":"NOT A REFERENCE"}`)); err == nil {
		t.Fatal("Validate accepted an unparseable VM image reference")
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
	cfg := driverConfig(Config{}, nil, nil)
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
	// The manifest is a Linux path namespace and the driver that renders one
	// only ever runs on a Linux host, so it composes host paths with filepath
	// and the artifact bundle below is a real directory from t.TempDir. On
	// Windows both come back as "C:\..." with backslashes, which the manifest
	// correctly refuses. What this asserts is the port and socket map, and that
	// is not a thing the host's path syntax has an opinion about.
	if runtime.GOOS == "windows" {
		t.Skip("the driver renders Linux host paths; see krunvm for the format's own tests")
	}
	driver := &Driver{
		runtimeDir:         "/run/user/1000/discobox/libkrun",
		controlPlaneSocket: "/run/user/1000/discobox/server.sock",
	}
	image := bundleAt(t, rootArtifact, kernelArtifact, libraryArtifact, passtArtifact)
	manifest := driver.manifest("pool_1", image, "/state/pool_1/data.raw", "/state/pool_1/cache.raw", vmsize.Size{VCPUs: 2, MemoryMiB: 2048})
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
	if manifest.KernelImage != image.Path(kernelArtifact) {
		t.Fatalf("kernel = %q, want the image's artifact", manifest.KernelImage)
	}
	if manifest.RootDisk != image.Path(rootArtifact) {
		t.Fatalf("root disk = %q, want the image's artifact", manifest.RootDisk)
	}
	if manifest.PasstPath != image.Path(passtArtifact) {
		t.Fatalf("passt = %q, want the image's artifact", manifest.PasstPath)
	}
	if manifest.LibraryPath != image.Path(libraryArtifact) {
		t.Fatalf("libkrun = %q, want the image's artifact", manifest.LibraryPath)
	}
}

// A provider configured with its own passt or libkrun runs those rather than
// the image's, one at a time.
func TestManifestPrefersConfiguredRuntimeOverTheImages(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the driver renders Linux host paths; see krunvm for the format's own tests")
	}
	image := bundleAt(t, rootArtifact, kernelArtifact, libraryArtifact, passtArtifact)
	//nolint:gosec // G101: passt is the network daemon, not a password.
	driver := &Driver{
		runtimeDir:         "/run/user/1000/discobox/libkrun",
		controlPlaneSocket: "/run/user/1000/discobox/server.sock",
		passtPath:          "/nix/store/passt/bin/passt",
	}
	manifest := driver.manifest("pool_1", image, "/state/pool_1/data.raw", "/state/pool_1/cache.raw", vmsize.Size{VCPUs: 2, MemoryMiB: 2048})
	if manifest.PasstPath != driver.passtPath || manifest.LibraryPath != image.Path(libraryArtifact) {
		t.Fatalf("passt = %q, libkrun = %q; want the configured passt and the image's libkrun", manifest.PasstPath, manifest.LibraryPath)
	}
	driver.passtPath, driver.libraryPath = "", "/nix/store/libkrun/lib/libkrun.so.1"
	manifest = driver.manifest("pool_1", image, "/state/pool_1/data.raw", "/state/pool_1/cache.raw", vmsize.Size{VCPUs: 2, MemoryMiB: 2048})
	if manifest.PasstPath != image.Path(passtArtifact) || manifest.LibraryPath != driver.libraryPath {
		t.Fatalf("passt = %q, libkrun = %q; want the image's passt and the configured libkrun", manifest.PasstPath, manifest.LibraryPath)
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

// bundleAt resolves an artifact set out of a directory holding the named files.
// The resolver is exercised by its own package's tests; what matters here is
// which file the driver reaches for.
func bundleAt(t *testing.T, artifacts ...string) *guestimage.Bundle {
	t.Helper()
	dir := t.TempDir()
	var wanted []guestimage.Artifact
	for _, artifact := range artifacts {
		if err := os.WriteFile(filepath.Join(dir, artifact), []byte("artifact"), 0o600); err != nil {
			t.Fatal(err)
		}
		wanted = append(wanted, guestimage.Artifact{Name: artifact})
	}
	resolver, err := guestimage.New(guestimage.Config{
		OverrideDir: dir,
		Artifacts:   wanted,
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

func TestReleaseManifestBypassesLocalImageOverrides(t *testing.T) {
	m := &releasemanifest.Manifest{Format: releasemanifest.Format, Images: releasemanifest.Images{VM: "example.com/guest:v8", Libkrun: "example.com/libkrun:v8"}}
	defaults := dockerworker.ServerDefaults{Release: m}
	cfg := Config{VMImage: "old:local", VMImageDir: t.TempDir(), VMImageLocalDir: t.TempDir()}
	image, err := imageResolver(cfg, defaults)
	if err != nil {
		t.Fatal(err)
	}
	// A resolver reports no reference while an override directory is in use,
	// so the release's reference is also proof the directory was dropped.
	if image.Reference() != m.Images.Libkrun || image.LocalDir() != "" {
		t.Fatalf("release reference did not supersede local overrides: reference %q, local %q", image.Reference(), image.LocalDir())
	}
}

// A key the one libkrun image replaced would do nothing, so a write that sets
// one is refused, naming the key and what replaced it (ADR 0148 §5).
func TestValidateRefusesSupersededImageKeys(t *testing.T) {
	err := Validate(json.RawMessage(`{"kernelImageDir":"/opt/kernel"}`))
	if err == nil || !strings.Contains(err.Error(), "kernelImageDir") || !strings.Contains(err.Error(), "vmImageDir") {
		t.Fatalf("Validate() = %v, want a refusal naming kernelImageDir and vmImageDir", err)
	}
	if err := Validate(json.RawMessage(`{"guestImage":""}`)); err != nil {
		t.Fatalf("an empty superseded key sets nothing, but Validate() = %v", err)
	}
}
