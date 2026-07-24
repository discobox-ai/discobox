// Package libkrun registers the Linux libkrun provider. Each pool gets one
// microVM while dockerworker.Engine continues to own the pool-agent container
// and Docker behavior inside that VM.
package libkrun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/obot-platform/discobox/localipc"
	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/providers/dockerworker"
	"github.com/obot-platform/discobox/server/providers/poolruntime"
)

const (
	ProviderType = "libkrun"

	defaultVCPUs        = 2
	defaultMemoryMiB    = 4096
	defaultDataDiskGiB  = 64
	defaultCacheDiskGiB = 32

	storageNamespace = "libkrun"
	// Preserve the original default directory namespace so existing launchers
	// and writable disks remain adoptable after the provider-type rename.
	legacyStorageNamespace = "local-vm"

	controlPlaneVSOCKPort = 3001
	agentVSOCKPort        = 3002
	lifecycleVSOCKPort    = 3003
	dockerVSOCKPort       = 3004

	labelProviderType = "discobox.provider_type"
)

// Config is the persisted libkrun provider configuration.
type Config struct {
	RootImage          string `json:"rootImage,omitempty"`
	KernelImage        string `json:"kernelImage,omitempty"`
	StateDir           string `json:"stateDir,omitempty"`
	RuntimeDir         string `json:"runtimeDir,omitempty"`
	ControlPlaneSocket string `json:"controlPlaneSocket,omitempty"`
	LauncherPath       string `json:"launcherPath,omitempty"`
	MkfsPath           string `json:"mkfsPath,omitempty"`
	WorkerImage        string `json:"workerImage,omitempty"`
	VCPUs              int    `json:"vcpus,omitempty"`
	MemoryMiB          int    `json:"memoryMiB,omitempty"`
	DataDiskGiB        int64  `json:"dataDiskGiB,omitempty"`
	CacheDiskGiB       int64  `json:"cacheDiskGiB,omitempty"`
}

func Decode(data json.RawMessage) (Config, error) {
	return poolruntime.DecodeConfig[Config](data, ProviderType)
}

func Validate(data json.RawMessage) error {
	cfg, err := Decode(data)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(strings.TrimSpace(cfg.RootImage)) {
		return fmt.Errorf("libkrun rootImage must be an absolute path")
	}
	if !filepath.IsAbs(strings.TrimSpace(cfg.KernelImage)) {
		return fmt.Errorf("libkrun kernelImage must be an absolute path")
	}
	for name, value := range map[string]string{
		"stateDir":   cfg.StateDir,
		"runtimeDir": cfg.RuntimeDir,
	} {
		if strings.TrimSpace(value) != "" && !filepath.IsAbs(value) {
			return fmt.Errorf("libkrun %s must be an absolute path", name)
		}
	}
	endpoint := effectiveControlPlaneSocket(cfg.ControlPlaneSocket)
	parsed, err := localipc.Parse(endpoint)
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
	return nil
}

func FactoryWithPoolManager(poolManager poolruntime.PoolManager, imageSync *dockerworker.DevelopmentImageSynchronizer) sandbox.ProviderFactory {
	return func(ctx context.Context, instance *model.SandboxProviderInstance) (sandbox.Provider, error) {
		return newFromInstance(ctx, instance, poolManager, imageSync)
	}
}

func newFromInstance(_ context.Context, instance *model.SandboxProviderInstance, poolManager poolruntime.PoolManager, imageSync *dockerworker.DevelopmentImageSynchronizer) (sandbox.Provider, error) {
	cfg, err := Decode(instance.Config)
	if err != nil {
		return nil, err
	}
	driver, err := NewDriver(driverConfig(cfg))
	if err != nil {
		return nil, err
	}
	engine, err := dockerworker.New(dockerworker.Config{
		ControlPlaneURL:       localipc.LogicalHTTPBaseURL,
		Image:                 dockerworker.EffectivePoolImage(cfg.WorkerImage),
		AgentVSOCKPort:        agentVSOCKPort,
		ControlPlaneVSOCKPort: controlPlaneVSOCKPort,
		Systemd:               true,
		Labels:                map[string]string{labelProviderType: ProviderType},
		DevelopmentImageSync:  imageSync,
	}, driver)
	if err != nil {
		_ = driver.Close()
		return nil, err
	}
	return poolruntime.New(engine, Definition(), poolManager), nil
}

func driverConfig(cfg Config) DriverConfig {
	endpoint, _ := localipc.Parse(effectiveControlPlaneSocket(cfg.ControlPlaneSocket))
	return DriverConfig{
		RootImage:          cfg.RootImage,
		KernelImage:        cfg.KernelImage,
		StateDir:           effectiveStateDir(cfg.StateDir),
		RuntimeDir:         effectiveRuntimeDir(cfg.RuntimeDir),
		ControlPlaneSocket: endpoint.Value,
		LauncherPath:       cfg.LauncherPath,
		MkfsPath:           cfg.MkfsPath,
		VCPUs:              effectiveInt(cfg.VCPUs, defaultVCPUs),
		MemoryMiB:          effectiveInt(cfg.MemoryMiB, defaultMemoryMiB),
		DataDiskGiB:        effectiveInt64(cfg.DataDiskGiB, defaultDataDiskGiB),
		CacheDiskGiB:       effectiveInt64(cfg.CacheDiskGiB, defaultCacheDiskGiB),
	}
}

func effectiveControlPlaneSocket(value string) string {
	if strings.TrimSpace(value) == "" {
		return localipc.DefaultEndpoint()
	}
	return strings.TrimSpace(value)
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

// Definition describes the libkrun provider for provider catalogs.
func Definition() sandbox.ProviderDefinition {
	return sandbox.ProviderDefinition{
		Name:        "libkrun",
		Icon:        "server",
		Description: "Runs one Linux KVM-backed libkrun microVM per pool with VSOCK control traffic and outbound-only user-mode networking.",
		ConfigFields: []sandbox.ProviderConfigField{
			{Key: "rootImage", Label: "Root QCOW2 Image", Type: "string", Required: true, Description: "Absolute path to the immutable root image built by the libkrun image builder."},
			{Key: "kernelImage", Label: "Linux Kernel Image", Type: "string", Required: true, Description: "Absolute path to the Docker-built Discobox libkrun kernel."},
			{Key: "workerImage", Label: "Worker Image", Type: "string", Placeholder: dockerworker.DefaultPoolImage, Description: "Pool-agent container image launched inside each VM.", Advanced: true},
			{Key: "vcpus", Label: "VM vCPUs", Type: "number", Placeholder: strconv.Itoa(defaultVCPUs)},
			{Key: "memoryMiB", Label: "VM Memory (MiB)", Type: "number", Placeholder: strconv.Itoa(defaultMemoryMiB)},
			{Key: "dataDiskGiB", Label: "Data Disk (GiB)", Type: "number", Placeholder: strconv.FormatInt(defaultDataDiskGiB, 10)},
			{Key: "cacheDiskGiB", Label: "Cache Disk (GiB)", Type: "number", Placeholder: strconv.FormatInt(defaultCacheDiskGiB, 10)},
			{Key: "stateDir", Label: "VM State Directory", Type: "string", Placeholder: defaultStateDir(), Advanced: true},
			{Key: "runtimeDir", Label: "VM Runtime Directory", Type: "string", Placeholder: defaultRuntimeDir(), Advanced: true},
			{Key: "controlPlaneSocket", Label: "Control Plane Unix Socket", Type: "string", Placeholder: localipc.DefaultEndpoint(), Advanced: true},
			{Key: "launcherPath", Label: "discobox-krun Path", Type: "string", Placeholder: "discobox-krun", Advanced: true},
			{Key: "mkfsPath", Label: "mkfs.ext4 Path", Type: "string", Placeholder: "mkfs.ext4", Advanced: true},
		},
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

func effectiveStateDir(configured string) string {
	return effectiveStorageDir(configured, defaultStateDir(), legacyStateDir())
}

func effectiveRuntimeDir(configured string) string {
	return effectiveStorageDir(configured, defaultRuntimeDir(), legacyRuntimeDir())
}

func effectiveStorageDir(configured, canonical, legacy string) string {
	if configured = strings.TrimSpace(configured); configured != "" {
		return configured
	}
	if info, err := os.Stat(legacy); err == nil && info.IsDir() {
		return legacy
	}
	return canonical
}
