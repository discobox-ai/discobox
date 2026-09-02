package server

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/server/internal/apperrors"

	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	appservice "github.com/discobox-ai/discobox/server/internal/service"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/x/id"
)

// testDefaultProjectID is a fixture project ID for tests that stub out the
// service layer instead of going through InitializeDefaults, which now
// generates a real ID at first boot rather than using a fixed one.
var testDefaultProjectID = id.NewString(id.PrefixProject)

type routerTestServices struct {
	mu             sync.Mutex
	user           model.User
	project        model.Project
	harnessConfigs map[string]model.HarnessConfig
	providers      map[string]model.SandboxProviderInstance
	pools          map[string]model.Pool
	sandboxes      map[string]model.Sandbox
	sandboxLease   *services.HTTPClientLease
	sandboxScopes  []string
	console        *stubPoolConsole
	consoleErr     error
	poolLog        *stubPoolLog
	poolLogErr     error
}

func newRouterTestServices() *routerTestServices {
	now := time.Now().UTC()
	user := model.User{
		ID:        appservice.DefaultUserID,
		Email:     "local@example.com",
		Provider:  "local",
		Subject:   "local",
		CreatedAt: now,
		UpdatedAt: now,
	}
	project := model.Project{
		ID:          testDefaultProjectID,
		OwnerUserID: user.ID,
		Name:        "Default Project",
		Owner:       &user,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	return &routerTestServices{
		user:           user,
		project:        project,
		harnessConfigs: make(map[string]model.HarnessConfig),
		providers:      make(map[string]model.SandboxProviderInstance),
		sandboxes:      make(map[string]model.Sandbox),
	}
}

func (s *routerTestServices) ListProjects(context.Context) ([]model.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	project := s.projectWithSandboxes()
	return []model.Project{project}, nil
}

func (s *routerTestServices) GetProject(_ context.Context, projectID string) (*model.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	project := s.projectWithSandboxes()
	return &project, nil
}

func (s *routerTestServices) CreateProject(context.Context, services.CreateProjectBody) (*model.Project, error) {
	return nil, apperrors.NewStatusError(http.StatusNotImplemented, "not implemented")
}

func (s *routerTestServices) UpdateProject(_ context.Context, projectID string, _ services.UpdateProjectBody) (*model.Project, error) {
	return s.GetProject(context.Background(), projectID)
}

func (s *routerTestServices) DeleteProject(_ context.Context, projectID string) error {
	if projectID != s.project.ID {
		return apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	return apperrors.NewStatusError(http.StatusConflict, "project is the default project")
}

func (s *routerTestServices) SetDefaultProject(_ context.Context, projectID string) (*model.Project, error) {
	return s.GetProject(context.Background(), projectID)
}

func (s *routerTestServices) ListJobs(_ context.Context, projectID string) ([]model.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	return nil, nil
}

func (s *routerTestServices) GetJob(_ context.Context, projectID, _ string) (*model.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	return nil, apperrors.NewStatusError(http.StatusNotFound, "job not found")
}

func (s *routerTestServices) ForceJob(_ context.Context, projectID, _ string) (*model.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	return nil, apperrors.NewStatusError(http.StatusNotFound, "job not found")
}

func (s *routerTestServices) FallbackHarnessConfig(context.Context, string) (*model.HarnessConfig, error) {
	return nil, nil
}

func (s *routerTestServices) ListSandboxes(_ context.Context, projectID, sourceRoot, _ string) ([]model.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	sandboxes := s.sortedSandboxes()
	if sourceRoot == "" {
		return sandboxes, nil
	}
	filtered := make([]model.Sandbox, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		if sandbox.Source.Root() == sourceRoot {
			filtered = append(filtered, sandbox)
		}
	}
	return filtered, nil
}

func (s *routerTestServices) CreateSandbox(_ context.Context, projectID string, input services.CreateSandboxBody) (*model.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	config := input.Config

	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	harnessConfigID, err := s.resolveHarnessConfigID(config.HarnessConfigId, input.HarnessName)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	var source *model.GitSource
	if inputSource, ok := config.Source.Get(); ok {
		converted := services.GitSourceToModel(inputSource)
		source = &converted
	}
	user := services.SandboxUserToModel(config.User)
	sandbox := model.Sandbox{
		ID:                id.NewString(id.PrefixProject),
		ProjectID:         s.project.ID,
		CreatedByUserID:   s.user.ID,
		PoolID:            input.PoolId.Or(""),
		Name:              config.Name,
		Description:       services.OptStringPtr(config.Description),
		ResourceLifecycle: model.ResourceLifecycle{DesiredState: model.DesiredStatePresent, State: model.SandboxStatePending, Generation: 1},
		CreatedAt:         now,
		UpdatedAt:         now,
		CreatedBy:         &s.user,
		SandboxManifest: model.SandboxManifest{
			HarnessConfigID:      harnessConfigID,
			Model:                services.OptStringPtr(config.Model),
			ModelServiceTier:     services.OptStringPtr(config.ModelServiceTier),
			ModelReasoningLevel:  services.OptStringPtr(config.ModelReasoningLevel),
			Prompt:               config.Prompt,
			Source:               source,
			SourceCodeReferences: stubSourceCodeReferences(config.SourceCodeReferences),
			UserName:             user.Name,
			UserUID:              user.UID,
			UserGID:              user.GID,
			UserGroupName:        user.GroupName,
			UserAdditionalGroups: user.AdditionalGroups,
			HomeDirectory:        user.HomeDirectory,
		},
	}
	s.sandboxes[sandbox.ID] = sandbox
	return &sandbox, nil
}

func (s *routerTestServices) GetSandbox(_ context.Context, projectID, sandboxID string) (*model.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sandbox, err := s.getSandbox(projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	return &sandbox, nil
}

func (s *routerTestServices) AcquireSandboxHTTPClient(_ context.Context, projectID, sandboxID string, scopes []string) (*services.HTTPClientLease, *model.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sandbox, err := s.getSandbox(projectID, sandboxID)
	if err != nil {
		return nil, nil, err
	}
	s.sandboxScopes = append([]string(nil), scopes...)
	return s.sandboxLease, &sandbox, nil
}

// AwaitSandboxHTTPClient is the waiting acquire. The stub's sandboxes are
// always either there or not, so the two answer the same.
func (s *routerTestServices) AwaitSandboxHTTPClient(ctx context.Context, projectID, sandboxID string, scopes []string) (*services.HTTPClientLease, *model.Sandbox, error) {
	return s.AcquireSandboxHTTPClient(ctx, projectID, sandboxID, scopes)
}

func (s *routerTestServices) UpdateSandbox(_ context.Context, projectID, sandboxID string, input services.UpdateSandboxBody) (*model.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sandbox, err := s.getSandbox(projectID, sandboxID)
	if err != nil {
		return nil, err
	}

	if config, ok := input.Config.Get(); ok {
		if name, ok := config.Name.Get(); ok {
			sandbox.Name = name
		}
	}
	sandbox.UpdatedAt = time.Now().UTC()
	s.sandboxes[sandbox.ID] = sandbox
	return &sandbox, nil
}

// DeleteSandbox archives: the row survives with desired state archived, which
// is what lets unarchive and the retention purge find it again (ADR 0022 §2).
func (s *routerTestServices) DeleteSandbox(_ context.Context, projectID, sandboxID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sandbox, err := s.getSandbox(projectID, sandboxID)
	if err != nil {
		return err
	}
	sandbox.DesiredState = model.DesiredStateArchived
	sandbox.SetState(model.SandboxStateArchived)
	return nil
}

func (s *routerTestServices) UnarchiveSandbox(_ context.Context, projectID, sandboxID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sandbox, err := s.getSandbox(projectID, sandboxID)
	if err != nil {
		return err
	}
	if sandbox.DesiredState != model.DesiredStateArchived {
		return apperrors.NewStatusError(http.StatusConflict, "sandbox is not archived")
	}
	sandbox.DesiredState = model.DesiredStatePresent
	sandbox.SetState(model.SandboxStateReady)
	return nil
}

func (s *routerTestServices) PurgeSandbox(_ context.Context, projectID, sandboxID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.getSandbox(projectID, sandboxID); err != nil {
		return err
	}
	delete(s.sandboxes, sandboxID)
	return nil
}

func (s *routerTestServices) StartSandbox(_ context.Context, projectID, sandboxID string, _ services.StartSandboxBody) (*model.Sandbox, error) {
	return s.instructSandbox(projectID, sandboxID)
}

func (s *routerTestServices) StopSandbox(_ context.Context, projectID, sandboxID string, _ services.StopSandboxBody) (*model.Sandbox, error) {
	return s.instructSandbox(projectID, sandboxID)
}

func (s *routerTestServices) RestartSandbox(_ context.Context, projectID, sandboxID string, _ services.RestartSandboxBody) (*model.Sandbox, error) {
	return s.instructSandbox(projectID, sandboxID)
}

func (s *routerTestServices) RepairSandbox(_ context.Context, projectID, sandboxID string) (*model.Sandbox, error) {
	return s.instructSandbox(projectID, sandboxID)
}

func (s *routerTestServices) UpgradeSandbox(_ context.Context, projectID, sandboxID string, _ services.UpgradeSandboxBody) (*model.Sandbox, error) {
	return s.instructSandbox(projectID, sandboxID)
}

func (s *routerTestServices) CompleteSandboxSourcePush(_ context.Context, projectID, sandboxID string, _ services.CompleteSandboxSourcePushBody) (*model.Sandbox, error) {
	return s.instructSandbox(projectID, sandboxID)
}

func (s *routerTestServices) CompleteSandboxApply(_ context.Context, projectID, sandboxID string, _ services.CompleteSandboxApplyBody) (*model.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sandbox, err := s.getSandbox(projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	return &sandbox, nil
}

func (s *routerTestServices) ReconcileSandbox(_ context.Context, projectID, sandboxID string) (*model.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sandbox, err := s.getSandbox(projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	return &sandbox, nil
}

func (s *routerTestServices) AssignSandboxHarnessSecrets(_ context.Context, projectID, sandboxID, _ string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.getSandbox(projectID, sandboxID); err != nil {
		return nil, err
	}
	return map[string]string{}, nil
}

func (s *routerTestServices) ConfigureHarnessConfig(_ context.Context, projectID, configID string) (*model.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	if _, ok := s.harnessConfigs[configID]; !ok {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "harness config not found")
	}
	now := time.Now().UTC()
	return &model.Sandbox{
		ID: id.NewString(id.PrefixSandbox), ProjectID: projectID, Name: "configure-test",
		ResourceLifecycle: model.ResourceLifecycle{DesiredState: model.DesiredStatePresent, State: model.SandboxStatePending, Generation: 1},
		CreatedAt:         now, UpdatedAt: now,
	}, nil
}

func (s *routerTestServices) AttachHarnessConfigConfigure(_ context.Context, projectID, configID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	if _, ok := s.harnessConfigs[configID]; !ok {
		return apperrors.NewStatusError(http.StatusNotFound, "harness config not found")
	}
	return nil
}

func (s *routerTestServices) CommitHarnessConfigConfigure(_ context.Context, projectID, configID string) (*model.HarnessConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	config, ok := s.harnessConfigs[configID]
	if !ok {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "harness config not found")
	}
	config.Configured = true
	s.harnessConfigs[configID] = config
	return &config, nil
}

func (s *routerTestServices) DeconfigureHarnessConfig(_ context.Context, projectID, configID string) (*model.HarnessConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	config, ok := s.harnessConfigs[configID]
	if !ok {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "harness config not found")
	}
	config.Configured = false
	s.harnessConfigs[configID] = config
	return &config, nil
}

func (s *routerTestServices) RefreshHarnessConfigImage(_ context.Context, projectID, configID string) (*model.HarnessConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	config, ok := s.harnessConfigs[configID]
	if !ok {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "harness config not found")
	}
	return &config, nil
}

func (s *routerTestServices) ListHarnessConfigs(_ context.Context, projectID string) ([]model.HarnessConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	configs := make([]model.HarnessConfig, 0, len(s.harnessConfigs))
	for _, config := range s.harnessConfigs {
		configs = append(configs, config)
	}
	slices.SortFunc(configs, func(a, b model.HarnessConfig) int {
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

func (s *routerTestServices) CreateHarnessConfig(_ context.Context, projectID string, input services.CreateHarnessConfigBody) (*model.HarnessConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	name := strings.TrimSpace(input.Name.Or(""))
	runCommand := []string{"test-harness"}
	now := time.Now().UTC()
	config := model.HarnessConfig{ID: id.NewString(id.PrefixHarnessConfig), ProjectID: projectID, Name: name, RunCommand: runCommand, CreatedAt: now, UpdatedAt: now}
	s.harnessConfigs[config.ID] = config
	if s.project.DefaultHarnessConfigID == "" {
		s.project.DefaultHarnessConfigID = config.ID
	}
	return &config, nil
}

func (s *routerTestServices) GetHarnessConfig(_ context.Context, projectID, configID string) (*model.HarnessConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	config, ok := s.harnessConfigs[configID]
	if !ok {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "harness config not found")
	}
	return &config, nil
}

func (s *routerTestServices) UpdateHarnessConfig(_ context.Context, projectID, configID string, input services.UpdateHarnessConfigBody) (*model.HarnessConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	config, ok := s.harnessConfigs[configID]
	if !ok {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "harness config not found")
	}
	if name, ok := input.Name.Get(); ok {
		config.Name = name
	}
	config.UpdatedAt = time.Now().UTC()
	s.harnessConfigs[config.ID] = config
	return &config, nil
}

func (s *routerTestServices) SetDefaultHarnessConfig(_ context.Context, projectID, configID string) (*model.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	if _, ok := s.harnessConfigs[configID]; !ok {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "harness config not found")
	}
	s.project.DefaultHarnessConfigID = configID
	project := s.projectWithSandboxes()
	return &project, nil
}

func (s *routerTestServices) UnsetDefaultHarnessConfig(_ context.Context, projectID, configID string) (*model.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	if _, ok := s.harnessConfigs[configID]; !ok {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "harness config not found")
	}
	if s.project.DefaultHarnessConfigID != configID {
		return nil, apperrors.NewStatusError(http.StatusConflict, "harness config is not the project default")
	}
	s.project.DefaultHarnessConfigID = ""
	project := s.projectWithSandboxes()
	return &project, nil
}

func (s *routerTestServices) DeleteHarnessConfig(_ context.Context, projectID, configID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	if _, ok := s.harnessConfigs[configID]; !ok {
		return apperrors.NewStatusError(http.StatusNotFound, "harness config not found")
	}
	delete(s.harnessConfigs, configID)
	if s.project.DefaultHarnessConfigID == configID {
		s.project.DefaultHarnessConfigID = ""
	}
	return nil
}

func (s *routerTestServices) ListHarnessConfigSecretBindings(_ context.Context, projectID, configID string) ([]model.HarnessConfigSecretBinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	if _, ok := s.harnessConfigs[configID]; !ok {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "harness config not found")
	}
	return nil, nil
}

func (s *routerTestServices) SetHarnessConfigSecretBinding(_ context.Context, projectID, configID, envName, secretID string) (*model.HarnessConfigSecretBinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	if _, ok := s.harnessConfigs[configID]; !ok {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "harness config not found")
	}
	return &model.HarnessConfigSecretBinding{ID: "binding-1", ProjectID: projectID, HarnessConfigID: configID, EnvName: envName, SecretID: secretID}, nil
}

func (s *routerTestServices) DeleteHarnessConfigSecretBinding(_ context.Context, projectID, configID, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	if _, ok := s.harnessConfigs[configID]; !ok {
		return apperrors.NewStatusError(http.StatusNotFound, "harness config not found")
	}
	return nil
}

func (s *routerTestServices) ListSandboxProviderCatalogItems(context.Context) ([]services.SandboxProviderCatalogItem, error) {
	return []services.SandboxProviderCatalogItem{{ID: "digitalocean", Name: "DigitalOcean"}}, nil
}

func (s *routerTestServices) ListSandboxProviderInstances(_ context.Context, projectID string) ([]model.SandboxProviderInstance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	return s.sortedProviders(), nil
}

func (s *routerTestServices) CreateSandboxProviderInstance(_ context.Context, projectID string, input services.CreateSandboxProviderInstanceBody) (*model.SandboxProviderInstance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	now := time.Now().UTC()
	provider := model.SandboxProviderInstance{ID: id.NewString(id.PrefixSandboxProvider), ProjectID: projectID, Type: input.Type, Name: input.Name, Config: services.RawMessage(input.Config), CreatedAt: now, UpdatedAt: now}
	s.providers[provider.ID] = provider
	return &provider, nil
}

func (s *routerTestServices) ListPools(_ context.Context, projectID string) ([]model.Pool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	pools := make([]model.Pool, 0, len(s.pools))
	for _, pool := range s.pools {
		pools = append(pools, pool)
	}
	return pools, nil
}

func (s *routerTestServices) CreatePool(_ context.Context, projectID string, input services.CreatePoolBody) (*model.Pool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	now := time.Now().UTC()
	pool := model.Pool{ID: id.NewString(id.PrefixPool), ProjectID: projectID, CreatedAt: now, UpdatedAt: now, PoolManifest: model.PoolManifest{Name: input.Name, ProviderInstanceID: input.ProviderInstanceId}}
	if s.pools == nil {
		s.pools = map[string]model.Pool{}
	}
	s.pools[pool.ID] = pool
	return &pool, nil
}

func (s *routerTestServices) GetPool(_ context.Context, projectID, poolID string) (*model.Pool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	pool, ok := s.pools[poolID]
	if !ok {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "pool not found")
	}
	return &pool, nil
}

func (s *routerTestServices) UpdatePool(_ context.Context, projectID, poolID string, input services.UpdatePoolBody) (*model.Pool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	pool, ok := s.pools[poolID]
	if !ok {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "pool not found")
	}
	if name, ok := input.Name.Get(); ok {
		pool.Name = name
	}
	s.pools[poolID] = pool
	return &pool, nil
}

func (s *routerTestServices) DeletePool(_ context.Context, projectID, poolID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID != s.project.ID {
		return apperrors.NewStatusError(http.StatusNotFound, "project not found")
	}
	if _, ok := s.pools[poolID]; !ok {
		return apperrors.NewStatusError(http.StatusNotFound, "pool not found")
	}
	delete(s.pools, poolID)
	return nil
}

func (s *routerTestServices) GetSandboxProviderInstance(_ context.Context, projectID, providerID string) (*model.SandboxProviderInstance, error) {
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

func (s *routerTestServices) UpdateSandboxProviderInstance(_ context.Context, projectID, providerID string, input services.UpdateSandboxProviderInstanceBody) (*model.SandboxProviderInstance, error) {
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
		provider.Config = services.RawMessage(input.Config)
	}
	if disabled, ok := input.Disabled.Get(); ok {
		provider.Disabled = disabled
	}
	provider.UpdatedAt = time.Now().UTC()
	s.providers[provider.ID] = provider
	return &provider, nil
}

func (s *routerTestServices) DeleteSandboxProviderInstance(_ context.Context, projectID, providerID string) error {
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

func (s *routerTestServices) ReconcilePool(_ context.Context, projectID, poolID string) (*model.Pool, error) {
	return s.GetPool(context.Background(), projectID, poolID)
}

// OpenPoolConsole hands out the stub console the test installed, so the console
// route can be exercised without a provider or a Docker daemon.
func (s *routerTestServices) OpenPoolConsole(_ context.Context, projectID, poolID string, opts sandbox.ConsoleOptions) (sandbox.PTY, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.consoleErr != nil {
		return nil, s.consoleErr
	}
	if s.console == nil {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "pool not found")
	}
	s.console.projectID = projectID
	s.console.poolID = poolID
	s.console.openOpts = opts
	return s.console, nil
}

// OpenPoolLogs hands out the stub log stream the test installed, so the logs
// route can be exercised without a provider or a pool host.
func (s *routerTestServices) OpenPoolLogs(_ context.Context, projectID, poolID string, opts sandbox.PoolLogOptions) (*sandbox.PoolLogStream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poolLogErr != nil {
		return nil, s.poolLogErr
	}
	if s.poolLog == nil {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "pool not found")
	}
	s.poolLog.projectID = projectID
	s.poolLog.poolID = poolID
	s.poolLog.openOpts = opts
	return &sandbox.PoolLogStream{Source: s.poolLog.source, ReadCloser: s.poolLog}, nil
}

func (s *routerTestServices) SetDefaultPool(ctx context.Context, projectID, poolID string) (*model.Project, error) {
	if _, err := s.GetPool(ctx, projectID, poolID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.project.DefaultPoolID = poolID
	s.mu.Unlock()
	return s.GetProject(ctx, projectID)
}

func (s *routerTestServices) UnsetDefaultPool(ctx context.Context, projectID, poolID string) (*model.Project, error) {
	if _, err := s.GetPool(ctx, projectID, poolID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.project.DefaultPoolID != poolID {
		s.mu.Unlock()
		return nil, apperrors.NewStatusError(http.StatusConflict, "pool is not the project default")
	}
	s.project.DefaultPoolID = ""
	s.mu.Unlock()
	return s.GetProject(ctx, projectID)
}

func (s *routerTestServices) RegisterPool(context.Context, services.RegisterPoolBody) (*services.RegisterPoolResponseBody, error) {
	return &services.RegisterPoolResponseBody{}, nil
}

func (s *routerTestServices) UpdatePoolStatus(context.Context, string, services.UpdatePoolStatusBody) (*model.Pool, error) {
	return &model.Pool{}, nil
}

func (s *routerTestServices) ReportPoolSandboxStates(context.Context, string, services.ReportPoolSandboxStatesBody) error {
	return nil
}

func (s *routerTestServices) MintSandboxAgentStatusTokens(context.Context, string, services.MintSandboxAgentStatusTokensBody) (*services.MintSandboxAgentStatusTokensResponseBody, error) {
	return &services.MintSandboxAgentStatusTokensResponseBody{}, nil
}

func (s *routerTestServices) ReportPoolResources(context.Context, string, services.ReportPoolResourcesBody) error {
	return nil
}

func (s *routerTestServices) ReportSandboxAgentStatus(context.Context, string, services.ReportSandboxAgentStatusBody) error {
	return nil
}

// instructSandbox stands in for forwarding a power instruction to the pool
// agent. It writes no state, because the real one does not: what became of the
// instruction arrives later on the agent's reporting channel (ADR 0017 §9).
func (s *routerTestServices) instructSandbox(projectID, sandboxID string) (*model.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sandbox, err := s.getSandbox(projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	return &sandbox, nil
}

func (s *routerTestServices) getSandbox(projectID, sandboxID string) (model.Sandbox, error) {
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

func (s *routerTestServices) resolveHarnessConfigID(harnessConfigID, harnessName services.OptString) (*string, error) {
	if id, ok := harnessConfigID.Get(); ok && id != "" {
		if _, ok := s.harnessConfigs[id]; !ok {
			return nil, apperrors.NewStatusError(http.StatusNotFound, "harness config not found")
		}
		return &id, nil
	}
	name, ok := harnessName.Get()
	if ok && strings.TrimSpace(name) != "" {
		for _, config := range s.harnessConfigs {
			if config.Name == strings.TrimSpace(name) {
				id := config.ID
				return &id, nil
			}
		}
		return nil, apperrors.NewStatusError(http.StatusNotFound, "harness config not found")
	}
	return nil, nil
}

func stubSourceCodeReferences(input services.OptSandboxCreateConfigSourceCodeReferences) model.SourceCodeReferences {
	refs, ok := input.Get()
	if !ok {
		return nil
	}
	return services.SourceCodeReferencesToModel(refs)
}

func (s *routerTestServices) projectWithSandboxes() model.Project {
	project := s.project
	project.Owner = &s.user
	project.HarnessConfigs = make([]model.HarnessConfig, 0, len(s.harnessConfigs))
	for _, config := range s.harnessConfigs {
		project.HarnessConfigs = append(project.HarnessConfigs, config)
	}
	project.SandboxProviderInstances = s.sortedProviders()
	project.Sandboxes = s.sortedSandboxes()
	return project
}

func (s *routerTestServices) sortedProviders() []model.SandboxProviderInstance {
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

func (s *routerTestServices) sortedSandboxes() []model.Sandbox {
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
