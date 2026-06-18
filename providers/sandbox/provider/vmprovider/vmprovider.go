package vmprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/providers/sandbox/vm"
	sandbox "github.com/obot-platform/discobox/sandboxprovider"
)

// StringList accepts either a JSON array of strings or a comma-separated string.
type StringList []string

func (l *StringList) UnmarshalJSON(data []byte) error {
	var values []string
	if err := json.Unmarshal(data, &values); err == nil {
		*l = CleanStringList(values)
		return nil
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*l = CleanStringList(strings.Split(value, ","))
	return nil
}

func (l StringList) Values() []string { return CleanStringList(l) }

func CleanStringList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

// WorkerPoolConfigFields contains common persisted warm-worker pool settings.
type WorkerPoolConfigFields struct {
	PoolSize   int `json:"poolSize,omitempty"`
	MinWorkers int `json:"minWorkers,omitempty"`
	MaxWorkers int `json:"maxWorkers,omitempty"`
	MinHealthy int `json:"minHealthyWorkers,omitempty"`
}

func (c WorkerPoolConfigFields) WorkerPoolConfig() vm.WorkerPoolConfig {
	return vm.NormalizeWorkerPoolConfig(c.PoolSize, c.MinWorkers, c.MaxWorkers, c.MinHealthy)
}

func DecodeConfig[T any](data json.RawMessage, providerType string) (T, error) {
	var cfg T
	if len(data) > 0 {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("decode %s provider config: %w", providerType, err)
		}
	}
	return cfg, nil
}

func RequireControlPlaneURL(providerType, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s controlPlaneUrl is required", providerType)
	}
	return nil
}

type NewProviderFunc func(context.Context, vm.Config) (*vm.Provider, error)

type WorkerProviderConfig struct {
	ControlPlaneURL string
	DefaultImage    string
	AgentPort       int
	WorkerPool      vm.WorkerPoolConfig
	WorkerStore     vm.WorkerStore
	EnsureWorkers   bool
}

func NewWorkerProvider(ctx context.Context, cfg WorkerProviderConfig, newProvider NewProviderFunc) (sandbox.Provider, error) {
	provider, err := newProvider(ctx, vm.Config{
		ControlPlaneURL: cfg.ControlPlaneURL,
		DefaultImage:    cfg.DefaultImage,
		AgentPort:       cfg.AgentPort,
	})
	if err != nil {
		return nil, err
	}
	factory := func(ctx context.Context, vmCfg vm.Config) (sandbox.Provider, error) {
		return newProvider(ctx, vmCfg)
	}
	workerProvider := vm.NewWorkerProvider(provider, cfg.WorkerPool, func(ctx context.Context, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker, token string) error {
		return vm.LaunchWorker(ctx, project, provider, worker, token, vm.LaunchWorkerConfig{
			ControlPlaneURL: cfg.ControlPlaneURL,
			DefaultImage:    cfg.DefaultImage,
			AgentPort:       cfg.AgentPort,
			Factory:         factory,
		})
	}, cfg.WorkerStore, func(ctx context.Context, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker) error {
		return vm.RemoveWorker(ctx, project, provider, worker, vm.LaunchWorkerConfig{
			ControlPlaneURL: cfg.ControlPlaneURL,
			DefaultImage:    cfg.DefaultImage,
			AgentPort:       cfg.AgentPort,
			Factory:         factory,
		})
	})
	return workerProvider, nil
}
