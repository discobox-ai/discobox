package sandboxes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/obot-platform/discobox/orchestration"
	"github.com/obot-platform/discobox/server/internal/apperrors"

	"github.com/obot-platform/discobox/id"
	"github.com/obot-platform/discobox/server/internal/auth"
	"github.com/obot-platform/discobox/server/internal/model"
	services "github.com/obot-platform/discobox/server/internal/services"

	sandboxauth "github.com/obot-platform/discobox/server/internal/auth/sandbox"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/store"
)

// Service owns sandbox API behavior, orchestration, provider catalog state, and
// sandbox auth dependencies.
type Service struct {
	store            *store.Store
	jobs             SandboxJobManager
	sandboxProviders *sandbox.ProviderManager
	providerStore    any
	sandboxAuth      *sandboxauth.Manager
	defaultUserID    string
	defaultImage     string
}

type SandboxJobManager interface {
	CreateSandbox(context.Context, *model.Sandbox) (*model.Sandbox, error)
	StartSandbox(context.Context, string, string) (*model.Sandbox, error)
	StopSandbox(context.Context, string, string) (*model.Sandbox, error)
	RestartSandbox(context.Context, string, string) (*model.Sandbox, error)
	DeleteSandbox(context.Context, string, string) (*model.Sandbox, error)
	SubmitSandboxReconcile(context.Context, string, string) (*model.Sandbox, error)
}

type JobRegistrar interface {
	Register(orchestration.Type, orchestration.Executor, ...orchestration.ExecutorOption) error
}

func NewService(store *store.Store, manager *sandbox.ProviderManager, defaultUserID string, jobs SandboxJobManager, providerStore ...any) *Service {
	svc := &Service{
		store:            store,
		jobs:             jobs,
		sandboxProviders: manager,
		defaultUserID:    defaultUserID,
		defaultImage:     sandbox.DefaultSandboxImageName,
	}
	if len(providerStore) > 0 {
		svc.providerStore = providerStore[0]
	}
	return svc
}

func (s *Service) RegisterJobs(registrar JobRegistrar, opts ...orchestration.ExecutorOption) error {
	return registrar.Register(
		SandboxReconcileType,
		NewSandboxReconcileExecutor(
			s.store,
			WithSandboxProviderManager(s.sandboxProviders),
			WithSandboxAuthenticator(s.sandboxAuth),
		),
		opts...,
	)
}

func (s *Service) SetSandboxAuthManager(manager *sandboxauth.Manager) {
	s.sandboxAuth = manager
}

func (s *Service) SetDefaultSandboxImage(image string) {
	if image = strings.TrimSpace(image); image != "" {
		s.defaultImage = image
	}
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
	Capabilities ProviderStatus
	ConfigFields []ProviderConfigField
}

func (s *Service) ListSandboxes(ctx context.Context, projectID string) ([]model.Sandbox, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, mapAPIError(err, "project not found")
	}
	return s.store.ListSandboxes(ctx, projectID)
}

func (s *Service) CreateSandbox(ctx context.Context, projectID string, input services.CreateSandboxBody) (*model.Sandbox, error) {
	config := input.Config
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, mapAPIError(err, "project not found")
	}
	providerID := services.OptStringPtr(input.ProviderInstanceId)
	if providerID == nil && project.DefaultSandboxProviderID != "" {
		id := project.DefaultSandboxProviderID
		providerID = &id
	}
	if providerID == nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "sandbox provider instance is required")
	}
	provider, err := s.store.GetSandboxProviderInstance(ctx, projectID, *providerID)
	if err != nil {
		return nil, mapAPIError(err, "provider instance not found")
	}
	if provider.Disabled {
		return nil, fmt.Errorf("provider instance disabled")
	}
	agentConfigID, err := s.resolveAgentConfigID(ctx, project, config.AgentConfigId, input.AgentName)
	if err != nil {
		return nil, err
	}

	if strings.TrimSpace(config.Name) == "" {
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
	if refs, ok := config.SourceCodeReferences.Get(); ok {
		sourceCodeReferences = services.SourceCodeReferencesToModel(refs)
	}
	var source *model.GitSource
	if inputSource, ok := config.Source.Get(); ok {
		converted := services.GitSourceToModel(inputSource)
		source = &converted
	}
	services.DefaultGitSourceSlugs(source, sourceCodeReferences)
	userName, userUID, userGID, homeDirectory := services.SandboxUserToModel(config.User)
	image := strings.TrimSpace(config.Image.Or(""))
	if image == "" {
		image = s.defaultImage
	}
	sandbox := &model.Sandbox{
		ID:                       sandboxID,
		ProjectID:                projectID,
		CreatedByUserID:          userID,
		ProviderInstanceID:       providerID,
		AgentConfigID:            agentConfigID,
		Name:                     config.Name,
		Description:              services.OptStringPtr(config.Description),
		ResourceLifecycle:        model.NewResourceLifecycle(model.SandboxCreateOperation, nil),
		AgentModel:               services.OptStringPtr(config.AgentModel),
		AgentModelServiceTier:    services.OptStringPtr(config.AgentModelServiceTier),
		AgentModelReasoningLevel: services.OptStringPtr(config.AgentModelReasoningLevel),
		Prompt:                   config.Prompt,
		Image:                    image,
		Env:                      map[string]string(config.Env.Or(nil)),
		Source:                   source,
		SourceCodeReferences:     sourceCodeReferences,
		UserName:                 userName,
		UserUID:                  userUID,
		UserGID:                  userGID,
		HomeDirectory:            homeDirectory,
		CPUVCPUs:                 config.CpuVcpus.Or(0),
		MemoryBytes:              config.MemoryBytes.Or(0),
		StorageBytes:             config.StorageBytes.Or(0),
	}
	if s.jobs == nil {
		return nil, fmt.Errorf("job manager is required")
	}
	assignments, err := s.prepareSandboxSecrets(ctx, projectID, sandbox, config.Secrets)
	if err != nil {
		return nil, err
	}
	created, err := s.jobs.CreateSandbox(ctx, sandbox)
	if err != nil {
		return nil, err
	}
	for _, assignment := range assignments {
		if err := s.store.CreateSandboxSecret(ctx, assignment); err != nil {
			return nil, fmt.Errorf("persist sandbox secret assignment: %w", err)
		}
	}
	return created, nil
}

func (s *Service) resolveAgentConfigID(ctx context.Context, project *model.Project, agentConfigID, agentName services.OptString) (*string, error) {
	if project == nil {
		return nil, fmt.Errorf("project is required")
	}
	if id, ok := agentConfigID.Get(); ok && id != "" {
		config, err := s.store.GetAgentConfig(ctx, project.ID, id)
		if err != nil {
			return nil, mapAPIError(err, "agent config not found")
		}
		return &config.ID, nil
	}
	name, ok := agentName.Get()
	if ok && strings.TrimSpace(name) != "" {
		selector := strings.TrimSpace(name)
		// Prefer the stable slug (e.g. "codex"), then fall back to the display name.
		if config, err := s.store.GetAgentConfigBySlug(ctx, project.ID, selector); err == nil {
			return &config.ID, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, mapAPIError(err, "agent config not found")
		}
		config, err := s.store.GetAgentConfigByName(ctx, project.ID, selector)
		if err != nil {
			return nil, mapAPIError(err, "agent config not found")
		}
		return &config.ID, nil
	}
	return nil, nil
}

func (s *Service) GetSandbox(ctx context.Context, projectID, sandboxID string) (*model.Sandbox, error) {
	sandbox, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, mapAPIError(err, "sandbox not found")
	}
	return sandbox, nil
}

func (s *Service) AcquireSandboxHTTPClient(ctx context.Context, projectID, sandboxID string, scopes []string) (*services.HTTPClientLease, *model.Sandbox, error) {
	if err := authorizeRequestedScopes(ctx, scopes); err != nil {
		return nil, nil, err
	}
	sandboxModel, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, nil, mapAPIError(err, "sandbox not found")
	}
	if sandboxModel.Phase != model.SandboxPhaseRunning {
		return nil, sandboxModel, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf("sandbox is not running: phase=%s", sandboxModel.Phase))
	}
	if sandboxModel.WorkerID == nil || strings.TrimSpace(*sandboxModel.WorkerID) == "" {
		return nil, sandboxModel, apperrors.NewStatusError(http.StatusConflict, "sandbox worker is not assigned")
	}
	worker, err := s.store.GetWorker(ctx, strings.TrimSpace(*sandboxModel.WorkerID))
	if err != nil {
		return nil, sandboxModel, mapAPIError(err, "sandbox worker not found")
	}
	if worker.Phase != model.WorkerPhaseActive || !worker.Ready {
		return nil, sandboxModel, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf("sandbox worker is not active: worker=%s phase=%s ready=%t", worker.ID, worker.Phase, worker.Ready))
	}
	if s.sandboxProviders == nil {
		return nil, nil, fmt.Errorf("sandbox provider manager is required")
	}
	if sandboxModel.ProviderInstanceID != nil && strings.TrimSpace(*sandboxModel.ProviderInstanceID) != "" {
		provider, err := s.store.GetSandboxProviderInstance(ctx, projectID, *sandboxModel.ProviderInstanceID)
		if err != nil {
			return nil, nil, mapAPIError(err, "provider instance not found")
		}
		sandboxModel.ProviderInstance = provider
	}
	provider, err := s.sandboxProviders.ResolveForSandbox(ctx, sandboxModel)
	if err != nil {
		return nil, nil, err
	}
	lease, err := provider.AcquireHTTPClient(ctx, sandbox.SandboxRef{ProjectID: sandboxModel.ProjectID, SandboxID: sandboxModel.ID}, sandboxModel.RuntimeState, scopes)
	if err != nil {
		return nil, nil, err
	}
	return lease, sandboxModel, nil
}

func authorizeRequestedScopes(ctx context.Context, scopes []string) error {
	if len(scopes) == 0 {
		return nil
	}
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok || principal.Type != auth.PrincipalTypeUser || principal.UserID == "" {
		return apperrors.NewStatusError(http.StatusForbidden, "user access required")
	}
	for _, scope := range scopes {
		if !principal.HasScope(scope) {
			return apperrors.NewStatusError(http.StatusForbidden, "user scope required: "+scope)
		}
	}
	return nil
}

func (s *Service) UpdateSandbox(ctx context.Context, projectID, sandboxID string, input services.UpdateSandboxBody) (*model.Sandbox, error) {
	sandbox, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, mapAPIError(err, "sandbox not found")
	}

	if config, ok := input.Config.Get(); ok {
		if name, ok := config.Name.Get(); ok {
			sandbox.Name = name
		}
	}

	if err := s.store.UpdateSandbox(ctx, sandbox); err != nil {
		return nil, err
	}
	return s.store.GetSandbox(ctx, projectID, sandboxID)
}

func (s *Service) DeleteSandbox(ctx context.Context, projectID, sandboxID string) error {
	if s.jobs == nil {
		return fmt.Errorf("job manager is required")
	}
	_, err := s.jobs.DeleteSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return mapAPIError(err, "sandbox not found")
	}
	return nil
}

func (s *Service) StartSandbox(ctx context.Context, projectID, sandboxID string, _ services.StartSandboxBody) (*model.Sandbox, error) {
	if s.jobs == nil {
		return nil, fmt.Errorf("job manager is required")
	}
	sandbox, err := s.jobs.StartSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, mapAPIError(err, "sandbox not found")
	}
	return sandbox, nil
}

func (s *Service) StopSandbox(ctx context.Context, projectID, sandboxID string, _ services.StopSandboxBody) (*model.Sandbox, error) {
	if s.jobs == nil {
		return nil, fmt.Errorf("job manager is required")
	}
	sandbox, err := s.jobs.StopSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, mapAPIError(err, "sandbox not found")
	}
	return sandbox, nil
}

func (s *Service) RestartSandbox(ctx context.Context, projectID, sandboxID string, _ services.RestartSandboxBody) (*model.Sandbox, error) {
	if s.jobs == nil {
		return nil, fmt.Errorf("job manager is required")
	}
	sandbox, err := s.jobs.RestartSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, mapAPIError(err, "sandbox not found")
	}
	return sandbox, nil
}

func (s *Service) ReconcileSandbox(ctx context.Context, projectID, sandboxID string) (*model.Sandbox, error) {
	if s.jobs == nil {
		return nil, fmt.Errorf("job manager is required")
	}
	sandbox, err := s.jobs.SubmitSandboxReconcile(ctx, projectID, sandboxID)
	if err != nil {
		return nil, mapAPIError(err, "sandbox not found")
	}
	return sandbox, nil
}

// RegisterSandboxProvider registers a runtime sandbox provider.
func (s *Service) RegisterSandboxProvider(id string, provider sandbox.Provider) {
	if s.sandboxProviders != nil {
		s.sandboxProviders.RegisterProvider(id, provider)
	}
}

// SandboxProviderManager returns the service-owned provider manager.
func (s *Service) SandboxProviderManager() *sandbox.ProviderManager {
	return s.sandboxProviders
}

// NewSandboxReconcileExecutor returns a provider-manager-backed sandbox reconcile executor.
func (s *Service) NewSandboxReconcileExecutor() *SandboxReconcileExecutor {
	return NewSandboxReconcileExecutor(
		s.store,
		WithSandboxProviderManager(s.sandboxProviders),
		WithSandboxAuthenticator(s.sandboxAuth),
	)
}

// RegisterSandboxProviderDefinition registers provider metadata without an implementation.
func (s *Service) RegisterSandboxProviderDefinition(id string, definition ProviderDefinition) {
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
func (s *Service) ListSandboxProviderStatuses() map[string]ProviderStatus {
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
