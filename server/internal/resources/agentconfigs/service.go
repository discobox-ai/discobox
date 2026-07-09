package agentconfigs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/server/internal/agentdefs"
	"github.com/obot-platform/discobox/server/internal/apperrors"

	"github.com/obot-platform/discobox/server/internal/model"
	services "github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
)

type Service struct {
	store *store.Store
}

func NewService(store *store.Store) *Service {
	return &Service{store: store}
}

func (s *Service) ListAgentConfigs(ctx context.Context, projectID string) ([]model.AgentConfig, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apiError(err, "project not found")
	}
	configs, err := s.store.ListAgentConfigs(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for i := range configs {
		configs[i] = *agentdefs.Resolve(&configs[i])
	}
	return configs, nil
}

func (s *Service) CreateAgentConfig(ctx context.Context, projectID string, input services.CreateAgentConfigBody) (*model.AgentConfig, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, apiError(err, "project not found")
	}
	definitionID := ""
	var definition *model.AgentConfigDefinition
	if rawDefinitionID, isSet := input.DefinitionId.Get(); isSet && strings.TrimSpace(rawDefinitionID) != "" {
		found, ok := agentdefs.DefinitionByID(strings.TrimSpace(rawDefinitionID))
		if !ok {
			return nil, apperrors.NewStatusError(http.StatusNotFound, "agent config definition not found")
		}
		definition = found
		definitionID = found.ID
	}

	name := strings.TrimSpace(input.Name.Or(""))
	slug := strings.TrimSpace(input.Slug.Or(""))
	explicitSlug := slug != ""

	// Default the slug: an explicit slug wins, then the definition id, then a slug
	// derived from the name.
	if slug == "" {
		if definitionID != "" {
			slug = definitionID
		} else if name != "" {
			slug = agentdefs.Slugify(name)
		}
	}
	// Default the name: an explicit slug names the config so multiple configs can
	// extend one definition without colliding on the definition's display name;
	// otherwise fall back to the definition name, then the slug.
	if name == "" {
		switch {
		case explicitSlug:
			name = slug
		case definition != nil:
			name = definition.Name
		default:
			name = slug
		}
	}
	if slug == "" || name == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "agent config name or slug is required")
	}
	if err := agentdefs.ValidateSlug(slug); err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	if _, err := s.store.GetAgentConfigBySlug(ctx, projectID, slug); err == nil {
		return nil, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf("agent config slug %q already in use", slug))
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if _, err := s.store.GetAgentConfigByName(ctx, projectID, name); err == nil {
		return nil, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf("agent config name %q already in use", name))
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	// Store only the fields the caller explicitly set as overrides; unset fields
	// (nil) are inherited from the definition at resolve time.
	installCommand, _ := input.InstallCommand.Get()
	runCommand, _ := input.RunCommand.Get()
	relaunchCommand, _ := input.RelaunchCommand.Get()
	var files []model.AgentConfigFile
	if apiFiles, ok := input.Files.Get(); ok {
		files = agentConfigFilesFromAPI(apiFiles)
	}
	var secrets []model.AgentConfigSecret
	if apiSecrets, ok := input.Secrets.Get(); ok {
		secrets, err = agentConfigSecretsFromAPI(apiSecrets)
		if err != nil {
			return nil, err
		}
	}
	if definitionID == "" && len(runCommand) == 0 {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "agent config run command is required when no definition is provided")
	}

	config := &model.AgentConfig{
		ProjectID:       projectID,
		Slug:            slug,
		DefinitionID:    definitionID,
		Name:            name,
		InstallCommand:  installCommand,
		RunCommand:      runCommand,
		RelaunchCommand: relaunchCommand,
		Files:           files,
		Secrets:         secrets,
	}
	// Store only fields that differ from the definition so inherited fields keep
	// tracking definition upgrades.
	agentdefs.Sparsify(config)
	if err := s.store.CreateAgentConfig(ctx, config); err != nil {
		return nil, err
	}
	if project.DefaultAgentConfigID == "" {
		project.DefaultAgentConfigID = config.ID
		if err := s.store.UpsertProject(ctx, project); err != nil {
			return nil, err
		}
	}
	stored, err := s.store.GetAgentConfig(ctx, projectID, config.ID)
	if err != nil {
		return nil, err
	}
	return agentdefs.Resolve(stored), nil
}

func (s *Service) GetAgentConfig(ctx context.Context, projectID, configID string) (*model.AgentConfig, error) {
	config, err := s.store.GetAgentConfig(ctx, projectID, configID)
	if err != nil {
		return nil, apiError(err, "agent config not found")
	}
	return agentdefs.Resolve(config), nil
}

func (s *Service) UpdateAgentConfig(ctx context.Context, projectID, configID string, input services.UpdateAgentConfigBody) (*model.AgentConfig, error) {
	config, err := s.store.GetAgentConfig(ctx, projectID, configID)
	if err != nil {
		return nil, apiError(err, "agent config not found")
	}
	if nameValue, ok := input.Name.Get(); ok {
		name := strings.TrimSpace(nameValue)
		if name == "" {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "agent config name is required")
		}
		config.Name = name
	}
	if installCommand, ok := input.InstallCommand.Get(); ok {
		config.InstallCommand = installCommand
	}
	if runCommand, ok := input.RunCommand.Get(); ok {
		config.RunCommand = runCommand
	}
	if relaunchCommand, ok := input.RelaunchCommand.Get(); ok {
		config.RelaunchCommand = relaunchCommand
	}
	if apiFiles, ok := input.Files.Get(); ok {
		config.Files = agentConfigFilesFromAPI(apiFiles)
	}
	if apiSecrets, ok := input.Secrets.Get(); ok {
		secrets, err := agentConfigSecretsFromAPI(apiSecrets)
		if err != nil {
			return nil, err
		}
		config.Secrets = secrets
	}
	// A fully custom config (no definition to inherit from) must keep a run command.
	if config.DefinitionID == "" && len(config.RunCommand) == 0 {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "agent config run command is required when no definition is provided")
	}
	// Re-sparsify so a client that writes back the whole resolved object only
	// pins fields it actually changed away from the definition.
	agentdefs.Sparsify(config)
	if err := s.store.UpdateAgentConfig(ctx, config); err != nil {
		return nil, err
	}
	stored, err := s.store.GetAgentConfig(ctx, projectID, configID)
	if err != nil {
		return nil, err
	}
	return agentdefs.Resolve(stored), nil
}

func agentConfigFilesFromAPI(files []apimodel.AgentConfigFile) []model.AgentConfigFile {
	if len(files) == 0 {
		return nil
	}
	out := make([]model.AgentConfigFile, 0, len(files))
	for _, file := range files {
		out = append(out, model.AgentConfigFile{Path: file.Path, Content: file.Content, CreateOnly: file.CreateOnly.Or(false)})
	}
	return out
}

var envVarNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func agentConfigSecretsFromAPI(secrets []apimodel.AgentConfigSecret) ([]model.AgentConfigSecret, error) {
	if len(secrets) == 0 {
		return nil, nil
	}
	out := make([]model.AgentConfigSecret, 0, len(secrets))
	seen := make(map[string]struct{}, len(secrets))
	for _, secret := range secrets {
		name := strings.TrimSpace(secret.Name)
		if !envVarNamePattern.MatchString(name) {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("agent config secret name %q must be a valid environment variable name", secret.Name))
		}
		if _, dup := seen[name]; dup {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("agent config secret %q is declared more than once", name))
		}
		seen[name] = struct{}{}
		out = append(out, model.AgentConfigSecret{Name: name, Required: secret.Required.Or(false)})
	}
	return out, nil
}

func (s *Service) SetDefaultAgentConfig(ctx context.Context, projectID, configID string) (*model.Project, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, apiError(err, "project not found")
	}
	config, err := s.store.GetAgentConfig(ctx, projectID, configID)
	if err != nil {
		return nil, apiError(err, "agent config not found")
	}
	project.DefaultAgentConfigID = config.ID
	if err := s.store.UpsertProject(ctx, project); err != nil {
		return nil, err
	}
	return s.store.GetProject(ctx, projectID)
}

func (s *Service) DeleteAgentConfig(ctx context.Context, projectID, configID string) error {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return apiError(err, "project not found")
	}
	config, err := s.store.GetAgentConfig(ctx, projectID, configID)
	if err != nil {
		return apiError(err, "agent config not found")
	}
	if project.DefaultAgentConfigID == config.ID {
		return apperrors.NewStatusError(http.StatusConflict, "cannot delete the default agent config; set a different default first")
	}
	if err := s.store.DeleteAgentConfig(ctx, projectID, configID); err != nil {
		if errors.Is(err, store.ErrInUse) {
			return apperrors.NewStatusError(http.StatusConflict, "agent config is in use by a sandbox")
		}
		return apiError(err, "agent config not found")
	}
	return nil
}

// ListAgentConfigSecretBindings returns the env→secret bindings for an agent config.
func (s *Service) ListAgentConfigSecretBindings(ctx context.Context, projectID, configID string) ([]model.AgentConfigSecretBinding, error) {
	if _, err := s.store.GetAgentConfig(ctx, projectID, configID); err != nil {
		return nil, apiError(err, "agent config not found")
	}
	return s.store.ListAgentConfigSecretBindings(ctx, projectID, configID)
}

// SetAgentConfigSecretBinding binds (or rebinds) one of an agent config's
// environment variables to a project secret.
func (s *Service) SetAgentConfigSecretBinding(ctx context.Context, projectID, configID, envName, secretID string) (*model.AgentConfigSecretBinding, error) {
	config, err := s.store.GetAgentConfig(ctx, projectID, configID)
	if err != nil {
		return nil, apiError(err, "agent config not found")
	}
	envName = strings.TrimSpace(envName)
	if !envVarNamePattern.MatchString(envName) {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("environment variable name %q is invalid", envName))
	}
	secretID = strings.TrimSpace(secretID)
	if secretID == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "secret ID is required")
	}
	if _, err := s.store.GetSecret(ctx, projectID, secretID); err != nil {
		return nil, apiError(err, "secret not found")
	}
	binding := &model.AgentConfigSecretBinding{
		ProjectID:     projectID,
		AgentConfigID: config.ID,
		EnvName:       envName,
		SecretID:      secretID,
	}
	if err := s.store.UpsertAgentConfigSecretBinding(ctx, binding); err != nil {
		return nil, err
	}
	return binding, nil
}

// DeleteAgentConfigSecretBinding removes an agent config's binding for an
// environment variable.
func (s *Service) DeleteAgentConfigSecretBinding(ctx context.Context, projectID, configID, envName string) error {
	if _, err := s.store.GetAgentConfig(ctx, projectID, configID); err != nil {
		return apiError(err, "agent config not found")
	}
	if err := s.store.DeleteAgentConfigSecretBinding(ctx, projectID, configID, strings.TrimSpace(envName)); err != nil {
		return apiError(err, "agent config secret binding not found")
	}
	return nil
}

func apiError(err error, notFoundMessage string) error {
	if errors.Is(err, store.ErrNotFound) {
		return apperrors.NewStatusError(http.StatusNotFound, notFoundMessage)
	}
	return err
}
