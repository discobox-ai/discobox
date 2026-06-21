package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/obot-platform/discobox/server/internal/apperrors"
	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	workeragent "github.com/obot-platform/discobox/worker-agent"
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

// WorkerProvider is a VM-backed sandbox provider with a warm worker pool.
//
// The embedded Provider owns VM runtime mechanics. WorkerProvider owns the
// Disco worker-pool behavior that only applies to VM/worker-backed providers.
type WorkerProvider struct {
	*Provider
	poolConfig           WorkerPoolConfig
	launch               WorkerLauncher
	remove               WorkerRemover
	manager              WorkerManager
	ensureRunningWorkers bool
}

type ProviderInstanceManager interface {
	GetProject(ctx context.Context, projectID string) (*model.Project, error)
	GetSandboxProviderInstance(ctx context.Context, projectID, providerID string) (*model.SandboxProviderInstance, error)
}

type WorkerLookupManager interface {
	GetWorker(ctx context.Context, workerID string) (*model.Worker, error)
}

func NewWorkerProvider(provider *Provider, poolConfig WorkerPoolConfig, launch WorkerLauncher, manager WorkerManager, remove ...WorkerRemover) *WorkerProvider {
	workerProvider := &WorkerProvider{Provider: provider, poolConfig: poolConfig, launch: launch, manager: manager}
	if len(remove) > 0 {
		workerProvider.remove = remove[0]
	}
	return workerProvider
}

func (p *WorkerProvider) EnsureWorkerPool(ctx context.Context, manager WorkerManager, project *model.Project, provider *model.SandboxProviderInstance) error {
	if err := EnsureWorkerPool(ctx, manager, project, provider, p.poolConfig); err != nil {
		return err
	}
	if !p.ensureRunningWorkers {
		return nil
	}
	return p.ensureActiveWorkers(ctx, manager, project, provider)
}

func (p *WorkerProvider) EnsureProviderInstance(ctx context.Context, manager any, project *model.Project, provider *model.SandboxProviderInstance) error {
	workerManager, ok := manager.(WorkerManager)
	if !ok {
		return fmt.Errorf("worker manager is required")
	}
	return p.EnsureWorkerPool(ctx, workerManager, project, provider)
}

type WorkerBootstrapTokenManager interface {
	CreateWorkerBootstrapToken(ctx context.Context, token *model.WorkerBootstrapToken) error
}

func (p *WorkerProvider) ReconcileWorker(ctx context.Context, manager any, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker) error {
	bootstrapManager, ok := manager.(WorkerBootstrapTokenManager)
	if !ok {
		return fmt.Errorf("worker bootstrap token manager is required")
	}
	if p.launch == nil {
		return fmt.Errorf("worker launcher is required")
	}
	token, err := CreateWorkerBootstrap(ctx, bootstrapManager, project, worker)
	if err != nil {
		return err
	}
	return p.launch(ctx, project, provider, worker, token)
}

func (p *WorkerProvider) RemoveWorker(ctx context.Context, _ any, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker) error {
	if p.remove == nil {
		worker.RuntimeState = nil
		worker.Ready = false
		worker.Schedulable = false
		return nil
	}
	return p.remove(ctx, project, provider, worker)
}

func (p *WorkerProvider) EnsureRunningWorkers() {
	p.ensureRunningWorkers = true
}

func (p *WorkerProvider) ensureActiveWorkers(ctx context.Context, manager WorkerManager, project *model.Project, provider *model.SandboxProviderInstance) error {
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

func (p *WorkerProvider) Create(ctx context.Context, ref sandbox.SandboxRef, state []byte, opts sandbox.CreateOptions) (*sandbox.Sandbox, []byte, error) {
	if p.manager == nil {
		return nil, nil, fmt.Errorf("worker manager is required")
	}
	if workerID, err := workerIDFromRuntimeState(state); err == nil {
		if opts.WorkerID == workerID {
			if err := p.validateStateWorker(ctx, ref, opts, workerID); err != nil {
				return nil, state, err
			}
			return p.createOnWorker(ctx, ref, state, opts, workerID)
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
	return p.createOnWorker(ctx, ref, nil, opts, worker.ID)
}

func (p *WorkerProvider) validateStateWorker(ctx context.Context, ref sandbox.SandboxRef, opts sandbox.CreateOptions, workerID string) error {
	if opts.WorkerID != workerID {
		return sandbox.ErrNoSandboxCapacity
	}
	lookup, ok := p.manager.(WorkerLookupManager)
	if !ok {
		return fmt.Errorf("worker lookup manager is required")
	}
	worker, err := lookup.GetWorker(ctx, workerID)
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

func (p *WorkerProvider) createOnWorker(ctx context.Context, ref sandbox.SandboxRef, state []byte, opts sandbox.CreateOptions, workerID string) (*sandbox.Sandbox, []byte, error) {
	if p.Provider == nil {
		if runtimeSandbox := workerRuntimeSandboxFromState(ref.SandboxID, state); runtimeSandbox != nil && runtimeSandbox.Metadata["worker_id"] != "" {
			return runtimeSandbox, state, nil
		}
		runtimeSandbox := &sandbox.Sandbox{
			ID:        workerID,
			SandboxID: ref.SandboxID,
			Status:    sandbox.StatusRunning,
			Image:     "warm-worker",
			Metadata:  map[string]string{"worker_id": workerID},
		}
		state, err := json.Marshal(runtimeSandbox)
		if err != nil {
			return nil, nil, err
		}
		return runtimeSandbox, state, nil
	}
	lease, err := p.AcquireWorkerHTTPClient(ctx, workerID)
	if err != nil {
		return nil, state, err
	}
	defer lease.Release()
	client, err := newWorkerAgentClient(lease)
	if err != nil {
		return nil, state, err
	}
	workerSandbox, err := client.WorkerCreateSandbox(ctx, workerCreateRequestFromOptions(ref.SandboxID, opts), workerclient.WorkerCreateSandboxParams{ProjectId: ref.ProjectID, WorkerId: workerID})
	if err != nil {
		mapped := mapWorkerClientError(err)
		if errors.Is(mapped, sandbox.ErrAlreadyExists) {
			workerSandbox, err = client.WorkerGetSandbox(ctx, workerclient.WorkerGetSandboxParams{ProjectId: ref.ProjectID, WorkerId: workerID, SandboxId: ref.SandboxID})
			if err != nil {
				return nil, state, mapWorkerClientError(err)
			}
		} else {
			return nil, state, mapped
		}
	}
	runtimeSandbox := sandboxFromWorker(workerSandbox, workerID)
	state, err = json.Marshal(runtimeSandbox)
	if err != nil {
		return nil, state, err
	}
	return runtimeSandbox, state, nil
}

func (p *WorkerProvider) findSchedulableWorker(ctx context.Context, sb *model.Sandbox) (*model.Worker, error) {
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

func (p *WorkerProvider) ensureWorkerCapacity(ctx context.Context, sb *model.Sandbox) error {
	if sb == nil || sb.ProviderInstanceID == nil || *sb.ProviderInstanceID == "" {
		return sandbox.ErrNoSandboxCapacity
	}
	providerManager, ok := p.manager.(ProviderInstanceManager)
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
	return p.EnsureWorkerPool(ctx, p.manager, project, provider)
}

func (p *WorkerProvider) waitForSchedulableWorker(ctx context.Context, sb *model.Sandbox) (*model.Worker, error) {
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

func (p *WorkerProvider) Start(ctx context.Context, ref sandbox.SandboxRef, state []byte) (*sandbox.Sandbox, []byte, error) {
	if p.Provider == nil {
		runtimeSandbox := workerRuntimeSandboxFromState(ref.SandboxID, state)
		return runtimeSandbox, state, nil
	}
	workerID, err := workerIDFromRuntimeState(state)
	if err != nil {
		return nil, state, err
	}
	lease, err := p.AcquireWorkerHTTPClient(ctx, workerID)
	if err != nil {
		return nil, state, err
	}
	defer lease.Release()
	client, err := newWorkerAgentClient(lease)
	if err != nil {
		return nil, state, err
	}
	workerSandbox, err := client.WorkerStartSandbox(ctx, &workerapimodel.WorkerSandboxOperationRequest{}, workerclient.WorkerStartSandboxParams{ProjectId: ref.ProjectID, WorkerId: workerID, SandboxId: ref.SandboxID})
	if err != nil {
		return nil, state, mapWorkerClientError(err)
	}
	runtimeSandbox := sandboxFromWorker(workerSandbox, workerID)
	state, err = json.Marshal(runtimeSandbox)
	if err != nil {
		return nil, state, err
	}
	return runtimeSandbox, state, nil
}

func (p *WorkerProvider) Stop(ctx context.Context, ref sandbox.SandboxRef, state []byte, _ time.Duration) (*sandbox.Sandbox, []byte, error) {
	if p.Provider == nil {
		runtimeSandbox := workerRuntimeSandboxFromState(ref.SandboxID, state)
		if runtimeSandbox != nil {
			runtimeSandbox.Status = sandbox.StatusStopped
		}
		return runtimeSandbox, state, nil
	}
	workerID, err := workerIDFromRuntimeState(state)
	if err != nil {
		return nil, state, err
	}
	lease, err := p.AcquireWorkerHTTPClient(ctx, workerID)
	if err != nil {
		return nil, state, err
	}
	defer lease.Release()
	client, err := newWorkerAgentClient(lease)
	if err != nil {
		return nil, state, err
	}
	workerSandbox, err := client.WorkerStopSandbox(ctx, &workerapimodel.WorkerSandboxOperationRequest{}, workerclient.WorkerStopSandboxParams{ProjectId: ref.ProjectID, WorkerId: workerID, SandboxId: ref.SandboxID})
	if err != nil {
		return nil, state, mapWorkerClientError(err)
	}
	runtimeSandbox := sandboxFromWorker(workerSandbox, workerID)
	state, err = json.Marshal(runtimeSandbox)
	if err != nil {
		return nil, state, err
	}
	return runtimeSandbox, state, nil
}

func (p *WorkerProvider) Get(ctx context.Context, ref sandbox.SandboxRef, state []byte) (*sandbox.Sandbox, error) {
	if p.Provider == nil {
		return workerRuntimeSandboxFromState(ref.SandboxID, state), nil
	}
	workerID, err := workerIDFromRuntimeState(state)
	if err != nil {
		return nil, err
	}
	lease, err := p.AcquireWorkerHTTPClient(ctx, workerID)
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	client, err := newWorkerAgentClient(lease)
	if err != nil {
		return nil, err
	}
	workerSandbox, err := client.WorkerGetSandbox(ctx, workerclient.WorkerGetSandboxParams{ProjectId: ref.ProjectID, WorkerId: workerID, SandboxId: ref.SandboxID})
	if err != nil {
		return nil, mapWorkerClientError(err)
	}
	return sandboxFromWorker(workerSandbox, workerID), nil
}

func (p *WorkerProvider) AcquireHTTPClient(ctx context.Context, _ sandbox.SandboxRef, state []byte) (*sandbox.HTTPClientLease, error) {
	if p.Provider == nil {
		return nil, errors.New("worker provider does not provide sandbox HTTP access")
	}
	workerID, err := workerIDFromRuntimeState(state)
	if err != nil {
		return nil, err
	}
	return p.AcquireWorkerHTTPClient(ctx, workerID)
}

func (p *WorkerProvider) Remove(ctx context.Context, ref sandbox.SandboxRef, state []byte, _ ...sandbox.RemoveOption) ([]byte, error) {
	if p.Provider == nil {
		return nil, nil
	}
	workerID, err := workerIDFromRuntimeState(state)
	if err != nil {
		return state, err
	}
	lease, err := p.AcquireWorkerHTTPClient(ctx, workerID)
	if err != nil {
		return state, err
	}
	defer lease.Release()
	client, err := newWorkerAgentClient(lease)
	if err != nil {
		return state, err
	}
	if err := client.WorkerDeleteSandbox(ctx, workerclient.WorkerDeleteSandboxParams{ProjectId: ref.ProjectID, WorkerId: workerID, SandboxId: ref.SandboxID}); err != nil {
		return state, mapWorkerClientError(err)
	}
	return nil, nil
}

func workerRuntimeSandboxFromState(sandboxID string, state []byte) *sandbox.Sandbox {
	var runtimeSandbox sandbox.Sandbox
	if len(state) > 0 && json.Unmarshal(state, &runtimeSandbox) == nil && (runtimeSandbox.ID != "" || runtimeSandbox.Metadata["worker_id"] != "") {
		if runtimeSandbox.SandboxID == "" {
			runtimeSandbox.SandboxID = sandboxID
		}
		if runtimeSandbox.ID == "" {
			runtimeSandbox.ID = runtimeSandbox.Metadata["worker_id"]
		}
		runtimeSandbox.Status = sandbox.StatusRunning
		return &runtimeSandbox
	}
	return &sandbox.Sandbox{SandboxID: sandboxID, Status: sandbox.StatusRunning, Image: "warm-worker"}
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
			return workeragent.Bootstrap{ControlPlaneURL: cfg.ControlPlaneURL, ProjectID: project.ID, WorkerID: worker.ID, Token: token, AgentPort: cfg.AgentPort}, nil
		}),
		Metadata: cfg.Labels,
	})
	if err != nil {
		return err
	}
	labels := map[string]string{"discobox.worker_id": worker.ID, "discobox.worker_agent": "true", "discobox.provider_instance_id": provider.ID}
	for key, value := range cfg.Labels {
		labels[key] = value
	}
	ref := sandbox.SandboxRef{ProjectID: project.ID, SandboxID: "worker-" + worker.ID}
	state := worker.RuntimeState
	if len(state) > 0 {
		runtimeWorker, err := providerImpl.Get(ctx, ref, state)
		if errors.Is(err, sandbox.ErrNotFound) || shouldRecreateWorkerRuntime(runtimeWorker, cfg.DefaultImage) {
			state = nil
			worker.RuntimeState = nil
			worker.Ready = false
			worker.Schedulable = false
			worker.Phase = model.WorkerPhaseRegistering
		} else if err != nil {
			return err
		}
	}
	_, state, err = providerImpl.Create(ctx, ref, state, sandbox.CreateOptions{Labels: labels})
	if errors.Is(err, sandbox.ErrAlreadyExists) {
		return nil
	}
	if err == nil {
		worker.RuntimeState, err = safeWorkerRuntimeState(state)
	}
	return err
}

func RemoveWorker(ctx context.Context, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker, cfg LaunchWorkerConfig) error {
	providerImpl, err := cfg.Factory(ctx, Config{
		ControlPlaneURL: cfg.ControlPlaneURL,
		DefaultImage:    cfg.DefaultImage,
		AgentPort:       cfg.AgentPort,
		Metadata:        cfg.Labels,
	})
	if err != nil {
		return err
	}
	ref := sandbox.SandboxRef{ProjectID: project.ID, SandboxID: "worker-" + worker.ID}
	if len(worker.RuntimeState) > 0 {
		if _, err := providerImpl.Remove(ctx, ref, worker.RuntimeState, sandbox.RemoveVolumes()); err != nil && !errors.Is(err, sandbox.ErrNotFound) {
			return err
		}
	}
	worker.RuntimeState = nil
	worker.Ready = false
	worker.Schedulable = false
	worker.Degraded = false
	return nil
}

func shouldRecreateWorkerRuntime(runtimeWorker *sandbox.Sandbox, desiredImage string) bool {
	if runtimeWorker == nil {
		return true
	}
	if runtimeWorker.Status != sandbox.StatusRunning {
		return true
	}
	return strings.TrimSpace(desiredImage) != "" && runtimeWorker.Image != desiredImage
}

func safeWorkerRuntimeState(state []byte) ([]byte, error) {
	if len(state) == 0 {
		return nil, nil
	}
	var data struct {
		InstanceID string `json:"instanceId"`
	}
	if err := json.Unmarshal(state, &data); err != nil {
		return nil, err
	}
	if data.InstanceID == "" {
		return nil, sandbox.ErrNotFound
	}
	return json.Marshal(data)
}

const defaultWorkerBaseURL = "https://worker"

type workerSecuritySource struct {
	token string
}

func (s workerSecuritySource) WorkerBearerAuth(context.Context, workerclient.OperationName) (workerclient.WorkerBearerAuth, error) {
	return workerclient.WorkerBearerAuth{Token: s.token}, nil
}

func newWorkerAgentClient(lease *sandbox.HTTPClientLease) (*workerclient.Client, error) {
	httpClient := http.DefaultClient
	baseURL := defaultWorkerBaseURL
	authToken := ""
	if lease != nil {
		if lease.Client != nil {
			httpClient = lease.Client
		}
		if strings.TrimSpace(lease.BaseURL) != "" {
			baseURL = lease.BaseURL
		}
		authToken = strings.TrimSpace(lease.AuthToken)
	}
	return workerclient.NewClient(strings.TrimRight(baseURL, "/"), workerSecuritySource{token: authToken}, workerclient.WithClient(httpClient))
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
	if opts.SourceURL != "" {
		out.SourceUrl = workerclient.NewOptString(opts.SourceURL)
	}
	if opts.SourceRef != "" {
		out.SourceRef = workerclient.NewOptString(opts.SourceRef)
	}
	if opts.SourceRefType != "" {
		out.SourceRefType = workerclient.NewOptString(opts.SourceRefType)
	}
	if opts.SourceDirectory != "" {
		out.SourceDirectory = workerclient.NewOptString(opts.SourceDirectory)
	}
	if opts.SourceCodeReferences != nil {
		out.SourceCodeReferences = workerclient.NewOptWorkerSandboxCreateRequestSourceCodeReferences(workerSourceCodeReferences(opts.SourceCodeReferences))
	}
	if opts.UserUID != nil {
		out.UserUid = workerclient.NewOptInt64(int64(*opts.UserUID))
	}
	if opts.UserGID != nil {
		out.UserGid = workerclient.NewOptInt64(int64(*opts.UserGID))
	}
	if opts.WorkspacePath != "" {
		out.WorkspacePath = workerclient.NewOptString(opts.WorkspacePath)
	}
	if opts.WorkspaceSource != "" {
		out.WorkspaceSource = workerclient.NewOptString(opts.WorkspaceSource)
	}
	if opts.WorkspaceRef != "" {
		out.WorkspaceRef = workerclient.NewOptString(opts.WorkspaceRef)
	}
	if opts.WorkingDirectory != "" {
		out.WorkingDirectory = workerclient.NewOptString(opts.WorkingDirectory)
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
		workerRef := workerapimodel.GitSourceReference{
			Directory: ref.Directory,
		}
		if ref.URL != "" {
			if parsed, err := url.Parse(ref.URL); err == nil {
				workerRef.URL = *parsed
			}
		}
		if ref.Ref != nil {
			workerRef.Ref = workerclient.NewOptString(*ref.Ref)
		}
		if ref.RefType != nil {
			workerRef.RefType = workerclient.NewOptString(*ref.RefType)
		}
		out[key] = workerRef
	}
	return out
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
	return err
}
