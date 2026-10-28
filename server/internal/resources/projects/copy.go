package projects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-faster/jx"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/x/id"
)

// copyPlan is a resolved, authorized copy request: which project to copy from
// and what to take. A nil source means nothing is copied.
type copyPlan struct {
	source    *model.Project
	providers bool
	pools     bool
	harnesses bool
}

func (p copyPlan) empty() bool {
	return p.source == nil || (!p.providers && !p.pools && !p.harnesses)
}

// resolveCopyPlan validates the copy inputs before anything is written, so a
// bad source or an unauthorized one fails without leaving a project behind.
func (s *Service) resolveCopyPlan(ctx context.Context, userID string, input services.CreateProjectBody) (copyPlan, error) {
	sourceID := strings.TrimSpace(input.CopyFromProjectId.Or(""))
	if sourceID == "" {
		return copyPlan{}, nil
	}
	var (
		source *model.Project
		err    error
	)
	// A person discobot vouches for may copy from the project discobot signed,
	// and "default" means that one; whether they are in any other is not
	// discobox's to know (ADR 26-10-07-005 §2).
	principal, _ := auth.PrincipalFromContext(ctx)
	if principal.Asserted() {
		if sourceID == "default" {
			sourceID = principal.ProjectID
		}
		if sourceID == "" || sourceID != principal.ProjectID {
			return copyPlan{}, apperrors.NewStatusError(http.StatusForbidden, "source project access denied: the discobot assertion is for another project")
		}
	}
	// POST /projects carries no project path parameter, so the "default" alias
	// the project authorizer resolves for project-scoped routes has to be
	// resolved here instead.
	if sourceID == "default" {
		source, err = s.store.GetDefaultProjectForUser(ctx, userID)
	} else {
		source, err = s.store.GetProject(ctx, sourceID)
	}
	if err != nil {
		return copyPlan{}, apperrors.NotFound(err, "source project not found")
	}
	if !principal.Asserted() {
		member, err := s.store.IsProjectMember(ctx, source.ID, userID)
		if err != nil {
			return copyPlan{}, err
		}
		if !member {
			return copyPlan{}, apperrors.NewStatusError(http.StatusForbidden, "source project access denied")
		}
	}

	plan := copyPlan{source: source}
	if !input.Copy.Set || input.Copy.Null {
		plan.providers, plan.pools, plan.harnesses = true, true, true
		return plan, nil
	}
	for _, item := range input.Copy.Value {
		switch item {
		case serverapi.CreateProjectBodyCopyItemProviders:
			plan.providers = true
		case serverapi.CreateProjectBodyCopyItemPools:
			plan.pools = true
		case serverapi.CreateProjectBodyCopyItemHarnesses:
			plan.harnesses = true
		default:
			return copyPlan{}, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("unknown copy item %q", item))
		}
	}
	// A pool binds to a provider instance in its own project, so there is
	// nothing to bind copied pools to unless the providers come along.
	if plan.pools {
		plan.providers = true
	}
	return plan, nil
}

// copyInto seeds the new project from the plan's source. Ordering is
// deliberate: the database-only copies run first and roll the project back if
// they fail, then pools last, because a pool create schedules a real runtime
// host. A failure part-way through pool creation therefore leaves the project
// and its already-created pools in place rather than orphaning hosts.
func (s *Service) copyInto(ctx context.Context, project *model.Project, plan copyPlan) error {
	if plan.empty() {
		return nil
	}
	source := plan.source
	providerIDs := map[string]string{}
	if plan.providers {
		var err error
		providerIDs, err = s.copyProviders(ctx, source.ID, project.ID)
		if err != nil {
			return s.abandon(ctx, project.ID, fmt.Errorf("copy provider instances: %w", err))
		}
	}
	if plan.harnesses {
		defaultHarnessConfigID, err := s.copyHarnessConfigs(ctx, source, project.ID)
		if err != nil {
			return s.abandon(ctx, project.ID, fmt.Errorf("copy harness configs: %w", err))
		}
		if defaultHarnessConfigID != "" {
			project.DefaultHarnessConfigID = defaultHarnessConfigID
			if err := s.store.UpsertProject(ctx, project); err != nil {
				return s.abandon(ctx, project.ID, err)
			}
		}
	}
	if plan.pools {
		if err := s.copyPools(ctx, source, project, providerIDs); err != nil {
			return fmt.Errorf("copy pools: %w", err)
		}
	}
	return nil
}

// copyProviders duplicates the source project's provider instances and returns
// the source-to-copy ID mapping pools are rebound through.
func (s *Service) copyProviders(ctx context.Context, sourceProjectID, projectID string) (map[string]string, error) {
	sourceProviders, err := s.store.ListSandboxProviderInstances(ctx, sourceProjectID)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]string, len(sourceProviders))
	for i := range sourceProviders {
		sourceProvider := &sourceProviders[i]
		created, err := s.providers.CreateSandboxProviderInstance(ctx, projectID, services.CreateSandboxProviderInstanceBody{
			Type:   sourceProvider.Type,
			Name:   sourceProvider.Name,
			Config: jx.Raw(sourceProvider.Config),
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", sourceProvider.Name, err)
		}
		// Disabled is not a create input, but a disabled provider that comes
		// back enabled would start dialing a backend the user turned off.
		if sourceProvider.Disabled {
			created.Disabled = true
			if err := s.store.UpdateSandboxProviderInstance(ctx, created); err != nil {
				return nil, err
			}
		}
		ids[sourceProvider.ID] = created.ID
	}
	return ids, nil
}

// copyPools recreates the source project's pools against the copied provider
// instances and carries the default-pool choice across.
func (s *Service) copyPools(ctx context.Context, source *model.Project, project *model.Project, providerIDs map[string]string) error {
	sourcePools, err := s.store.ListPools(ctx, source.ID)
	if err != nil {
		return err
	}
	for i := range sourcePools {
		sourcePool := &sourcePools[i]
		providerID, ok := providerIDs[sourcePool.ProviderInstanceID]
		if !ok {
			// The source pool's provider instance was deleted out from under it,
			// so there is nothing to bind the copy to.
			continue
		}
		created, err := s.pools.CreatePool(ctx, project.ID, services.CreatePoolBody{
			Name:               sourcePool.Name,
			ProviderInstanceId: providerID,
			CpuVcpus:           serverapi.NewOptFloat64(sourcePool.CPUVCPUs),
			MemoryBytes:        serverapi.NewOptInt64(sourcePool.MemoryBytes),
			StorageBytes:       serverapi.NewOptInt64(sourcePool.StorageBytes),
		})
		if err != nil {
			return fmt.Errorf("%s: %w", sourcePool.Name, err)
		}
		// A pool's egress level is one of its settings, and a copy of a pool
		// restricted for the network it sits on is restricted the same
		// (ADR 26-10-01-032).
		if err := s.store.SetPoolEgress(ctx, project.ID, created.ID, sourcePool.Egress); err != nil {
			return fmt.Errorf("%s: %w", sourcePool.Name, err)
		}
		// The copy's default pool is the copy's own, not the one it was
		// copied from.
		if source.DefaultPoolID == sourcePool.ID {
			project.DefaultPoolID = created.ID
			if err := s.store.UpsertProject(ctx, project); err != nil {
				return err
			}
		}
	}
	return nil
}

// copyHarnessConfigs carries the source project's configured harnesses across
// and returns the copy of its default harness config, if it had one.
//
// The built-in harnesses are already seeded in the new project against the
// server's current images, so a built-in copy overlays only what the configure
// flow produced — files, the secrets it minted, and the Configured flag — and
// the egress level a person set, and
// never the image or its snapshotted label metadata, which would otherwise pin
// the new project to whatever image the source happened to be seeded from.
func (s *Service) copyHarnessConfigs(ctx context.Context, source *model.Project, projectID string) (string, error) {
	sourceConfigs, err := s.store.ListHarnessConfigs(ctx, source.ID)
	if err != nil {
		return "", err
	}
	// Two harness configs may bind the same secret, and a secret must be copied
	// once so both bindings point at one credential in the new project.
	secretIDs := map[string]string{}
	defaultHarnessConfigID := ""
	for i := range sourceConfigs {
		sourceConfig := &sourceConfigs[i]
		config, err := s.copyHarnessConfig(ctx, sourceConfig, projectID, secretIDs)
		if err != nil {
			return "", fmt.Errorf("%s: %w", sourceConfig.Slug, err)
		}
		if source.DefaultHarnessConfigID == sourceConfig.ID {
			defaultHarnessConfigID = config.ID
		}
	}
	return defaultHarnessConfigID, s.copyProjectBindings(ctx, source.ID, projectID, secretIDs)
}

func (s *Service) copyHarnessConfig(ctx context.Context, sourceConfig *model.HarnessConfig, projectID string, secretIDs map[string]string) (*model.HarnessConfig, error) {
	bindings, err := s.store.ListSecretBindings(ctx, sourceConfig.ProjectID, store.SecretBindingFilter{TargetKind: model.SecretBindingTargetHarnessConfig, TargetID: sourceConfig.ID})
	if err != nil {
		return nil, err
	}
	configuredSecretIDs := make([]string, 0, len(sourceConfig.ConfiguredSecretIDs))
	for _, sourceSecretID := range sourceConfig.ConfiguredSecretIDs {
		secretID, err := s.copySecret(ctx, sourceConfig.ProjectID, projectID, sourceSecretID, secretIDs)
		if err != nil {
			return nil, err
		}
		if secretID != "" {
			configuredSecretIDs = append(configuredSecretIDs, secretID)
		}
	}

	config, err := s.store.GetHarnessConfigBySlug(ctx, projectID, sourceConfig.Slug)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if config == nil {
		// Not one of the seeded built-ins: a user-created harness config, copied
		// whole. Its image is its own identity, so it does come across.
		config = &model.HarnessConfig{}
		*config = *sourceConfig
		config.ID = id.NewString(id.PrefixHarnessConfig)
		config.ProjectID = projectID
		config.ConfigureSandboxID = ""
		config.ConfigureError = ""
		config.ConfiguredSecretIDs = configuredSecretIDs
		if err := s.store.CreateHarnessConfig(ctx, config); err != nil {
			return nil, err
		}
	} else {
		config.Configured = sourceConfig.Configured
		config.ConfiguredFiles = sourceConfig.ConfiguredFiles
		config.ConfiguredSecretIDs = configuredSecretIDs
		if err := s.store.UpdateHarnessConfig(ctx, config); err != nil {
			return nil, err
		}
		// The egress level a person set is theirs, like the configure flow's
		// result; the hosts the image declares come with the new project's own
		// seeding.
		if err := s.store.SetHarnessConfigEgress(ctx, projectID, config.ID, sourceConfig.Egress); err != nil {
			return nil, err
		}
	}

	return config, s.copyBindings(ctx, sourceConfig.ProjectID, projectID, bindings, config.ID, secretIDs)
}

// copyBindings carries bindings across onto the copy of their target, each a
// new binding of the copy of its secret, with the same routes, lifetime and
// purpose, recorded as made by the system for the copy. A harness config's
// and the project's are copied; a discobox's are not: the new project has no
// discoboxes.
func (s *Service) copyBindings(ctx context.Context, sourceProjectID, projectID string, bindings []model.SecretBinding, targetID string, secretIDs map[string]string) error {
	for i := range bindings {
		source := &bindings[i]
		secretID, err := s.copySecret(ctx, sourceProjectID, projectID, source.SecretID, secretIDs)
		if err != nil {
			return err
		}
		if secretID == "" {
			continue
		}
		binding := &model.SecretBinding{
			ProjectID:  projectID,
			SecretID:   secretID,
			TargetKind: source.TargetKind,
			TargetID:   targetID,
			Use:        source.Use,
			Delegate:   source.Delegate,
			EnvName:    source.EnvName,
			Routes:     source.Routes,
			Raw:        source.Raw,
			ExpiresAt:  source.ExpiresAt,
			CreatedBy:  source.CreatedBy,
		}
		if err := s.store.CreateSecretBinding(ctx, binding, store.BindingChange{Actor: model.SecretBindingActorSystem, Cause: "copy:" + source.ID}); err != nil {
			return err
		}
	}
	return nil
}

// copyProjectBindings carries the source project's own bindings across.
func (s *Service) copyProjectBindings(ctx context.Context, sourceProjectID, projectID string, secretIDs map[string]string) error {
	bindings, err := s.store.ListSecretBindings(ctx, sourceProjectID, store.SecretBindingFilter{TargetKind: model.SecretBindingTargetProject, TargetID: sourceProjectID})
	if err != nil {
		return err
	}
	return s.copyBindings(ctx, sourceProjectID, projectID, bindings, projectID, secretIDs)
}

// copySecret duplicates one secret into the new project, once. Sealing binds
// ciphertext to project and secret ID, so the value is opened and re-sealed
// under the copy's identity rather than moved as bytes.
func (s *Service) copySecret(ctx context.Context, sourceProjectID, projectID, sourceSecretID string, secretIDs map[string]string) (string, error) {
	sourceSecretID = strings.TrimSpace(sourceSecretID)
	if sourceSecretID == "" {
		return "", nil
	}
	if secretID, ok := secretIDs[sourceSecretID]; ok {
		return secretID, nil
	}
	sourceSecret, err := s.store.GetSecret(ctx, sourceProjectID, sourceSecretID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// A binding or configured-secret ID that outlived its secret is not
			// worth failing the whole project copy over.
			return "", nil
		}
		return "", err
	}
	value, err := s.store.OpenSecretValue(ctx, sourceSecret)
	if err != nil {
		return "", err
	}
	var plaintext []byte
	if value != nil {
		//nolint:gosec // Secret values are marshaled before store encryption re-seals them.
		if plaintext, err = json.Marshal(value); err != nil {
			return "", err
		}
	}
	secretID := id.NewString(id.PrefixSecret)
	secret := &model.Secret{
		ID:             secretID,
		ProjectID:      projectID,
		Name:           sourceSecret.Name,
		Type:           sourceSecret.Type,
		Routes:         sourceSecret.Routes,
		Anonymous:      sourceSecret.Anonymous,
		Format:         sourceSecret.Format,
		FormatSet:      sourceSecret.FormatSet,
		MaxBindingTTL:  sourceSecret.MaxBindingTTL,
		AllowRaw:       sourceSecret.AllowRaw,
		EncryptedValue: plaintext,
	}
	if err := s.store.CreateSecret(ctx, secret); err != nil {
		return "", err
	}
	secretIDs[sourceSecretID] = secret.ID
	return secret.ID, nil
}
