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
	sandboxes     SandboxRuntime
	dirtier       Dirtier
}

func NewService(store *store.Store) *Service {
	return &Service{store: store, inspector: defaultImageInspector{}}
}

// SetHarnessImages installs per-harness image overrides (built-in slug → image).
// Dev builds use this so the seeded built-ins point at freshly tagged images.
// Call SeedBuiltIns afterward to apply them.
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
	image := strings.TrimSpace(input.Image)
	if image == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "harness image is required")
	}
	// The image label is authoritative. Inspect it once here to snapshot the
	// harness metadata onto the config; nothing re-reads it afterward.
	var inspected *imageMetadata
	if s.inspector != nil {
		metadata, inspectErr := s.inspector.Inspect(ctx, image)
		if inspectErr != nil {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, inspectErr.Error())
		}
		inspected = &metadata
	}

	imageDigest := ""
	name := strings.TrimSpace(input.Name.Or(""))
	slug := strings.TrimSpace(input.Slug.Or(""))
	if inspected != nil {
		imageDigest = inspected.Digest
		if name == "" {
			name = strings.TrimSpace(inspected.Harness.Name)
		}
		if slug == "" {
			slug = strings.TrimSpace(inspected.Harness.ID)
		}
	}
	if slug == "" && name != "" {
		slug = harnessdefs.Slugify(name)
	}
	if name == "" {
		name = slug
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

	var runCommand, relaunchCommand, configCommand []string
	var files []model.HarnessConfigFile
	if apiFiles, ok := input.Files.Get(); ok {
		files = services.HarnessConfigFilesToModel(apiFiles)
	}
	var secrets []model.HarnessConfigSecret
	var env map[string]string
	var volumes []harness.Volume
	var additionalGroups []string
	if inspected != nil {
		runCommand, relaunchCommand, configCommand, files, secrets, env, volumes, additionalGroups = harnessMetadataFields(inspected.ImageMetadata)
	}
	if len(runCommand) == 0 {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "harness config run command is required")
	}

	config := &model.HarnessConfig{
		ProjectID:        projectID,
		Slug:             slug,
		Name:             name,
		Image:            image,
		ImageDigest:      imageDigest,
		RunCommand:       runCommand,
		RelaunchCommand:  relaunchCommand,
		ConfigCommand:    configCommand,
		Files:            files,
		Secrets:          secrets,
		Env:              env,
		Volumes:          volumes,
		AdditionalGroups: additionalGroups,
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
	if apiFiles, ok := input.ConfiguredFiles.Get(); ok {
		config.ConfiguredFiles = services.HarnessConfigFilesToModel(apiFiles)
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

// RefreshHarnessConfigImage re-inspects the config's image and re-snapshots the
// label metadata and digest onto it.
//
// Registration snapshots the label once and nothing re-reads it, which is
// correct for an immutable reference but leaves a config pointing at a rebuilt
// tag describing an image that no longer exists. Built-in configs get this for
// free from SeedBuiltIns on every server start; a config registered from a
// user-supplied image has no such trigger, and without one its ImageDigest can
// never move — so no sandbox created from it could ever report an available
// upgrade (ADR 0016 §7).
//
// Configured is never flipped: re-snapshotting the image's baseline leaves
// whatever the configure flow produced intact, exactly as seeding does.
func (s *Service) RefreshHarnessConfigImage(ctx context.Context, projectID, configID string) (*model.HarnessConfig, error) {
	config, err := s.store.GetHarnessConfig(ctx, projectID, configID)
	if err != nil {
		return nil, apiError(err, "harness config not found")
	}
	if s.inspector == nil {
		return nil, apperrors.NewStatusError(http.StatusServiceUnavailable, "image inspection is unavailable")
	}
	image := strings.TrimSpace(config.Image)
	if image == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "harness config has no image to refresh")
	}
	metadata, err := s.inspector.Inspect(ctx, image)
	if err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	runCommand, relaunchCommand, configCommand, files, secrets, env, volumes, additionalGroups := harnessMetadataFields(metadata.ImageMetadata)
	if len(runCommand) == 0 {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "harness config run command is required")
	}
	previousDigest := config.ImageDigest
	config.ImageDigest = metadata.Digest
	config.RunCommand, config.RelaunchCommand, config.ConfigCommand = runCommand, relaunchCommand, configCommand
	config.Files, config.Secrets, config.Env, config.Volumes = files, secrets, env, volumes
	config.AdditionalGroups = additionalGroups
	if err := s.store.UpdateHarnessConfig(ctx, config); err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "refreshed harness config image",
		"harnessConfigId", config.ID, "image", image,
		"previousImageDigest", previousDigest, "imageDigest", metadata.Digest)
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

// UnsetDefaultHarnessConfig clears the project's default harness config when it
// currently points at configID, leaving the project with no default. New
// sandboxes created without an explicit harness then run agent-less. Clearing a
// config that is not the default is rejected so the intent is unambiguous; this
// is also how a client releases the default before disabling that harness.
func (s *Service) UnsetDefaultHarnessConfig(ctx context.Context, projectID, configID string) (*model.Project, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, apiError(err, "project not found")
	}
	config, err := s.store.GetHarnessConfig(ctx, projectID, configID)
	if err != nil {
		return nil, apiError(err, "harness config not found")
	}
	if project.DefaultHarnessConfigID != config.ID {
		return nil, apperrors.NewStatusError(http.StatusConflict, "harness config is not the project default")
	}
	project.DefaultHarnessConfigID = ""
	if err := s.store.UpsertProject(ctx, project); err != nil {
		return nil, err
	}
	return s.store.GetProject(ctx, projectID)
}

func (s *Service) DeleteHarnessConfig(ctx context.Context, projectID, configID string) error {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return apiError(err, "project not found")
	}
	config, err := s.store.GetHarnessConfig(ctx, projectID, configID)
	if err != nil {
		return apiError(err, "harness config not found")
	}
	// Deleting a built-in is meaningless: the server seeds it again on the next
	// start. Deconfiguring is the way to turn one off.
	if config.BuiltIn {
		return apperrors.NewStatusError(http.StatusConflict,
			fmt.Sprintf("harness %q is built in and cannot be deleted; run `disco box harness deconfigure %s` to turn it off", config.Slug, config.Slug))
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

// SeedBuiltIns creates the included harness configs for a project and keeps their
// images current. Built-in configs track their image unconditionally: a dev
// rebuild bumps DISCOBOX_HARNESS_<SLUG>_IMAGE and restarts the server, and the
// new image is clobbered in here along with a fresh snapshot of its label
// metadata.
//
// The image is inspected on every pass, even when the reference is unchanged.
// A reference is not an identity: `:local` and other stable dev tags are
// rebuilt in place, so comparing references alone would leave ImageDigest
// pinned to a build that no longer exists under that tag. Sandbox upgrade
// detection compares digests (ADR 0016 §7), so a digest that never moves
// reports every sandbox as current forever.
//
// Seeding never flips Configured — a re-imaged harness keeps whatever the
// configure flow produced. Best-effort per harness: one whose image cannot be
// inspected is logged and skipped so an unavailable image never blocks startup.
func (s *Service) SeedBuiltIns(ctx context.Context, projectID string) error {
	if s.inspector == nil {
		return nil
	}
	for _, seed := range harnessdefs.Seeds(s.harnessImages) {
		image := strings.TrimSpace(seed.Image)
		if image == "" {
			continue
		}
		existing, err := s.store.GetHarnessConfigBySlug(ctx, projectID, seed.Slug)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		metadata, inspectErr := s.inspector.Inspect(ctx, image)
		if inspectErr != nil {
			slog.WarnContext(ctx, "skip built-in harness seed; image unavailable",
				"slug", seed.Slug, "image", image, "error", inspectErr)
			continue
		}
		if existing != nil && existing.Image == image && existing.ImageDigest == metadata.Digest {
			continue
		}
		if existing == nil {
			config := &model.HarnessConfig{
				ProjectID: projectID, Slug: seed.Slug, Name: seed.Name,
				BuiltIn: true, Configured: false, Image: image, ImageDigest: metadata.Digest,
			}
			config.RunCommand, config.RelaunchCommand, config.ConfigCommand, config.Files, config.Secrets, config.Env, config.Volumes, config.AdditionalGroups = harnessMetadataFields(metadata.ImageMetadata)
			if err := s.store.CreateHarnessConfig(ctx, config); err != nil {
				return err
			}
			slog.InfoContext(ctx, "seeded built-in harness config",
				"harnessConfigId", config.ID, "slug", seed.Slug, "image", image)
			continue
		}
		previousDigest := existing.ImageDigest
		existing.Image = image
		existing.ImageDigest = metadata.Digest
		existing.RunCommand, existing.RelaunchCommand, existing.ConfigCommand, existing.Files, existing.Secrets, existing.Env, existing.Volumes, existing.AdditionalGroups = harnessMetadataFields(metadata.ImageMetadata)
		if err := s.store.UpdateHarnessConfig(ctx, existing); err != nil {
			return err
		}
		slog.InfoContext(ctx, "updated built-in harness config image",
			"harnessConfigId", existing.ID, "slug", seed.Slug, "image", image,
			"previousImageDigest", previousDigest, "imageDigest", metadata.Digest)
	}
	return nil
}

// harnessMetadataFields snapshots the mutable config fields declared by a
// harness image's label metadata.
func harnessMetadataFields(metadata harness.ImageMetadata) (runCommand, relaunchCommand, configCommand []string, files []model.HarnessConfigFile, secrets []model.HarnessConfigSecret, env map[string]string, volumes []harness.Volume, additionalGroups []string) {
	image := metadata.Harness
	if image == nil {
		return nil, nil, nil, nil, nil, nil, nil, nil
	}
	runCommand = append([]string{}, image.RunCommand...)
	relaunchCommand = append([]string{}, image.RelaunchCommand...)
	if image.Config != nil {
		configCommand = append([]string{}, image.Config.Command...)
	}
	files = make([]model.HarnessConfigFile, 0, len(image.Files))
	for _, file := range image.Files {
		files = append(files, model.HarnessConfigFile{Path: file.Path, Content: file.Content, CreateOnly: file.CreateOnly, Template: file.Template})
	}
	secrets = make([]model.HarnessConfigSecret, 0, len(image.Secrets))
	for _, secret := range image.Secrets {
		secrets = append(secrets, model.HarnessConfigSecret{Name: secret.Name, Required: secret.Required, OneOfGroup: secret.OneOfGroup})
	}
	if len(metadata.Env) > 0 {
		env = make(map[string]string, len(metadata.Env))
		for k, v := range metadata.Env {
			env[k] = v
		}
	}
	volumes = append([]harness.Volume{}, metadata.Volumes...)
	additionalGroups = append([]string{}, metadata.AdditionalGroups...)
	return runCommand, relaunchCommand, configCommand, files, secrets, env, volumes, additionalGroups
}

func apiError(err error, notFoundMessage string) error {
	if errors.Is(err, store.ErrNotFound) {
		return apperrors.NewStatusError(http.StatusNotFound, notFoundMessage)
	}
	return err
}
