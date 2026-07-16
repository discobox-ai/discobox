package harnessconfigs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/obot-platform/discobox/harness"
	"github.com/obot-platform/discobox/server/internal/apperrors"
	"github.com/obot-platform/discobox/server/internal/harnessdefs"

	"github.com/obot-platform/discobox/server/internal/model"
	services "github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
)

type Service struct {
	store         *store.Store
	inspector     imageInspector
	harnessImages map[string]string
}

func NewService(store *store.Store) *Service {
	return &Service{store: store, inspector: defaultImageInspector{}}
}

// SetHarnessImages installs per-definition image overrides (definition ID →
// image). Dev builds use this so definition-backed configs resolve freshly
// tagged harness images. Call ReconcileDefinitionImages afterward to refresh
// already-stored configs.
func (s *Service) SetHarnessImages(images map[string]string) {
	s.harnessImages = images
}

func (s *Service) ListHarnessConfigs(ctx context.Context, projectID string) ([]model.HarnessConfig, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apiError(err, "project not found")
	}
	configs, err := s.store.ListHarnessConfigs(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return configs, nil
}

func (s *Service) CreateHarnessConfig(ctx context.Context, projectID string, input services.CreateHarnessConfigBody) (*model.HarnessConfig, error) {
	_, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, apiError(err, "project not found")
	}
	definitionID := ""
	image, _ := input.Image.Get()
	image = strings.TrimSpace(image)
	imageDigest := ""
	var definition *model.HarnessDefinition
	if rawDefinitionID, isSet := input.DefinitionId.Get(); isSet && strings.TrimSpace(rawDefinitionID) != "" {
		found, ok := s.definitionByID(strings.TrimSpace(rawDefinitionID))
		if !ok {
			return nil, apperrors.NewStatusError(http.StatusNotFound, "harness config definition not found")
		}
		definition = found
		definitionID = found.ID
		if image == "" {
			image = found.Image
		}
	}
	var inspected *imageMetadata
	if image != "" && s.inspector != nil {
		metadata, inspectErr := s.inspector.Inspect(ctx, image)
		if inspectErr != nil {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, inspectErr.Error())
		}
		inspected = &metadata
		imageDigest = metadata.Digest
		if definition != nil && metadata.Harness.ID != definition.ID {
			return nil, apperrors.NewStatusError(http.StatusBadRequest,
				fmt.Sprintf("harness image declares id %q, expected %q", metadata.Harness.ID, definition.ID))
		}
	}

	name := strings.TrimSpace(input.Name.Or(""))
	slug := strings.TrimSpace(input.Slug.Or(""))
	if inspected != nil {
		if name == "" {
			name = strings.TrimSpace(inspected.Harness.Name)
		}
		if slug == "" {
			slug = strings.TrimSpace(inspected.Harness.ID)
		}
	}
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
	if image == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "harness image is required")
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

	var runCommand, relaunchCommand []string
	var files []model.HarnessConfigFile
	if apiFiles, ok := input.Files.Get(); ok {
		files = services.HarnessConfigFilesToModel(apiFiles)
	}
	var secrets []model.HarnessConfigSecret
	if inspected != nil {
		runCommand, relaunchCommand, files, secrets = harnessMetadataFields(inspected.Harness)
	}
	if definitionID == "" && len(runCommand) == 0 {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "harness config run command is required when no definition is provided")
	}

	config := &model.HarnessConfig{
		ProjectID:       projectID,
		Slug:            slug,
		DefinitionID:    definitionID,
		Name:            name,
		Image:           image,
		ImageDigest:     imageDigest,
		RunCommand:      runCommand,
		RelaunchCommand: relaunchCommand,
		Files:           files,
		Secrets:         secrets,
	}
	if err := s.store.CreateHarnessConfig(ctx, config); err != nil {
		return nil, err
	}
	stored, err := s.store.GetHarnessConfig(ctx, projectID, config.ID)
	if err != nil {
		return nil, err
	}
	return stored, nil
}

func (s *Service) GetHarnessConfig(ctx context.Context, projectID, configID string) (*model.HarnessConfig, error) {
	config, err := s.store.GetHarnessConfig(ctx, projectID, configID)
	if err != nil {
		return nil, apiError(err, "harness config not found")
	}
	return config, nil
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
	if apiFiles, ok := input.Files.Get(); ok {
		config.Files = services.HarnessConfigFilesToModel(apiFiles)
	}
	if len(config.RunCommand) == 0 {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "harness config run command is required when no definition is provided")
	}
	if err := s.store.UpdateHarnessConfig(ctx, config); err != nil {
		return nil, err
	}
	stored, err := s.store.GetHarnessConfig(ctx, projectID, configID)
	if err != nil {
		return nil, err
	}
	return stored, nil
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
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return apiError(err, "project not found")
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

// ReconcileDefinitionImages re-points definition-backed harness configs at the
// definition's currently resolved image and re-snapshots its label metadata when
// the stored image is stale. Dev rebuilds bump DISCOBOX_HARNESS_<ID>_IMAGE and
// restart the server; this refresh is what makes existing configs pick up the
// freshly tagged image. Best-effort per config: one that cannot be inspected is
// left as-is and logged, so an unavailable image never blocks startup.
func (s *Service) ReconcileDefinitionImages(ctx context.Context) error {
	if len(s.harnessImages) == 0 || s.inspector == nil {
		return nil
	}
	configs, err := s.store.ListDefinitionBackedHarnessConfigs(ctx)
	if err != nil {
		return err
	}
	for i := range configs {
		config := &configs[i]
		definition, ok := s.definitionByID(config.DefinitionID)
		if !ok {
			continue
		}
		image := strings.TrimSpace(definition.Image)
		if image == "" || image == config.Image {
			continue
		}
		metadata, err := s.inspector.Inspect(ctx, image)
		if err != nil {
			slog.WarnContext(ctx, "skip stale harness config image refresh",
				"harnessConfigId", config.ID, "image", image, "error", err)
			continue
		}
		config.Image = image
		config.ImageDigest = metadata.Digest
		config.RunCommand, config.RelaunchCommand, config.Files, config.Secrets = harnessMetadataFields(metadata.Harness)
		if err := s.store.UpdateHarnessConfig(ctx, config); err != nil {
			return err
		}
		slog.InfoContext(ctx, "refreshed harness config image",
			"harnessConfigId", config.ID, "image", image, "imageDigest", metadata.Digest)
	}
	return nil
}

// harnessMetadataFields snapshots the mutable config fields declared by a
// harness image's label metadata.
func harnessMetadataFields(image harness.Image) (runCommand, relaunchCommand []string, files []model.HarnessConfigFile, secrets []model.HarnessConfigSecret) {
	runCommand = append([]string{}, image.RunCommand...)
	relaunchCommand = append([]string{}, image.RelaunchCommand...)
	files = make([]model.HarnessConfigFile, 0, len(image.Files))
	for _, file := range image.Files {
		files = append(files, model.HarnessConfigFile{Path: file.Path, Content: file.Content, CreateOnly: file.CreateOnly, Template: file.Template})
	}
	secrets = make([]model.HarnessConfigSecret, 0, len(image.Secrets))
	for _, secret := range image.Secrets {
		secrets = append(secrets, model.HarnessConfigSecret{Name: secret.Name, Required: secret.Required, OneOfGroup: secret.OneOfGroup})
	}
	return runCommand, relaunchCommand, files, secrets
}

func apiError(err error, notFoundMessage string) error {
	if errors.Is(err, store.ErrNotFound) {
		return apperrors.NewStatusError(http.StatusNotFound, notFoundMessage)
	}
	return err
}
