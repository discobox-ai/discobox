// Package vz registers the macOS "vz" provider type: each pool gets one
// Virtualization.framework VM running its own dockerd, and the shared
// dockerworker engine still owns the pool-agent container and every Docker
// mechanic (ADR 0062).
//
// The framework is part of macOS, so this backend ships no launcher, no
// hypervisor library, and no VM image to install. Its guest boots from
// artifacts pulled straight from a registry by server/providers/guestimage, and
// the development images are then built inside that guest. A Mac needs a
// codesigned server binary and a network, and nothing else — in particular, no
// Docker daemon on the host.
package vz

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/adrg/xdg"

	guestvsock "github.com/discobox-ai/discobox/pool-agent/vsock"
	"github.com/discobox-ai/discobox/pool-agent/wire"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
	"github.com/discobox-ai/discobox/server/providers/guestimage"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
	"github.com/discobox-ai/discobox/server/providers/vz/internal/vzvm"
)

const (
	ProviderType = "vz"

	// Disk sizes are ceilings, not allocations: both images are sparse and cost
	// only what the guest writes. The data disk holds everything that survives a
	// pool restart — images, layers, volumes, containers — so it is sized for a
	// real workload rather than for the first sandbox.
	defaultDataDiskGiB  = 100
	defaultCacheDiskGiB = 32

	storageNamespace = "vz"

	labelProviderType = "discobox.provider_type"

	// hostShareDir is the host filesystem the guest sees, and hostShareTag is
	// the virtiofs tag it arrives under. /Users is every account's home
	// directory on a Mac and so every checkout a developer would start a
	// sandbox from; nothing outside it is exported, so the share is the user's
	// own files rather than the machine's.
	//
	// The tag is a contract with the guest image, which mounts it at
	// hostShareDir — the same absolute path the Mac uses. That equality is what
	// makes a host path usable verbatim as a bind source inside the guest, so
	// renaming either is a coordinated guest release, like a VSOCK port.
	hostShareDir = "/Users"
	hostShareTag = "discobox-users"
)

// The boot artifacts the guest image publishes. The initrd is required in
// practice — the distribution builds virtio as modules — but is declared
// optional so a guest image built with those drivers compiled in still boots.
const (
	kernelArtifact = "vmlinux"
	initrdArtifact = "initrd.img"
	rootArtifact   = "root.ext4"
)

// guestImageDockerfile is the Dockerfile in a discobox checkout that produces
// the whole artifact set, in the path form BuildKit's frontend wants. Its
// context is the repository root, which is why the path is spelled from there.
const guestImageDockerfile = "server/providers/vz/image/Dockerfile"

// guestImagePlatform is what the guest runs on, which is the Mac's own
// architecture: the artifacts boot on Virtualization.framework, and the
// framework runs a guest of the host's architecture or none at all. It is
// pinned rather than left to the builder because the builder is a Linux daemon
// inside the VM, whose idea of "native" is the same only by coincidence.
func guestImagePlatform() string {
	return "linux/" + runtime.GOARCH
}

// DefaultGuestImage is the published VM image. It is released and versioned on
// its own line, independently of the discobox release, because it is a
// distribution userland that boots dockerd and changes when the distribution
// does rather than when Discobox does (ADR 0062 §3). A release pins this to a
// digest; a tag here means whoever runs the server decides which build they
// get.
//
// Published as discobox-vm, with no backend in the name: vz is the only driver
// that boots it today, but libkrun is expected to boot the same artifacts once
// ADR 0062 §9 lands. It is deliberately not called a pool image — that already
// means the pool-agent container (dockerworker.DefaultPoolImage), which every
// VM provider exposes as workerImage alongside this one.
const DefaultGuestImage = "ghcr.io/discobox-ai/discobox-vm@sha256:689cb9bc05c1304358209ae9afeec70d2ee7b5d0cda786c558e2bcef6f4a76dd"

// Config is the persisted vz provider configuration.
type Config struct {
	poolruntime.PoolPolicy

	// GuestImage overrides the published guest image.
	GuestImage string `json:"guestImage,omitempty"`
	// GuestImageDir boots artifacts already on disk instead of pulling any. It
	// is an assertion: a broken directory fails rather than falling back, which
	// is what makes it usable for bisecting a guest image.
	GuestImageDir string `json:"guestImageDir,omitempty"`
	// GuestImageLocalDir is the conventional output of a local guest image
	// build, preferred over the published image when it is complete and ignored
	// when it is not. Defaulted, so `task build:vz-guest` is the entire act of
	// adopting a locally built guest and deleting its directory is the entire
	// act of going back.
	GuestImageLocalDir string `json:"guestImageLocalDir,omitempty"`
	// GuestImageCacheDir holds one directory per pulled guest image digest.
	GuestImageCacheDir string `json:"guestImageCacheDir,omitempty"`
	// StateDir roots each pool's data and cache disks.
	StateDir string `json:"stateDir,omitempty"`
	// WorkerImage is the pool-agent container image launched inside each VM.
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
		"guestImageDir":      cfg.GuestImageDir,
		"guestImageLocalDir": cfg.GuestImageLocalDir,
		"guestImageCacheDir": cfg.GuestImageCacheDir,
		"stateDir":           cfg.StateDir,
	} {
		if dir := strings.TrimSpace(value); dir != "" && !filepath.IsAbs(dir) {
			return fmt.Errorf("vz %s must be an absolute path", field)
		}
	}
	if cfg.VCPUs < 0 || cfg.MemoryMiB < 0 || cfg.DataDiskGiB < 0 || cfg.CacheDiskGiB < 0 {
		return fmt.Errorf("vz sizing values must not be negative")
	}
	if cfg.VCPUs > 255 {
		return fmt.Errorf("vz vcpus must not exceed 255")
	}
	if cfg.MemoryMiB > 0 && cfg.MemoryMiB < 512 {
		return fmt.Errorf("vz memoryMiB must be at least 512")
	}
	if cfg.DataDiskGiB > 4096 || cfg.CacheDiskGiB > 4096 {
		return fmt.Errorf("vz disk sizes must not exceed 4096 GiB")
	}
	// Building the resolver is the configuration check: it is what rejects an
	// unparseable reference or a relative path, and it touches no network.
	if _, err := guestResolver(cfg); err != nil {
		return err
	}
	return nil
}

func FactoryWithPoolManager(poolManager poolruntime.PoolManager, imageSync *dockerworker.DevelopmentImageSynchronizer, streams StreamSink) sandbox.ProviderFactory {
	return func(ctx context.Context, instance *model.SandboxProviderInstance) (sandbox.Provider, error) {
		return newFromInstance(ctx, instance, poolManager, imageSync, streams)
	}
}

func newFromInstance(_ context.Context, instance *model.SandboxProviderInstance, poolManager poolruntime.PoolManager, imageSync *dockerworker.DevelopmentImageSynchronizer, streams StreamSink) (sandbox.Provider, error) {
	cfg, err := Decode(instance.Config)
	if err != nil {
		return nil, err
	}
	guest, err := guestResolver(cfg)
	if err != nil {
		return nil, err
	}
	progress := sandbox.PoolProgressReporterFor(poolManager)
	driver, err := NewDriver(DriverConfig{
		Guest:               guest,
		StateDir:            effectiveStateDir(cfg.StateDir),
		VCPUs:               cfg.VCPUs,
		MemoryMiB:           cfg.MemoryMiB,
		DataDiskGiB:         cfg.DataDiskGiB,
		CacheDiskGiB:        cfg.CacheDiskGiB,
		SharedDirectories:   hostShares(),
		ControlPlaneStreams: streams,
		ProgressReporter:    progress,
	})
	if err != nil {
		return nil, err
	}
	engine, err := dockerworker.New(engineConfig(cfg, imageSync, progress), driver)
	if err != nil {
		_ = driver.Close()
		return nil, err
	}
	definition := Definition()
	definition.LocalSourceRoots = localSourceRoots()
	return poolruntime.New(engine, definition, poolManager), nil
}

// hostShares are the host directories every pool VM exports to its guest.
//
// It is one directory, /Users, and it is exported read-only: a sandbox reads a
// developer's checkout to clone it and has no business writing to the files on
// their Mac. Everything the sandbox then does happens in its own copy on the
// pool's data disk.
//
// A Mac that somehow has no /Users exports nothing rather than failing to start
// pools: the share would be rejected by the framework, and this backend has to
// run on a machine whose sources are pushed instead.
func hostShares() []vzvm.SharedDirectory {
	if info, err := os.Stat(hostShareDir); err != nil || !info.IsDir() {
		return nil
	}
	return []vzvm.SharedDirectory{{Tag: hostShareTag, HostPath: hostShareDir, ReadOnly: true}}
}

// hostMounts carries the shares on into the pool-agent container, where the
// engine binds each under /host. That is the path the pool agent clones from,
// so a share the guest has and the container does not is a share nothing can
// use.
func hostMounts() []dockerworker.HostMount {
	shares := hostShares()
	mounts := make([]dockerworker.HostMount, 0, len(shares))
	for _, share := range shares {
		mounts = append(mounts, dockerworker.HostMount{Source: share.HostPath, ReadOnly: share.ReadOnly})
	}
	return mounts
}

// localSourceRoots are the host paths a sandbox on this backend can clone a
// local source directory out of, which is exactly the set the guest is given.
//
// Publishing them is what stops the client pushing a repository it does not
// have to: a source under one of these roots is delivered by the sandbox
// cloning it through the share (see server/internal/resources/sandboxes,
// sourceNeedsPush). Unlike the Docker provider there is no remote-daemon case
// to exclude — a Virtualization.framework VM is always on this machine.
func localSourceRoots() []string {
	shares := hostShares()
	roots := make([]string, 0, len(shares))
	for _, share := range shares {
		roots = append(roots, share.HostPath)
	}
	return roots
}

// engineConfig renders the pool engine configuration for one provider instance.
// It is separate from newFromInstance so the transport invariants can be
// asserted without a VM or a pool manager.
func engineConfig(cfg Config, imageSync *dockerworker.DevelopmentImageSynchronizer, progress sandbox.PoolProgressReporter) dockerworker.Config {
	return dockerworker.Config{
		// Both directions are VSOCK, as for libkrun: the agent dials host CID 2
		// for the control plane and listens on its own port for inbound
		// requests. macOS opens no TCP listener and raises no firewall prompt.
		ControlPlaneURL:      wire.VSOCKURL(guestvsock.HostCID, controlPlaneVSOCKPort),
		AgentListenURL:       wire.VSOCKListenURL(agentVSOCKPort),
		Image:                dockerworker.EffectivePoolImage(cfg.WorkerImage),
		Labels:               map[string]string{labelProviderType: ProviderType},
		HostMounts:           hostMounts(),
		DevelopmentImageSync: imageSync,
		ProgressReporter:     progress,
		ProxyAuditRetention:  cfg.ProxyAuditRetention.Value(),
	}
}

// guestResolver builds the artifact resolver for one configuration. It is
// shared by Validate and construction so a bad guest image is reported when the
// provider is configured rather than when a pool first starts.
func guestResolver(cfg Config) (*guestimage.Resolver, error) {
	return guestimage.New(guestimage.Config{
		Reference:   effectiveGuestImage(cfg.GuestImage),
		OverrideDir: strings.TrimSpace(cfg.GuestImageDir),
		LocalDir:    effectiveGuestLocalDir(cfg.GuestImageLocalDir),
		CacheDir:    effectiveGuestCacheDir(cfg.GuestImageCacheDir),
		Artifacts: []guestimage.Artifact{
			{Name: kernelArtifact},
			{Name: initrdArtifact, Optional: true},
			{Name: rootArtifact},
		},
	})
}

func effectiveGuestImage(configured string) string {
	if value := strings.TrimSpace(configured); value != "" {
		return value
	}
	return DefaultGuestImage
}

// effectiveGuestLocalDir names where a local guest image build lands. It is
// the same path `task build:vz-guest` writes, and that agreement is the whole
// mechanism: nothing is configured to adopt a local build.
func effectiveGuestLocalDir(configured string) string {
	if value := strings.TrimSpace(configured); value != "" {
		return value
	}
	return filepath.Join(defaultStorageRoot(), "guest", "local")
}

func effectiveGuestCacheDir(configured string) string {
	if value := strings.TrimSpace(configured); value != "" {
		return value
	}
	return filepath.Join(defaultStorageRoot(), "guest")
}

func effectiveStateDir(configured string) string {
	if value := strings.TrimSpace(configured); value != "" {
		return value
	}
	return filepath.Join(defaultStorageRoot(), "pools")
}

// defaultStorageRoot puts guest images and pool disks under the platform's data
// directory. On macOS that is ~/Library/Application Support, which is excluded
// from Time Machine backups by users far less often than it should be — pool
// disks are large and reconstructible, which is why they live beside the guest
// cache rather than in the server's own data directory.
func defaultStorageRoot() string {
	if home := strings.TrimSpace(xdg.DataHome); home != "" {
		return filepath.Join(home, "discobox", storageNamespace)
	}
	return filepath.Join(os.TempDir(), "discobox", storageNamespace)
}

// defaultVCPUs and defaultMemoryMiB size a pool VM from the host: every vCPU,
// and half the memory (see vzvm.HostResources). They are functions rather than
// constants because the answer depends on the machine, and they are clamped to
// what Virtualization.framework accepts.
func defaultVCPUs() int {
	return int(vzvm.DefaultHostResources().CPUCount)
}

func defaultMemoryMiB() int {
	return int(vzvm.DefaultHostResources().MemoryBytes / (1024 * 1024))
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

// Definition describes the vz provider for provider catalogs.
func Definition() sandbox.ProviderDefinition {
	return sandbox.ProviderDefinition{
		Name:        "Apple Virtualization",
		Icon:        "server",
		Description: "Runs one Virtualization.framework VM per pool on macOS, with VSOCK control traffic and no Docker daemon on the host.",
		ConfigFields: append([]sandbox.ProviderConfigField{
			{Key: "guestImage", Label: "Guest Image", Type: "string", Placeholder: DefaultGuestImage, Description: "Published guest image carrying the kernel, initrd, and root filesystem.", Advanced: true},
			{Key: "guestImageDir", Label: "Guest Artifact Directory", Type: "string", Description: "Boot these artifacts instead of the published image, and fail if they are missing.", Advanced: true},
			{Key: "guestImageLocalDir", Label: "Local Guest Build", Type: "string", Placeholder: effectiveGuestLocalDir(""), Description: "Where a local guest image build lands; used automatically when complete.", Advanced: true},
			{Key: "workerImage", Label: "Worker Image", Type: "string", Placeholder: dockerworker.DefaultPoolImage, Description: "Pool-agent container image launched inside each VM.", Advanced: true},
			{Key: "vcpus", Label: "VM vCPUs", Type: "number", Placeholder: strconv.Itoa(defaultVCPUs()), Description: "Defaults to every host vCPU."},
			{Key: "memoryMiB", Label: "VM Memory (MiB)", Type: "number", Placeholder: strconv.Itoa(defaultMemoryMiB()), Description: "Defaults to half of host memory."},
			{Key: "dataDiskGiB", Label: "Data Disk (GiB)", Type: "number", Placeholder: strconv.FormatInt(defaultDataDiskGiB, 10)},
			{Key: "cacheDiskGiB", Label: "Cache Disk (GiB)", Type: "number", Placeholder: strconv.FormatInt(defaultCacheDiskGiB, 10)},
			{Key: "stateDir", Label: "Pool Disk Directory", Type: "string", Placeholder: effectiveStateDir(""), Advanced: true},
			{Key: "guestImageCacheDir", Label: "Guest Image Cache", Type: "string", Placeholder: effectiveGuestCacheDir(""), Advanced: true},
		}, poolruntime.PoolPolicyConfigFields()...),
	}
}
