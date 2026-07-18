package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/obot-platform/discobox/server/internal/apperrors"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/resources/pools"
	sandboxesvc "github.com/obot-platform/discobox/server/internal/resources/sandboxes"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	services "github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
)

type Service struct {
	store     *store.Store
	sandboxes SandboxCatalogService
	pools     *pools.ControlPlane
}

type SandboxCatalogService interface {
	ListSandboxProviderCatalog() []SandboxProviderCatalogItem
	SandboxProviderManager() *sandbox.ProviderManager
}

type SandboxProviderCatalogItem = sandboxesvc.SandboxProviderCatalogItem

func NewService(store *store.Store, sandboxes SandboxCatalogService, poolManager *pools.ControlPlane) *Service {
	return &Service{store: store, sandboxes: sandboxes, pools: poolManager}
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
		Available: status.Available,
		State:     status.State,
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
	return s.store.ListSandboxProviderInstances(ctx, projectID)
}

func (s *Service) CreateSandboxProviderInstance(ctx context.Context, projectID string, input services.CreateSandboxProviderInstanceBody) (*model.SandboxProviderInstance, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
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
	if _, err := s.sandboxes.SandboxProviderManager().ResolveInstance(ctx, provider); err != nil {
		return nil, err
	}
	return s.store.GetSandboxProviderInstance(ctx, projectID, provider.ID)
}

func (s *Service) GetSandboxProviderInstance(ctx context.Context, projectID, providerID string) (*model.SandboxProviderInstance, error) {
	provider, err := s.store.GetSandboxProviderInstance(ctx, projectID, providerID)
	if err != nil {
		return nil, mapAPIError(err, "provider instance not found")
	}
	return provider, nil
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
	// Pools bind to a provider instance immutably, so a provider instance with
	// pools cannot be deleted; workers and sandboxes hang off the pools.
	pools, err := s.store.ListPoolsForProviderInstance(ctx, projectID, providerID)
	if err != nil {
		return err
	}
	if len(pools) > 0 {
		return apperrors.NewStatusError(http.StatusConflict, "provider instance has pools")
	}
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
			if _, err := s.sandboxes.SandboxProviderManager().ResolveInstance(ctx, provider); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) EnqueueProviderPools(ctx context.Context, projectID, providerID string) error {
	poolRows, err := s.store.ListPoolsForProviderInstance(ctx, projectID, providerID)
	if err != nil {
		return err
	}
	for i := range poolRows {
		if err := s.pools.SchedulePoolReconciliation(ctx, projectID, poolRows[i].ID); err != nil {
			return err
		}
	}
	return nil
}
