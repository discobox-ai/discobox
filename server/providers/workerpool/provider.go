package workerpool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/obot-platform/discobox/server/internal/apperrors"
	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/transport"
	workeragent "github.com/obot-platform/discobox/worker-agent"
)

const (
	defaultWorkerCapacityWaitTimeout  = 30 * time.Second
	defaultWorkerCapacityPollInterval = time.Second
)

var (
	workerCapacityWaitTimeout  = defaultWorkerCapacityWaitTimeout
	workerCapacityPollInterval = defaultWorkerCapacityPollInterval
)

// WorkerManager is the control-plane surface a worker pool needs.
type WorkerManager = sandbox.WorkerManager

// WorkerProvider owns worker runtime lifecycle and worker-agent connectivity.
// It is implemented by the dockerworker engine; the pool never sees Docker or
// VM details.
type WorkerProvider interface {
	Close() error
	// EnsureWorker creates or drift-corrects the worker runtime. It is
	// idempotent and updates the worker row's runtime state and scheduling
	// flags in place; the caller persists the row.
	EnsureWorker(ctx context.Context, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker, bootstrap workeragent.Bootstrap) error
	// RepairWorker replaces an unhealthy worker runtime while preserving worker
	// identity and worker-local state.
	RepairWorker(ctx context.Context, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker, bootstrap workeragent.Bootstrap, reason string) error
	RemoveWorker(ctx context.Context, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker) error
	// AcquireWorkerAgentClient returns an HTTP client lease that reaches the
	// worker-agent API for the worker.
	AcquireWorkerAgentClient(ctx context.Context, worker *model.Worker) (*transport.HTTPClientLease, error)
}

// WorkerPoolProvider is a sandbox provider backed by a worker pool.
//
// WorkerPoolProvider owns sandbox placement and worker-pool reconciliation.
// The worker provider owns the runtime mechanics for individual workers.
type WorkerPoolProvider struct {
	workerProvider WorkerProvider
	definition     sandbox.ProviderDefinition
	poolConfig     WorkerPoolConfig
	manager        WorkerManager
}

// New creates a worker-pool sandbox provider over a worker provider.
func New(provider WorkerProvider, definition sandbox.ProviderDefinition, poolConfig WorkerPoolConfig, manager WorkerManager) *WorkerPoolProvider {
	return &WorkerPoolProvider{workerProvider: provider, definition: definition, poolConfig: poolConfig, manager: manager}
}

func (p *WorkerPoolProvider) Initialize(ctx context.Context, provider *model.SandboxProviderInstance) error {
	return p.manager.ScheduleWorkerProviderReconciliation(ctx, provider.ProjectID, provider.ID)
}

func (p *WorkerPoolProvider) List(context.Context) ([]*sandbox.Sandbox, error) {
	return nil, nil
}

func (p *WorkerPoolProvider) Close() error {
	return p.workerProvider.Close()
}

func (p *WorkerPoolProvider) Definition() sandbox.ProviderDefinition {
	return p.definition
}

func (p *WorkerPoolProvider) Status() sandbox.ProviderStatus {
	return sandbox.ProviderStatus{Available: true, State: "ready"}
}

func (p *WorkerPoolProvider) Reconcile(context.Context) error {
	return nil
}

func (p *WorkerPoolProvider) RemoveProject(context.Context, string) error {
	return nil
}

func (p *WorkerPoolProvider) ensureWorkerPool(ctx context.Context, manager WorkerManager, project *model.Project, provider *model.SandboxProviderInstance) error {
	if err := ensureWorkerPool(ctx, manager, project, provider, p.poolConfig); err != nil {
		return err
	}
	return p.ensureActiveWorkers(ctx, manager, project, provider)
}

func (p *WorkerPoolProvider) ReconcileWorkerProvider(ctx context.Context, manager sandbox.WorkerManager, project *model.Project, provider *model.SandboxProviderInstance) error {
	if manager == nil {
		return fmt.Errorf("worker manager is required")
	}
	return p.ensureWorkerPool(ctx, manager, project, provider)
}

func (p *WorkerPoolProvider) ReconcileWorker(ctx context.Context, manager sandbox.WorkerManager, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker) error {
	if manager == nil {
		return fmt.Errorf("worker manager is required")
	}
	bootstrap, err := workerBootstrap(ctx, manager, project, worker)
	if err != nil {
		return err
	}
	if err := p.workerProvider.EnsureWorker(ctx, project, provider, worker, bootstrap); err != nil {
		return err
	}
	if workerRegistrationTimeout <= 0 {
		return nil
	}
	return manager.ScheduleWorkerProviderReconciliationAt(ctx, provider.ProjectID, provider.ID, time.Now().UTC().Add(workerRegistrationTimeout))
}

func (p *WorkerPoolProvider) RepairWorker(ctx context.Context, manager sandbox.WorkerManager, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker, reason string) error {
	if manager == nil {
		return fmt.Errorf("worker manager is required")
	}
	bootstrap, err := workerBootstrap(ctx, manager, project, worker)
	if err != nil {
		return err
	}
	if err := p.workerProvider.RepairWorker(ctx, project, provider, worker, bootstrap, reason); err != nil {
		return err
	}
	if workerRegistrationTimeout <= 0 {
		return nil
	}
	return manager.ScheduleWorkerProviderReconciliationAt(ctx, provider.ProjectID, provider.ID, time.Now().UTC().Add(workerRegistrationTimeout))
}

func (p *WorkerPoolProvider) RemoveWorker(ctx context.Context, _ sandbox.WorkerManager, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker) error {
	return p.workerProvider.RemoveWorker(ctx, project, provider, worker)
}

// workerBootstrap mints the registration credentials handed to a new worker
// runtime. The worker provider fills runtime-specific fields such as the
// control plane URL and harness port.
func workerBootstrap(ctx context.Context, manager WorkerManager, project *model.Project, worker *model.Worker) (workeragent.Bootstrap, error) {
	token, err := createWorkerBootstrap(ctx, manager, project, worker)
	if err != nil {
		return workeragent.Bootstrap{}, err
	}
	controlPlanePublicKey, err := manager.EnsureWorkerAgentTrustKey(ctx)
	if err != nil {
		return workeragent.Bootstrap{}, err
	}
	return workeragent.Bootstrap{
		ProjectID:       project.ID,
		WorkerID:        worker.ID,
		Token:           token,
		ControlPlaneKey: controlPlanePublicKey,
	}, nil
}

func (p *WorkerPoolProvider) ensureActiveWorkers(ctx context.Context, manager WorkerManager, project *model.Project, provider *model.SandboxProviderInstance) error {
	workers, err := manager.ListWorkers(ctx, provider.ProjectID, provider.ID)
	if err != nil {
		return err
	}
	for i := range workers {
		worker := &workers[i]
		if !startupReconcileWorker(worker) {
			continue
		}
		if err := p.ReconcileWorker(ctx, manager, project, provider, worker); err != nil {
			return err
		}
	}
	return nil
}

func startupReconcileWorker(worker *model.Worker) bool {
	return activeWorker(worker) &&
		worker.ObservedGeneration == worker.Generation &&
		worker.LastOperationStatus == model.OperationStatusSuccess
}

func (p *WorkerPoolProvider) Create(ctx context.Context, ref sandbox.SandboxRef, state []byte, opts sandbox.CreateOptions) (*sandbox.Sandbox, []byte, error) {
	if p.manager == nil {
		return nil, nil, fmt.Errorf("worker manager is required")
	}
	if workerID, err := workerIDFromRuntimeState(state); err == nil {
		if opts.WorkerID == workerID {
			if err := p.validateStateWorker(ctx, ref, opts, workerID); err != nil {
				return nil, state, err
			}
			worker, err := p.manager.GetWorker(ctx, workerID)
			if err != nil {
				return nil, state, err
			}
			return p.createOnWorker(ctx, ref, state, opts, worker)
		}
		if opts.WorkerID != "" {
			return nil, state, sandbox.ErrNoSandboxCapacity
		}
	} else if len(state) > 0 && !errors.Is(err, sandbox.ErrNotFound) {
		return nil, state, err
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
	return p.createOnWorker(ctx, ref, nil, opts, worker)
}

func (p *WorkerPoolProvider) validateStateWorker(ctx context.Context, ref sandbox.SandboxRef, opts sandbox.CreateOptions, workerID string) error {
	if opts.WorkerID != workerID {
		return sandbox.ErrNoSandboxCapacity
	}
	worker, err := p.manager.GetWorker(ctx, workerID)
	if err != nil {
		return err
	}
	if worker.ProjectID != ref.ProjectID {
		return sandbox.ErrNoSandboxCapacity
	}
	if opts.ProviderInstanceID != "" && worker.ProviderInstanceID != opts.ProviderInstanceID {
		return sandbox.ErrNoSandboxCapacity
	}
	if !activeWorker(worker) {
		return sandbox.ErrNoSandboxCapacity
	}
	return nil
}

func (p *WorkerPoolProvider) createOnWorker(ctx context.Context, ref sandbox.SandboxRef, state []byte, opts sandbox.CreateOptions, worker *model.Worker) (*sandbox.Sandbox, []byte, error) {
	client, err := p.agentClientForWorker(ctx, worker)
	if err != nil {
		return nil, state, err
	}
	return client.Create(ctx, ref, state, opts)
}

func (p *WorkerPoolProvider) findSchedulableWorker(ctx context.Context, sb *model.Sandbox) (*model.Worker, error) {
	worker, err := p.manager.FindSchedulableWorker(ctx, sb)
	if err == nil {
		return worker, nil
	}
	if !errors.Is(err, apperrors.ErrNotFound) {
		return nil, err
	}
	if err := p.ensureWorkerCapacity(ctx, sb); err != nil {
		return nil, err
	}
	return p.waitForSchedulableWorker(ctx, sb)
}

func (p *WorkerPoolProvider) ensureWorkerCapacity(ctx context.Context, sb *model.Sandbox) error {
	if sb == nil || sb.ProviderInstanceID == nil || *sb.ProviderInstanceID == "" {
		return sandbox.ErrNoSandboxCapacity
	}
	project, err := p.manager.GetProject(ctx, sb.ProjectID)
	if err != nil {
		return err
	}
	provider, err := p.manager.GetSandboxProviderInstance(ctx, sb.ProjectID, *sb.ProviderInstanceID)
	if err != nil {
		return err
	}
	return p.ensureWorkerPool(ctx, p.manager, project, provider)
}

func (p *WorkerPoolProvider) waitForSchedulableWorker(ctx context.Context, sb *model.Sandbox) (*model.Worker, error) {
	deadline := time.Now().Add(workerCapacityWaitTimeout)
	for {
		worker, err := p.manager.FindSchedulableWorker(ctx, sb)
		if err == nil {
			return worker, nil
		}
		if !errors.Is(err, apperrors.ErrNotFound) {
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

func (p *WorkerPoolProvider) Update(ctx context.Context, ref sandbox.SandboxRef, state []byte, opts sandbox.UpdateOptions) (*sandbox.Sandbox, []byte, error) {
	client, err := p.agentClientFromState(ctx, state)
	if err != nil {
		return nil, state, err
	}
	return client.Update(ctx, ref, state, opts)
}

func (p *WorkerPoolProvider) Start(ctx context.Context, ref sandbox.SandboxRef, state []byte) (*sandbox.Sandbox, []byte, error) {
	client, err := p.agentClientFromState(ctx, state)
	if err != nil {
		return nil, state, err
	}
	return client.Start(ctx, ref, state)
}

func (p *WorkerPoolProvider) Stop(ctx context.Context, ref sandbox.SandboxRef, state []byte, timeout time.Duration) (*sandbox.Sandbox, []byte, error) {
	client, err := p.agentClientFromState(ctx, state)
	if err != nil {
		return nil, state, err
	}
	return client.Stop(ctx, ref, state, timeout)
}

func (p *WorkerPoolProvider) Get(ctx context.Context, ref sandbox.SandboxRef, state []byte) (*sandbox.Sandbox, error) {
	client, err := p.agentClientFromState(ctx, state)
	if err != nil {
		return nil, err
	}
	return client.Get(ctx, ref, state)
}

func (p *WorkerPoolProvider) AcquireHTTPClient(ctx context.Context, ref sandbox.SandboxRef, state []byte, scopes []string) (*transport.HTTPClientLease, error) {
	client, err := p.agentClientFromState(ctx, state)
	if err != nil {
		return nil, err
	}
	return client.AcquireHTTPClient(ctx, ref, state, scopes)
}

func (p *WorkerPoolProvider) Remove(ctx context.Context, ref sandbox.SandboxRef, state []byte, opts ...sandbox.RemoveOption) ([]byte, error) {
	client, err := p.agentClientFromState(ctx, state)
	if err != nil {
		return state, err
	}
	return client.Remove(ctx, ref, state, opts...)
}

func (p *WorkerPoolProvider) agentClientFromState(ctx context.Context, state []byte) (*workerAgentClient, error) {
	workerID, err := workerIDFromRuntimeState(state)
	if err != nil {
		return nil, err
	}
	worker, err := p.manager.GetWorker(ctx, workerID)
	if err != nil {
		return nil, err
	}
	return p.agentClientForWorker(ctx, worker)
}

func (p *WorkerPoolProvider) agentClientForWorker(ctx context.Context, worker *model.Worker) (*workerAgentClient, error) {
	lease, err := p.acquireWorkerAgentClient(ctx, worker)
	if err != nil {
		return nil, err
	}
	return &workerAgentClient{workerID: worker.ID, tokenIssuer: p.manager, lease: lease}, nil
}

func (p *WorkerPoolProvider) acquireWorkerAgentClient(ctx context.Context, worker *model.Worker) (*transport.HTTPClientLease, error) {
	lease, err := p.workerProvider.AcquireWorkerAgentClient(ctx, worker)
	if err == nil {
		return lease, nil
	}
	if p.manager == nil || worker == nil || strings.TrimSpace(worker.ID) == "" {
		return nil, err
	}
	worker, retryErr := p.reconcileWorkerAfterClientError(ctx, worker.ID)
	if retryErr != nil {
		return nil, retryErr
	}
	return p.workerProvider.AcquireWorkerAgentClient(ctx, worker)
}

func (p *WorkerPoolProvider) reconcileWorkerAfterClientError(ctx context.Context, workerID string) (*model.Worker, error) {
	current, err := p.manager.GetWorker(ctx, workerID)
	if err != nil {
		return nil, err
	}
	if current == nil || strings.TrimSpace(current.ID) == "" {
		return nil, sandbox.ErrNoSandboxCapacity
	}
	if err := p.manager.ScheduleWorkerReconciliation(ctx, current.ID); err != nil {
		return nil, err
	}
	return p.waitForWorkerReconcile(ctx, current.ID)
}

// waitForWorkerReconcile polls the worker row until its recorded operation
// reaches a terminal status. Reconciliation is level-triggered: the reconciler
// writes progress onto the resource itself, so the resource — not a job row —
// is the thing to watch.
func (p *WorkerPoolProvider) waitForWorkerReconcile(ctx context.Context, workerID string) (*model.Worker, error) {
	deadline := time.Now().Add(workerCapacityWaitTimeout)
	for {
		worker, err := p.manager.GetWorker(ctx, workerID)
		if err != nil {
			return nil, err
		}
		if worker != nil && workerOperationTerminal(worker) {
			return worker, nil
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

// workerOperationTerminal reports whether the worker's last recorded operation
// settled (success or failure) rather than being queued or in flight.
func workerOperationTerminal(worker *model.Worker) bool {
	switch worker.LastOperationStatus {
	case model.OperationStatusSuccess, model.OperationStatusFailed:
		return true
	default:
		return false
	}
}

func workerIDFromRuntimeState(state []byte) (string, error) {
	var runtimeSandbox sandbox.Sandbox
	if len(state) == 0 {
		return "", sandbox.ErrNotFound
	}
	if err := json.Unmarshal(state, &runtimeSandbox); err != nil {
		return "", err
	}
	if runtimeSandbox.Metadata != nil && runtimeSandbox.Metadata["worker_id"] != "" {
		return runtimeSandbox.Metadata["worker_id"], nil
	}
	return "", sandbox.ErrNotFound
}
