package sandboxservice

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/obot-platform/discobox/apperrors"

	"github.com/obot-platform/discobox/id"
	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/server/internal/api"
	"github.com/obot-platform/discobox/server/internal/auth"

	sandboxprovider "github.com/obot-platform/discobox/sandboxprovider"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/sandbox/jobs"
	"github.com/obot-platform/discobox/server/internal/sandboxauth"
	"github.com/obot-platform/discobox/server/internal/store"
)

// Service owns sandbox API behavior, orchestration, provider catalog state, and
// sandbox auth dependencies.
type Service struct {
	store            *store.Store
	sandboxes        *jobs.SandboxSubmitter
	sandboxProviders *sandboxprovider.ProviderManager
	providerStore    any
	sandboxAuth      *sandboxauth.Manager
	defaultUserID    string
}

func NewService(store *store.Store, sandboxes *jobs.SandboxSubmitter, manager *sandboxprovider.ProviderManager, defaultUserID string, providerStore ...any) *Service {
	svc := &Service{
		store:            store,
		sandboxes:        sandboxes,
		sandboxProviders: manager,
		defaultUserID:    defaultUserID,
	}
	if len(providerStore) > 0 {
		svc.providerStore = providerStore[0]
	}
	return svc
}

func (s *Service) SetSandboxAuthManager(manager *sandboxauth.Manager) {
	s.sandboxAuth = manager
}

func mapAPIError(err error, notFoundMessage string) error {
	if errors.Is(err, store.ErrNotFound) {
		return apperrors.NewStatusError(http.StatusNotFound, notFoundMessage)
	}
	return err
}

// SandboxProviderCatalogItem describes a registered sandbox provider type.
type SandboxProviderCatalogItem struct {
	ID           string
	Name         string
	Icon         string
	Description  string
	Available    bool
	BuiltIn      bool
	Capabilities sandbox.ProviderStatus
	ConfigFields []sandbox.ProviderConfigField
}

func (s *Service) ListSandboxes(ctx context.Context, projectID string) ([]model.Sandbox, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, mapAPIError(err, "project not found")
	}
	return s.store.ListSandboxes(ctx, projectID)
}

func (s *Service) CreateSandbox(ctx context.Context, projectID string, input api.CreateSandboxBody) (*model.Sandbox, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, mapAPIError(err, "project not found")
	}
	providerID := api.OptStringPtr(input.ProviderInstanceId)
	if providerID == nil && project.DefaultSandboxProviderID != "" {
		id := project.DefaultSandboxProviderID
		providerID = &id
	}
	if providerID != nil {
		provider, err := s.store.GetSandboxProviderInstance(ctx, projectID, *providerID)
		if err != nil {
			return nil, mapAPIError(err, "provider instance not found")
		}
		if provider.Disabled {
			return nil, fmt.Errorf("provider instance disabled")
		}
	}
	agentConfigID, err := s.resolveAgentConfigID(ctx, projectID, input.AgentConfigId, input.AgentName)
	if err != nil {
		return nil, err
	}

	if strings.TrimSpace(input.Name) == "" {
		return nil, fmt.Errorf("sandbox name is required")
	}
	userID := s.defaultUserID
	if authenticatedUserID, err := auth.UserID(ctx); err == nil {
		userID = authenticatedUserID
	}
	sandboxID, err := id.New()
	if err != nil {
		return nil, err
	}
	sourceCodeReferences := model.SourceCodeReferences(nil)
	if refs, ok := input.SourceCodeReferences.Get(); ok {
		var err error
		sourceCodeReferences, err = api.Convert[model.SourceCodeReferences](refs)
		if err != nil {
			return nil, err
		}
	}
	sandbox := &model.Sandbox{
		ID:                       sandboxID,
		ProjectID:                projectID,
		CreatedByUserID:          userID,
		ProviderInstanceID:       providerID,
		AgentConfigID:            agentConfigID,
		Name:                     input.Name,
		Description:              api.OptStringPtr(input.Description),
		ResourceLifecycle:        model.NewResourceLifecycle(model.SandboxCreateOperation, nil),
		AgentModel:               api.OptStringPtr(input.AgentModel),
		AgentModelServiceTier:    api.OptStringPtr(input.AgentModelServiceTier),
		AgentModelReasoningLevel: api.OptStringPtr(input.AgentModelReasoningLevel),
		Prompt:                   api.OptStringPtr(input.Prompt),
		SourceURL:                api.OptURIStringPtr(input.SourceUrl),
		SourceRef:                api.OptStringPtr(input.SourceRef),
		SourceRefType:            api.OptStringPtr(input.SourceRefType),
		SourceDirectory:          api.OptStringPtr(input.SourceDirectory),
		WorkingDirectory:         api.OptStringPtr(input.WorkingDirectory),
		SourceCodeReferences:     sourceCodeReferences,
		UserUID:                  api.OptIntPtr(input.UserUid),
		UserGID:                  api.OptIntPtr(input.UserGid),
		CPUVCPUs:                 input.CpuVcpus.Or(0),
		MemoryBytes:              input.MemoryBytes.Or(0),
		StorageBytes:             input.StorageBytes.Or(0),
		RuntimeState:             api.RawMessage(input.RuntimeState),
	}
	return s.sandboxes.Create(ctx, sandbox)
}

func (s *Service) resolveAgentConfigID(ctx context.Context, projectID string, agentConfigID, agentName api.OptString) (*string, error) {
	if id, ok := agentConfigID.Get(); ok && id != "" {
		config, err := s.store.GetAgentConfig(ctx, projectID, id)
		if err != nil {
			return nil, mapAPIError(err, "agent config not found")
		}
		return &config.ID, nil
	}
	name, ok := agentName.Get()
	if !ok || strings.TrimSpace(name) == "" {
		return nil, nil
	}
	config, err := s.store.GetAgentConfigByName(ctx, projectID, strings.TrimSpace(name))
	if err != nil {
		return nil, mapAPIError(err, "agent config not found")
	}
	return &config.ID, nil
}

func (s *Service) GetSandbox(ctx context.Context, projectID, sandboxID string) (*model.Sandbox, error) {
	sandbox, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, mapAPIError(err, "sandbox not found")
	}
	return sandbox, nil
}

func (s *Service) UpdateSandbox(ctx context.Context, projectID, sandboxID string, input api.UpdateSandboxBody) (*model.Sandbox, error) {
	sandbox, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, mapAPIError(err, "sandbox not found")
	}

	if name, ok := input.Name.Get(); ok {
		sandbox.Name = name
	}

	if err := s.store.UpdateSandbox(ctx, sandbox); err != nil {
		return nil, err
	}
	return s.store.GetSandbox(ctx, projectID, sandboxID)
}

func (s *Service) DeleteSandbox(ctx context.Context, projectID, sandboxID string) error {
	_, err := s.beginSandboxOperation(ctx, projectID, sandboxID, model.SandboxDeleteOperation)
	return err
}

func (s *Service) StartSandbox(ctx context.Context, projectID, sandboxID string, _ api.StartSandboxBody) (*model.Sandbox, error) {
	return s.beginSandboxOperation(ctx, projectID, sandboxID, model.SandboxStartOperation)
}

func (s *Service) StopSandbox(ctx context.Context, projectID, sandboxID string, _ api.StopSandboxBody) (*model.Sandbox, error) {
	return s.beginSandboxOperation(ctx, projectID, sandboxID, model.SandboxStopOperation)
}

func (s *Service) RestartSandbox(ctx context.Context, projectID, sandboxID string, _ api.RestartSandboxBody) (*model.Sandbox, error) {
	return s.beginSandboxOperation(ctx, projectID, sandboxID, model.SandboxRestartOperation, func(sandbox *model.Sandbox) {
		sandbox.RestartGeneration++
	})
}

func (s *Service) beginSandboxOperation(ctx context.Context, projectID, sandboxID string, spec model.OperationSpec, mutate ...func(*model.Sandbox)) (*model.Sandbox, error) {
	sandbox, err := s.sandboxes.Submit(ctx, projectID, sandboxID, spec, mutate...)
	if err != nil {
		return nil, mapAPIError(err, "sandbox not found")
	}
	return sandbox, nil
}

// RegisterSandboxProvider registers a runtime sandbox provider.
func (s *Service) RegisterSandboxProvider(id string, provider sandboxprovider.Provider) {
	if s.sandboxProviders != nil {
		s.sandboxProviders.RegisterProvider(id, provider)
	}
}

// SandboxProviderManager returns the service-owned provider manager.
func (s *Service) SandboxProviderManager() *sandboxprovider.ProviderManager {
	return s.sandboxProviders
}

// NewSandboxReconciler returns a provider-manager-backed sandbox reconciler.
func (s *Service) NewSandboxReconciler() *sandbox.SandboxReconciler {
	return sandbox.NewSandboxReconciler(
		s.store,
		sandbox.WithSandboxProviderManager(s.sandboxProviders),
		sandbox.WithSandboxAuthenticator(s.sandboxAuth),
	)
}

// NewWorkerReconciler returns a provider-manager-backed worker reconciler.
func (s *Service) NewWorkerReconciler() *sandbox.WorkerReconciler {
	return sandbox.NewWorkerReconciler(
		s.store,
		sandbox.WithWorkerProviderManager(s.sandboxProviders),
	)
}

// RegisterSandboxProviderDefinition registers provider metadata without an implementation.
func (s *Service) RegisterSandboxProviderDefinition(id string, definition sandbox.ProviderDefinition) {
	if s.sandboxProviders != nil {
		s.sandboxProviders.RegisterProviderDefinition(id, definition)
	}
}

// ListSandboxProviderNames returns registered runtime provider IDs.
func (s *Service) ListSandboxProviderNames() []string {
	if s.sandboxProviders == nil {
		return nil
	}
	return s.sandboxProviders.ListProviders()
}

// DefaultSandboxProviderName returns the active process default provider ID.
func (s *Service) DefaultSandboxProviderName() string {
	if s.sandboxProviders == nil {
		return ""
	}
	s.sandboxProviders.EnsureDefaultAvailable()
	return s.sandboxProviders.DefaultProviderName()
}

// ListSandboxProviderStatuses returns statuses for registered runtime providers.
func (s *Service) ListSandboxProviderStatuses() map[string]sandbox.ProviderStatus {
	if s.sandboxProviders == nil {
		return nil
	}
	return s.sandboxProviders.ListProviderStatuses()
}

// ListSandboxProviderCatalog returns registered provider definitions with status.
func (s *Service) ListSandboxProviderCatalog() []SandboxProviderCatalogItem {
	if s.sandboxProviders == nil {
		return nil
	}
	statuses := s.sandboxProviders.ListProviderStatuses()
	definitions := s.sandboxProviders.ListProviderDefinitions()
	out := make([]SandboxProviderCatalogItem, 0, len(definitions))
	for id, definition := range definitions {
		status, registered := statuses[id]
		name := definition.Name
		if name == "" {
			name = id
		}
		description := definition.Description
		if description == "" {
			description = "Built-in " + name + " sandbox driver"
		}
		out = append(out, SandboxProviderCatalogItem{
			ID:           id,
			Name:         name,
			Icon:         definition.Icon,
			Description:  description,
			Available:    !registered || status.Available,
			BuiltIn:      registered,
			Capabilities: status,
			ConfigFields: definition.ConfigFields,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Service) CreateSandboxAuthToken(ctx context.Context, projectID, sandboxID string) (string, error) {
	if s == nil || s.sandboxAuth == nil {
		return "", nil
	}
	sb, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return "", mapAPIError(err, "sandbox not found")
	}
	return s.sandboxAuth.CreateToken(ctx, sandboxauth.TokenClaims{
		ProjectID: sb.ProjectID,
		SandboxID: sb.ID,
		UserID:    sb.CreatedByUserID,
	})
}
