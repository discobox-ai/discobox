package workerpool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
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

// WorkerPoolConfigFields contains common persisted worker pool settings.
type WorkerPoolConfigFields struct {
	PoolSize   int `json:"poolSize,omitempty"`
	MinWorkers int `json:"minWorkers,omitempty"`
	MaxWorkers int `json:"maxWorkers,omitempty"`
	MinHealthy int `json:"minHealthyWorkers,omitempty"`
}

func (c WorkerPoolConfigFields) WorkerPoolConfig() WorkerPoolConfig {
	return NormalizeWorkerPoolConfig(c.PoolSize, c.MinWorkers, c.MaxWorkers, c.MinHealthy)
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

type NewProviderFunc func(context.Context, VMProviderConfig) (WorkerProvider, error)

type VMProviderConfig struct {
	ControlPlaneURL string
	DefaultImage    string
	AgentPort       int
}

type VMWorkerPoolProviderConfig struct {
	ControlPlaneURL string
	DefaultImage    string
	AgentPort       int
	WorkerPool      WorkerPoolConfig
	WorkerManager   WorkerManager
	EnsureWorkers   bool
}

func NewVMWorkerPoolProvider(ctx context.Context, cfg VMWorkerPoolProviderConfig, newProvider NewProviderFunc) (sandbox.Provider, error) {
	provider, err := newProvider(ctx, VMProviderConfig{
		ControlPlaneURL: cfg.ControlPlaneURL,
		DefaultImage:    cfg.DefaultImage,
		AgentPort:       cfg.AgentPort,
	})
	if err != nil {
		return nil, err
	}
	return NewWorkerPoolProvider(provider, cfg.WorkerPool, cfg.WorkerManager, cfg.EnsureWorkers), nil
}
