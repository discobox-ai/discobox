package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/obot-platform/discobox/internal/model"
	"github.com/obot-platform/discobox/internal/sandbox"
	"github.com/obot-platform/discobox/internal/store"
	"github.com/obot-platform/discobox/internal/workeragent"
)

// WorkerProvider is a VM-backed sandbox provider with a warm worker pool.
//
// The embedded Provider owns VM runtime mechanics. WorkerProvider owns the
// Disco worker-pool behavior that only applies to VM/worker-backed providers.
type WorkerProvider struct {
	*Provider
	poolConfig WorkerPoolConfig
	launch     WorkerLauncher
	store      WorkerStore
}

func NewWorkerProvider(provider *Provider, poolConfig WorkerPoolConfig, launch WorkerLauncher, store WorkerStore) *WorkerProvider {
	return &WorkerProvider{Provider: provider, poolConfig: poolConfig, launch: launch, store: store}
}

func (p *WorkerProvider) EnsureWorkerPool(ctx context.Context, store WorkerStore, project *model.Project, provider *model.SandboxProviderInstance) error {
	return EnsureWorkerPool(ctx, store, project, provider, p.poolConfig, p.launch)
}

func (p *WorkerProvider) EnsureProviderInstance(ctx context.Context, store any, project *model.Project, provider *model.SandboxProviderInstance) error {
	workerStore, ok := store.(WorkerStore)
	if !ok {
		return fmt.Errorf("worker store is required")
	}
	return p.EnsureWorkerPool(ctx, workerStore, project, provider)
}

func (p *WorkerProvider) Create(ctx context.Context, ref sandbox.SandboxRef, _ []byte, opts sandbox.CreateOptions) (*sandbox.Sandbox, []byte, error) {
	if p.store == nil {
		return nil, nil, fmt.Errorf("worker store is required")
	}
	providerInstanceID := opts.ProviderInstanceID
	sb := &model.Sandbox{
		ID:           ref.SandboxID,
		ProjectID:    ref.ProjectID,
		CPUVCPUs:     opts.CPUVCPUs,
		MemoryBytes:  opts.MemoryBytes,
		StorageBytes: opts.StorageBytes,
	}
	if providerInstanceID != "" {
		sb.ProviderInstanceID = &providerInstanceID
	}
	worker, err := p.store.ClaimWorker(ctx, sb)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, sandbox.ErrNoSandboxCapacity
		}
		return nil, nil, err
	}
	runtimeSandbox := workerRuntimeSandbox(ref.SandboxID, worker)
	state, err := json.Marshal(runtimeSandbox)
	if err != nil {
		return nil, nil, err
	}
	return runtimeSandbox, state, nil
}

func (p *WorkerProvider) Start(_ context.Context, ref sandbox.SandboxRef, state []byte) (*sandbox.Sandbox, []byte, error) {
	runtimeSandbox := workerRuntimeSandboxFromState(ref.SandboxID, state)
	return runtimeSandbox, state, nil
}

func (p *WorkerProvider) Stop(_ context.Context, ref sandbox.SandboxRef, state []byte, _ time.Duration) (*sandbox.Sandbox, []byte, error) {
	runtimeSandbox := workerRuntimeSandboxFromState(ref.SandboxID, state)
	if runtimeSandbox != nil {
		runtimeSandbox.Status = sandbox.StatusStopped
	}
	return runtimeSandbox, state, nil
}

func (p *WorkerProvider) Remove(context.Context, sandbox.SandboxRef, []byte, ...sandbox.RemoveOption) ([]byte, error) {
	return nil, nil
}

func workerRuntimeSandbox(sandboxID string, worker *model.Worker) *sandbox.Sandbox {
	return &sandbox.Sandbox{
		ID:        worker.ID,
		SandboxID: sandboxID,
		Status:    sandbox.StatusRunning,
		Image:     "warm-worker",
		CreatedAt: worker.CreatedAt,
		StartedAt: worker.RegisteredAt,
		Metadata: map[string]string{
			"worker_id":            worker.ID,
			"provider_instance_id": worker.ProviderInstanceID,
			"worker_scheduling":    worker.SchedulingPreference(),
		},
	}
}

func workerRuntimeSandboxFromState(sandboxID string, state []byte) *sandbox.Sandbox {
	var runtimeSandbox sandbox.Sandbox
	if len(state) > 0 && json.Unmarshal(state, &runtimeSandbox) == nil && runtimeSandbox.ID != "" {
		if runtimeSandbox.SandboxID == "" {
			runtimeSandbox.SandboxID = sandboxID
		}
		runtimeSandbox.Status = sandbox.StatusRunning
		return &runtimeSandbox
	}
	return &sandbox.Sandbox{SandboxID: sandboxID, Status: sandbox.StatusRunning, Image: "warm-worker"}
}

// WorkerProviderFactory builds the VM provider used to launch a warm worker VM.
type WorkerProviderFactory func(ctx context.Context, cfg Config) (sandbox.Provider, error)

// LaunchWorkerConfig contains the provider-neutral data needed to boot a worker VM.
type LaunchWorkerConfig struct {
	ControlPlaneURL string
	DefaultImage    string
	AgentPort       int
	Factory         WorkerProviderFactory
	Labels          map[string]string
}

func LaunchWorker(ctx context.Context, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker, token string, cfg LaunchWorkerConfig) error {
	providerImpl, err := cfg.Factory(ctx, Config{
		ControlPlaneURL: cfg.ControlPlaneURL,
		DefaultImage:    cfg.DefaultImage,
		AgentPort:       cfg.AgentPort,
		Bootstrap: BootstrapProviderFunc(func(context.Context, sandbox.SandboxRef, sandbox.CreateOptions) (WorkerBootstrap, error) {
			return workeragent.Bootstrap{ControlPlaneURL: cfg.ControlPlaneURL, TenantID: project.TenantID, ProjectID: project.ID, WorkerID: worker.ID, Token: token, AgentPort: cfg.AgentPort}, nil
		}),
		Metadata: cfg.Labels,
	})
	if err != nil {
		return err
	}
	labels := map[string]string{"discobox.worker_id": worker.ID, "discobox.provider_instance_id": provider.ID}
	for key, value := range cfg.Labels {
		labels[key] = value
	}
	ref := sandbox.SandboxRef{TenantID: project.TenantID, ProjectID: project.ID, SandboxID: "worker-" + worker.ID}
	_, _, err = providerImpl.Create(ctx, ref, nil, sandbox.CreateOptions{Labels: labels})
	return err
}
