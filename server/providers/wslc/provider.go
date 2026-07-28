// Package wslc registers the Windows "wslc" provider type: each pool gets one
// dedicated WSL Containers (wslc) VM with its own dockerd, launched through the
// vendored wslcsession library. The shared dockerworker engine still owns the
// pool-agent container and all Docker behavior; this package owns only the
// per-pool VM lifecycle and the two connection leases into it.
//
// wslc absorbs everything libkrun's Rust launcher has to build by hand — the
// kernel, the network, the disks — so this driver is structurally the local
// docker driver: NewSession for VM lifecycle, DialGuest* for the leases. VMs
// are deliberately non-persistent: they die with the discobox-server process.
// Only each pool's /var/lib/docker survives (a VHD under StorageDir), which is
// sufficient because the guest dockerd keeps all image/layer state there.
package wslc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/providers/dockerworker"
	"github.com/obot-platform/discobox/server/providers/poolruntime"
	"github.com/obot-platform/discobox/server/providers/wslc/relay"
)

const (
	ProviderType = "wslc"

	defaultAgentPort  = 3002
	defaultCPUCount   = 2
	defaultMemoryMiB  = 4096
	defaultMaxStgMiB  = 65536 // 64 GiB, dynamically expanding
	labelProviderType = "discobox.provider_type"
)

// Config is the persisted wslc provider configuration.
type Config struct {
	// ControlPlaneURL is the URL the in-guest pool-agent registers with. It
	// defaults to the Unix socket the guest relay serves, so the Windows server
	// never listens on TCP. Left configurable for tests.
	ControlPlaneURL string `json:"controlPlaneUrl,omitempty"`
	// WorkerImage is the pool-agent container image launched inside each VM.
	WorkerImage string `json:"workerImage,omitempty"`
	// StorageDir is the root under which each pool gets a private VHD-backed
	// /var/lib/docker at <StorageDir>/<poolID>. It defaults to a per-user
	// location rather than to wslc's own default, which is RAM-backed tmpfs:
	// that is far too small to build or hold pool images, and the failure it
	// produces ("no space left on device", part-way through an image build) does
	// not point at storage configuration.
	StorageDir    string `json:"storageDir,omitempty"`
	CPUCount      int    `json:"cpuCount,omitempty"`
	MemoryMiB     int    `json:"memoryMiB,omitempty"`
	MaxStorageMiB int64  `json:"maxStorageMiB,omitempty"`
	AgentPort     int    `json:"agentPort,omitempty"`
	// RelayStagingDir overrides where the embedded guest relay is written before
	// being mounted into guests.
	RelayStagingDir string `json:"relayStagingDir,omitempty"`
}

func Decode(data json.RawMessage) (Config, error) {
	return poolruntime.DecodeConfig[Config](data, ProviderType)
}

func Validate(data json.RawMessage) error {
	cfg, err := Decode(data)
	if err != nil {
		return err
	}
	if dir := strings.TrimSpace(cfg.StorageDir); dir != "" && !filepath.IsAbs(dir) {
		return fmt.Errorf("wslc storageDir must be an absolute path")
	}
	if cfg.CPUCount < 0 || cfg.MemoryMiB < 0 || cfg.MaxStorageMiB < 0 {
		return fmt.Errorf("wslc sizing values must not be negative")
	}
	if cfg.AgentPort < 0 || cfg.AgentPort > 65535 {
		return fmt.Errorf("wslc agentPort must be between 0 and 65535")
	}
	return nil
}

func FactoryWithPoolManager(poolManager poolruntime.PoolManager, imageSync *dockerworker.DevelopmentImageSynchronizer, streams StreamSink) sandbox.ProviderFactory {
	return func(ctx context.Context, instance *model.SandboxProviderInstance) (sandbox.Provider, error) {
		return newFromInstance(ctx, instance, poolManager, imageSync, streams)
	}
}

func newFromInstance(_ context.Context, instance *model.SandboxProviderInstance, poolManager poolruntime.PoolManager, imageSync *dockerworker.DevelopmentImageSynchronizer, streams StreamSink) (sandbox.Provider, error) {
	// Every pool needs the guest relay, which is embedded at build time. Fail
	// here, where the message can name the build task, rather than at pool
	// start with a missing-file error from inside a VM.
	if !relay.Available() {
		return nil, relay.ErrNotBuilt
	}
	cfg, err := Decode(instance.Config)
	if err != nil {
		return nil, err
	}
	driver, err := NewDriver(driverConfig(cfg, streams))
	if err != nil {
		return nil, err
	}
	engine, err := dockerworker.New(engineConfig(cfg, imageSync), driver)
	if err != nil {
		_ = driver.Close()
		return nil, err
	}
	return poolruntime.New(engine, Definition(), poolManager), nil
}

// engineConfig renders the pool engine configuration for one provider instance.
// It is separate from newFromInstance so the invariants below can be asserted
// without a VM, a Docker daemon, or a pool manager.
func engineConfig(cfg Config, imageSync *dockerworker.DevelopmentImageSynchronizer) dockerworker.Config {
	return dockerworker.Config{
		// The agent reaches the control plane over a Unix socket the guest relay
		// serves, so no host TCP listener — and therefore no Windows firewall
		// prompt — is ever needed. The scheme is the entire configuration.
		ControlPlaneURL: effectiveControlPlaneURL(cfg.ControlPlaneURL),
		RelaySocketDir:  GuestSocketDir,
		// wslc persists only /var/lib/docker, so pool state is placed inside it.
		// The container still sees layout.ContainerRoot; only the daemon-side
		// location moves, which is exactly what HostStateRoot expresses.
		HostStateRoot: GuestStateRoot,
		Image:         dockerworker.EffectivePoolImage(cfg.WorkerImage),
		// The agent still listens on the guest's loopback; the control plane
		// reaches it by opening a stream on the same relay session.
		AgentPort: effectiveInt(cfg.AgentPort, defaultAgentPort),
		// Publish that port at its fixed number inside the guest, as every other
		// VM-backed driver does. The relay dials a fixed guest address, so a
		// loopback-ephemeral binding — the local driver's default — leaves
		// nothing listening where the relay connects, and every control-plane
		// request to the agent fails with EOF. This opens no port on Windows:
		// the guest is a private VM whose only inbound path is the relay
		// session itself.
		PublicAgentPort:      true,
		Systemd:              true,
		Labels:               map[string]string{labelProviderType: ProviderType},
		DevelopmentImageSync: imageSync,
	}
}

func driverConfig(cfg Config, streams StreamSink) DriverConfig {
	return DriverConfig{
		StorageDir:          effectiveStorageDir(cfg.StorageDir),
		CPUCount:            effectiveInt(cfg.CPUCount, defaultCPUCount),
		MemoryMiB:           effectiveInt(cfg.MemoryMiB, defaultMemoryMiB),
		MaxStorageMiB:       effectiveInt64(cfg.MaxStorageMiB, defaultMaxStgMiB),
		AgentPort:           effectiveInt(cfg.AgentPort, defaultAgentPort),
		RelayStagingDir:     effectiveRelayStagingDir(cfg.RelayStagingDir),
		ControlPlaneStreams: streams,
	}
}

// effectiveControlPlaneURL keeps the guest socket as the default. It stays
// configurable so a test can point the agent somewhere reachable without a relay.
func effectiveControlPlaneURL(configured string) string {
	if value := strings.TrimSpace(configured); value != "" {
		return value
	}
	return ControlPlaneURL
}

// effectiveStorageDir keeps every pool on a real VHD by default. wslc's own
// default is ephemeral tmpfs, which cannot hold an image build.
func effectiveStorageDir(configured string) string {
	if value := strings.TrimSpace(configured); value != "" {
		return value
	}
	if local := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); local != "" {
		return filepath.Join(local, "discobox", "wslc")
	}
	return filepath.Join(os.TempDir(), "discobox-wslc")
}

// effectiveRelayStagingDir picks where the embedded relay is written for
// mounting into guests. All pools share it: the binary is identical.
func effectiveRelayStagingDir(configured string) string {
	if value := strings.TrimSpace(configured); value != "" {
		return value
	}
	if local := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); local != "" {
		return filepath.Join(local, "discobox", "relay")
	}
	return filepath.Join(os.TempDir(), "discobox-relay")
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

// Definition describes the wslc provider for provider catalogs.
func Definition() sandbox.ProviderDefinition {
	return sandbox.ProviderDefinition{
		Name:        "wslc",
		Icon:        "server",
		Description: "Runs one WSL Containers (wslc) VM per pool on Windows, with its own dockerd. Only /var/lib/docker persists; no host TCP port is opened.",
		ConfigFields: []sandbox.ProviderConfigField{
			{Key: "workerImage", Label: "Worker Image", Type: "string", Placeholder: dockerworker.DefaultPoolImage, Description: "Pool-agent container image launched inside each VM.", Advanced: true},
			{Key: "cpuCount", Label: "VM vCPUs", Type: "number", Placeholder: strconv.Itoa(defaultCPUCount)},
			{Key: "memoryMiB", Label: "VM Memory (MiB)", Type: "number", Placeholder: strconv.Itoa(defaultMemoryMiB)},
			{Key: "maxStorageMiB", Label: "Max /var/lib/docker (MiB)", Type: "number", Placeholder: strconv.FormatInt(defaultMaxStgMiB, 10)},
			{Key: "storageDir", Label: "VM Storage Directory", Type: "string", Placeholder: effectiveStorageDir(""), Description: "Root directory holding each pool's persistent /var/lib/docker VHD.", Advanced: true},
			{Key: "agentPort", Label: "Harness Port", Type: "number", Placeholder: strconv.Itoa(defaultAgentPort), Advanced: true},
		},
	}
}
