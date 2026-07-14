package harnessconfigs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/obot-platform/discobox/server/internal/apperrors"
	"github.com/obot-platform/discobox/server/internal/harnessdefs"

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

func (s *Service) ListHarnessConfigs(ctx context.Context, projectID string) ([]model.HarnessConfig, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apiError(err, "project not found")
	}
	configs, err := s.store.ListHarnessConfigs(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for i := range configs {
		configs[i] = *harnessdefs.Resolve(&configs[i])
	}
	return configs, nil
}

func (s *Service) CreateHarnessConfig(ctx context.Context, projectID string, input services.CreateHarnessConfigBody) (*model.HarnessConfig, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, apiError(err, "project not found")
	}
	definitionID := ""
	var definition *model.HarnessDefinition
	if rawDefinitionID, isSet := input.DefinitionId.Get(); isSet && strings.TrimSpace(rawDefinitionID) != "" {
		found, ok := harnessdefs.DefinitionByID(strings.TrimSpace(rawDefinitionID))
		if !ok {
			return nil, apperrors.NewStatusError(http.StatusNotFound, "harness config definition not found")
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
			slug = harnessdefs.Slugify(name)
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
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "harness config name or slug is required")
	}
	if err := harnessdefs.ValidateSlug(slug); err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	if _, err := s.store.GetHarnessConfigBySlug(ctx, projectID, slug); err == nil {
		return nil, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf("harness config slug %q already in use", slug))
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if _, err := s.store.GetHarnessConfigByName(ctx, projectID, name); err == nil {
		return nil, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf("harness config name %q already in use", name))
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	// Store only the fields the caller explicitly set as overrides; unset fields
	// (nil) are inherited from the definition at resolve time.
	installCommand, _ := input.InstallCommand.Get()
	runCommand, _ := input.RunCommand.Get()
	relaunchCommand, _ := input.RelaunchCommand.Get()
	var files []model.HarnessConfigFile
	if apiFiles, ok := input.Files.Get(); ok {
		files = services.HarnessConfigFilesToModel(apiFiles)
	}
	var secrets []model.HarnessConfigSecret
	if apiSecrets, ok := input.Secrets.Get(); ok {
		secrets, err = services.HarnessConfigSecretsToModel(apiSecrets)
		if err != nil {
			return nil, err
		}
	}
	if definitionID == "" && len(runCommand) == 0 {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "harness config run command is required when no definition is provided")
	}

	config := &model.HarnessConfig{
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
	harnessdefs.Sparsify(config)
	if err := s.store.CreateHarnessConfig(ctx, config); err != nil {
		return nil, err
	}
	if project.DefaultHarnessConfigID == "" {
		project.DefaultHarnessConfigID = config.ID
		if err := s.store.UpsertProject(ctx, project); err != nil {
			return nil, err
		}
	}
	stored, err := s.store.GetHarnessConfig(ctx, projectID, config.ID)
	if err != nil {
		return nil, err
	}
	return harnessdefs.Resolve(stored), nil
}

func (s *Service) GetHarnessConfig(ctx context.Context, projectID, configID string) (*model.HarnessConfig, error) {
	config, err := s.store.GetHarnessConfig(ctx, projectID, configID)
	if err != nil {
		return nil, apiError(err, "harness config not found")
	}
	return harnessdefs.Resolve(config), nil
}

func (s *Service) UpdateHarnessConfig(ctx context.Context, projectID, configID string, input services.UpdateHarnessConfigBody) (*model.HarnessConfig, error) {
	config, err := s.store.GetHarnessConfig(ctx, projectID, configID)
	if err != nil {
		return nil, apiError(err, "harness config not found")
	}
	if nameValue, ok := input.Name.Get(); ok {
		name := strings.TrimSpace(nameValue)
		if name == "" {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "harness config name is required")
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
		config.Files = services.HarnessConfigFilesToModel(apiFiles)
	}
	if apiSecrets, ok := input.Secrets.Get(); ok {
		secrets, err := services.HarnessConfigSecretsToModel(apiSecrets)
		if err != nil {
			return nil, err
		}
		config.Secrets = secrets
	}
	// A fully custom config (no definition to inherit from) must keep a run command.
	if config.DefinitionID == "" && len(config.RunCommand) == 0 {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "harness config run command is required when no definition is provided")
	}
	// Re-sparsify so a client that writes back the whole resolved object only
	// pins fields it actually changed away from the definition.
	harnessdefs.Sparsify(config)
	if err := s.store.UpdateHarnessConfig(ctx, config); err != nil {
		return nil, err
	}
	stored, err := s.store.GetHarnessConfig(ctx, projectID, configID)
	if err != nil {
		return nil, err
	}
	return harnessdefs.Resolve(stored), nil
}

func (s *Service) SetDefaultHarnessConfig(ctx context.Context, projectID, configID string) (*model.Project, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, apiError(err, "project not found")
	}
	config, err := s.store.GetHarnessConfig(ctx, projectID, configID)
	if err != nil {
		return nil, apiError(err, "harness config not found")
	}
	project.DefaultHarnessConfigID = config.ID
	if err := s.store.UpsertProject(ctx, project); err != nil {
		return nil, err
	}
	return s.store.GetProject(ctx, projectID)
}

func (s *Service) DeleteHarnessConfig(ctx context.Context, projectID, configID string) error {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return apiError(err, "project not found")
	}
	config, err := s.store.GetHarnessConfig(ctx, projectID, configID)
	if err != nil {
		return apiError(err, "harness config not found")
	}
	if project.DefaultHarnessConfigID == config.ID {
		return apperrors.NewStatusError(http.StatusConflict, "cannot delete the default harness config; set a different default first")
	}
	if err := s.store.DeleteHarnessConfig(ctx, projectID, configID); err != nil {
		if errors.Is(err, store.ErrInUse) {
			return apperrors.NewStatusError(http.StatusConflict, "harness config is in use by a sandbox")
		}
		return apiError(err, "harness config not found")
	}
	return nil
}

// ListHarnessConfigSecretBindings returns the env→secret bindings for a harness config.
func (s *Service) ListHarnessConfigSecretBindings(ctx context.Context, projectID, configID string) ([]model.HarnessConfigSecretBinding, error) {
	if _, err := s.store.GetHarnessConfig(ctx, projectID, configID); err != nil {
		return nil, apiError(err, "harness config not found")
	}
	return s.store.ListHarnessConfigSecretBindings(ctx, projectID, configID)
}

// SetHarnessConfigSecretBinding binds (or rebinds) one of a harness config's
// environment variables to a project secret.
func (s *Service) SetHarnessConfigSecretBinding(ctx context.Context, projectID, configID, envName, secretID string) (*model.HarnessConfigSecretBinding, error) {
	config, err := s.store.GetHarnessConfig(ctx, projectID, configID)
	if err != nil {
		return nil, apiError(err, "harness config not found")
	}
	envName = strings.TrimSpace(envName)
	if !services.HarnessConfigEnvVarNamePattern.MatchString(envName) {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("environment variable name %q is invalid", envName))
	}
	secretID = strings.TrimSpace(secretID)
	if secretID == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "secret ID is required")
	}
	if _, err := s.store.GetSecret(ctx, projectID, secretID); err != nil {
		return nil, apiError(err, "secret not found")
	}
	binding := &model.HarnessConfigSecretBinding{
		ProjectID:       projectID,
		HarnessConfigID: config.ID,
		EnvName:         envName,
		SecretID:        secretID,
	}
	if err := s.store.UpsertHarnessConfigSecretBinding(ctx, binding); err != nil {
		return nil, err
	}
	return binding, nil
}

// DeleteHarnessConfigSecretBinding removes a harness config's binding for an
// environment variable.
func (s *Service) DeleteHarnessConfigSecretBinding(ctx context.Context, projectID, configID, envName string) error {
	if _, err := s.store.GetHarnessConfig(ctx, projectID, configID); err != nil {
		return apiError(err, "harness config not found")
	}
	if err := s.store.DeleteHarnessConfigSecretBinding(ctx, projectID, configID, strings.TrimSpace(envName)); err != nil {
		return apiError(err, "harness config secret binding not found")
	}
	return nil
}

func apiError(err error, notFoundMessage string) error {
	if errors.Is(err, store.ErrNotFound) {
		return apperrors.NewStatusError(http.StatusNotFound, notFoundMessage)
	}
	return err
}
