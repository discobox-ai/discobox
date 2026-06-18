// Package service contains application services.
package service

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/obot-platform/discobox/apperrors"

	"github.com/obot-platform/discobox/id"
	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/server/internal/api"
)

const (
	defaultUserID    = "00000000000000000000000001"
	defaultProjectID = "00000000000000000000000002"
)

// Stub is an in-memory implementation used while the real store/provider
// layers are being designed.
type Stub struct {
	mu           sync.Mutex
	user         model.User
	project      model.Project
	agentConfigs map[string]model.AgentConfig
	providers    map[string]model.SandboxProviderInstance
	sandboxes    map[string]model.Sandbox
}

func NewStub() *Stub {
	now := time.Now().UTC()
	user := model.User{
		ID:        defaultUserID,
		Email:     "local@example.com",
		Provider:  "local",
		Subject:   "local",
		CreatedAt: now,
		UpdatedAt: now,
	}
	project := model.Project{
		ID:          defaultProjectID,
		OwnerUserID: user.ID,
		Name:        "Default Project",
		Slug:        "default",
		Owner:       &user,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	return &Stub{
		user:         user,
		project:      project,
		agentConfigs: make(map[string]model.AgentConfig),
		providers:    make(map[string]model.SandboxProviderInstance),
		sandboxes:    make(map[string]model.Sandbox),
	}
}

func (s *Stub) ListProjects(context.Context) ([]model.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	project := s.projectWithSandboxes()
	return []model.Project{project}, nil
}

func (s *Stub) GetProject(_ context.Context, projectID string) (*model.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	project := s.projectWithSandboxes()
	return &project, nil
}

func (s *Stub) ListJobs(_ context.Context, projectID string) ([]model.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	return nil, nil
}

func (s *Stub) GetJob(_ context.Context, projectID, _ string) (*model.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	return nil, apperrors.NewStatusError(http.StatusNotFound, "job not found")
}

func (s *Stub) ListSandboxes(_ context.Context, projectID string) ([]model.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	return s.sortedSandboxes(), nil
}

func (s *Stub) CreateSandbox(_ context.Context, projectID string, input api.CreateSandboxBody) (*model.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	agentConfigID, err := s.resolveAgentConfigID(input.AgentConfigId, input.AgentName)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	sandbox := model.Sandbox{
		ID:                       id.NewString(),
		ProjectID:                s.project.ID,
		CreatedByUserID:          s.user.ID,
		ProviderInstanceID:       api.OptStringPtr(input.ProviderInstanceId),
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
		SourceCodeReferences:     stubSourceCodeReferences(input.SourceCodeReferences),
		UserUID:                  api.OptIntPtr(input.UserUid),
		UserGID:                  api.OptIntPtr(input.UserGid),
		CreatedAt:                now,
		UpdatedAt:                now,
		CreatedBy:                &s.user,
	}
	s.sandboxes[sandbox.ID] = sandbox
	return &sandbox, nil
}

func (s *Stub) GetSandbox(_ context.Context, projectID, sandboxID string) (*model.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sandbox, err := s.getSandbox(projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	return &sandbox, nil
}

func (s *Stub) UpdateSandbox(_ context.Context, projectID, sandboxID string, input api.UpdateSandboxBody) (*model.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sandbox, err := s.getSandbox(projectID, sandboxID)
	if err != nil {
		return nil, err
	}

	if name, ok := input.Name.Get(); ok {
		sandbox.Name = name
	}
	sandbox.UpdatedAt = time.Now().UTC()
	s.sandboxes[sandbox.ID] = sandbox
	return &sandbox, nil
}

func (s *Stub) DeleteSandbox(_ context.Context, projectID, sandboxID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.getSandbox(projectID, sandboxID); err != nil {
		return err
	}
	delete(s.sandboxes, sandboxID)
	return nil
}

func (s *Stub) StartSandbox(_ context.Context, projectID, sandboxID string, _ api.StartSandboxBody) (*model.Sandbox, error) {
	return s.beginSandboxOperation(projectID, sandboxID, model.SandboxStartOperation)
}

func (s *Stub) StopSandbox(_ context.Context, projectID, sandboxID string, _ api.StopSandboxBody) (*model.Sandbox, error) {
	return s.beginSandboxOperation(projectID, sandboxID, model.SandboxStopOperation)
}

func (s *Stub) RestartSandbox(_ context.Context, projectID, sandboxID string, _ api.RestartSandboxBody) (*model.Sandbox, error) {
	return s.beginSandboxOperation(projectID, sandboxID, model.SandboxRestartOperation)
}

func (s *Stub) MaxProjectEventSeq(_ context.Context, projectID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if projectID != s.project.ID {
		return 0, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	return 0, nil
}

func (s *Stub) ListProjectEventsAfterSeq(_ context.Context, projectID string, _ int64, _ []string) ([]model.ProjectEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	return []model.ProjectEvent{}, nil
}

func (s *Stub) ListProjectResourceSnapshots(_ context.Context, projectID string, _ []string, _ int64) ([]model.ProjectEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	return []model.ProjectEvent{}, nil
}

func (s *Stub) SubscribeProjectEvents(ctx context.Context, projectID string) (<-chan model.ProjectEvent, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if projectID != s.project.ID {
		return nil, nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	ch := make(chan model.ProjectEvent)
	unsubscribe := func() {
		select {
		case <-ctx.Done():
		default:
			close(ch)
		}
	}
	return ch, unsubscribe, nil
}

func (s *Stub) ListAgentConfigDefinitions(context.Context) ([]model.AgentConfigDefinition, error) {
	return cloneAgentConfigDefinitions(agentConfigDefinitions), nil
}

func (s *Stub) GetAgentConfigDefinition(_ context.Context, definitionID string) (*model.AgentConfigDefinition, error) {
	definition, ok := agentConfigDefinitionByID(definitionID)
	if !ok {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "agent config definition not found")
	}
	return definition, nil
}

func (s *Stub) ListAgentConfigs(_ context.Context, projectID string) ([]model.AgentConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	configs := make([]model.AgentConfig, 0, len(s.agentConfigs))
	for _, config := range s.agentConfigs {
		configs = append(configs, config)
	}
	slices.SortFunc(configs, func(a, b model.AgentConfig) int {
		if a.CreatedAt.Before(b.CreatedAt) {
			return -1
		}
		if a.CreatedAt.After(b.CreatedAt) {
			return 1
		}
		return 0
	})
	return configs, nil
}

func (s *Stub) CreateAgentConfig(_ context.Context, projectID string, input api.CreateAgentConfigBody) (*model.AgentConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	var definition *model.AgentConfigDefinition
	if definitionID, isSet := input.DefinitionId.Get(); isSet {
		var found bool
		definition, found = agentConfigDefinitionByID(definitionID)
		if !found {
			return nil, apperrors.NewStatusError(http.StatusNotFound, "agent config definition not found")
		}
	}
	name := strings.TrimSpace(input.Name.Or(""))
	if name == "" && definition != nil {
		name = definition.Name
	}
	installCommand := input.InstallCommand.Or("")
	if installCommand == "" && definition != nil {
		installCommand = definition.InstallCommand
	}
	runCommand := input.RunCommand.Or("")
	if strings.TrimSpace(runCommand) == "" && definition != nil {
		runCommand = definition.RunCommand
	}
	capabilities := api.RawMessage(input.Capabilities)
	if capabilities == nil && definition != nil {
		capabilities = cloneRawMessage(definition.Capabilities)
	}
	now := time.Now().UTC()
	config := model.AgentConfig{ID: id.NewString(), ProjectID: projectID, Name: name, InstallCommand: installCommand, RunCommand: runCommand, Capabilities: capabilities, CreatedAt: now, UpdatedAt: now}
	s.agentConfigs[config.ID] = config
	return &config, nil
}

func (s *Stub) GetAgentConfig(_ context.Context, projectID, configID string) (*model.AgentConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	config, ok := s.agentConfigs[configID]
	if !ok {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "agent config not found")
	}
	return &config, nil
}

func (s *Stub) UpdateAgentConfig(_ context.Context, projectID, configID string, input api.UpdateAgentConfigBody) (*model.AgentConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	config, ok := s.agentConfigs[configID]
	if !ok {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "agent config not found")
	}
	if name, ok := input.Name.Get(); ok {
		config.Name = name
	}
	if installCommand, ok := input.InstallCommand.Get(); ok {
		config.InstallCommand = installCommand
	}
	if runCommand, ok := input.RunCommand.Get(); ok {
		config.RunCommand = runCommand
	}
	if len(input.Capabilities) > 0 {
		config.Capabilities = api.RawMessage(input.Capabilities)
	}
	config.UpdatedAt = time.Now().UTC()
	s.agentConfigs[config.ID] = config
	return &config, nil
}

func (s *Stub) DeleteAgentConfig(_ context.Context, projectID, configID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	if _, ok := s.agentConfigs[configID]; !ok {
		return apperrors.NewStatusError(http.StatusNotFound, "agent config not found")
	}
	delete(s.agentConfigs, configID)
	return nil
}

func (s *Stub) ListSandboxProviderCatalogItems(context.Context) ([]api.SandboxProviderCatalogItem, error) {
	return []api.SandboxProviderCatalogItem{{ID: "digitalocean", Name: "DigitalOcean", Available: true, BuiltIn: true}}, nil
}

func (s *Stub) ListSandboxProviderInstances(_ context.Context, projectID string) ([]model.SandboxProviderInstance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	return s.sortedProviders(), nil
}

func (s *Stub) CreateSandboxProviderInstance(_ context.Context, projectID string, input api.CreateSandboxProviderInstanceBody) (*model.SandboxProviderInstance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	now := time.Now().UTC()
	provider := model.SandboxProviderInstance{ID: id.NewString(), ProjectID: projectID, Type: input.Type, Name: input.Name, Config: api.RawMessage(input.Config), CreatedAt: now, UpdatedAt: now}
	s.providers[provider.ID] = provider
	if s.project.DefaultSandboxProviderID == "" {
		s.project.DefaultSandboxProviderID = provider.ID
	}
	return &provider, nil
}

func (s *Stub) GetSandboxProviderInstance(_ context.Context, projectID, providerID string) (*model.SandboxProviderInstance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	provider, ok := s.providers[providerID]
	if !ok {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "provider instance not found")
	}
	return &provider, nil
}

func (s *Stub) UpdateSandboxProviderInstance(_ context.Context, projectID, providerID string, input api.UpdateSandboxProviderInstanceBody) (*model.SandboxProviderInstance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	provider, ok := s.providers[providerID]
	if !ok {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "provider instance not found")
	}
	if name, ok := input.Name.Get(); ok {
		provider.Name = name
	}
	if len(input.Config) > 0 {
		provider.Config = api.RawMessage(input.Config)
	}
	if disabled, ok := input.Disabled.Get(); ok {
		provider.Disabled = disabled
	}
	provider.UpdatedAt = time.Now().UTC()
	s.providers[provider.ID] = provider
	return &provider, nil
}

func (s *Stub) DeleteSandboxProviderInstance(_ context.Context, projectID, providerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	if _, ok := s.providers[providerID]; !ok {
		return apperrors.NewStatusError(http.StatusNotFound, "provider instance not found")
	}
	delete(s.providers, providerID)
	return nil
}

func (s *Stub) RegisterWorker(context.Context, api.RegisterWorkerBody) (*api.RegisterWorkerResponseBody, error) {
	return &api.RegisterWorkerResponseBody{AuthToken: "stub"}, nil
}

func (s *Stub) UpdateWorkerStatus(context.Context, string, api.UpdateWorkerStatusBody) (*model.Worker, error) {
	return &model.Worker{}, nil
}

func (s *Stub) beginSandboxOperation(projectID, sandboxID string, spec model.OperationSpec) (*model.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sandbox, err := s.getSandbox(projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	sandbox.BeginOperation(spec, nil)
	sandbox.UpdatedAt = time.Now().UTC()
	s.sandboxes[sandbox.ID] = sandbox
	return &sandbox, nil
}

func (s *Stub) getSandbox(projectID, sandboxID string) (model.Sandbox, error) {
	if projectID != s.project.ID {
		return model.Sandbox{}, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	sandbox, ok := s.sandboxes[sandboxID]
	if !ok {
		return model.Sandbox{}, apperrors.NewStatusError(http.StatusNotFound, "sandbox not found")
	}
	sandbox.CreatedBy = &s.user
	return sandbox, nil
}

func (s *Stub) resolveAgentConfigID(agentConfigID, agentName api.OptString) (*string, error) {
	if id, ok := agentConfigID.Get(); ok {
		if _, ok := s.agentConfigs[id]; !ok {
			return nil, apperrors.NewStatusError(http.StatusNotFound, "agent config not found")
		}
		return &id, nil
	}
	name, ok := agentName.Get()
	if !ok {
		return nil, nil
	}
	for _, config := range s.agentConfigs {
		if config.Name == name {
			id := config.ID
			return &id, nil
		}
	}
	return nil, apperrors.NewStatusError(http.StatusNotFound, "agent config not found")
}

func stubSourceCodeReferences(input api.OptCreateSandboxBodySourceCodeReferences) model.SourceCodeReferences {
	refs, ok := input.Get()
	if !ok {
		return nil
	}
	out, err := api.Convert[model.SourceCodeReferences](refs)
	if err != nil {
		return nil
	}
	return out
}

func (s *Stub) projectWithSandboxes() model.Project {
	project := s.project
	project.Owner = &s.user
	project.AgentConfigs = make([]model.AgentConfig, 0, len(s.agentConfigs))
	for _, config := range s.agentConfigs {
		project.AgentConfigs = append(project.AgentConfigs, config)
	}
	project.SandboxProviderInstances = s.sortedProviders()
	project.Sandboxes = s.sortedSandboxes()
	return project
}

func (s *Stub) sortedProviders() []model.SandboxProviderInstance {
	providers := make([]model.SandboxProviderInstance, 0, len(s.providers))
	for _, provider := range s.providers {
		providers = append(providers, provider)
	}
	slices.SortFunc(providers, func(a, b model.SandboxProviderInstance) int {
		if a.CreatedAt.Before(b.CreatedAt) {
			return -1
		}
		if a.CreatedAt.After(b.CreatedAt) {
			return 1
		}
		return 0
	})
	return providers
}

func (s *Stub) sortedSandboxes() []model.Sandbox {
	sandboxes := make([]model.Sandbox, 0, len(s.sandboxes))
	for _, sandbox := range s.sandboxes {
		sandbox.CreatedBy = &s.user
		sandboxes = append(sandboxes, sandbox)
	}
	slices.SortFunc(sandboxes, func(a, b model.Sandbox) int {
		if a.CreatedAt.Before(b.CreatedAt) {
			return -1
		}
		if a.CreatedAt.After(b.CreatedAt) {
			return 1
		}
		return 0
	})
	return sandboxes
}
