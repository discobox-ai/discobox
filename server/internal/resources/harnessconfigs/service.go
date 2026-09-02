package harnessconfigs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/discobox-ai/discobox/devimage"
	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/harnessdefs"

	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

type Service struct {
	store         *store.Store
	inspector     imageInspector
	harnessImages map[string]string
	sandboxes     SandboxRuntime
	dirtier       Dirtier
}

func NewService(store *store.Store) *Service {
	return &Service{
		store:     store,
		inspector: defaultImageInspector{},
		// Read here rather than threaded down from config: seeding is what the
		// override is for, and every process that seeds — the server, and a
		// test binary that constructs a project — has to honor it. CI points
		// these at label-only stand-ins for the real harness images
		// (ADR 0066 §7).
		harnessImages: harnessdefs.ImageOverridesFromEnv(os.Getenv),
	}
}

// SetDevelopmentImages lets seeding resolve harness metadata from the
// development image manifest. In build-mode the images do not exist anywhere
// until a pool builds them, so without this every built-in harness is skipped
// at startup and the project has no harness to run.
func (s *Service) SetDevelopmentImages(images []devimage.Image) {
	s.inspector = newDevImageInspector(images, s.inspector)
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
	if s.inspector == nil {
		return nil, apperrors.NewStatusError(http.StatusServiceUnavailable, "image inspection is unavailable")
	}
	inspected, err := s.inspector.Inspect(ctx, image)
	if err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}

	name := strings.TrimSpace(input.Name.Or(""))
	slug := strings.TrimSpace(input.Slug.Or(""))
	imageDigest := inspected.Digest
	// A manifest may name the harness, and needs not: an image that declares
	// none is registered under the name the caller gave (ADR 0086 §5), which is
	// why nothing here dereferences an absent harness block.
	if imageHarness := inspected.Harness; imageHarness != nil {
		if name == "" {
			name = strings.TrimSpace(imageHarness.Name)
		}
		if slug == "" {
			slug = strings.TrimSpace(imageHarness.ID)
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

	// The label is the whole snapshot, `input.Files` included: the image owns
	// its seed files at registration, and `UpdateHarnessConfig` is where a
	// caller changes them afterward.
	//
	// No check that a run command came back, either: an image that declares
	// none is taken to install the conventional harness.RunCommand, which
	// conventionCommands supplies (ADR 0086 §3). The label validator already
	// rejects a blank one.
	runCommand, relaunchCommand, configCommand, configReminder, files, secrets, env, volumes, additionalGroups := harnessMetadataFields(slug, inspected.ImageMetadata)

	config := &model.HarnessConfig{
		ProjectID:        projectID,
		Slug:             slug,
		Name:             name,
		Image:            image,
		ImageDigest:      imageDigest,
		RunCommand:       runCommand,
		RelaunchCommand:  relaunchCommand,
		ConfigCommand:    configCommand,
		ConfigReminder:   configReminder,
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
	runCommand, relaunchCommand, configCommand, configReminder, files, secrets, env, volumes, additionalGroups := harnessMetadataFields(config.Slug, metadata.ImageMetadata)
	previousDigest := config.ImageDigest
	config.ImageDigest = metadata.Digest
	config.RunCommand, config.RelaunchCommand, config.ConfigCommand = runCommand, relaunchCommand, configCommand
	config.ConfigReminder = configReminder
	config.Files, config.Secrets, config.Env, config.Volumes = files, secrets, env, volumes
	config.AdditionalGroups = additionalGroups
	if err := s.store.UpdateHarnessConfig(ctx, config); err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "refreshed harness config image",
		"harnessConfigId", config.ID, "image", image,
		"previousImageDigest", previousDigest, "imageDigest", metadata.Digest)
	s.applyResolvedImageDigest(ctx, projectID, config.ID, previousDigest, metadata.Digest)
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

// builtInDeleteHint is what to do instead of deleting a built-in, when there is
// anything to do. A harness already off has nothing to turn off, and one whose
// image declares no configure command cannot be turned off at all.
func builtInDeleteHint(config *model.HarnessConfig) string {
	switch {
	case !config.Configured:
		return "; it is already off"
	case len(config.ConfigCommand) == 0:
		return ""
	default:
		return fmt.Sprintf("; run `discobox admin harness deconfigure %s` to turn it off", config.Slug)
	}
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
	// start. Deconfiguring is the way to turn one off — but only where that
	// would do something, so the refusal does not send anyone to a command that
	// answers "already off" or refuses them in turn.
	if config.BuiltIn {
		return apperrors.NewStatusError(http.StatusConflict,
			fmt.Sprintf("harness %q is built in and cannot be deleted%s", config.Slug, builtInDeleteHint(config)))
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
	// Sandboxes already running this config hold a sentinel bound to whatever
	// the env named before. Rebinding here is what keeps the assignment and the
	// binding from drifting apart the moment they can.
	if s.sandboxes != nil {
		if err := s.sandboxes.RebindHarnessConfigSecrets(ctx, projectID, config.ID); err != nil {
			return nil, err
		}
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
	for _, seed := range s.seeds() {
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
				BuiltIn: true, Image: image, ImageDigest: metadata.Digest,
			}
			config.RunCommand, config.RelaunchCommand, config.ConfigCommand, config.ConfigReminder, config.Files, config.Secrets, config.Env, config.Volumes, config.AdditionalGroups = harnessMetadataFields(seed.Slug, metadata.ImageMetadata)
			// Born configured when there is nothing to collect. `shell` is the
			// harness that lands here, but by declaring no secrets rather than
			// by being itself: a fresh project has to be usable before anyone
			// configures anything, and an image with no credentials already is.
			// Only set at creation — reseeding never revisits Configured.
			config.Configured = len(config.Secrets) == 0
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
		existing.RunCommand, existing.RelaunchCommand, existing.ConfigCommand, existing.ConfigReminder, existing.Files, existing.Secrets, existing.Env, existing.Volumes, existing.AdditionalGroups = harnessMetadataFields(seed.Slug, metadata.ImageMetadata)
		if err := s.store.UpdateHarnessConfig(ctx, existing); err != nil {
			return err
		}
		slog.InfoContext(ctx, "updated built-in harness config image",
			"harnessConfigId", existing.ID, "slug", seed.Slug, "image", image,
			"previousImageDigest", previousDigest, "imageDigest", metadata.Digest)
		s.applyResolvedImageDigest(ctx, projectID, existing.ID, previousDigest, metadata.Digest)
	}
	return nil
}

// applyResolvedImageDigest is the single place a harness config's resolved
// image reaches its sandboxes (ADR 0082 §1).
//
// The rule is stated on the field rather than on a list of callers: wherever
// ImageDigest is written to a value different from the one it replaced, that
// config's eligible stopped sandboxes are re-pinned onto it. A built-in
// reseeded at startup and a custom harness re-pulled by its owner are the same
// event to a sandbox running it, so they go through the same function instead
// of each remembering independently — and a future writer of the field inherits
// the behavior by calling this rather than by being amended into a list.
//
// A digest that did not move does nothing: the fan-out costs a query per
// config, and SeedBuiltIns runs on every project ensure.
//
// Best effort. The digest is already persisted by the time this runs, so a
// failure here loses the upgrade, not the refresh — and the sandboxes it
// missed keep reporting an available upgrade and are picked up the next time
// this config's image resolves.
func (s *Service) applyResolvedImageDigest(ctx context.Context, projectID, configID, previousDigest, digest string) {
	if s.sandboxes == nil || strings.TrimSpace(digest) == "" || previousDigest == digest {
		return
	}
	if err := s.sandboxes.UpgradeHarnessConfigSandboxes(ctx, projectID, configID); err != nil {
		slog.WarnContext(ctx, "harness image resolved but stopped sandboxes were not upgraded",
			"harnessConfigId", configID, "previousImageDigest", previousDigest,
			"imageDigest", digest, "error", err)
	}
}

// seeds are the built-in harness configs: every harness in the registry,
// `shell` included. It is the end of the resolution chain rather than a
// different kind of thing, so nothing here treats it differently (ADR 0043).
func (s *Service) seeds() []harnessdefs.Seed {
	return harnessdefs.Seeds(s.harnessImages)
}

// conventionCommands resolves what a terminal types for this harness: the
// image's own commands when it declares them, and the convention otherwise
// (ADR 0086 §3).
//
// The convention is resolved here, at registration, because this is where the
// reserved shell slug is known. The sandbox knows its harness by the config's
// generated id, so it cannot tell a login shell from an image that declared
// nothing — and those two must not resolve alike. Snapshotting the resolved
// commands also keeps them visible in `discobox harness show` and overridable
// through the config, which is what makes the manifest's copy an override.
//
// `shell` gets neither command. It is the one harness whose terminal *is* the
// shell being launched, so a command typed into it would be a second shell
// inside the first (ADR 0043 §1's reserved slug is the whole test).
func conventionCommands(slug string, image harness.Image) (runCommand, relaunchCommand []string) {
	if strings.TrimSpace(slug) == harness.ShellSlug {
		return append([]string{}, image.RunCommand...), append([]string{}, image.RelaunchCommand...)
	}
	runCommand = append([]string{}, image.RunCommand...)
	if len(runCommand) == 0 {
		runCommand = []string{harness.RunCommand}
	}
	relaunchCommand = append([]string{}, image.RelaunchCommand...)
	if len(relaunchCommand) == 0 {
		relaunchCommand = []string{harness.RunCommand, harness.ResumeFlag}
	}
	return runCommand, relaunchCommand
}

// harnessMetadataFields snapshots the mutable config fields declared by a
// harness image's label metadata, resolving the run and relaunch commands
// against the convention for the harness registered under slug.
func harnessMetadataFields(slug string, metadata harness.ImageMetadata) (runCommand, relaunchCommand, configCommand []string, configReminder string, files []model.HarnessConfigFile, secrets []model.HarnessConfigSecret, env map[string]string, volumes []harness.Volume, additionalGroups []string) {
	image := metadata.Harness
	if image == nil {
		image = &harness.Image{}
	}
	runCommand, relaunchCommand = conventionCommands(slug, *image)
	if image.Config != nil {
		configCommand = append([]string{}, image.Config.Command...)
		configReminder = strings.TrimSpace(image.Config.Reminder)
	}
	files = make([]model.HarnessConfigFile, 0, len(image.Files))
	for _, file := range image.Files {
		files = append(files, model.HarnessConfigFile{Path: file.Path, Content: file.Content, CreateOnly: file.CreateOnly, Template: file.Template})
	}
	secrets = make([]model.HarnessConfigSecret, 0, len(image.Secrets))
	for _, secret := range image.Secrets {
		secrets = append(secrets, model.HarnessConfigSecret{
			Name: secret.Name, Required: secret.Required, OneOfGroup: secret.OneOfGroup,
			// Delivery decides whether the sentinel is exported into the
			// harness's environment at all, so dropping it here would leave
			// every later layer with nothing to act on.
			Delivery: secret.Delivery,
		})
	}
	if len(metadata.Env) > 0 {
		env = make(map[string]string, len(metadata.Env))
		for k, v := range metadata.Env {
			env[k] = v
		}
	}
	volumes = append([]harness.Volume{}, metadata.Volumes...)
	additionalGroups = append([]string{}, metadata.AdditionalGroups...)
	return runCommand, relaunchCommand, configCommand, configReminder, files, secrets, env, volumes, additionalGroups
}

func apiError(err error, notFoundMessage string) error {
	if errors.Is(err, store.ErrNotFound) {
		return apperrors.NewStatusError(http.StatusNotFound, notFoundMessage)
	}
	return err
}
