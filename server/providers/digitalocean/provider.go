package digitalocean

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/providers/workerpool"
	"github.com/obot-platform/discobox/server/providers/workerpool/vm"
)

var Factory sandbox.ProviderFactory = NewFromInstance

func FactoryWithWorkerManager(workerManager workerpool.WorkerManager) sandbox.ProviderFactory {
	return func(ctx context.Context, instance *model.SandboxProviderInstance) (sandbox.Provider, error) {
		return newFromInstance(ctx, instance, workerManager)
	}
}

type Config struct {
	Token           string                `json:"token,omitempty"`
	TokenEnv        string                `json:"tokenEnv,omitempty"`
	ControlPlaneURL string                `json:"controlPlaneUrl,omitempty"`
	APIBaseURL      string                `json:"apiBaseUrl,omitempty"`
	Region          string                `json:"region,omitempty"`
	Size            string                `json:"size,omitempty"`
	Image           string                `json:"image,omitempty"`
	SSHKeys         workerpool.StringList `json:"sshKeys,omitempty"`
	VPCUUID         string                `json:"vpcUuid,omitempty"`
	Tags            workerpool.StringList `json:"tags,omitempty"`
	Backups         bool                  `json:"backups,omitempty"`
	IPv6            bool                  `json:"ipv6,omitempty"`
	Monitoring      bool                  `json:"monitoring,omitempty"`
	AgentPort       int                   `json:"agentPort,omitempty"`
	workerpool.WorkerPoolConfigFields
}

func Decode(data json.RawMessage) (Config, error) {
	return workerpool.DecodeConfig[Config](data, "digitalocean")
}

func Validate(data json.RawMessage) error {
	cfg, err := Decode(data)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Token) == "" && strings.TrimSpace(cfg.TokenEnv) == "" {
		return fmt.Errorf("digitalocean token or tokenEnv is required")
	}
	return workerpool.RequireControlPlaneURL("digitalocean", cfg.ControlPlaneURL)
}

func NewFromInstance(ctx context.Context, instance *model.SandboxProviderInstance) (sandbox.Provider, error) {
	return newFromInstance(ctx, instance, nil)
}

func newFromInstance(ctx context.Context, instance *model.SandboxProviderInstance, workerManager workerpool.WorkerManager) (sandbox.Provider, error) {
	cfg, err := Decode(instance.Config)
	if err != nil {
		return nil, err
	}
	return workerpool.NewVMWorkerPoolProvider(ctx, workerpool.VMWorkerPoolProviderConfig{
		ControlPlaneURL: cfg.ControlPlaneURL,
		DefaultImage:    cfg.Image,
		AgentPort:       cfg.AgentPort,
		WorkerPool:      cfg.WorkerPoolConfig(),
		WorkerManager:   workerManager,
	}, func(ctx context.Context, vmConfig vm.Config) (*vm.Provider, error) {
		return newProvider(ctx, cfg, vmConfig)
	})
}

func newProvider(_ context.Context, cfg Config, vmConfig vm.Config) (*vm.Provider, error) {
	return NewProvider(DriverConfig{
		Token:      accessToken(cfg),
		APIBaseURL: cfg.APIBaseURL,
		Region:     cfg.Region,
		Size:       cfg.Size,
		Image:      cfg.Image,
		SSHKeys:    cfg.SSHKeys.Values(),
		VPCUUID:    cfg.VPCUUID,
		Tags:       cfg.Tags.Values(),
		Backups:    cfg.Backups,
		IPv6:       cfg.IPv6,
		Monitoring: cfg.Monitoring,
		AgentPort:  cfg.AgentPort,
	}, vmConfig)
}

func accessToken(cfg Config) string {
	token := strings.TrimSpace(cfg.Token)
	if token != "" {
		return token
	}
	tokenEnv := strings.TrimSpace(cfg.TokenEnv)
	if tokenEnv == "" {
		tokenEnv = "DIGITALOCEAN_ACCESS_TOKEN"
	}
	return strings.TrimSpace(os.Getenv(tokenEnv))
}
