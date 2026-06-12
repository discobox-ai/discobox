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

const (
	defaultWorkerCapacityWaitTimeout  = 30 * time.Second
	defaultWorkerCapacityPollInterval = time.Second
)

var (
	workerCapacityWaitTimeout  = defaultWorkerCapacityWaitTimeout
	workerCapacityPollInterval = defaultWorkerCapacityPollInterval
)

// WorkerProvider is a VM-backed sandbox provider with a warm worker pool.
//
// The embedded Provider owns VM runtime mechanics. WorkerProvider owns the
// Disco worker-pool behavior that only applies to VM/worker-backed providers.
type WorkerProvider struct {
	*Provider
	poolConfig           WorkerPoolConfig
	launch               WorkerLauncher
	store                WorkerStore
	ensureRunningWorkers bool
}

type ProviderInstanceStore interface {
	GetProject(ctx context.Context, projectID string) (*model.Project, error)
	GetSandboxProviderInstance(ctx context.Context, projectID, providerID string) (*model.SandboxProviderInstance, error)
}

func NewWorkerProvider(provider *Provider, poolConfig WorkerPoolConfig, launch WorkerLauncher, store WorkerStore) *WorkerProvider {
	return &WorkerProvider{Provider: provider, poolConfig: poolConfig, launch: launch, store: store}
}

func (p *WorkerProvider) EnsureWorkerPool(ctx context.Context, store WorkerStore, project *model.Project, provider *model.SandboxProviderInstance) error {
	if err := EnsureWorkerPool(ctx, store, project, provider, p.poolConfig); err != nil {
		return err
	}
	if !p.ensureRunningWorkers {
		return nil
	}
	return p.ensureActiveWorkers(ctx, store, project, provider)
}

func (p *WorkerProvider) EnsureProviderInstance(ctx context.Context, store any, project *model.Project, provider *model.SandboxProviderInstance) error {
	workerStore, ok := store.(WorkerStore)
	if !ok {
		return fmt.Errorf("worker store is required")
	}
	return p.EnsureWorkerPool(ctx, workerStore, project, provider)
}

type WorkerBootstrapTokenStore interface {
	CreateWorkerBootstrapToken(ctx context.Context, token *model.WorkerBootstrapToken) error
}

func (p *WorkerProvider) ReconcileWorker(ctx context.Context, store any, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker) error {
	bootstrapStore, ok := store.(WorkerBootstrapTokenStore)
	if !ok {
		return fmt.Errorf("worker bootstrap token store is required")
	}
	if p.launch == nil {
		return fmt.Errorf("worker launcher is required")
	}
	token, err := CreateWorkerBootstrap(ctx, bootstrapStore, project, worker)
	if err != nil {
		return err
	}
	return p.launch(ctx, project, provider, worker, token)
}

func (p *WorkerProvider) EnsureRunningWorkers() {
	p.ensureRunningWorkers = true
}

func (p *WorkerProvider) ensureActiveWorkers(ctx context.Context, store WorkerStore, project *model.Project, provider *model.SandboxProviderInstance) error {
	workers, err := store.ListWorkers(ctx, provider.ProjectID, provider.ID)
	if err != nil {
		return err
	}
	for i := range workers {
		worker := &workers[i]
		if !reconciledActiveWorker(worker) {
			continue
		}
		if err := p.ReconcileWorker(ctx, store, project, provider, worker); err != nil {
			return err
		}
	}
	return nil
}

func reconciledActiveWorker(worker *model.Worker) bool {
	return activeWorker(worker) &&
		worker.ObservedGeneration == worker.Generation &&
		worker.LastOperationStatus == model.OperationStatusSuccess
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
	worker, err := p.findSchedulableWorker(ctx, sb)
	if err != nil {
		return nil, nil, err
	}
	runtimeSandbox := workerRuntimeSandbox(ref.SandboxID, worker)
	state, err := json.Marshal(runtimeSandbox)
	if err != nil {
		return nil, nil, err
	}
	return runtimeSandbox, state, nil
}

func (p *WorkerProvider) findSchedulableWorker(ctx context.Context, sb *model.Sandbox) (*model.Worker, error) {
	worker, err := p.store.FindSchedulableWorker(ctx, sb)
	if err == nil {
		return worker, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if err := p.ensureWorkerCapacity(ctx, sb); err != nil {
		return nil, err
	}
	return p.waitForSchedulableWorker(ctx, sb)
}

func (p *WorkerProvider) ensureWorkerCapacity(ctx context.Context, sb *model.Sandbox) error {
	if sb == nil || sb.ProviderInstanceID == nil || *sb.ProviderInstanceID == "" {
		return sandbox.ErrNoSandboxCapacity
	}
	providerStore, ok := p.store.(ProviderInstanceStore)
	if !ok {
		return sandbox.ErrNoSandboxCapacity
	}
	project, err := providerStore.GetProject(ctx, sb.ProjectID)
	if err != nil {
		return err
	}
	provider, err := providerStore.GetSandboxProviderInstance(ctx, sb.ProjectID, *sb.ProviderInstanceID)
	if err != nil {
		return err
	}
	return p.EnsureWorkerPool(ctx, p.store, project, provider)
}

func (p *WorkerProvider) waitForSchedulableWorker(ctx context.Context, sb *model.Sandbox) (*model.Worker, error) {
	deadline := time.Now().Add(workerCapacityWaitTimeout)
	for {
		worker, err := p.store.FindSchedulableWorker(ctx, sb)
		if err == nil {
			return worker, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		if workerCapacityWaitTimeout <= 0 || !time.Now().Before(deadline) {
			return nil, sandbox.ErrNoSandboxCapacity
		}
		timer := time.NewTimer(workerCapacityPollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
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
