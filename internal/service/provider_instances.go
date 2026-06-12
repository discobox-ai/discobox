package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/obot-platform/discobox/internal/api"
	"github.com/obot-platform/discobox/internal/model"
	"github.com/obot-platform/discobox/internal/sandbox"
)

func (s *Service) ListSandboxProviderCatalogItems(context.Context) ([]api.SandboxProviderCatalogItem, error) {
	items := s.ListSandboxProviderCatalog()
	out := make([]api.SandboxProviderCatalogItem, 0, len(items))
	for _, item := range items {
		out = append(out, api.SandboxProviderCatalogItem{
			ID:           item.ID,
			Name:         item.Name,
			Icon:         item.Icon,
			Description:  item.Description,
			Available:    item.Available,
			BuiltIn:      item.BuiltIn,
			Capabilities: item.Capabilities,
			ConfigFields: item.ConfigFields,
		})
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
	if strings.TrimSpace(input.Name) == "" {
		return nil, apiError(fmt.Errorf("name is required"), "")
	}
	if strings.TrimSpace(input.Type) == "" {
		return nil, apiError(fmt.Errorf("type is required"), "")
	}
	if err := s.SandboxProviderManager().ValidateProviderConfig(input.Type, input.Config); err != nil {
		return nil, err
	}
	provider := &model.SandboxProviderInstance{ProjectID: projectID, Type: input.Type, Name: input.Name, Config: input.Config}
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
	return provider, nil
}

func (s *Service) UpdateSandboxProviderInstance(ctx context.Context, projectID, providerID string, input api.UpdateSandboxProviderInstanceBody) (*model.SandboxProviderInstance, error) {
	provider, err := s.store.GetSandboxProviderInstance(ctx, projectID, providerID)
	if err != nil {
		return nil, apiError(err, "provider instance not found")
	}
	if input.Name != nil {
		provider.Name = *input.Name
	}
	if input.Config != nil {
		if err := s.SandboxProviderManager().ValidateProviderConfig(provider.Type, input.Config); err != nil {
			return nil, err
		}
		provider.Config = input.Config
	}
	if input.Disabled != nil {
		provider.Disabled = *input.Disabled
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
