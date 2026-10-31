package harnessconfigs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/harnesscapture"
	"github.com/discobox-ai/discobox/pkg/hostscope"
	"github.com/discobox-ai/discobox/proxy/egress"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/devbundles"
	"github.com/discobox-ai/discobox/server/internal/harnessdefs"

	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

type Service struct {
	store     *store.Store
	inspector imageInspector
	// verified is shared by every inspector of bundles, whichever reads them.
	verified             *verifiedBundles
	harnesses            harnessdefs.References
	requireBuiltInImages bool
	sandboxes            SandboxRuntime
	dirtier              Dirtier
}

// NewService builds the service. overlayDir is the one directory a file://
// manifest reference may name a file in; empty refuses every such reference.
func NewService(store *store.Store, harnesses harnessdefs.References, requireBuiltInImages bool, overlayDir string) *Service {
	verified := &verifiedBundles{}
	return &Service{
		store:                store,
		inspector:            defaultImageInspector{overlayDir: overlayDir, verified: verified},
		verified:             verified,
		harnesses:            harnesses,
		requireBuiltInImages: requireBuiltInImages,
	}
}

// SetDevelopmentBundles lets seeding resolve harness metadata from the
// development bundles. They are in no daemon or registry: in copy-mode only
// in the layouts the watcher exported, and in build-mode nowhere until a pool
// builds them, so without this every built-in harness is skipped at startup
// and the project has no harness to run.
func (s *Service) SetDevelopmentBundles(bundles *devbundles.Set) {
	s.inspector = newDevImageInspector(bundles, s.inspector, s.verified)
}

func (s *Service) ListHarnessConfigs(ctx context.Context, projectID string) ([]model.HarnessConfig, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
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
		return nil, apperrors.NotFound(err, "project not found")
	}
	// A harness is its bundle, or, for a template with no image, its manifest
	// file (ADR 26-10-16-423 §2).
	bundle, manifestFile := strings.TrimSpace(input.Bundle.Or("")), strings.TrimSpace(input.Image.Or(""))
	if bundle == "" && manifestFile == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "harness bundle is required")
	}
	// The labels are authoritative. Inspect them once here to snapshot the
	// harness metadata onto the config; nothing re-reads them afterward.
	if s.inspector == nil {
		return nil, apperrors.NewStatusError(http.StatusServiceUnavailable, "bundle inspection is unavailable")
	}
	inspected, err := s.resolveHarness(ctx, s.platformBundle(), bundle, manifestFile)
	if err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}

	name := strings.TrimSpace(input.Name.Or(""))
	slug := strings.TrimSpace(input.Slug.Or(""))
	// A manifest may name the harness, and needs not: a bundle that declares
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

	// The label is the whole snapshot, `input.Files` included: the bundle owns
	// its seed files at registration, and `UpdateHarnessConfig` is where a
	// caller changes them afterward.
	//
	// No check that a run command came back, either: a bundle that declares
	// none is taken to install the conventional harness.RunCommand, which
	// conventionCommands supplies (ADR 0086 §3). The label validator already
	// rejects a blank one.
	config := &model.HarnessConfig{
		ProjectID:    projectID,
		Slug:         slug,
		Name:         name,
		Bundle:       bundle,
		BundleDigest: inspected.BundleDigest,
		Image:        manifestFile,
		ImageDigest:  inspected.Digest,
	}
	snapshotImageMetadata(config, inspected)
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
		return nil, apperrors.NotFound(err, "harness config not found")
	}
	return config, nil
}

func (s *Service) UpdateHarnessConfig(ctx context.Context, projectID, configID string, input services.UpdateHarnessConfigBody) (*model.HarnessConfig, error) {
	config, err := s.store.GetHarnessConfig(ctx, projectID, configID)
	if err != nil {
		return nil, apperrors.NotFound(err, "harness config not found")
	}
	if input.Revision != config.Revision {
		return nil, apperrors.NewStatusError(http.StatusConflict, "harness configuration changed; reload it before saving")
	}

	// A bundle registered for the config is a new manifest, read and written
	// the way a refresh does. It is registered on its own: the snapshot it
	// takes rewrites the files and the egress hosts a person would be setting
	// beside it, and one request saved as two writes could keep half of it.
	if bundle, ok := input.Bundle.Get(); ok {
		if input.Name.Set || input.Files.Set || input.ConfiguredFiles.Set || input.Egress.Set {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "a harness bundle is registered on its own, with no other change beside it")
		}
		if strings.TrimSpace(bundle) == "" {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "harness bundle is required")
		}
		if config.BuiltIn {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "a built-in harness tracks the bundle its server ships")
		}
		metadata, err := s.reinspect(ctx, config, bundle)
		if err != nil {
			return nil, err
		}
		return s.resnapshot(ctx, config, bundle, metadata)
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
	var personLevel *egress.Policy
	if level, ok := input.Egress.Get(); ok {
		if config.Egress, err = services.EgressPolicyToModel(level); err != nil {
			return nil, err
		}
		personLevel = &config.Egress
	}
	if err := s.updateHarnessConfig(ctx, config, personLevel); err != nil {
		return nil, err
	}
	stored, err := s.store.GetHarnessConfig(ctx, projectID, configID)
	if err != nil {
		return nil, err
	}
	return stored, nil
}

// RefreshHarnessConfigImage re-inspects the config's bundle, or its manifest
// file, and re-snapshots the label metadata and digest onto it.
//
// Registration snapshots the labels once and nothing re-reads them, which is
// correct for an immutable reference but leaves a config pointing at a rebuilt
// tag describing a bundle that no longer exists. Built-in configs get this for
// free from SeedBuiltIns on every server start; a config registered from a
// user-supplied bundle has no such trigger, and without one its BundleDigest
// can never move — so no sandbox created from it could ever report an
// available upgrade (ADR 0016 §7).
//
// Configured is never flipped: re-snapshotting the bundle's baseline leaves
// whatever the configure flow produced intact, exactly as seeding does.
func (s *Service) RefreshHarnessConfigImage(ctx context.Context, projectID, configID string) (*model.HarnessConfig, error) {
	config, err := s.store.GetHarnessConfig(ctx, projectID, configID)
	if err != nil {
		return nil, apperrors.NotFound(err, "harness config not found")
	}
	metadata, err := s.reinspect(ctx, config, config.Bundle)
	if err != nil {
		return nil, err
	}
	return s.resnapshot(ctx, config, config.Bundle, metadata)
}

// reinspect inspects bundle, the one config has or the one being registered
// for it, or config's manifest file.
func (s *Service) reinspect(ctx context.Context, config *model.HarnessConfig, bundle string) (imageMetadata, error) {
	if s.inspector == nil {
		return imageMetadata{}, apperrors.NewStatusError(http.StatusServiceUnavailable, "bundle inspection is unavailable")
	}
	bundle, manifestFile := strings.TrimSpace(bundle), strings.TrimSpace(config.Image)
	// A harness registered before bundles has a manifest its image's labels
	// gave it, and no image any more (internal/database): it keeps that
	// manifest until a bundle is registered for it, and a refresh has nothing
	// to read it again from.
	if bundle == "" && manifestFile == "" {
		return imageMetadata{}, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("harness config %s was registered before bundles: register its bundle to refresh it", config.Slug))
	}
	if bundle != "" {
		manifestFile = ""
	}
	metadata, err := s.resolveHarness(ctx, s.platformBundle(), bundle, manifestFile)
	if err != nil {
		return imageMetadata{}, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	return metadata, nil
}

// resnapshot writes what reinspect read of config's bundle or manifest file
// onto it.
func (s *Service) resnapshot(ctx context.Context, config *model.HarnessConfig, bundle string, metadata imageMetadata) (*model.HarnessConfig, error) {
	bundle = strings.TrimSpace(bundle)
	previous := configPins(config)
	previousHosts := config.ImageEgressHosts
	config.Bundle, config.BundleDigest = bundle, metadata.BundleDigest
	if bundle != "" {
		config.Image = ""
	}
	config.ImageDigest = metadata.Digest
	snapshotImageMetadata(config, metadata)
	// The snapshot and the routes it declares onto configured secrets commit
	// together, so a failed refresh leaves the old digest for a retry to move.
	if err := s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		if err := txStore.UpdateHarnessConfig(ctx, config); err != nil {
			return err
		}
		if err := s.syncConfiguredSecretRoutes(ctx, txStore, txDB, config); err != nil {
			return err
		}
		return s.markEgressChangedTx(ctx, txStore, txDB, config, !slices.Equal(previousHosts, config.ImageEgressHosts))
	}); err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "refreshed harness config",
		"harnessConfigId", config.ID, "bundle", bundle, "manifestFile", config.Image,
		"previousBundleDigest", previous.bundle, "bundleDigest", metadata.BundleDigest, "manifestFileDigest", metadata.Digest)
	s.applyResolvedPins(ctx, config.ProjectID, config.ID, previous, configPins(config))
	stored, err := s.store.GetHarnessConfig(ctx, config.ProjectID, config.ID)
	if err != nil {
		return nil, err
	}
	return stored, nil
}

func (s *Service) SetDefaultHarnessConfig(ctx context.Context, projectID, configID string) (*model.Project, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	config, err := s.store.GetHarnessConfig(ctx, projectID, configID)
	if err != nil {
		return nil, apperrors.NotFound(err, "harness config not found")
	}
	project.DefaultHarnessConfigID = config.ID
	if err := s.store.UpsertProject(ctx, project); err != nil {
		return nil, err
	}
	return s.store.GetProject(ctx, projectID)
}

// UnsetDefaultHarnessConfig clears the project's default harness config when it
// currently points at configID, leaving the project with no default. New
// sandboxes created without an explicit harness are then refused until one is
// named or a new default is set (ADR 0048). Clearing a config that is not the
// default is rejected so the intent is unambiguous; this is also how a client
// releases the default before disabling that harness.
func (s *Service) UnsetDefaultHarnessConfig(ctx context.Context, projectID, configID string) (*model.Project, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	config, err := s.store.GetHarnessConfig(ctx, projectID, configID)
	if err != nil {
		return nil, apperrors.NotFound(err, "harness config not found")
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
		return apperrors.NotFound(err, "project not found")
	}
	config, err := s.store.GetHarnessConfig(ctx, projectID, configID)
	if err != nil {
		return apperrors.NotFound(err, "harness config not found")
	}
	// Deleting a built-in is meaningless: the server seeds it again on the next
	// start. Deconfiguring is the way to turn one off — but only where that
	// would do something, so the refusal does not send anyone to a command that
	// answers "already off" or refuses them in turn.
	if config.BuiltIn {
		return apperrors.NewStatusError(http.StatusConflict,
			fmt.Sprintf("harness %q is built in and cannot be deleted%s", config.Slug, builtInDeleteHint(config)))
	}
	// No discobox runs the config, but discoboxes it was attached to, and
	// those handed a secret on under its delegate bindings, held what it
	// delivered: they are marked.
	var markHolders store.BindingsDeletedFunc
	if s.sandboxes != nil {
		markHolders = func(txStore *store.Store, txDB *gorm.DB, deleted []model.SecretBinding) error {
			return s.sandboxes.MarkBindingHoldersDirtyTx(ctx, txStore, txDB, projectID, deleted)
		}
	}
	if err := s.store.DeleteHarnessConfig(ctx, projectID, configID, markHolders); err != nil {
		if errors.Is(err, store.ErrInUse) {
			return apperrors.NewStatusError(http.StatusConflict, "harness config is in use by a sandbox")
		}
		return apperrors.NotFound(err, "harness config not found")
	}
	return nil
}

// SeedBuiltIns creates the included harness configs for a project and keeps their
// bundles current. Built-in configs track their bundle unconditionally: a dev
// rebuild bumps DISCOBOX_HARNESS_<SLUG>_BUNDLE and restarts the server, and the
// new bundle is clobbered in here along with a fresh snapshot of its label
// metadata.
//
// The bundle is inspected on every pass, even when the reference is unchanged.
// A reference is not an identity: `:local` and other stable dev tags are
// rebuilt in place, so comparing references alone would leave BundleDigest
// pinned to a build that no longer exists under that tag. Sandbox upgrade
// detection compares digests (ADR 0016 §7), so a digest that never moves
// reports every sandbox as current forever.
//
// Seeding never flips Configured — a rebuilt harness keeps whatever the
// configure flow produced. Best-effort per harness: one whose bundle cannot be
// inspected is logged and skipped so an unavailable bundle never blocks startup.
func (s *Service) SeedBuiltIns(ctx context.Context, projectID string) error {
	if s.inspector == nil {
		return nil
	}
	platformBundle := s.platformBundle()
	for _, seed := range s.seeds() {
		bundle := strings.TrimSpace(seed.Bundle)
		if bundle == "" {
			continue
		}
		existing, err := s.store.GetHarnessConfigBySlug(ctx, projectID, seed.Slug)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if existing != nil && !existing.BuiltIn {
			return fmt.Errorf("built-in harness %s conflicts with a user-created harness config", seed.Slug)
		}
		metadata, inspectErr := s.resolveHarness(ctx, platformBundle, bundle, "")
		if inspectErr != nil {
			if s.requireBuiltInImages {
				return fmt.Errorf("inspect release harness %s (bundle %s): %w", seed.Slug, bundle, inspectErr)
			}
			slog.WarnContext(ctx, "skip built-in harness seed; bundle unavailable",
				"slug", seed.Slug, "bundle", bundle, "error", inspectErr)
			continue
		}
		// The platforms, the image kind and a runnable image left from
		// before bundles are compared too, so a built-in recorded with them is
		// rewritten as the bundle it is now. The secret declarations are
		// compared for the same reason: one snapshotted by a server that did
		// not read a field of them yet (a rejection scope, ADR 26-10-13-595)
		// carries the digest and not the field, and would otherwise be skipped
		// for good.
		if existing != nil && existing.Image == "" && existing.ImageDigest == "" &&
			existing.Bundle == bundle && existing.BundleDigest == metadata.BundleDigest &&
			existing.ManifestDigest == manifestDigest(metadata) &&
			slices.Equal(existing.Platforms, metadata.Platforms) && existing.ImageKind == metadata.ImageKind &&
			sameSecretDeclarations(existing.Secrets, declaredSecrets(metadata.Harness)) {
			continue
		}
		if existing == nil {
			config := &model.HarnessConfig{
				ProjectID: projectID, Slug: seed.Slug, Name: seed.Name,
				BuiltIn: true, Bundle: bundle, BundleDigest: metadata.BundleDigest,
			}
			snapshotImageMetadata(config, metadata)
			// Born configured when there is nothing to collect. `shell` is the
			// harness that lands here, but by declaring no secrets rather than
			// by being itself: a fresh project has to be usable before anyone
			// configures anything, and a bundle with no credentials already is.
			// Only set at creation — reseeding never revisits Configured.
			config.Configured = len(config.Secrets) == 0
			if err := s.store.CreateHarnessConfig(ctx, config); err != nil {
				return err
			}
			slog.InfoContext(ctx, "seeded built-in harness config",
				"harnessConfigId", config.ID, "slug", seed.Slug, "bundle", bundle)
			continue
		}
		previous := configPins(existing)
		previousHosts := existing.ImageEgressHosts
		existing.Bundle, existing.BundleDigest = bundle, metadata.BundleDigest
		existing.Image, existing.ImageDigest = "", ""
		snapshotImageMetadata(existing, metadata)
		// As at refresh: the snapshot and its routes commit together, so a
		// seed that fails leaves the config for the next one to find unequal.
		if err := s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
			if err := txStore.UpdateHarnessConfig(ctx, existing); err != nil {
				return err
			}
			if err := s.syncConfiguredSecretRoutes(ctx, txStore, txDB, existing); err != nil {
				return err
			}
			return s.markEgressChangedTx(ctx, txStore, txDB, existing, !slices.Equal(previousHosts, existing.ImageEgressHosts))
		}); err != nil {
			return err
		}
		slog.InfoContext(ctx, "updated built-in harness config bundle",
			"harnessConfigId", existing.ID, "slug", seed.Slug, "bundle", bundle,
			"previousBundleDigest", previous.bundle, "bundleDigest", metadata.BundleDigest)
		s.applyResolvedPins(ctx, projectID, existing.ID, previous, configPins(existing))
	}
	return nil
}

// EnsureHarnessAvailable fails when a project has no harness config at all.
//
// Seeding is best-effort per harness, and that is right while some of them
// seeded: the project is short a harness and every other one still runs. None
// of them is a different condition. Sandbox create resolves an explicit harness
// or the project default and refuses when it has neither (ADR 0048), so a
// project with no config has no path left that ends anywhere but an error, and
// the causes — a bundle published without its manifest labels, a daemon that
// is not running, a registry that cannot be reached — are all outside what anyone
// can fix from inside the product. The server's startup calls this and refuses
// to serve; nothing else has to, because nothing else can be first.
//
// Any config counts, not only a built-in. A project seeded on an earlier boot,
// or one whose only harness was registered by hand, has something to run
// whatever this boot's bundles do.
//
// The reasons are re-inspected rather than remembered from the seeding pass:
// this runs only when there is nothing to run, so it costs one inspection per
// built-in on a server that is about to fail anyway, and it puts why each bundle
// was unusable in the error instead of leaving it in a log line above it.
func (s *Service) EnsureHarnessAvailable(ctx context.Context, projectID string) error {
	configs, err := s.store.ListHarnessConfigs(ctx, projectID)
	if err != nil {
		return err
	}
	if len(configs) > 0 {
		return nil
	}
	seeds := s.seeds()
	reasons := make([]string, 0, len(seeds))
	platformBundle := s.platformBundle()
	for _, seed := range seeds {
		bundle := strings.TrimSpace(seed.Bundle)
		switch {
		case bundle == "":
			reasons = append(reasons, fmt.Sprintf("%s: no bundle", seed.Slug))
		case s.inspector == nil:
			reasons = append(reasons, fmt.Sprintf("%s (%s): bundle inspection is unavailable", seed.Slug, bundle))
		default:
			if _, inspectErr := s.resolveHarness(ctx, platformBundle, bundle, ""); inspectErr != nil {
				reasons = append(reasons, fmt.Sprintf("%s (%s): %v", seed.Slug, bundle, inspectErr))
				continue
			}
			// Inspectable now and still not seeded: whatever failed was the
			// write, and SeedBuiltIns returned that error rather than skipping.
			// Reported as its own reason so a puzzling case is not silently
			// left out of a list of them.
			reasons = append(reasons, fmt.Sprintf("%s (%s): bundle is available but no config was created", seed.Slug, bundle))
		}
	}
	if len(reasons) == 0 {
		return fmt.Errorf("project %s has no harness, and no built-in harnesses are defined", projectID)
	}
	return fmt.Errorf("project %s has no harness to run: %s", projectID, strings.Join(reasons, "; "))
}

// updateHarnessConfig saves config, and the egress level a person set on it
// when personLevel is given: the whole-row save never writes that level
// (store.SetHarnessConfigEgress). A level given marks the config's
// discoboxes for an egress push in the same transaction (ADR 26-10-01-032
// §5); a change to the hosts its bundle declares is marked where the bundle is
// re-snapshotted, with its secret routes.
func (s *Service) updateHarnessConfig(ctx context.Context, config *model.HarnessConfig, personLevel *egress.Policy) error {
	return s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		if err := txStore.UpdateHarnessConfig(ctx, config); err != nil {
			return err
		}
		if personLevel != nil {
			if err := txStore.SetHarnessConfigEgress(ctx, config.ProjectID, config.ID, *personLevel); err != nil {
				return err
			}
			config.Revision++
		}
		return s.markEgressChangedTx(ctx, txStore, txDB, config, personLevel != nil)
	})
}

// markEgressChangedTx marks the egress push of config's discoboxes when its
// egress level changed, in the caller's transaction (ADR 26-10-01-032 §5).
func (s *Service) markEgressChangedTx(ctx context.Context, txStore *store.Store, txDB *gorm.DB, config *model.HarnessConfig, changed bool) error {
	if !changed || s.sandboxes == nil {
		return nil
	}
	return s.sandboxes.MarkProxyPolicyChangedTx(ctx, txStore, txDB, config.ProjectID, "", config.ID)
}

// pins is what a harness config pins a sandbox to: its bundle, or its
// manifest file, and the manifest composed from it.
type pins struct {
	bundle       string
	manifestFile string
	manifest     string
}

func configPins(config *model.HarnessConfig) pins {
	return pins{bundle: config.BundleDigest, manifestFile: config.ImageDigest, manifest: config.ManifestDigest}
}

func (p pins) empty() bool {
	return strings.TrimSpace(p.bundle) == "" && strings.TrimSpace(p.manifestFile) == ""
}

// applyResolvedPins is the single place a harness config's resolved bundle
// or manifest file reaches its sandboxes (ADR 0082 §1, ADR 26-10-16-423 §7).
//
// The rule is stated on the fields rather than on a list of callers: wherever
// BundleDigest or ImageDigest is written to a value different from the one it
// replaced, that config's eligible stopped sandboxes are re-pinned onto it. A
// built-in reseeded at startup and a custom harness re-pulled by its owner are
// the same event to a sandbox running it, so they go through the same
// function instead of each remembering independently — and a future writer of
// the fields inherits the behavior by calling this rather than by being
// amended into a list.
//
// Pins that did not move do nothing: the fan-out costs a query per config, and
// SeedBuiltIns runs on every project ensure.
//
// Best effort. The pins are already persisted by the time this runs, so a
// failure here loses the upgrade, not the refresh — and the sandboxes it
// missed keep reporting an available upgrade and are picked up the next time
// this config's pins move.
func (s *Service) applyResolvedPins(ctx context.Context, projectID, configID string, previous, resolved pins) {
	if s.sandboxes == nil || resolved.empty() || previous == resolved {
		return
	}
	if err := s.sandboxes.UpgradeHarnessConfigSandboxes(ctx, projectID, configID); err != nil {
		slog.WarnContext(ctx, "harness bundle resolved but stopped sandboxes were not upgraded",
			"harnessConfigId", configID, "previousBundleDigest", previous.bundle,
			"bundleDigest", resolved.bundle, "previousManifestFileDigest", previous.manifestFile,
			"manifestFileDigest", resolved.manifestFile, "previousManifestDigest", previous.manifest,
			"manifestDigest", resolved.manifest, "error", err)
	}
}

// seeds are the built-in harness configs: every harness in the registry,
// `shell` included. `shell` is an ordinary harness bundle rather than a
// different kind of thing, so nothing here treats it differently (ADR 0043).
func (s *Service) seeds() []harnessdefs.Seed {
	return harnessdefs.Seeds(s.harnesses, s.requireBuiltInImages)
}

// resolveHarness inspects what a harness config is snapshotted from: a
// harness bundle, composed on the platform bundle (ADR 26-10-16-423 §2); or a
// manifest file, which is the whole manifest of a template with no image. The
// result is the bundle's manifest and digest, or the file's with its digest,
// platforms and kind.
func (s *Service) resolveHarness(ctx context.Context, platformBundle *platformBundle, bundleRef, manifestFile string) (imageMetadata, error) {
	bundleRef, manifestFile = strings.TrimSpace(bundleRef), strings.TrimSpace(manifestFile)
	if manifestFile != "" {
		if !isManifestFile(manifestFile) {
			return imageMetadata{}, fmt.Errorf("harness image %q is no manifest file: a harness is registered by its bundle, and no harness is an image", manifestFile)
		}
		if bundleRef != "" {
			return imageMetadata{}, fmt.Errorf("harness %q is a manifest file, which is a whole manifest and takes no bundle", manifestFile)
		}
		return s.inspector.InspectManifestFile(ctx, manifestFile)
	}
	if bundleRef == "" {
		return imageMetadata{}, fmt.Errorf("harness bundle is required: a harness's manifest is read from its bundle")
	}
	base, err := platformBundle.inspect(ctx)
	if err != nil {
		return imageMetadata{}, err
	}
	// The manifest is composed from the labels alone, so it is what admits
	// the bundle to having its layers read.
	var inspected imageMetadata
	harnessBundle, err := s.inspector.InspectBundle(ctx, bundleRef, func(labels map[string]string) error {
		composed, composeErr := composeBundleMetadata(base, bundleInspection{Labels: labels})
		inspected.ImageMetadata = composed
		return composeErr
	})
	if err != nil {
		return imageMetadata{}, err
	}
	inspected.BundleDigest = harnessBundle.Digest
	return inspected, nil
}

// platformBundle is the platform bundle this server's release pins, inspected
// once by whatever resolves several harnesses on it, and only when one needs
// it: a manifest file is composed on nothing.
type platformBundle struct {
	inspector imageInspector
	ref       string
	once      sync.Once
	inspected bundleInspection
	err       error
}

// platformBundle is the platform bundle the service's references name, which
// server configuration resolves (config.Load); none is an error at the first
// harness that needs it, as a seed with no bundle is.
func (s *Service) platformBundle() *platformBundle {
	return &platformBundle{inspector: s.inspector, ref: strings.TrimSpace(s.harnesses.PlatformBundle)}
}

// admitPlatform admits a platform bundle by its layer, which the composition
// of every harness on it reads.
func admitPlatform(labels map[string]string) error {
	if _, ok := labels[harness.ImageLayerLabelPrefix+harness.PlatformLayer]; !ok {
		return fmt.Errorf("platform bundle carries no %s%s layer", harness.ImageLayerLabelPrefix, harness.PlatformLayer)
	}
	return nil
}

func (p *platformBundle) inspect(ctx context.Context) (bundleInspection, error) {
	p.once.Do(func() {
		if p.ref == "" {
			p.err = fmt.Errorf("no platform bundle is configured: a harness's manifest is composed on one")
			return
		}
		p.inspected, p.err = p.inspector.InspectBundle(ctx, p.ref, admitPlatform)
		if p.err != nil {
			p.err = fmt.Errorf("platform bundle: %w", p.err)
		}
	})
	return p.inspected, p.err
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

// snapshotImageMetadata writes the mutable fields a harness image's label
// metadata declares onto config, resolving the run and relaunch commands
// against the convention for the slug it is registered under.
//
// It takes the config rather than returning the fields because every caller
// wants exactly this set on exactly one config, and a snapshot that grows a
// field should not have to grow a return value at three call sites to reach
// them. Identity — id, slug, bundle, digests, Configured — is the caller's;
// this is only what the labels say, the platforms a manifest file's template
// runs on, and the kind of box the harness runs on.
func snapshotImageMetadata(config *model.HarnessConfig, inspected imageMetadata) {
	config.Platforms = inspected.Platforms
	config.ImageKind = inspected.ImageKind
	config.ManifestDigest = manifestDigest(inspected)
	metadata := inspected.ImageMetadata
	image := metadata.Harness
	if image == nil {
		image = &harness.Image{}
	}
	config.SaveBack = image.SaveBack
	config.RunCommand, config.RelaunchCommand = conventionCommands(config.Slug, *image)
	config.ConfigCommand, config.ConfigReminder, config.ConfigPorts = nil, "", nil
	if image.Config != nil {
		config.ConfigCommand = append([]string{}, image.Config.Command...)
		config.ConfigReminder = strings.TrimSpace(image.Config.Reminder)
		config.ConfigPorts = append([]harness.ConfigPort{}, image.Config.Ports...)
	}
	files := make([]model.HarnessConfigFile, 0, len(image.Files))
	for _, file := range image.Files {
		files = append(files, model.HarnessConfigFile{Path: file.Path, Content: file.Content, CreateOnly: file.CreateOnly})
	}
	config.Files = files
	config.Secrets = declaredSecrets(image)
	config.Env = nil
	if len(metadata.Env) > 0 {
		env := make(map[string]string, len(metadata.Env))
		for k, v := range metadata.Env {
			env[k] = v
		}
		config.Env = env
	}
	config.Volumes = append([]harness.Volume{}, metadata.Volumes...)
	config.AdditionalGroups = append([]string{}, metadata.AdditionalGroups...)
	config.ImageEgressHosts = egress.Normalize(image.EgressHosts)
}

// manifestDigest is the digest of the composed manifest a config snapshots:
// its canonical JSON, whose maps encoding/json writes in key order. It moves
// with the harness bundle's labels and the platform's layer they are composed
// on, and with nothing else (ADR 26-10-16-423 §2).
func manifestDigest(inspected imageMetadata) string {
	encoded, err := json.Marshal(inspected.ImageMetadata)
	if err != nil {
		// A manifest that does not marshal pins nothing that matches it, so
		// the config is re-snapshotted rather than kept.
		return ""
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// declaredSecrets is an image's secret declarations as a config snapshots
// them. An image that declares no harness declares none.
func declaredSecrets(image *harness.Image) []model.HarnessConfigSecret {
	if image == nil {
		return []model.HarnessConfigSecret{}
	}
	secrets := make([]model.HarnessConfigSecret, 0, len(image.Secrets))
	for _, secret := range image.Secrets {
		secrets = append(secrets, model.HarnessConfigSecret{
			Name: secret.Name, Required: secret.Required, OneOfGroup: secret.OneOfGroup,
			// Delivery decides whether the sentinel is exported into the
			// harness's environment at all, so dropping it here would leave
			// every later layer with nothing to act on.
			Delivery:       secret.Delivery,
			RejectionScope: rejectionScope(secret.RejectionScope),
			Routes:         declaredRoutes(secret.Routes),
		})
	}
	return secrets
}

// sameSecretDeclarations reports whether a config already snapshots exactly
// these declarations, every field of each.
func sameSecretDeclarations(a, b []model.HarnessConfigSecret) bool {
	return slices.EqualFunc(a, b, func(x, y model.HarnessConfigSecret) bool {
		return x.Name == y.Name && x.Required == y.Required && x.OneOfGroup == y.OneOfGroup && x.Delivery == y.Delivery &&
			slices.EqualFunc(x.RejectionScope, y.RejectionScope, func(p, q model.HarnessSecretRejectionRule) bool {
				return p.Host == q.Host && slices.Equal(p.Paths, q.Paths)
			}) &&
			model.SameRoutes(x.Routes, y.Routes)
	})
}

// rejectionScope carries a harness secret's rejection scope onto the config,
// with each host in the one spelling the proxy reports a destination in.
func rejectionScope(rules []harness.RejectionRule) []model.HarnessSecretRejectionRule {
	if len(rules) == 0 {
		return nil
	}
	out := make([]model.HarnessSecretRejectionRule, 0, len(rules))
	for _, rule := range rules {
		out = append(out, model.HarnessSecretRejectionRule{Host: hostscope.Normalize(rule.Host), Paths: slices.Clone(rule.Paths)})
	}
	return out
}

// declaredRoutes are the routes an image declares for a secret, as the
// config records them: normalized, which the image's validation already
// checked they are.
func declaredRoutes(routes []harnesscapture.Route) []model.SecretRoute {
	var out []model.SecretRoute
	for _, route := range routes {
		out = append(out, model.SecretRoute(route))
	}
	if normalized, err := model.NormalizeRoutes(out); err == nil {
		return normalized
	}
	return out
}
