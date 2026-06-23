package docker

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/providers/workerpool"
	"github.com/obot-platform/discobox/server/providers/workerpool/vm"
)

const workerImageEnv = "DISCOBOX_DOCKER_WORKER_IMAGE"

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
	return NewProvider(ctx, DriverConfig{
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
