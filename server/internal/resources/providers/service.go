package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/obot-platform/discobox/apperrors"
	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/orchestration"
	sandbox "github.com/obot-platform/discobox/sandboxprovider"
	sandboxesvc "github.com/obot-platform/discobox/server/internal/resources/sandboxes"
	"github.com/obot-platform/discobox/server/internal/resources/workers"
	services "github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
)

type Service struct {
	store     *store.Store
	sandboxes SandboxCatalogService
	workers   *workers.Manager
	jobs      JobManager
}

type SandboxCatalogService interface {
	ListSandboxProviderCatalog() []SandboxProviderCatalogItem
	SandboxProviderManager() *sandbox.ProviderManager
}

type SandboxProviderCatalogItem = sandboxesvc.SandboxProviderCatalogItem

type JobManager interface {
	EnqueueWorkerCurrent(context.Context, *model.Worker) (*orchestration.Job, error)
	EnqueueProviderCurrent(context.Context, string, string) (*orchestration.Job, error)
}

func NewService(store *store.Store, sandboxes SandboxCatalogService, workerManager *workers.Manager) *Service {
	return &Service{store: store, sandboxes: sandboxes, workers: workerManager}
}

func (s *Service) SetJobManager(manager JobManager) {
	s.jobs = manager
}

func mapAPIError(err error, notFoundMessage string) error {
	if errors.Is(err, store.ErrNotFound) {
		return apperrors.NewStatusError(http.StatusNotFound, notFoundMessage)
	}
	return err
}

func (s *Service) ListSandboxProviderCatalogItems(context.Context) ([]services.SandboxProviderCatalogItem, error) {
	items := s.sandboxes.ListSandboxProviderCatalog()
	out := make([]services.SandboxProviderCatalogItem, 0, len(items))
	for _, item := range items {
		out = append(out, providerCatalogItemToService(item))
	}
	return out, nil
}

func providerCatalogItemToService(item SandboxProviderCatalogItem) services.SandboxProviderCatalogItem {
	out := services.SandboxProviderCatalogItem{
		ID:           item.ID,
		Name:         item.Name,
		Available:    item.Available,
		BuiltIn:      item.BuiltIn,
		Capabilities: providerStatusToService(item.Capabilities),
	}
	if item.Icon != "" {
		out.Icon = services.OptString{Value: item.Icon, Set: true}
	}
	if item.Description != "" {
		out.Description = services.OptString{Value: item.Description, Set: true}
	}
	if item.ConfigFields != nil {
		fields := make([]services.ProviderConfigField, 0, len(item.ConfigFields))
		for _, field := range item.ConfigFields {
			fields = append(fields, providerConfigFieldToService(field))
		}
		out.ConfigFields = services.OptNilProviderConfigFieldArray{Value: fields, Set: true}
	}
	return out
}

func providerConfigFieldToService(field sandboxesvc.ProviderConfigField) services.ProviderConfigField {
	out := services.ProviderConfigField{
		Key:   field.Key,
		Label: field.Label,
		Type:  field.Type,
	}
	if field.Description != "" {
		out.Description = services.OptString{Value: field.Description, Set: true}
	}
	if field.Placeholder != "" {
		out.Placeholder = services.OptString{Value: field.Placeholder, Set: true}
	}
	if field.Required {
		out.Required = services.OptBool{Value: field.Required, Set: true}
	}
	if field.Advanced {
		out.Advanced = services.OptBool{Value: field.Advanced, Set: true}
	}
	if field.CredentialProvider != "" {
		out.CredentialProvider = services.OptString{Value: field.CredentialProvider, Set: true}
	}
	if field.CredentialAuthType != "" {
		out.CredentialAuthType = services.OptString{Value: field.CredentialAuthType, Set: true}
	}
	return out
}

func providerStatusToService(status sandboxesvc.ProviderStatus) services.ProviderStatus {
	out := services.ProviderStatus{
		Available:          status.Available,
		State:              status.State,
		SupportsResources:  status.SupportsResources,
		SupportsInspection: status.SupportsInspection,
		SupportsClearCache: status.SupportsClearCache,
		SupportsImages:     status.SupportsImages,
	}
	if status.Message != "" {
		out.Message = services.OptString{Value: status.Message, Set: true}
	}
	if status.Details != nil {
		if data, err := json.Marshal(status.Details); err == nil {
			out.Details = data
		}
	}
	return out
}

func (s *Service) ListSandboxProviderInstances(ctx context.Context, projectID string) ([]model.SandboxProviderInstance, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, mapAPIError(err, "project not found")
	}
	providers, err := s.store.ListSandboxProviderInstancesWithWorkers(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for i := range providers {
		providers[i].Status = providerStatusFromWorkers(providers[i].Workers)
	}
	return providers, nil
}

func (s *Service) CreateSandboxProviderInstance(ctx context.Context, projectID string, input services.CreateSandboxProviderInstanceBody) (*model.SandboxProviderInstance, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, mapAPIError(err, "project not found")
	}
	if strings.TrimSpace(input.Type) == "" {
		return nil, mapAPIError(fmt.Errorf("type is required"), "")
	}
	if err := s.sandboxes.SandboxProviderManager().ValidateProviderConfig(input.Type, services.RawMessage(input.Config)); err != nil {
		return nil, err
	}
	provider := &model.SandboxProviderInstance{ProjectID: projectID, Type: input.Type, Name: input.Name, Config: services.RawMessage(input.Config)}
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
		return nil, mapAPIError(err, "provider instance not found")
	}
	if err := s.attachProviderStatus(ctx, projectID, provider); err != nil {
		return nil, err
	}
	return provider, nil
}

func (s *Service) attachProviderStatus(ctx context.Context, projectID string, provider *model.SandboxProviderInstance) error {
	if provider == nil {
		return nil
	}
	workers, err := s.store.ListWorkers(ctx, projectID, provider.ID)
	if err != nil {
		return err
	}
	provider.Status = providerStatusFromWorkers(workers)
	return nil
}

func providerStatusFromWorkers(workers []model.Worker) *model.SandboxProviderInstanceStatus {
	statusWorkers := providerStatusWorkers(workers)
	status := &model.SandboxProviderInstanceStatus{
		Workers: make([]model.ProviderWorkerStatus, 0, len(statusWorkers)),
	}
	for i := range statusWorkers {
		worker := statusWorkers[i]
		if !providerWorkerTerminalDeleted(worker) {
			status.WorkerCount++
			if worker.Ready {
				status.ReadyWorkers++
			}
			if worker.Schedulable {
				status.SchedulableWorkers++
			}
			if worker.Degraded {
				status.DegradedWorkers++
			}
			if providerWorkerHasError(worker) {
				status.FailedWorkers++
			}
		}
		if worker.ErrorMessage != nil {
			status.LastError = worker.ErrorMessage
		} else if providerWorkerCleanupError(worker) {
			status.LastError = worker.StatusMessage
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

func providerStatusWorkers(workers []model.Worker) []model.Worker {
	active := make([]model.Worker, 0, len(workers))
	for i := range workers {
		worker := workers[i]
		if providerWorkerActiveForStatus(worker) {
			active = append(active, worker)
		}
	}
	if len(active) > 0 {
		return active
	}
	return workers
}

func providerWorkerActiveForStatus(worker model.Worker) bool {
	if worker.RevokedAt != nil {
		return false
	}
	if worker.Phase == model.WorkerPhaseFailed || worker.LastOperationStatus == model.OperationStatusFailed {
		return false
	}
	switch worker.DesiredState {
	case model.WorkerDesiredStateDeleted, model.WorkerDesiredStateDrained:
		return false
	default:
		return true
	}
}

func providerWorkerTerminalDeleted(worker model.Worker) bool {
	return worker.DesiredState == model.WorkerDesiredStateDeleted ||
		worker.Phase == model.WorkerPhaseDeleted
}

func providerWorkerHasError(worker model.Worker) bool {
	return worker.LastOperationStatus == model.OperationStatusFailed ||
		worker.ErrorMessage != nil ||
		providerWorkerCleanupError(worker)
}

func providerWorkerCleanupError(worker model.Worker) bool {
	return worker.DesiredState == model.WorkerDesiredStateDeleted &&
		worker.LastOperationStatus == model.OperationStatusPending &&
		worker.StatusMessage != nil
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

func (s *Service) UpdateSandboxProviderInstance(ctx context.Context, projectID, providerID string, input services.UpdateSandboxProviderInstanceBody) (*model.SandboxProviderInstance, error) {
	provider, err := s.store.GetSandboxProviderInstance(ctx, projectID, providerID)
	if err != nil {
		return nil, mapAPIError(err, "provider instance not found")
	}
	if name, ok := input.Name.Get(); ok {
		provider.Name = name
	}
	if len(input.Config) > 0 {
		config := services.RawMessage(input.Config)
		if err := s.sandboxes.SandboxProviderManager().ValidateProviderConfig(provider.Type, config); err != nil {
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
	return mapAPIError(s.store.DeleteSandboxProviderInstance(ctx, projectID, providerID), "provider instance not found")
}

// EnsureExistingSandboxProviderInstances schedules provider startup
// reconciliation for persisted provider instances.
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
			if s.jobs == nil {
				if err := s.ensureSandboxProviderInstance(ctx, project, provider); err != nil {
					return err
				}
				continue
			}
			if _, err := s.jobs.EnqueueProviderCurrent(ctx, project.ID, provider.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) EnqueueProviderWorkers(ctx context.Context, projectID, providerID string) error {
	if s.jobs == nil {
		return nil
	}
	workers, err := s.store.ListWorkers(ctx, projectID, providerID)
	if err != nil {
		return err
	}
	for i := range workers {
		if _, err := s.jobs.EnqueueWorkerCurrent(ctx, &workers[i]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) ensureSandboxProviderInstance(ctx context.Context, project *model.Project, provider *model.SandboxProviderInstance) error {
	if provider == nil || provider.Disabled {
		return nil
	}
	runtimeProvider, err := s.sandboxes.SandboxProviderManager().ResolveInstance(ctx, provider)
	if err != nil {
		return err
	}
	ensurer, ok := runtimeProvider.(sandbox.ProviderInstanceEnsurer)
	if !ok {
		return nil
	}
	providerManager := any(s.store)
	if s.workers != nil {
		providerManager = s.workers
	}
	return ensurer.EnsureProviderInstance(ctx, providerManager, project, provider)
}

func (s *Service) ReconcileProviderJob(ctx context.Context, projectID, providerID, _ string) error {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return err
	}
	provider, err := s.store.GetSandboxProviderInstance(ctx, projectID, providerID)
	if err != nil {
		return err
	}
	return s.ensureSandboxProviderInstance(ctx, project, provider)
}
