package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"

	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/providers/workerpool"
	"github.com/obot-platform/discobox/server/providers/workerpool/vm"
)

const workerImageEnv = "DISCOBOX_DOCKER_WORKER_IMAGE"
const workerAgentMountLayoutVersion = 4

func FactoryWithWorkerManager(workerManager workerpool.WorkerManager) sandbox.ProviderFactory {
	return func(ctx context.Context, instance *model.SandboxProviderInstance) (sandbox.Provider, error) {
		return newFromInstance(ctx, instance, workerManager)
	}
}

type Config struct {
	ControlPlaneURL string                `json:"controlPlaneUrl,omitempty"`
	Host            string                `json:"host,omitempty"`
	Image           string                `json:"image,omitempty"`
	Network         string                `json:"network,omitempty"`
	AgentPort       int                   `json:"agentPort,omitempty"`
	Systemd         *bool                 `json:"systemd,omitempty"`
	Privileged      *bool                 `json:"privileged,omitempty"`
	CgroupNSMode    string                `json:"cgroupNsMode,omitempty"`
	Command         workerpool.StringList `json:"command,omitempty"`
	DockerSocket    string                `json:"bindDockerSocket,omitempty"`
	HostMounts      []HostMount           `json:"hostMounts,omitempty"`
	workerpool.WorkerPoolConfigFields
}

func Decode(data json.RawMessage) (Config, error) {
	return workerpool.DecodeConfig[Config](data, ProviderType)
}

func Validate(data json.RawMessage) error {
	_, err := Decode(data)
	return err
}

func newFromInstance(ctx context.Context, instance *model.SandboxProviderInstance, workerManager workerpool.WorkerManager) (sandbox.Provider, error) {
	cfg, err := Decode(instance.Config)
	if err != nil {
		return nil, err
	}
	cfg.Image = configuredWorkerImage(cfg.Image)
	return workerpool.NewVMWorkerPoolProvider(ctx, workerpool.VMWorkerPoolProviderConfig{
		ControlPlaneURL: cfg.ControlPlaneURL,
		DefaultImage:    cfg.Image,
		AgentPort:       cfg.AgentPort,
		WorkerPool:      cfg.WorkerPoolConfig(),
		WorkerManager:   workerManager,
		EnsureWorkers:   true,
	}, func(ctx context.Context, providerConfig workerpool.VMProviderConfig) (workerpool.WorkerProvider, error) {
		return newProvider(ctx, cfg, vm.Config{
			ControlPlaneURL: providerConfig.ControlPlaneURL,
			DefaultImage:    providerConfig.DefaultImage,
			AgentPort:       providerConfig.AgentPort,
		})
	})
}

func DefaultWorkerImage() string {
	return configuredWorkerImage(DefaultImage())
}

func configuredWorkerImage(image string) string {
	if value := strings.TrimSpace(os.Getenv(workerImageEnv)); value != "" {
		return value
	}
	return image
}

func newProvider(ctx context.Context, cfg Config, vmConfig vm.Config) (*vm.Provider, error) {
	vmConfig.Metadata = mergeStringMaps(vmConfig.Metadata, map[string]string{
		labelWorkerConfig: workerAgentConfigRevision(cfg, vmConfig),
	})
	return NewProvider(ctx, DriverConfig{
		Host:         cfg.Host,
		Image:        cfg.Image,
		Network:      cfg.Network,
		AgentPort:    cfg.AgentPort,
		Systemd:      cfg.systemdValue(),
		Privileged:   cfg.Privileged,
		CgroupNSMode: cfg.CgroupNSMode,
		Command:      cfg.Command.Values(),
		DockerSocket: cfg.DockerSocket,
		HostMounts:   cfg.HostMounts,
	}, vmConfig)
}

func (c Config) systemdValue() bool {
	if c.Systemd == nil {
		return true
	}
	return *c.Systemd
}

func workerAgentConfigRevision(cfg Config, vmConfig vm.Config) string {
	systemd := cfg.systemdValue()
	privileged := systemd
	if cfg.Privileged != nil {
		privileged = *cfg.Privileged
	}
	command := cfg.Command.Values()
	if len(command) == 0 && systemd {
		command = []string{"/usr/local/bin/discobox-worker-agent"}
	}
	agentPort := cfg.AgentPort
	if agentPort == 0 {
		agentPort = DefaultAgentPort()
	}
	controlPlaneURL := strings.TrimSpace(vmConfig.ControlPlaneURL)
	if controlPlaneURL == "" {
		controlPlaneURL = defaultDockerControlPlaneURL()
	}
	image := strings.TrimSpace(cfg.Image)
	if image == "" {
		image = DefaultImage()
	}
	dockerSocket := cleanAbsPath(cfg.DockerSocket)
	if dockerSocket == "" {
		dockerSocket = dockerSocketPath
	}
	payload := struct {
		ControlPlaneURL    string      `json:"controlPlaneUrl"`
		Image              string      `json:"image"`
		Network            string      `json:"network"`
		AgentPort          int         `json:"agentPort"`
		Systemd            bool        `json:"systemd"`
		Privileged         bool        `json:"privileged"`
		CgroupNSMode       string      `json:"cgroupNsMode"`
		Command            []string    `json:"command,omitempty"`
		DockerSocket       string      `json:"bindDockerSocket"`
		HostMounts         []HostMount `json:"hostMounts,omitempty"`
		MountLayoutVersion int         `json:"mountLayoutVersion"`
	}{
		ControlPlaneURL:    controlPlaneURL,
		Image:              image,
		Network:            strings.TrimSpace(cfg.Network),
		AgentPort:          agentPort,
		Systemd:            systemd,
		Privileged:         privileged,
		CgroupNSMode:       strings.TrimSpace(cfg.CgroupNSMode),
		Command:            command,
		DockerSocket:       dockerSocket,
		HostMounts:         normalizeHostMounts(cfg.HostMounts),
		MountLayoutVersion: workerAgentMountLayoutVersion,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func mergeStringMaps(base map[string]string, overlays ...map[string]string) map[string]string {
	size := len(base)
	for _, overlay := range overlays {
		size += len(overlay)
	}
	merged := make(map[string]string, size)
	for key, value := range base {
		merged[key] = value
	}
	for _, overlay := range overlays {
		for key, value := range overlay {
			merged[key] = value
		}
	}
	return merged
}
