package docker

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/providers/sandbox/provider/vmprovider"
	"github.com/obot-platform/discobox/providers/sandbox/vm"
	dockerdriver "github.com/obot-platform/discobox/providers/sandbox/vm/docker"
	sandbox "github.com/obot-platform/discobox/sandboxprovider"
)

const ProviderType = dockerdriver.ProviderType
const workerImageEnv = "DISCOBOX_DOCKER_WORKER_IMAGE"

var Definition = dockerdriver.Definition()
var Factory sandbox.ProviderFactory = NewFromInstance

func FactoryWithWorkerStore(workerStore vm.WorkerStore) sandbox.ProviderFactory {
	return func(ctx context.Context, instance *model.SandboxProviderInstance) (sandbox.Provider, error) {
		return newFromInstance(ctx, instance, workerStore)
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
	Command         vmprovider.StringList `json:"command,omitempty"`
	vmprovider.WorkerPoolConfigFields
}

func Decode(data json.RawMessage) (Config, error) {
	return vmprovider.DecodeConfig[Config](data, ProviderType)
}

func Validate(data json.RawMessage) error {
	_, err := Decode(data)
	return err
}

func NewFromInstance(ctx context.Context, instance *model.SandboxProviderInstance) (sandbox.Provider, error) {
	return newFromInstance(ctx, instance, nil)
}

func newFromInstance(ctx context.Context, instance *model.SandboxProviderInstance, workerStore vm.WorkerStore) (sandbox.Provider, error) {
	cfg, err := Decode(instance.Config)
	if err != nil {
		return nil, err
	}
	cfg.Image = configuredWorkerImage(cfg.Image)
	return vmprovider.NewWorkerProvider(ctx, vmprovider.WorkerProviderConfig{
		ControlPlaneURL: cfg.ControlPlaneURL,
		DefaultImage:    cfg.Image,
		AgentPort:       cfg.AgentPort,
		WorkerPool:      cfg.WorkerPoolConfig(),
		WorkerStore:     workerStore,
		EnsureWorkers:   true,
	}, func(ctx context.Context, vmConfig vm.Config) (*vm.Provider, error) {
		return newProvider(ctx, cfg, vmConfig)
	})
}

func DefaultWorkerImage() string {
	return configuredWorkerImage(dockerdriver.DefaultImage())
}

func configuredWorkerImage(image string) string {
	if value := strings.TrimSpace(os.Getenv(workerImageEnv)); value != "" {
		return value
	}
	return image
}

func newProvider(ctx context.Context, cfg Config, vmConfig vm.Config) (*vm.Provider, error) {
	return dockerdriver.NewProvider(ctx, dockerdriver.Config{
		Host:         cfg.Host,
		Image:        cfg.Image,
		Network:      cfg.Network,
		AgentPort:    cfg.AgentPort,
		Systemd:      cfg.systemdValue(),
		Privileged:   cfg.Privileged,
		CgroupNSMode: cfg.CgroupNSMode,
		Command:      cfg.Command.Values(),
	}, vmConfig)
}

func (c Config) systemdValue() bool {
	if c.Systemd == nil {
		return true
	}
	return *c.Systemd
}
