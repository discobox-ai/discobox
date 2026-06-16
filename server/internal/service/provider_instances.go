package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/obot-platform/discobox/model"
	sandbox "github.com/obot-platform/discobox/sandboxprovider"
	"github.com/obot-platform/discobox/server/internal/api"
)

func (s *Service) ListSandboxProviderCatalogItems(context.Context) ([]api.SandboxProviderCatalogItem, error) {
	items := s.ListSandboxProviderCatalog()
	out := make([]api.SandboxProviderCatalogItem, 0, len(items))
	for _, item := range items {
		converted, err := api.Convert[api.SandboxProviderCatalogItem](item)
		if err != nil {
			return nil, err
		}
		out = append(out, converted)
	}
	return out, nil
}

func (s *Service) ListSandboxProviderInstances(ctx context.Context, projectID string) ([]model.SandboxProviderInstance, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apiError(err, "project not found")
	}
	return s.store.ListSandboxProviderInstances(ctx, projectID)
}

func (s *Service) CreateSandboxProviderInstance(ctx context.Context, projectID string, input api.CreateSandboxProviderInstanceBody) (*model.SandboxProviderInstance, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, apiError(err, "project not found")
	}
	if strings.TrimSpace(input.Type) == "" {
		return nil, apiError(fmt.Errorf("type is required"), "")
	}
	if err := s.SandboxProviderManager().ValidateProviderConfig(input.Type, api.RawMessage(input.Config)); err != nil {
		return nil, err
	}
	provider := &model.SandboxProviderInstance{ProjectID: projectID, Type: input.Type, Name: input.Name, Config: api.RawMessage(input.Config)}
	if err := s.store.CreateSandboxProviderInstance(ctx, provider); err != nil {
		return nil, err
	}
	if project.DefaultSandboxProviderID == "" {
		project.DefaultSandboxProviderID = provider.ID
		_ = s.store.UpsertProject(ctx, project)
	}
	if err := s.ensureSandboxProviderInstance(ctx, project, provider); err != nil {
		return nil, err
	}
	return s.store.GetSandboxProviderInstance(ctx, projectID, provider.ID)
}

func (s *Service) GetSandboxProviderInstance(ctx context.Context, projectID, providerID string) (*model.SandboxProviderInstance, error) {
	provider, err := s.store.GetSandboxProviderInstance(ctx, projectID, providerID)
	if err != nil {
		return nil, apiError(err, "provider instance not found")
	}
	workers, err := s.store.ListWorkers(ctx, projectID, providerID)
	if err != nil {
		return nil, err
	}
	provider.Status = providerStatusFromWorkers(workers)
	return provider, nil
}

func providerStatusFromWorkers(workers []model.Worker) *model.SandboxProviderInstanceStatus {
	status := &model.SandboxProviderInstanceStatus{
		WorkerCount: len(workers),
		Workers:     make([]model.ProviderWorkerStatus, 0, len(workers)),
	}
	for i := range workers {
		worker := workers[i]
		if worker.Ready {
			status.ReadyWorkers++
		}
		if worker.Schedulable {
			status.SchedulableWorkers++
		}
		if worker.Degraded {
			status.DegradedWorkers++
		}
		if worker.LastOperationStatus == model.OperationStatusFailed {
			status.FailedWorkers++
		}
		if worker.ErrorMessage != nil {
			status.LastError = worker.ErrorMessage
		}
		status.Workers = append(status.Workers, model.ProviderWorkerStatus{
			ID:                    worker.ID,
			Identity:              worker.Identity,
			DesiredState:          worker.DesiredState,
			Phase:                 worker.Phase,
			Ready:                 worker.Ready,
			Schedulable:           worker.Schedulable,
			Degraded:              worker.Degraded,
			LastOperationStatus:   worker.LastOperationStatus,
			StatusMessage:         worker.StatusMessage,
			ErrorMessage:          worker.ErrorMessage,
			AvailableCPUVCPUs:     worker.AvailableCPUVCPUs,
			AvailableMemoryBytes:  worker.AvailableMemoryBytes,
			AvailableStorageBytes: worker.AvailableStorageBytes,
			RuntimeID:             providerWorkerRuntimeID(worker.RuntimeState),
			LastSeenAt:            worker.LastSeenAt,
		})
	}
	return status
}

func providerWorkerRuntimeID(state []byte) string {
	if len(state) == 0 {
		return ""
	}
	var data struct {
		InstanceID string `json:"instanceId"`
	}
	if err := json.Unmarshal(state, &data); err != nil || data.InstanceID == "" {
		return ""
	}
	return data.InstanceID
}

func (s *Service) UpdateSandboxProviderInstance(ctx context.Context, projectID, providerID string, input api.UpdateSandboxProviderInstanceBody) (*model.SandboxProviderInstance, error) {
	provider, err := s.store.GetSandboxProviderInstance(ctx, projectID, providerID)
	if err != nil {
		return nil, apiError(err, "provider instance not found")
	}
	if name, ok := input.Name.Get(); ok {
		provider.Name = name
	}
	if len(input.Config) > 0 {
		config := api.RawMessage(input.Config)
		if err := s.SandboxProviderManager().ValidateProviderConfig(provider.Type, config); err != nil {
			return nil, err
		}
		provider.Config = config
	}
	if disabled, ok := input.Disabled.Get(); ok {
		provider.Disabled = disabled
	}
	if err := s.store.UpdateSandboxProviderInstance(ctx, provider); err != nil {
		return nil, err
	}
	return s.store.GetSandboxProviderInstance(ctx, projectID, providerID)
}

func (s *Service) DeleteSandboxProviderInstance(ctx context.Context, projectID, providerID string) error {
	return apiError(s.store.DeleteSandboxProviderInstance(ctx, projectID, providerID), "provider instance not found")
}

// EnsureExistingSandboxProviderInstances runs provider instance startup reconciliation
// for persisted provider instances. Runtime-driven worker scaling after this point
// belongs inside the provider implementation.
func (s *Service) EnsureExistingSandboxProviderInstances(ctx context.Context) error {
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		return err
	}
	for i := range projects {
		project := &projects[i]
		providers, err := s.store.ListSandboxProviderInstances(ctx, project.ID)
		if err != nil {
			return err
		}
		for i := range providers {
			provider := &providers[i]
			if provider.Disabled {
				continue
			}
			if err := s.ensureSandboxProviderInstance(ctx, project, provider); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) ensureSandboxProviderInstance(ctx context.Context, project *model.Project, provider *model.SandboxProviderInstance) error {
	if provider == nil || provider.Disabled {
		return nil
	}
	runtimeProvider, err := s.SandboxProviderManager().ResolveInstance(ctx, provider)
	if err != nil {
		return err
	}
	ensurer, ok := runtimeProvider.(sandbox.ProviderInstanceEnsurer)
	if !ok {
		return nil
	}
	providerStore := any(s.store)
	if s.workerStore != nil {
		providerStore = s.workerStore
	}
	return ensurer.EnsureProviderInstance(ctx, providerStore, project, provider)
}
