package workerpool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/obot-platform/discobox/orchestration"
	"github.com/obot-platform/discobox/server/internal/apperrors"
	workeragentauth "github.com/obot-platform/discobox/server/internal/auth/workeragent"
	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/transport"
	workerclient "github.com/obot-platform/discobox/worker-agent/api/gen"
	workerapimodel "github.com/obot-platform/discobox/worker-agent/api/model"
)

const (
	defaultWorkerCapacityWaitTimeout  = 30 * time.Second
	defaultWorkerCapacityPollInterval = time.Second
)

var (
	workerCapacityWaitTimeout  = defaultWorkerCapacityWaitTimeout
	workerCapacityPollInterval = defaultWorkerCapacityPollInterval
)

// WorkerProvider owns worker runtime lifecycle and worker-local sandbox access.
type WorkerProvider interface {
	InitializeWorkerProvider(ctx context.Context, provider *model.SandboxProviderInstance, manager any) error
	CreateWorker(ctx context.Context, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker, token string, controlPlanePublicKey string) error
	RemoveWorker(ctx context.Context, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker) error
	AcquireWorkerHTTPClient(ctx context.Context, worker *model.Worker) (*transport.HTTPClientLease, error)
}

type workerProviderInventoryReconciler interface {
	ReconcileWorkerProviderInventory(ctx context.Context, manager any, project *model.Project, provider *model.SandboxProviderInstance) (bool, error)
}

// WorkerPoolProvider is a sandbox provider backed by a warm worker pool.
//
// WorkerPoolProvider owns sandbox placement and worker-pool reconciliation. The
// worker provider owns the runtime mechanics for individual workers.
type WorkerPoolProvider struct {
	workerProvider       WorkerProvider
	poolConfig           WorkerPoolConfig
	manager              WorkerManager
	ensureRunningWorkers bool
}

type providerInstanceManager interface {
	GetProject(ctx context.Context, projectID string) (*model.Project, error)
	GetSandboxProviderInstance(ctx context.Context, projectID, providerID string) (*model.SandboxProviderInstance, error)
}

func NewWorkerPoolProvider(provider WorkerProvider, poolConfig WorkerPoolConfig, manager WorkerManager, ensureRunningWorkers bool) *WorkerPoolProvider {
	return &WorkerPoolProvider{workerProvider: provider, poolConfig: poolConfig, manager: manager, ensureRunningWorkers: ensureRunningWorkers}
}

func (p *WorkerPoolProvider) Initialize(ctx context.Context, provider *model.SandboxProviderInstance) error {
	if err := p.workerProvider.InitializeWorkerProvider(ctx, provider, p.manager); err != nil {
		return err
	}
	return p.manager.ScheduleWorkerProviderReconciliation(ctx, provider.ProjectID, provider.ID)
}

func (p *WorkerPoolProvider) List(context.Context) ([]*sandbox.Sandbox, error) {
	return nil, nil
}

func (p *WorkerPoolProvider) Definition() sandbox.ProviderDefinition {
	if provider, ok := p.workerProvider.(sandbox.DefinitionProvider); ok {
		return provider.Definition()
	}
	return sandbox.ProviderDefinition{}
}

func (p *WorkerPoolProvider) Status() sandbox.ProviderStatus {
	if provider, ok := p.workerProvider.(sandbox.StatusProvider); ok {
		return provider.Status()
	}
	return sandbox.ProviderStatus{Available: true, State: "ready"}
}

func (p *WorkerPoolProvider) ensureWorkerPool(ctx context.Context, manager WorkerManager, project *model.Project, provider *model.SandboxProviderInstance) error {
	if err := ensureWorkerPool(ctx, manager, project, provider, p.poolConfig); err != nil {
		return err
	}
	if !p.ensureRunningWorkers {
		return nil
	}
	return p.ensureActiveWorkers(ctx, manager, project, provider)
}

func (p *WorkerPoolProvider) ReconcileWorkerProvider(ctx context.Context, manager any, project *model.Project, provider *model.SandboxProviderInstance) error {
	workerManager, ok := manager.(WorkerManager)
	if !ok {
		return fmt.Errorf("worker manager is required")
	}
	if reconciler, ok := p.workerProvider.(workerProviderInventoryReconciler); ok {
		_, err := reconciler.ReconcileWorkerProviderInventory(ctx, workerManager, project, provider)
		if err != nil {
			return err
		}
	}
	return p.ensureWorkerPool(ctx, workerManager, project, provider)
}

func (p *WorkerPoolProvider) ReconcileWorker(ctx context.Context, manager any, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker) error {
	workerManager, ok := manager.(WorkerManager)
	if !ok {
		return fmt.Errorf("worker manager is required")
	}
	if p.workerProvider == nil {
		return fmt.Errorf("worker provider is required")
	}
	token, err := createWorkerBootstrap(ctx, workerManager, project, worker)
	if err != nil {
		return err
	}
	controlPlanePublicKey, err := workerManager.EnsureWorkerAgentTrustKey(ctx)
	if err != nil {
		return err
	}
	return p.workerProvider.CreateWorker(ctx, project, provider, worker, token, controlPlanePublicKey)
}

func (p *WorkerPoolProvider) RemoveWorker(ctx context.Context, _ any, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker) error {
	if p.workerProvider == nil {
		return fmt.Errorf("worker provider is required")
	}
	return p.workerProvider.RemoveWorker(ctx, project, provider, worker)
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
	if p.workerProvider == nil {
		return nil, state, fmt.Errorf("worker provider is required")
	}
	provider, err := p.sandboxProviderForWorker(ctx, worker)
	if err != nil {
		return nil, state, err
	}
	return provider.Create(ctx, ref, state, opts)
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
	providerManager, ok := p.manager.(providerInstanceManager)
	if !ok {
		return sandbox.ErrNoSandboxCapacity
	}
	project, err := providerManager.GetProject(ctx, sb.ProjectID)
	if err != nil {
		return err
	}
	provider, err := providerManager.GetSandboxProviderInstance(ctx, sb.ProjectID, *sb.ProviderInstanceID)
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

func (p *WorkerPoolProvider) Start(ctx context.Context, ref sandbox.SandboxRef, state []byte) (*sandbox.Sandbox, []byte, error) {
	if p.workerProvider == nil {
		return nil, state, fmt.Errorf("worker provider is required")
	}
	provider, err := p.sandboxProviderFromState(ctx, state)
	if err != nil {
		return nil, state, err
	}
	return provider.Start(ctx, ref, state)
}

func (p *WorkerPoolProvider) Stop(ctx context.Context, ref sandbox.SandboxRef, state []byte, timeout time.Duration) (*sandbox.Sandbox, []byte, error) {
	if p.workerProvider == nil {
		return nil, state, fmt.Errorf("worker provider is required")
	}
	provider, err := p.sandboxProviderFromState(ctx, state)
	if err != nil {
		return nil, state, err
	}
	return provider.Stop(ctx, ref, state, timeout)
}

func (p *WorkerPoolProvider) Get(ctx context.Context, ref sandbox.SandboxRef, state []byte) (*sandbox.Sandbox, error) {
	if p.workerProvider == nil {
		return nil, fmt.Errorf("worker provider is required")
	}
	provider, err := p.sandboxProviderFromState(ctx, state)
	if err != nil {
		return nil, err
	}
	return provider.Get(ctx, ref, state)
}

func (p *WorkerPoolProvider) AcquireHTTPClient(ctx context.Context, ref sandbox.SandboxRef, state []byte, scopes []string) (*transport.HTTPClientLease, error) {
	if p.workerProvider == nil {
		return nil, fmt.Errorf("worker provider is required")
	}
	provider, err := p.sandboxProviderFromState(ctx, state)
	if err != nil {
		return nil, err
	}
	return provider.AcquireHTTPClient(ctx, ref, state, scopes)
}

func (p *WorkerPoolProvider) Remove(ctx context.Context, ref sandbox.SandboxRef, state []byte, opts ...sandbox.RemoveOption) ([]byte, error) {
	if p.workerProvider == nil {
		return state, fmt.Errorf("worker provider is required")
	}
	provider, err := p.sandboxProviderFromState(ctx, state)
	if err != nil {
		return state, err
	}
	return provider.Remove(ctx, ref, state, opts...)
}

func (p *WorkerPoolProvider) sandboxProviderFromState(ctx context.Context, state []byte) (sandbox.Provider, error) {
	workerID, err := workerIDFromRuntimeState(state)
	if err != nil {
		return nil, err
	}
	worker, err := p.manager.GetWorker(ctx, workerID)
	if err != nil {
		return nil, err
	}
	return p.sandboxProviderForWorker(ctx, worker)
}

func (p *WorkerPoolProvider) sandboxProviderForWorker(ctx context.Context, worker *model.Worker) (sandbox.Provider, error) {
	lease, err := p.acquireWorkerHTTPClient(ctx, worker)
	if err != nil {
		return nil, err
	}
	return &workerAgentSandboxProvider{workerID: worker.ID, tokenIssuer: p.manager, lease: lease}, nil
}

func (p *WorkerPoolProvider) acquireWorkerHTTPClient(ctx context.Context, worker *model.Worker) (*transport.HTTPClientLease, error) {
	lease, err := p.workerProvider.AcquireWorkerHTTPClient(ctx, worker)
	if err == nil {
		return lease, nil
	}
	if p.manager == nil || worker == nil || strings.TrimSpace(worker.ID) == "" {
		return nil, err
	}
	worker, retryErr := p.reconcileWorkerAfterHTTPClientError(ctx, worker.ID)
	if retryErr != nil {
		return nil, retryErr
	}
	return p.workerProvider.AcquireWorkerHTTPClient(ctx, worker)
}

func (p *WorkerPoolProvider) reconcileWorkerAfterHTTPClientError(ctx context.Context, workerID string) (*model.Worker, error) {
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

func (p *WorkerPoolProvider) waitForWorkerReconcile(ctx context.Context, workerID string) (*model.Worker, error) {
	deadline := time.Now().Add(workerCapacityWaitTimeout)
	for {
		worker, err := p.manager.GetWorker(ctx, workerID)
		if err != nil {
			return nil, err
		}
		if worker != nil && worker.LastJobID != nil && strings.TrimSpace(*worker.LastJobID) != "" {
			job, err := p.manager.GetJob(ctx, *worker.LastJobID)
			if err != nil {
				return nil, err
			}
			if workerReconcileJobTerminal(job) {
				return worker, nil
			}
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

func workerReconcileJobTerminal(job *orchestration.Job) bool {
	if job == nil {
		return false
	}
	switch job.Status {
	case orchestration.StatusCompleted, orchestration.StatusFailed, orchestration.StatusCanceled:
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
	if runtimeSandbox.ID != "" && runtimeSandbox.Image == "warm-worker" {
		return runtimeSandbox.ID, nil
	}
	return "", sandbox.ErrNotFound
}

const defaultWorkerBaseURL = "https://worker"

type workerAgentSandboxProvider struct {
	workerID    string
	tokenIssuer workerAgentTokenIssuer
	lease       *transport.HTTPClientLease
}

type workerAgentTokenIssuer interface {
	CreateWorkerAgentToken(ctx context.Context, claims workeragentauth.TokenClaims) (string, error)
	CreateSandboxAgentToken(ctx context.Context, claims workeragentauth.TokenClaims) (string, error)
}

func (p *workerAgentSandboxProvider) Initialize(context.Context, *model.SandboxProviderInstance) error {
	return nil
}

func (p *workerAgentSandboxProvider) List(context.Context) ([]*sandbox.Sandbox, error) {
	return nil, nil
}

func (p *workerAgentSandboxProvider) Create(ctx context.Context, ref sandbox.SandboxRef, state []byte, opts sandbox.CreateOptions) (*sandbox.Sandbox, []byte, error) {
	client, release, err := p.workerClient(ref, workeragentauth.ScopeSandboxWrite)
	if err != nil {
		return nil, state, err
	}
	defer release()
	workerSandbox, err := client.WorkerCreateSandbox(ctx, workerCreateRequestFromOptions(ref.SandboxID, opts), workerclient.WorkerCreateSandboxParams{ProjectId: ref.ProjectID, WorkerId: p.workerID})
	if err != nil {
		mapped := mapWorkerClientError(err)
		if errors.Is(mapped, sandbox.ErrAlreadyExists) {
			workerSandbox, err = client.WorkerGetSandbox(ctx, workerclient.WorkerGetSandboxParams{ProjectId: ref.ProjectID, WorkerId: p.workerID, SandboxId: ref.SandboxID})
			if err != nil {
				return nil, state, mapWorkerClientError(err)
			}
		} else {
			return nil, state, mapped
		}
	}
	runtimeSandbox := sandboxFromWorker(workerSandbox, p.workerID)
	state, err = json.Marshal(runtimeSandbox)
	if err != nil {
		return nil, state, err
	}
	return runtimeSandbox, state, nil
}

func (p *workerAgentSandboxProvider) Start(ctx context.Context, ref sandbox.SandboxRef, state []byte) (*sandbox.Sandbox, []byte, error) {
	client, release, err := p.workerClient(ref, workeragentauth.ScopeSandboxWrite)
	if err != nil {
		return nil, state, err
	}
	defer release()
	workerSandbox, err := client.WorkerStartSandbox(ctx, &workerapimodel.WorkerSandboxOperationRequest{}, workerclient.WorkerStartSandboxParams{ProjectId: ref.ProjectID, WorkerId: p.workerID, SandboxId: ref.SandboxID})
	if err != nil {
		return nil, state, mapWorkerClientError(err)
	}
	runtimeSandbox := sandboxFromWorker(workerSandbox, p.workerID)
	state, err = json.Marshal(runtimeSandbox)
	if err != nil {
		return nil, state, err
	}
	return runtimeSandbox, state, nil
}

func (p *workerAgentSandboxProvider) Stop(ctx context.Context, ref sandbox.SandboxRef, state []byte, _ time.Duration) (*sandbox.Sandbox, []byte, error) {
	client, release, err := p.workerClient(ref, workeragentauth.ScopeSandboxWrite)
	if err != nil {
		return nil, state, err
	}
	defer release()
	workerSandbox, err := client.WorkerStopSandbox(ctx, &workerapimodel.WorkerSandboxOperationRequest{}, workerclient.WorkerStopSandboxParams{ProjectId: ref.ProjectID, WorkerId: p.workerID, SandboxId: ref.SandboxID})
	if err != nil {
		return nil, state, mapWorkerClientError(err)
	}
	runtimeSandbox := sandboxFromWorker(workerSandbox, p.workerID)
	state, err = json.Marshal(runtimeSandbox)
	if err != nil {
		return nil, state, err
	}
	return runtimeSandbox, state, nil
}

func (p *workerAgentSandboxProvider) Remove(ctx context.Context, ref sandbox.SandboxRef, state []byte, _ ...sandbox.RemoveOption) ([]byte, error) {
	client, release, err := p.workerClient(ref, workeragentauth.ScopeSandboxWrite)
	if err != nil {
		return state, err
	}
	defer release()
	if err := client.WorkerDeleteSandbox(ctx, workerclient.WorkerDeleteSandboxParams{ProjectId: ref.ProjectID, WorkerId: p.workerID, SandboxId: ref.SandboxID}); err != nil {
		return state, mapWorkerClientError(err)
	}
	return nil, nil
}

func (p *workerAgentSandboxProvider) Get(ctx context.Context, ref sandbox.SandboxRef, _ []byte) (*sandbox.Sandbox, error) {
	client, release, err := p.workerClient(ref, workeragentauth.ScopeSandboxRead)
	if err != nil {
		return nil, err
	}
	defer release()
	workerSandbox, err := client.WorkerGetSandbox(ctx, workerclient.WorkerGetSandboxParams{ProjectId: ref.ProjectID, WorkerId: p.workerID, SandboxId: ref.SandboxID})
	if err != nil {
		return nil, mapWorkerClientError(err)
	}
	return sandboxFromWorker(workerSandbox, p.workerID), nil
}

func (p *workerAgentSandboxProvider) AcquireHTTPClient(_ context.Context, ref sandbox.SandboxRef, _ []byte, scopes []string) (*transport.HTTPClientLease, error) {
	lease := p.lease
	p.lease = nil
	if lease != nil && lease.AuthTokenProvider == nil {
		lease.AuthTokenProvider = p.authTokenProvider(ref, scopes...)
	}
	if lease != nil && lease.ForwardAuthTokenProvider == nil && requiresSandboxAgentToken(scopes) {
		lease.ForwardAuthTokenProvider = p.sandboxAgentAuthTokenProvider(ref, scopes...)
	}
	return lease, nil
}

func (p *workerAgentSandboxProvider) workerClient(ref sandbox.SandboxRef, scopes ...string) (*workerclient.Client, func(), error) {
	lease := p.lease
	p.lease = nil
	if lease != nil && lease.AuthTokenProvider == nil {
		lease.AuthTokenProvider = p.authTokenProvider(ref, scopes...)
	}
	client, err := newWorkerAgentClient(lease)
	if err != nil {
		if lease != nil {
			lease.Release()
		}
		return nil, nil, err
	}
	return client, func() {
		if lease != nil {
			lease.Release()
		}
	}, nil
}

func (p *workerAgentSandboxProvider) authTokenProvider(ref sandbox.SandboxRef, scopes ...string) func(context.Context) (string, error) {
	tokenScopes := append([]string(nil), scopes...)
	return func(ctx context.Context) (string, error) {
		if p.tokenIssuer == nil {
			return "", fmt.Errorf("worker-agent token issuer is required")
		}
		return p.tokenIssuer.CreateWorkerAgentToken(ctx, workeragentauth.TokenClaims{
			ProjectID: ref.ProjectID,
			WorkerID:  p.workerID,
			SandboxID: ref.SandboxID,
			Scopes:    tokenScopes,
		})
	}
}

func (p *workerAgentSandboxProvider) sandboxAgentAuthTokenProvider(ref sandbox.SandboxRef, scopes ...string) func(context.Context) (string, error) {
	tokenScopes := append([]string(nil), scopes...)
	return func(ctx context.Context) (string, error) {
		if p.tokenIssuer == nil {
			return "", fmt.Errorf("worker-agent token issuer is required")
		}
		return p.tokenIssuer.CreateSandboxAgentToken(ctx, workeragentauth.TokenClaims{
			ProjectID: ref.ProjectID,
			WorkerID:  p.workerID,
			SandboxID: ref.SandboxID,
			Scopes:    tokenScopes,
		})
	}
}

func requiresSandboxAgentToken(scopes []string) bool {
	for _, scope := range scopes {
		switch scope {
		case workeragentauth.ScopeTerminalRead, workeragentauth.ScopeTerminalWrite, "terminal:*", "*":
			return true
		}
	}
	return false
}

func newWorkerAgentClient(lease *transport.HTTPClientLease) (*workerclient.Client, error) {
	httpClient := http.DefaultClient
	baseURL := defaultWorkerBaseURL
	if lease != nil {
		if lease.Client != nil {
			httpClient = lease.Client
		}
		if strings.TrimSpace(lease.BaseURL) != "" {
			baseURL = lease.BaseURL
		}
	}
	return workerclient.NewClient(strings.TrimRight(baseURL, "/"), workerSecuritySource{lease: lease}, workerclient.WithClient(httpClient))
}

func workerCreateRequestFromOptions(sandboxID string, opts sandbox.CreateOptions) *workerapimodel.WorkerSandboxCreateRequest {
	out := &workerapimodel.WorkerSandboxCreateRequest{SandboxId: sandboxID}
	if opts.Image.Name != "" {
		out.Image = workerclient.NewOptString(opts.Image.Name)
	}
	if opts.Env != nil {
		out.Env = workerclient.NewOptWorkerSandboxCreateRequestEnv(workerclient.WorkerSandboxCreateRequestEnv(opts.Env))
	}
	if opts.Name != "" {
		out.Name = workerclient.NewOptString(opts.Name)
	}
	if opts.Description != nil {
		out.Description = workerclient.NewOptString(*opts.Description)
	}
	if opts.ProviderInstanceID != "" {
		out.ProviderInstanceId = workerclient.NewOptString(opts.ProviderInstanceID)
	}
	if opts.AgentConfigID != nil {
		out.AgentConfigId = workerclient.NewOptString(*opts.AgentConfigID)
	}
	if opts.ResolvedAgentConfig != nil {
		resolved := workerapimodel.ResolvedAgentConfig{
			ID:             opts.ResolvedAgentConfig.ID,
			Name:           opts.ResolvedAgentConfig.Name,
			InstallCommand: workerclient.NewOptString(opts.ResolvedAgentConfig.InstallCommand),
			RunCommand:     opts.ResolvedAgentConfig.RunCommand,
		}
		out.ResolvedAgentConfig = workerclient.NewOptResolvedAgentConfig(resolved)
	}
	if opts.AgentModel != nil {
		out.AgentModel = workerclient.NewOptString(*opts.AgentModel)
	}
	if opts.AgentModelServiceTier != nil {
		out.AgentModelServiceTier = workerclient.NewOptString(*opts.AgentModelServiceTier)
	}
	if opts.AgentModelReasoningLevel != nil {
		out.AgentModelReasoningLevel = workerclient.NewOptString(*opts.AgentModelReasoningLevel)
	}
	if opts.Prompt != nil {
		out.Prompt = workerclient.NewOptString(*opts.Prompt)
	}
	if opts.Source != nil {
		workerSource, err := workerGitSource(*opts.Source)
		if err == nil {
			out.Source = workerclient.NewOptGitSource(workerSource)
		}
	}
	if opts.SourceCodeReferences != nil {
		out.SourceCodeReferences = workerclient.NewOptWorkerSandboxCreateRequestSourceCodeReferences(workerSourceCodeReferences(opts.SourceCodeReferences))
	}
	user := workerapimodel.WorkerSandboxUser{}
	user.SetName(workerOptStringPtr(opts.UserName))
	if opts.UserUID != nil {
		user.SetUID(workerclient.NewOptInt64(int64(*opts.UserUID)))
	}
	if opts.UserGID != nil {
		user.SetGid(workerclient.NewOptInt64(int64(*opts.UserGID)))
	}
	user.SetHomeDirectory(workerOptStringPtr(opts.HomeDirectory))
	if user.Name.Set || user.UID.Set || user.Gid.Set || user.HomeDirectory.Set {
		out.User = workerclient.NewOptWorkerSandboxUser(user)
	}
	if opts.Resources != (sandbox.ResourceConfig{}) {
		out.Resources = workerclient.NewOptResourceConfig(workerResourceConfig(opts.Resources))
	}
	if opts.CPUVCPUs != 0 {
		out.CpuVcpus = workerclient.NewOptFloat64(opts.CPUVCPUs)
	}
	if opts.MemoryBytes != 0 {
		out.MemoryBytes = workerclient.NewOptInt64(opts.MemoryBytes)
	}
	if opts.StorageBytes != 0 {
		out.StorageBytes = workerclient.NewOptInt64(opts.StorageBytes)
	}
	return out
}

func workerOptStringPtr(value *string) workerclient.OptString {
	if value == nil {
		return workerclient.OptString{}
	}
	return workerclient.NewOptString(*value)
}

func workerResourceConfig(cfg sandbox.ResourceConfig) workerapimodel.ResourceConfig {
	return workerapimodel.ResourceConfig{
		MemoryMB: int64(cfg.MemoryMB),
		CPUCores: cfg.CPUCores,
		DiskMB:   int64(cfg.DiskMB),
		Timeout:  int64(cfg.Timeout),
	}
}

func workerSourceCodeReferences(in model.SourceCodeReferences) workerclient.WorkerSandboxCreateRequestSourceCodeReferences {
	out := make(workerclient.WorkerSandboxCreateRequestSourceCodeReferences, len(in))
	for key, ref := range in {
		workerRef, err := workerGitSource(ref)
		if err != nil {
			continue
		}
		out[key] = workerRef
	}
	return out
}

func workerGitSource(in model.GitSource) (workerapimodel.GitSource, error) {
	out := workerapimodel.GitSource{Kind: workerclient.GitSourceKind(in.Kind)}
	if out.Kind == "" {
		out.Kind = workerclient.GitSourceKindGit
	}
	if in.URL != nil && *in.URL != "" {
		parsed, err := url.Parse(*in.URL)
		if err != nil {
			return out, err
		}
		out.URL = workerclient.NewOptURI(*parsed)
	}
	if in.LocalDirectory != nil {
		out.LocalDirectory = workerclient.NewOptString(*in.LocalDirectory)
	}
	if in.Checkout != nil {
		checkout := workerapimodel.GitSourceCheckout{}
		if in.Checkout.Commit != nil {
			checkout.Commit = workerclient.NewOptString(*in.Checkout.Commit)
		}
		if in.Checkout.RefName != nil {
			checkout.RefName = workerclient.NewOptString(*in.Checkout.RefName)
		}
		if in.Checkout.RefType != nil {
			checkout.RefType = workerclient.NewOptString(*in.Checkout.RefType)
		}
		out.Checkout = workerclient.NewOptGitSourceCheckout(checkout)
	}
	if in.Workspace != nil {
		workspace := workerapimodel.GitSourceWorkspace{}
		if in.Workspace.Mode != "" {
			workspace.Mode = workerclient.NewOptGitSourceWorkspaceMode(workerclient.GitSourceWorkspaceMode(in.Workspace.Mode))
		}
		if in.Workspace.SnapshotRef != nil {
			workspace.SnapshotRef = workerclient.NewOptString(*in.Workspace.SnapshotRef)
		}
		if in.Workspace.BaseCommit != nil {
			workspace.BaseCommit = workerclient.NewOptString(*in.Workspace.BaseCommit)
		}
		out.Workspace = workerclient.NewOptGitSourceWorkspace(workspace)
	}
	if in.Destination != nil {
		destination := workerapimodel.GitSourceDestination{}
		if in.Destination.Directory != nil {
			destination.Directory = workerclient.NewOptString(*in.Destination.Directory)
		}
		if in.Destination.WorkingDirectory != nil {
			destination.WorkingDirectory = workerclient.NewOptString(*in.Destination.WorkingDirectory)
		}
		out.Destination = workerclient.NewOptGitSourceDestination(destination)
	}
	return out, nil
}

func sandboxFromWorker(in *workerapimodel.Sandbox, workerID string) *sandbox.Sandbox {
	if in == nil {
		return nil
	}
	metadata := map[string]string(in.Metadata)
	if metadata == nil {
		metadata = map[string]string{}
	}
	metadata["worker_id"] = workerID
	return &sandbox.Sandbox{
		ID:        in.ID,
		SandboxID: in.SandboxID,
		Status:    sandbox.Status(in.Status),
		Image:     in.Image,
		CreatedAt: in.CreatedAt,
		StartedAt: timePtrFromWorker(in.StartedAt),
		StoppedAt: timePtrFromWorker(in.StoppedAt),
		Error:     in.Error,
		Metadata:  metadata,
		Ports:     portsFromWorker(in.Ports),
		Env:       map[string]string(in.Env),
	}
}

func timePtrFromWorker(in workerclient.NilDateTime) *time.Time {
	if in.Null {
		return nil
	}
	return &in.Value
}

func portsFromWorker(in []workerapimodel.AssignedPort) []sandbox.AssignedPort {
	if in == nil {
		return nil
	}
	out := make([]sandbox.AssignedPort, 0, len(in))
	for _, port := range in {
		out = append(out, sandbox.AssignedPort{
			ContainerPort: int(port.ContainerPort),
			HostPort:      int(port.HostPort),
			HostIP:        port.HostIP,
			Protocol:      port.Protocol,
		})
	}
	return out
}

func mapWorkerClientError(err error) error {
	if err == nil {
		return nil
	}
	var statusErr *workerclient.ErrorModelStatusCode
	if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
		return sandbox.ErrNotFound
	}
	if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusConflict {
		return sandbox.ErrAlreadyExists
	}
	if errors.As(err, &statusErr) {
		return fmt.Errorf("worker-agent request failed: %s", workerClientErrorMessage(statusErr))
	}
	return err
}

func workerClientErrorMessage(statusErr *workerclient.ErrorModelStatusCode) string {
	if statusErr == nil {
		return ""
	}
	if detail, ok := statusErr.Response.Detail.Get(); ok && strings.TrimSpace(detail) != "" {
		return strings.TrimSpace(detail)
	}
	if title, ok := statusErr.Response.Title.Get(); ok && strings.TrimSpace(title) != "" {
		if statusErr.StatusCode != 0 {
			return fmt.Sprintf("status %d: %s", statusErr.StatusCode, strings.TrimSpace(title))
		}
		return strings.TrimSpace(title)
	}
	if statusErr.StatusCode != 0 {
		return fmt.Sprintf("status %d", statusErr.StatusCode)
	}
	return statusErr.Error()
}

type workerSecuritySource struct {
	lease *transport.HTTPClientLease
}

func (s workerSecuritySource) WorkerBearerAuth(ctx context.Context, _ workerclient.OperationName) (workerclient.WorkerBearerAuth, error) {
	token, err := s.lease.AuthorizationToken(ctx)
	if err != nil {
		return workerclient.WorkerBearerAuth{}, err
	}
	return workerclient.WorkerBearerAuth{Token: strings.TrimSpace(token)}, nil
}
