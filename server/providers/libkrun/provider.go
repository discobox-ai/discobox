// Package libkrun registers the Linux libkrun provider. Each pool gets one
// microVM while dockerworker.Engine continues to own the pool-agent container
// and Docker behavior inside that VM.
//
// The VM boots the same guest image a vz pool does, pulled straight from a
// registry by server/providers/guestimage, plus a libkrunfw-patched kernel that
// is libkrun's alone. Nothing is built on the host to start a pool, and libkrun
// itself is dlopened by the launcher child rather than linked into the server,
// so a machine that never enables this provider needs none of it installed
// (ADR 0013, ADR 0062 §9).
package libkrun

import (
	"context"
	"encoding/json"
	"fmt"
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
)

const (
	ProviderType = "libkrun"

	// Disk sizes are ceilings, not allocations: both images are sparse and the
	// guest grows into them. The data disk holds everything that survives a
	// pool restart — images, layers, volumes, containers — so it is sized for a
	// real workload rather than for the first sandbox.
	defaultDataDiskGiB  = 100
	defaultCacheDiskGiB = 32

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

// The artifacts each image carries. The guest image publishes a kernel and an
// initrd too, and this backend wants neither: libkrun boots the patched kernel
// from its own image, which has every driver this guest needs built in.
const (
	rootArtifact   = "root.ext4"
	kernelArtifact = "vmlinux"
)

// DefaultKernelImage is the published libkrun guest kernel.
//
// It is a second release line rather than a file in the guest image because the
// two move on unrelated clocks: this changes when libkrunfw or upstream Linux
// does, the guest when Debian or Docker does. Folding them together would make
// every guest rebuild compile a kernel and every kernel bump republish a
// userland.
//
// A digest rather than a tag, for the reason the guest image is one: a tag
// would let whoever runs the server decide which kernel they boot.
const DefaultKernelImage = "ghcr.io/discobox-ai/discobox-vm-kernel@sha256:23f0ce879e1dc3939fd0f498237d857d11478bec1c570d3f7222194ccf81955f"

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

	// GuestImage and KernelImage override the published images. Each has a
	// directory pair with the same meaning as vz's: an override that is an
	// assertion, and a local build that wins when it is complete.
	GuestImage          string `json:"guestImage,omitempty"`
	GuestImageDir       string `json:"guestImageDir,omitempty"`
	GuestImageLocalDir  string `json:"guestImageLocalDir,omitempty"`
	KernelImage         string `json:"kernelImage,omitempty"`
	KernelImageDir      string `json:"kernelImageDir,omitempty"`
	KernelImageLocalDir string `json:"kernelImageLocalDir,omitempty"`
	// ImageCacheDir holds one directory per pulled image digest, for both.
	// Sharing it is safe and deliberate: the cache is content-addressed.
	ImageCacheDir string `json:"imageCacheDir,omitempty"`

	StateDir           string `json:"stateDir,omitempty"`
	RuntimeDir         string `json:"runtimeDir,omitempty"`
	ControlPlaneSocket string `json:"controlPlaneSocket,omitempty"`
	// PasstPath and LibkrunPath name the two host dependencies this backend
	// has. Both are resolved by the loader or PATH when unset, which is what
	// installing the runtime environment is for.
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

func Validate(data json.RawMessage) error {
	cfg, err := Decode(data)
	if err != nil {
		return err
	}
	for field, value := range map[string]string{
		"guestImageDir":       cfg.GuestImageDir,
		"guestImageLocalDir":  cfg.GuestImageLocalDir,
		"kernelImageDir":      cfg.KernelImageDir,
		"kernelImageLocalDir": cfg.KernelImageLocalDir,
		"imageCacheDir":       cfg.ImageCacheDir,
		"stateDir":            cfg.StateDir,
		"runtimeDir":          cfg.RuntimeDir,
		"passtPath":           cfg.PasstPath,
		"libkrunPath":         cfg.LibkrunPath,
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
	// Building the resolvers is the configuration check: it is what rejects an
	// unparseable reference or a relative path, and it touches no network.
	if _, err := guestResolver(cfg); err != nil {
		return err
	}
	if _, err := kernelResolver(cfg); err != nil {
		return err
	}
	return nil
}

func FactoryWithPoolManager(poolManager poolruntime.PoolManager, imageSync *dockerworker.DevelopmentImageSynchronizer, serverDefaults dockerworker.ServerDefaults) sandbox.ProviderFactory {
	return func(ctx context.Context, instance *model.SandboxProviderInstance) (sandbox.Provider, error) {
		return newFromInstance(ctx, instance, poolManager, imageSync, serverDefaults)
	}
}

func newFromInstance(_ context.Context, instance *model.SandboxProviderInstance, poolManager poolruntime.PoolManager, imageSync *dockerworker.DevelopmentImageSynchronizer, serverDefaults dockerworker.ServerDefaults) (sandbox.Provider, error) {
	cfg, err := Decode(instance.Config)
	if err != nil {
		return nil, err
	}
	guest, err := guestResolver(cfg)
	if err != nil {
		return nil, err
	}
	kernel, err := kernelResolver(cfg)
	if err != nil {
		return nil, err
	}
	progress := sandbox.PoolProgressReporterFor(poolManager)
	driver, err := NewDriver(driverConfig(cfg, guest, kernel, progress))
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
		Image:                dockerworker.EffectivePoolImage(cfg.WorkerImage, serverDefaults.PoolImage),
		ImageRetention:       serverDefaults.ImageRetention,
		Labels:               map[string]string{labelProviderType: ProviderType},
		DevelopmentImageSync: imageSync,
		ProgressReporter:     progress,
		ProxyAuditRetention:  cfg.ProxyAuditRetention.Value(),
		SandboxIdleTimeout:   cfg.SandboxIdleTimeout.Value(),
	}
}

func driverConfig(cfg Config, guest, kernel *guestimage.Resolver, progress sandbox.PoolProgressReporter) DriverConfig {
	parsed, _ := endpoint.Parse(effectiveControlPlaneSocket(cfg.ControlPlaneSocket))
	return DriverConfig{
		Guest:              guest,
		Kernel:             kernel,
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

// guestResolver builds the root filesystem resolver. Only the root is asked
// for: the guest image's kernel and initrd belong to backends that boot a
// distribution kernel, and extracting artifacts this VM will never load would
// cost a machine hundreds of megabytes of cache for nothing.
func guestResolver(cfg Config) (*guestimage.Resolver, error) {
	return guestimage.New(guestimage.Config{
		Reference:   defaultString(cfg.GuestImage, guestimage.DefaultVMImage),
		OverrideDir: strings.TrimSpace(cfg.GuestImageDir),
		LocalDir:    defaultString(cfg.GuestImageLocalDir, effectiveGuestLocalDir("")),
		CacheDir:    effectiveImageCacheDir(cfg.ImageCacheDir, "guest"),
		Artifacts:   []guestimage.Artifact{{Name: rootArtifact}},
	})
}

func kernelResolver(cfg Config) (*guestimage.Resolver, error) {
	return guestimage.New(guestimage.Config{
		Reference:   defaultString(cfg.KernelImage, DefaultKernelImage),
		OverrideDir: strings.TrimSpace(cfg.KernelImageDir),
		LocalDir:    defaultString(cfg.KernelImageLocalDir, effectiveKernelLocalDir("")),
		CacheDir:    effectiveImageCacheDir(cfg.ImageCacheDir, "kernel"),
		Artifacts:   []guestimage.Artifact{{Name: kernelArtifact}},
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

func effectiveInt(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}

func effectiveInt64(value, fallback int64) int64 {
	if value <= 0 {
		return fallback
	}
	return value
}

// defaultVCPUs and defaultMemoryMiB size a pool VM from the host: every vCPU,
// and half the memory (see krunvm.DefaultHostResources). They are functions
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
		ConfigFields: append([]sandbox.ProviderConfigField{
			{Key: "guestImage", Label: "Guest Image", Type: "string", Placeholder: guestimage.DefaultVMImage, Description: "Published guest image carrying the root filesystem.", Advanced: true},
			{Key: "guestImageDir", Label: "Guest Artifact Directory", Type: "string", Description: "Boot these artifacts instead of the published image, and fail if they are missing.", Advanced: true},
			{Key: "guestImageLocalDir", Label: "Local Guest Build", Type: "string", Placeholder: effectiveGuestLocalDir(""), Description: "Where a local guest image build lands; used automatically when complete.", Advanced: true},
			{Key: "kernelImage", Label: "Kernel Image", Type: "string", Placeholder: DefaultKernelImage, Description: "Published image carrying the libkrunfw-patched kernel.", Advanced: true},
			{Key: "kernelImageDir", Label: "Kernel Artifact Directory", Type: "string", Description: "Boot this kernel instead of the published image, and fail if it is missing.", Advanced: true},
			{Key: "kernelImageLocalDir", Label: "Local Kernel Build", Type: "string", Placeholder: effectiveKernelLocalDir(""), Advanced: true},
			{Key: "workerImage", Label: "Worker Image", Type: "string", Placeholder: dockerworker.DefaultPoolImage, Description: "Pool-agent container image launched inside each VM.", Advanced: true},
			{Key: "vcpus", Label: "VM vCPUs", Type: "number", Placeholder: strconv.Itoa(defaultVCPUs()), Description: "Defaults to every host vCPU."},
			{Key: "memoryMiB", Label: "VM Memory (MiB)", Type: "number", Placeholder: strconv.Itoa(defaultMemoryMiB()), Description: "Defaults to half of host memory."},
			{Key: "dataDiskGiB", Label: "Data Disk (GiB)", Type: "number", Placeholder: strconv.FormatInt(defaultDataDiskGiB, 10)},
			{Key: "cacheDiskGiB", Label: "Cache Disk (GiB)", Type: "number", Placeholder: strconv.FormatInt(defaultCacheDiskGiB, 10)},
			{Key: "stateDir", Label: "Pool Disk Directory", Type: "string", Placeholder: defaultStateDir(), Advanced: true},
			{Key: "runtimeDir", Label: "VM Runtime Directory", Type: "string", Placeholder: defaultRuntimeDir(), Advanced: true},
			{Key: "imageCacheDir", Label: "Image Cache", Type: "string", Placeholder: defaultImageRoot(), Advanced: true},
			{Key: "controlPlaneSocket", Label: "Control Plane Unix Socket", Type: "string", Placeholder: endpoint.DefaultEndpoint(), Advanced: true},
			{Key: "passtPath", Label: "passt Path", Type: "string", Placeholder: "passt", Advanced: true},
			{Key: "libkrunPath", Label: "libkrun Library Path", Type: "string", Placeholder: "libkrun.so.1", Advanced: true},
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

// defaultImageRoot holds the pulled guest and kernel images.
//
// It is a dotted directory inside the pool disk root rather than a sibling of
// it, and the dot is what makes that safe: a pool's disks live at
// <stateDir>/<poolID>, and a pool ID has to start with a letter or a digit, so
// no pool can ever be given this name.
//
// It is rooted at the canonical state directory and never at the pre-rename
// one. The legacy path exists to find disks that were created under it; nothing
// ever cached an image there, so following it would only put the cache
// somewhere `task build:vm-guest` does not write.
func defaultImageRoot() string {
	return filepath.Join(defaultStateDir(), ".images")
}

func effectiveImageCacheDir(configured, kind string) string {
	if value := strings.TrimSpace(configured); value != "" {
		return filepath.Join(value, kind)
	}
	return filepath.Join(defaultImageRoot(), kind)
}

// effectiveGuestLocalDir names where a local guest image build lands. It is the
// same path `task build:vm-guest` writes, and that agreement is the whole
// mechanism: nothing is configured to adopt a local build.
func effectiveGuestLocalDir(configured string) string {
	if value := strings.TrimSpace(configured); value != "" {
		return value
	}
	return filepath.Join(defaultImageRoot(), "guest", "local")
}

func effectiveKernelLocalDir(configured string) string {
	if value := strings.TrimSpace(configured); value != "" {
		return value
	}
	return filepath.Join(defaultImageRoot(), "kernel", "local")
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
