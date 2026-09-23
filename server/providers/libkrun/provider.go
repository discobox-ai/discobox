// Package libkrun registers the Linux libkrun provider. Each pool gets one
// microVM while dockerworker.Engine continues to own the pool-agent container
// and Docker behavior inside that VM.
//
// The VM boots one image, pulled from a registry by
// server/providers/guestimage: the root disk of the guest a vz pool boots, the
// libkrunfw-patched kernel that is libkrun's alone, and the host runtime that
// boots them — libkrun itself and passt (ADR 0148 §5). Nothing is built or
// installed on the host to start a pool, and libkrun is dlopened by the
// launcher child rather than linked into the server (ADR 0062 §9).
package libkrun

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/discobox-ai/discobox/endpoint"
	guestvsock "github.com/discobox-ai/discobox/pool-agent/vsock"
	"github.com/discobox-ai/discobox/pool-agent/wire"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
	"github.com/discobox-ai/discobox/server/providers/guestimage"
	"github.com/discobox-ai/discobox/server/providers/libkrun/internal/krunvm"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
	"github.com/discobox-ai/discobox/server/providers/vmsize"
)

const (
	ProviderType = "libkrun"

	// Disk sizes are ceilings, not allocations: both images are sparse and the
	// guest grows into them. The data disk holds everything that survives a
	// pool restart — images, layers, volumes, containers — so it is sized for a
	// real workload rather than for the first sandbox.
	defaultDataDiskGiB  = 100
	defaultCacheDiskGiB = 50

	storageNamespace = "libkrun"
	// Preserve the original default directory namespace so writable disks
	// created before the provider-type rename are still found.
	legacyStorageNamespace = "local-vm"

	controlPlaneVSOCKPort = 3001
	agentVSOCKPort        = 3002
	lifecycleVSOCKPort    = 3003
	dockerVSOCKPort       = 3004

	labelProviderType = "discobox.provider_type"
)

// The artifacts the libkrun image carries (ADR 0148 §5): the guest's root disk,
// the libkrunfw-patched kernel libkrun boots it with, and the host runtime —
// the library the launcher dlopens and the passt it execs. kernel.config rides
// along so what booted can be read back off the artifact, and nothing here
// needs it.
const (
	rootArtifact    = "root.ext4"
	kernelArtifact  = "vmlinux"
	libraryArtifact = "libkrun.so.1"
	passtArtifact   = "passt"
)

// runtimeArtifacts are the files a guest built on its own lacks: everything in
// the libkrun image but the root disk. GuestImageBuildSpec pairs a freshly built
// guest with them so the local directory it writes is a whole image.
var runtimeArtifacts = []string{kernelArtifact, libraryArtifact, passtArtifact}

// DefaultImage is the published libkrun image: the shared guest's root disk
// packaged with the kernel and runtime built for it (vm-image/libkrun). It is
// one pin because a host can use none of the four files without the other
// three. `task vm:publish-krun` reports the digest to pin here, and a digest
// is what belongs here: a tag would let whoever runs the server decide which
// kernel and which libkrun they boot.
const DefaultImage = "ghcr.io/discobox-ai/discobox-vm-krun:v1"

// guestImageDockerfile is the Dockerfile in a discobox checkout that produces
// the guest artifact set, in the path form BuildKit's frontend wants. Its
// context is the repository root, which is why the path is spelled from there.
const guestImageDockerfile = "vm-image/Dockerfile"

// guestImagePlatform is what a libkrun guest runs on. It is pinned rather than
// left to the builder because the builder may be a Docker daemon inside the VM,
// whose idea of "native" is the same only by coincidence.
const guestImagePlatform = "linux/amd64"

// Config is the persisted libkrun provider configuration.
type Config struct {
	poolruntime.PoolPolicy

	// VMImage overrides the published libkrun image. Its directory pair has
	// the same meaning as vz's guest pair: an override that is an assertion,
	// and a local build that wins when it is complete.
	//
	// The keys are new rather than the guestImage/kernelImage pairs this
	// replaced, because those named a guest and a kernel on their own and
	// neither is a whole libkrun image (ADR 0148 §5). See supersededKeys for
	// what becomes of a configuration that still carries them.
	VMImage         string `json:"vmImage,omitempty"`
	VMImageDir      string `json:"vmImageDir,omitempty"`
	VMImageLocalDir string `json:"vmImageLocalDir,omitempty"`
	// ImageCacheDir holds one directory per pulled image digest.
	ImageCacheDir string `json:"imageCacheDir,omitempty"`

	StateDir           string `json:"stateDir,omitempty"`
	RuntimeDir         string `json:"runtimeDir,omitempty"`
	ControlPlaneSocket string `json:"controlPlaneSocket,omitempty"`
	// PasstPath and LibkrunPath override the passt and libkrun the image
	// carries — with `nix develop .#libkrun`'s, for one. Unset, the launcher
	// uses the image's.
	PasstPath    string `json:"passtPath,omitempty"`
	LibkrunPath  string `json:"libkrunPath,omitempty"`
	WorkerImage  string `json:"workerImage,omitempty"`
	VCPUs        int    `json:"vcpus,omitempty"`
	MemoryMiB    int    `json:"memoryMiB,omitempty"`
	DataDiskGiB  int64  `json:"dataDiskGiB,omitempty"`
	CacheDiskGiB int64  `json:"cacheDiskGiB,omitempty"`
}

func Decode(data json.RawMessage) (Config, error) {
	return poolruntime.DecodeConfig[Config](data, ProviderType)
}

// supersededKeys are the configuration keys the one libkrun image replaced
// (ADR 0148 §5). A saved configuration carrying one still loads — it is the
// user's record, and a provider that stopped loading would take its pools
// with it — but the key does nothing, so loading says so, and a write that
// sets one is refused rather than accepted and ignored.
var supersededKeys = []string{"guestImage", "guestImageDir", "guestImageLocalDir", "kernelImage", "kernelImageDir", "kernelImageLocalDir"}

// supersededKeysIn names the superseded keys data sets, in supersededKeys order.
func supersededKeysIn(data json.RawMessage) []string {
	var raw map[string]json.RawMessage
	if len(data) == 0 || json.Unmarshal(data, &raw) != nil {
		return nil
	}
	var found []string
	for _, key := range supersededKeys {
		if value, ok := raw[key]; ok && string(value) != `""` && string(value) != "null" {
			found = append(found, key)
		}
	}
	return found
}

func Validate(data json.RawMessage) error {
	cfg, err := Decode(data)
	if err != nil {
		return err
	}
	if keys := supersededKeysIn(data); len(keys) > 0 {
		return fmt.Errorf("libkrun %s no longer applies: a libkrun pool boots one image carrying the guest, the kernel, libkrun and passt; use vmImage, vmImageDir or vmImageLocalDir", strings.Join(keys, ", "))
	}
	for field, value := range map[string]string{
		"vmImageDir":      cfg.VMImageDir,
		"vmImageLocalDir": cfg.VMImageLocalDir,
		"imageCacheDir":   cfg.ImageCacheDir,
		"stateDir":        cfg.StateDir,
		"runtimeDir":      cfg.RuntimeDir,
		"passtPath":       cfg.PasstPath,
		"libkrunPath":     cfg.LibkrunPath,
	} {
		if path := strings.TrimSpace(value); path != "" && !filepath.IsAbs(path) {
			return fmt.Errorf("libkrun %s must be an absolute path", field)
		}
	}
	socket := effectiveControlPlaneSocket(cfg.ControlPlaneSocket)
	parsed, err := endpoint.Parse(socket)
	if err != nil {
		return fmt.Errorf("libkrun controlPlaneSocket: %w", err)
	}
	if parsed.Scheme != "unix" {
		return fmt.Errorf("libkrun controlPlaneSocket must use unix://")
	}
	if cfg.VCPUs < 0 || cfg.MemoryMiB < 0 || cfg.DataDiskGiB < 0 || cfg.CacheDiskGiB < 0 {
		return fmt.Errorf("libkrun sizing values must not be negative")
	}
	if cfg.VCPUs > 255 {
		return fmt.Errorf("libkrun vcpus must not exceed 255")
	}
	if cfg.MemoryMiB > 0 && cfg.MemoryMiB < 256 {
		return fmt.Errorf("libkrun memoryMiB must be at least 256")
	}
	if cfg.DataDiskGiB > 4096 || cfg.CacheDiskGiB > 4096 {
		return fmt.Errorf("libkrun disk sizes must not exceed 4096 GiB")
	}
	// Building the resolver is the configuration check: it is what rejects an
	// unparseable reference or a relative path, and it touches no network.
	_, err = imageResolver(cfg, dockerworker.ServerDefaults{})
	return err
}

func FactoryWithPoolManager(poolManager poolruntime.PoolManager, imageSync *dockerworker.DevelopmentImageSynchronizer, serverDefaults dockerworker.ServerDefaults) sandbox.ProviderFactory {
	return func(ctx context.Context, instance *model.SandboxProviderInstance) (sandbox.Provider, error) {
		return newFromInstance(ctx, instance, poolManager, imageSync, serverDefaults)
	}
}

func newFromInstance(ctx context.Context, instance *model.SandboxProviderInstance, poolManager poolruntime.PoolManager, imageSync *dockerworker.DevelopmentImageSynchronizer, serverDefaults dockerworker.ServerDefaults) (sandbox.Provider, error) {
	cfg, err := Decode(instance.Config)
	if err != nil {
		return nil, err
	}
	if keys := supersededKeysIn(instance.Config); len(keys) > 0 {
		slog.WarnContext(ctx, "libkrun provider configuration sets keys that no longer apply; its pools boot the libkrun image instead",
			"provider_id", instance.ID, "keys", keys, "image", defaultString(cfg.VMImage, DefaultImage))
	}
	image, err := imageResolver(cfg, serverDefaults)
	if err != nil {
		return nil, err
	}
	progress := sandbox.PoolProgressReporterFor(poolManager)
	driver, err := NewDriver(driverConfig(cfg, image, progress))
	if err != nil {
		return nil, err
	}
	engine, err := dockerworker.New(engineConfig(cfg, imageSync, progress, serverDefaults), driver)
	if err != nil {
		_ = driver.Close()
		return nil, err
	}
	return poolruntime.New(engine, Definition(), poolManager), nil
}

// engineConfig renders the pool engine configuration for one provider instance.
// It is separate from newFromInstance so the transport invariants can be
// asserted without a VM or a pool manager.
func engineConfig(cfg Config, imageSync *dockerworker.DevelopmentImageSynchronizer, progress sandbox.PoolProgressReporter, serverDefaults dockerworker.ServerDefaults) dockerworker.Config {
	return dockerworker.Config{
		// Both directions are VSOCK for a libkrun microVM: the guest dials host
		// CID 2 for the control plane, and the agent listens on its own VSOCK
		// port. The schemes are the whole configuration.
		ControlPlaneURL:      wire.VSOCKURL(guestvsock.HostCID, controlPlaneVSOCKPort),
		AgentListenURL:       wire.VSOCKListenURL(agentVSOCKPort),
		Image:                dockerworker.EffectivePoolImage(cfg.WorkerImage, serverDefaults),
		ImageRetention:       serverDefaults.ImageRetention,
		ImageCache:           serverDefaults.ImageCache,
		Labels:               map[string]string{labelProviderType: ProviderType},
		DevelopmentImageSync: imageSync,
		ProgressReporter:     progress,
		ProxyAuditRetention:  cfg.ProxyAuditRetention.Value(),
		SandboxIdleTimeout:   cfg.SandboxIdleTimeout.Value(),
	}
}

func driverConfig(cfg Config, image *guestimage.Resolver, progress sandbox.PoolProgressReporter) DriverConfig {
	parsed, _ := endpoint.Parse(effectiveControlPlaneSocket(cfg.ControlPlaneSocket))
	return DriverConfig{
		Image:              image,
		StateDir:           effectiveStateDir(cfg.StateDir),
		RuntimeDir:         effectiveRuntimeDir(cfg.RuntimeDir),
		ControlPlaneSocket: parsed.Value,
		PasstPath:          cfg.PasstPath,
		LibraryPath:        cfg.LibkrunPath,
		VCPUs:              cfg.VCPUs,
		MemoryMiB:          cfg.MemoryMiB,
		DataDiskGiB:        cfg.DataDiskGiB,
		CacheDiskGiB:       cfg.CacheDiskGiB,
		ProgressReporter:   progress,
	}
}

// imageResolver builds the resolver for the one image a libkrun pool boots.
// The guest image's own kernel and initrd belong to backends that boot a
// distribution kernel and are not in it at all.
//
// It fetches through images, the server's image store; Validate, fetching
// nothing, passes none. A release server boots the image its release manifest
// names and never a local build, so what a release runs is what it shipped.
func imageResolver(cfg Config, defaults dockerworker.ServerDefaults) (*guestimage.Resolver, error) {
	localDir := defaultString(cfg.VMImageLocalDir, effectiveImageLocalDir(""))
	if defaults.Release != nil {
		cfg.VMImage = defaults.Release.Images.Libkrun
		cfg.VMImageDir = ""
		localDir = ""
	}
	return guestimage.New(guestimage.Config{
		Images:      defaults.ImageCache,
		Reference:   defaultString(cfg.VMImage, DefaultImage),
		OverrideDir: strings.TrimSpace(cfg.VMImageDir),
		LocalDir:    localDir,
		CacheDir:    effectiveImageCacheDir(cfg.ImageCacheDir),
		Artifacts: []guestimage.Artifact{
			{Name: rootArtifact},
			{Name: kernelArtifact},
			{Name: libraryArtifact},
			{Name: passtArtifact},
		},
	})
}

func effectiveControlPlaneSocket(value string) string {
	if strings.TrimSpace(value) == "" {
		return endpoint.DefaultEndpoint()
	}
	return strings.TrimSpace(value)
}

func defaultString(value, fallback string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return fallback
}

func effectiveInt64(value, fallback int64) int64 {
	if value <= 0 {
		return fallback
	}
	return value
}

// defaultVCPUs and defaultMemoryMiB size a pool VM from the host: every vCPU,
// and half the memory (see vmsize, which every local VM provider shares, and
// krunvm.DefaultHostResources, which clamps it to libkrun). They are functions
// rather than constants because the answer depends on the machine.
func defaultVCPUs() int {
	return int(krunvm.DefaultHostResources().CPUCount)
}

func defaultMemoryMiB() int {
	return int(krunvm.DefaultHostResources().MemoryBytes / (1024 * 1024))
}

// Definition describes the libkrun provider for provider catalogs.
func Definition() sandbox.ProviderDefinition {
	return sandbox.ProviderDefinition{
		Name:        "libkrun",
		Icon:        "server",
		Description: "Runs one Linux KVM-backed libkrun microVM per pool with VSOCK control traffic and outbound-only user-mode networking.",
		// Each pool VM is sized from the pool's own size first (see vmsize).
		PoolSizeFields: vmsize.PoolSizeFields(),
		ConfigFields: append([]sandbox.ProviderConfigField{
			{Key: "vmImage", Label: "VM Image", Type: "string", Placeholder: DefaultImage, Description: "Published libkrun image carrying the root filesystem, the kernel, libkrun, and passt.", Advanced: true},
			{Key: "vmImageDir", Label: "VM Artifact Directory", Type: "string", Description: "Boot these artifacts instead of the published image, and fail if they are missing.", Advanced: true},
			{Key: "vmImageLocalDir", Label: "Local VM Build", Type: "string", Placeholder: effectiveImageLocalDir(""), Description: "Where a local libkrun image build lands; used automatically when complete.", Advanced: true},
			{Key: "workerImage", Label: "Worker Image", Type: "string", Placeholder: dockerworker.DefaultPoolImage, Description: "Pool-agent container image launched inside each VM.", Advanced: true},
			{Key: "vcpus", Label: "VM vCPUs", Type: "number", Placeholder: strconv.Itoa(defaultVCPUs()), Description: "Defaults to every host vCPU, up to libkrun's limit of 255. A pool's own cpuVcpus overrides it for that pool."},
			{Key: "memoryMiB", Label: "VM Memory (MiB)", Type: "number", Placeholder: strconv.Itoa(defaultMemoryMiB()), Description: "Defaults to half of host memory. A pool's own memoryBytes overrides it for that pool."},
			{Key: "dataDiskGiB", Label: "Data Disk (GiB)", Type: "number", Placeholder: strconv.FormatInt(defaultDataDiskGiB, 10)},
			{Key: "cacheDiskGiB", Label: "Cache Disk (GiB)", Type: "number", Placeholder: strconv.FormatInt(defaultCacheDiskGiB, 10)},
			{Key: "stateDir", Label: "Pool Disk Directory", Type: "string", Placeholder: defaultStateDir(), Advanced: true},
			{Key: "runtimeDir", Label: "VM Runtime Directory", Type: "string", Placeholder: defaultRuntimeDir(), Advanced: true},
			{Key: "imageCacheDir", Label: "Image Cache", Type: "string", Placeholder: defaultImageRoot(), Advanced: true},
			{Key: "controlPlaneSocket", Label: "Control Plane Unix Socket", Type: "string", Placeholder: endpoint.DefaultEndpoint(), Advanced: true},
			{Key: "passtPath", Label: "passt Path", Type: "string", Description: "Run this passt instead of the one the VM image carries.", Advanced: true},
			{Key: "libkrunPath", Label: "libkrun Library Path", Type: "string", Description: "Load this libkrun instead of the one the VM image carries.", Advanced: true},
		}, poolruntime.PoolPolicyConfigFields()...),
	}
}

func defaultStateDir() string {
	return stateDir(storageNamespace)
}

func legacyStateDir() string {
	return stateDir(legacyStorageNamespace)
}

func stateDir(namespace string) string {
	if value := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); value != "" {
		return filepath.Join(value, "discobox", namespace)
	}
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		return filepath.Join(home, ".local", "share", "discobox", namespace)
	}
	return filepath.Join(os.TempDir(), "discobox-state", namespace)
}

func defaultRuntimeDir() string {
	return runtimeDir(storageNamespace)
}

func legacyRuntimeDir() string {
	return runtimeDir(legacyStorageNamespace)
}

func runtimeDir(namespace string) string {
	if value := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); value != "" {
		return filepath.Join(value, "discobox", namespace)
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("discobox-%d", os.Getuid()), namespace)
}

// defaultImageRoot holds the pulled libkrun images.
//
// It is a dotted directory inside the pool disk root rather than a sibling of
// it, and the dot is what makes that safe: a pool's disks live at
// <stateDir>/<poolID>, and a pool ID has to start with a letter or a digit, so
// no pool can ever be given this name.
//
// It is rooted at the canonical state directory and never at the pre-rename
// one. The legacy path exists to find disks that were created under it; nothing
// ever cached an image there, so following it would only put the cache
// somewhere `task build:vm-krun` does not write.
func defaultImageRoot() string {
	return filepath.Join(defaultStateDir(), ".images")
}

func effectiveImageCacheDir(configured string) string {
	if value := strings.TrimSpace(configured); value != "" {
		return filepath.Join(value, "vm")
	}
	return filepath.Join(defaultImageRoot(), "vm")
}

// effectiveImageLocalDir names where a local libkrun image build lands. It is
// the same path `task build:vm-krun` writes, and that agreement is the whole
// mechanism: nothing is configured to adopt a local build.
func effectiveImageLocalDir(configured string) string {
	if value := strings.TrimSpace(configured); value != "" {
		return value
	}
	return filepath.Join(defaultImageRoot(), "vm", "local")
}

func effectiveStateDir(configured string) string {
	return effectiveStorageDir(configured, defaultStateDir(), legacyStateDir())
}

func effectiveRuntimeDir(configured string) string {
	return effectiveStorageDir(configured, defaultRuntimeDir(), legacyRuntimeDir())
}

// effectiveStorageDir keeps using a pre-rename directory when one exists, so a
// machine that ran the provider under its old name still finds its pool disks.
func effectiveStorageDir(configured, canonical, legacy string) string {
	if configured = strings.TrimSpace(configured); configured != "" {
		return configured
	}
	if info, err := os.Stat(legacy); err == nil && info.IsDir() {
		return legacy
	}
	return canonical
}
