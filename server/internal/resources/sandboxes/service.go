package sandboxes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/sourcecache"

	"github.com/discobox-ai/discobox/pkg/platform"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/pkg/sandboxmeta"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/reconcile"

	"github.com/discobox-ai/discobox/server/internal/auth"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/harnessdefs"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/x/id"

	sandboxauth "github.com/discobox-ai/discobox/server/internal/auth/sandbox"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

// Service owns sandbox API behavior, orchestration, provider catalog state, and
// sandbox auth dependencies.
type Service struct {
	store            *store.Store
	engine           *reconcile.Engine
	sandboxProviders *sandbox.ProviderManager
	sandboxAuth      *sandboxauth.Manager
	defaultUserID    string
	// archiveRetention is the server-wide default retention for archived
	// sandboxes; zero leaves DefaultArchiveRetention in force.
	archiveRetention time.Duration
	// serverPeerID is this server's own peer ID, the <server> of the
	// discobox://<server>/<discobox> address a sandbox is started with. Empty
	// when the server is not reachable at it, and then no address is given.
	serverPeerID string
	// secrets prepares the bindings a create gives the new discobox, which
	// holds a discobox giving them to what it was delegated (ADR
	// 26-10-27-880 §3).
	secrets *resourcesecrets.Service
	// release answers the bundles the release names that every delivered
	// sandbox is pinned to: the platform and the agent (ADR 26-10-16-423 §7,
	// ADR 26-10-24-953 §2). Nil pins none.
	release ReleaseBundles
}

// ReleaseBundles is what the release's bundles are pinned to.
type ReleaseBundles interface {
	// CurrentRelease resolves the release's references, at most as stale as
	// the resolver keeps them; each zero when the server names none.
	CurrentRelease(ctx context.Context) (services.ReleasePins, error)
	// LatestRelease is what the references last resolved to, without asking
	// again; each zero when it has not resolved.
	LatestRelease() services.ReleasePins
}

func NewService(store *store.Store, manager *sandbox.ProviderManager, defaultUserID string, engine *reconcile.Engine) *Service {
	svc := &Service{
		store:            store,
		engine:           engine,
		sandboxProviders: manager,
		defaultUserID:    defaultUserID,
	}
	return svc
}

// RegisterJobs installs the sandbox reconciler, and the one that delivers a
// sandbox's sentinels when a secret's routes change, on the level-triggered
// reconcile engine.
func (s *Service) RegisterJobs(opts ...reconcile.RegisterOption) error {
	if s.engine == nil {
		return errors.New("reconcile engine is required")
	}
	if err := s.engine.Register(SandboxResourceType, s.NewSandboxReconciler(), opts...); err != nil {
		return err
	}
	if err := s.engine.Register(SentinelsResourceType, s.NewSentinelReconciler()); err != nil {
		return err
	}
	if err := s.engine.Register(SandboxProxyPolicyResourceType, &sandboxProxyPolicyReconciler{store: s.store, providers: s.sandboxProviders}); err != nil {
		return err
	}
	if err := s.engine.Register(SandboxPowerResourceType, &sandboxPowerReconciler{store: s.store, providers: s.sandboxProviders, now: time.Now}); err != nil {
		return err
	}
	// Applies the sandbox events' retention (ADR 26-10-29-316 §4).
	return s.engine.Register(SandboxEventPruneResourceType, &sandboxEventPruneReconciler{store: s.store, now: time.Now})
}

func (s *Service) SetSandboxAuthManager(manager *sandboxauth.Manager) {
	s.sandboxAuth = manager
}

// SetArchiveRetention records the server-wide default retention for archived
// sandboxes. Projects that have set their own are unaffected; see
// WithArchiveRetention.
func (s *Service) SetArchiveRetention(retention time.Duration) {
	s.archiveRetention = retention
}

// SetServerPeerID records this server's peer ID, which names it in the address
// a sandbox is started with; see WithServerPeerID.
func (s *Service) SetServerPeerID(id string) {
	s.serverPeerID = strings.TrimSpace(id)
}

// SetSecrets gives the service the credential broker a create's bindings
// are prepared by.
func (s *Service) SetSecrets(secrets *resourcesecrets.Service) {
	s.secrets = secrets
}

// SetReleaseBundles gives the service what pins a sandbox to the release's
// bundles.
func (s *Service) SetReleaseBundles(release ReleaseBundles) {
	s.release = release
}

// SandboxProviderCatalogItem describes a registered sandbox provider type.
type SandboxProviderCatalogItem struct {
	ID           string
	Name         string
	Icon         string
	Description  string
	Capabilities ProviderStatus
	ConfigFields []ProviderConfigField
}

// ListSandboxes answers with the project's discoboxes.
func (s *Service) ListSandboxes(ctx context.Context, projectID, sourceRoot string, originKeys []string, tags []sandboxmeta.Selector) ([]model.Sandbox, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	sandboxes, err := s.store.ListSandboxes(ctx, projectID, sourceRoot, originKeys)
	if err != nil {
		return nil, err
	}
	return matchingTags(sandboxes, tags), nil
}

func (s *Service) CreateSandbox(ctx context.Context, projectID string, input services.CreateSandboxBody) (*model.Sandbox, error) {
	config := input.Config
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	poolID := services.OptStringPtr(input.PoolId)
	if poolID == nil && project.DefaultPoolID != "" {
		id := project.DefaultPoolID
		poolID = &id
	}
	if poolID == nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "sandbox pool is required")
	}
	pool, err := s.store.GetPool(ctx, projectID, *poolID)
	if err != nil {
		return nil, apperrors.NotFound(err, "pool not found")
	}
	provider, err := s.store.GetSandboxProviderInstance(ctx, projectID, pool.ProviderInstanceID)
	if err != nil {
		return nil, apperrors.NotFound(err, "provider instance not found")
	}
	if provider.Disabled {
		return nil, fmt.Errorf("provider instance disabled")
	}
	harnessConfigID, err := s.resolveHarnessConfigID(ctx, project, config.HarnessConfigId, input.HarnessName)
	if err != nil {
		return nil, err
	}

	if strings.TrimSpace(config.Name) == "" {
		return nil, fmt.Errorf("sandbox name is required")
	}
	// The account boot creates, and the one the pool agent chowns a source tree
	// to before the sandbox exists; a user it cannot create with a usable uid
	// is refused here rather than failing, or landing on root, later (ADR 0141).
	// The rule is the sandbox's platform's, and every sandbox this control plane
	// records is a Linux one: a platform without POSIX ids refuses any user but
	// its one account (ADR 0145 §5).
	user := services.SandboxUserToModel(config.User)
	if err := user.ValidateAccount("linux"); err != nil {
		return nil, err
	}
	// Names are unique within a project (idx_sandbox_project_name) because they
	// are an addressable handle, not just a label: `discobox admin ssh-config` emits
	// one as an ssh_config Host alias, and ssh applies the first matching block,
	// so a second sandbox answering to the same name would silently take the
	// first one's connections.
	taken, err := s.store.SandboxNameTaken(ctx, projectID, config.Name)
	if err != nil {
		return nil, err
	}
	if taken {
		return nil, apperrors.NewStatusError(http.StatusConflict,
			fmt.Sprintf("a sandbox named %q already exists in this project", config.Name))
	}
	// A discobox a sandbox creates is created as the user who created that
	// sandbox (ADR 0140 §4).
	userID := s.defaultUserID
	if actingUserID, err := auth.ActingUserID(ctx); err == nil {
		userID = actingUserID
	}
	// The sandbox creating this one, when a sandbox is (ADR 26-09-24-630 §1).
	var createdBySandboxID *string
	if principal, ok := auth.PrincipalFromContext(ctx); ok && principal.Type == auth.PrincipalTypeSandbox {
		if len(config.Secrets) > 0 {
			// Inline secrets put a value inside the new discobox where anything
			// in it can read it. A sandbox gives another discobox a secret it
			// was delegated, as a binding and a sentinel (bindings, below).
			return nil, apperrors.NewStatusError(http.StatusForbidden,
				"a discobox gives a new discobox a secret through bindings, never its value")
		}
		createdBySandboxID = &principal.SandboxID
	}
	sandboxID, err := id.New(id.PrefixSandbox)
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
	features, err := services.SandboxFeaturesToModel(config.Features.Value)
	if err != nil {
		return nil, err
	}
	skillsSources := services.SandboxSkillsSourcesToModel(config.SkillsSources)
	if err := validateSkillsSources(source, sourceCodeReferences, skillsSources); err != nil {
		return nil, err
	}
	services.DefaultGitSourceSlugs(source, sourceCodeReferences)
	var sourceRoot *string
	if root := source.Root(); root != "" {
		sourceRoot = &root
	}
	origin := services.OriginToModel(input.Origin)
	var originKey *string
	if key := model.SandboxOriginKey(origin, source); key != "" {
		originKey = &key
	}
	if err := resolveSourceDelivery(sandboxID, source, sourceCodeReferences, origin); err != nil {
		return nil, err
	}
	git := services.SandboxGitToModel(config.Git)
	skills, err := services.SandboxSkillsToModel(config.Skills)
	if err != nil {
		return nil, err
	}
	harnessMode := sandboxconfig.HarnessModeRun
	if mode, ok := config.HarnessMode.Get(); ok {
		harnessMode = string(mode)
	}
	harnessConfig, err := s.store.GetHarnessConfig(ctx, projectID, harnessConfigID)
	if err != nil {
		return nil, apperrors.NotFound(err, "harness config not found")
	}
	// The sandbox runs on its pool's platform and is the kind of box its pool
	// runs: a harness is a bundle, whose steps select the platform they run
	// on, and rules no pool out (ADR 26-10-16-423 §2, ADR 26-11-01-122). A
	// pool whose agent has not declared its platform yet answers none, and
	// the sandbox takes it when it is placed (store.SchedulablePoolForSandbox).
	sandboxPlatform := pool.Platform
	if err := validateSourceWorkingDirectory(source, sandboxPlatform); err != nil {
		return nil, err
	}
	// A harness is only selectable once its configure flow has succeeded.
	// harnessMode "config" is exempt: that is the configure flow itself.
	if harnessMode != sandboxconfig.HarnessModeConfig && !harnessConfig.Configured {
		return nil, apperrors.NewStatusError(http.StatusConflict,
			fmt.Sprintf("harness %q is not configured; run `discobox admin harness configure %s` first", harnessConfig.Slug, harnessConfig.Slug))
	}
	// A run and the configure flow alike are pinned to the harness config's
	// bundle, by digest: the tag is rebuilt in place by dev workflows, so it
	// alone does not say which bundle this sandbox applies (ADR 0016 §1), and
	// the configure command a configure flow runs is delivered in it.
	harnessPins, err := createHarnessPins(harnessConfig)
	if err != nil {
		return nil, err
	}
	// Every box applies the platform, whatever its harness, and is placed
	// with the agent: both are pinned here and moved only by a re-pin (ADR
	// 26-10-16-423 §7, ADR 26-10-24-953 §2). A bundle the server names and
	// cannot resolve fails the create rather than leave a box pinned to none,
	// and a box on a bundle needs both.
	release, err := s.currentRelease(ctx)
	if err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadGateway, err.Error())
	}
	if harnessPins.Bundle.Reference != "" && release.Platform.Digest == "" {
		return nil, apperrors.NewStatusError(http.StatusServiceUnavailable, "this server names no platform bundle, which every discobox on a harness bundle applies")
	}
	if harnessPins.Bundle.Reference != "" && release.Agent.Digest == "" {
		return nil, apperrors.NewStatusError(http.StatusServiceUnavailable, "this server names no agent bundle, which every discobox on a harness bundle is placed with")
	}
	sandbox := &model.Sandbox{
		ID:                 sandboxID,
		ProjectID:          projectID,
		CreatedByUserID:    userID,
		CreatedBySandboxID: createdBySandboxID,
		PoolID:             pool.ID,
		Platform:           sandboxPlatform,
		Name:               config.Name,
		Description:        services.OptStringPtr(config.Description),
		SandboxManifest: model.SandboxManifest{
			HarnessConfigID:       &harnessConfigID,
			HarnessMode:           harnessMode,
			Model:                 services.OptStringPtr(config.Model),
			ModelServiceTier:      services.OptStringPtr(config.ModelServiceTier),
			ModelReasoningLevel:   services.OptStringPtr(config.ModelReasoningLevel),
			Prompt:                config.Prompt,
			HarnessManifestDigest: harnessPins.ManifestDigest,
			Env:                   map[string]string(config.Env.Or(nil)),
			Source:                source,
			SourceCodeReferences:  sourceCodeReferences,
			UserName:              user.Name,
			UserUID:               user.UID,
			UserGID:               user.GID,
			UserGroupName:         user.GroupName,
			UserAdditionalGroups:  user.AdditionalGroups,
			HomeDirectory:         user.HomeDirectory,
			GitUserName:           git.UserName,
			GitUserEmail:          git.UserEmail,
			Skills:                skills,
			SkillsSources:         skillsSources,
			Features:              features,
		},
		SourceRoot: sourceRoot,
		Origin:     origin,
		OriginKey:  originKey,
	}
	sandbox.SetPins(release.Platform, harnessPins.Bundle)
	if release.Platform.Digest != "" {
		sandbox.SetAgentPin(release.Agent)
	}
	if level, ok := config.Egress.Get(); ok {
		if sandbox.Egress, err = services.EgressPolicyToModel(level); err != nil {
			return nil, err
		}
	}
	// Whether the new discobox may call the discobox API (ADR 26-10-27-880
	// §9). A discobox creating one passes on only what it has. The flag is not
	// confined to credentials it holds: a discobox with it may create children
	// on any harness config in the project, and each runs with its harness
	// config's credentials.
	if sandbox.DiscoboxAPI = input.DiscoboxApi.Or(false); sandbox.DiscoboxAPI && createdBySandboxID != nil {
		creator, err := s.store.GetSandbox(ctx, projectID, *createdBySandboxID)
		if err != nil {
			return nil, apperrors.NotFound(err, "the creating discobox was not found")
		}
		if !creator.DiscoboxAPI {
			return nil, apperrors.NewStatusError(http.StatusForbidden,
				"a discobox gives the discobox API only when it has it itself")
		}
	}
	if createdBySandboxID != nil {
		if err := s.refuseWiderThanCreator(ctx, *createdBySandboxID, sandbox); err != nil {
			return nil, err
		}
	}
	var prepared resourcesecrets.SandboxBindings
	if len(input.Bindings) > 0 {
		if s.secrets == nil {
			return nil, apperrors.NewStatusError(http.StatusServiceUnavailable, "this server cannot give a new discobox bindings")
		}
		if prepared, err = s.secrets.PrepareSandboxBindings(ctx, projectID, sandbox, input.Bindings); err != nil {
			return nil, err
		}
	}
	// The variables the create gives the sandbox itself, which a binding of
	// the harness config or the project for the same variable gives way to
	// (ADR 26-10-27-880 §2).
	ownEnvs := map[string]struct{}{}
	for _, binding := range prepared.Bindings {
		if binding.Use {
			ownEnvs[binding.EnvName] = struct{}{}
		}
	}
	for _, in := range config.Secrets {
		env := strings.TrimSpace(in.Env)
		if _, dup := ownEnvs[env]; dup {
			return nil, apperrors.NewStatusError(http.StatusBadRequest,
				fmt.Sprintf("%s is given to the new discobox twice; one environment variable carries one credential", env))
		}
	}
	for _, in := range config.Secrets {
		ownEnvs[strings.TrimSpace(in.Env)] = struct{}{}
	}
	if err := checkRequiredSecrets(ctx, s.store, sandbox, harnessConfigID, ownEnvs); err != nil {
		return nil, err
	}
	inlineSecrets, inline, err := inlineSandboxBindings(ctx, projectID, sandbox, config.Secrets)
	if err != nil {
		return nil, err
	}
	sandbox.SourceDeliveries = map[string]sourcecache.Delivery{}
	for _, entry := range clientDeliveredSources(sandbox) {
		if entry.source.CacheKey != nil && strings.HasPrefix(*entry.source.CacheKey, "remote/") && entry.source.Slug != nil {
			sandbox.SourceDeliveries[*entry.source.Slug] = sourcecache.Delivery{Status: "pending"}
		}
	}
	return s.createSandboxIntent(ctx, sandbox, inlineSecrets, inline, prepared)
}

// createHarnessPins is what a sandbox created on config is pinned to: its
// bundle (services.HarnessConfigPins). A config with none was registered
// before bundles, and one whose bundle has no digest
// is a development bundle no pool has built yet: no box can apply either.
func createHarnessPins(config *model.HarnessConfig) (services.HarnessPins, error) {
	pins, ok := services.HarnessConfigPins(config)
	switch {
	case ok:
		return pins, nil
	case pins.Bundle.Reference != "":
		return services.HarnessPins{}, apperrors.NewStatusError(http.StatusServiceUnavailable,
			fmt.Sprintf("harness %q's bundle %s has not been built yet: a pool's daemon builds a development bundle once the pool is ready, and the harness follows it then", config.Slug, pins.Bundle.Reference))
	default:
		return services.HarnessPins{}, apperrors.NewStatusError(http.StatusConflict,
			fmt.Sprintf("harness %q was registered before bundles, and no harness runs as an image any more: register its bundle with `discobox admin harness update %s --bundle <reference>`", config.Slug, config.Slug))
	}
}

// refuseOtherPlatform refuses a sandbox of sandboxPlatform on a pool that
// hosts another, with the platforms as the reason (ADR 0145 §1). It is the
// placement check as import makes it, for a sandbox whose platform its tree
// already fixed, before anything is written. A pool that has not declared
// what it hosts is not refused here: one whose agent has not reported yet — a
// new machine's, which is where a transfer usually goes — is waited for by
// the provider's placement (store.SchedulablePoolForSandbox), which checks the
// platform once it is declared, and refuses a pool that never declares one,
// before the tree is read (ADR 0145 §8).
func refuseOtherPlatform(sandboxPlatform platform.Platform, pool *model.Pool) error {
	if pool.Platform.IsZero() {
		return nil
	}
	if err := platform.Place(sandboxPlatform, pool.Platform); err != nil {
		return apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf("pool %q: %v", pool.Name, err))
	}
	return nil
}

// resolveHarnessConfigID is which harness a sandbox runs: what the request
// names, else the project default, else nothing — which is an error (ADR 0048).
//
// The chain does not end at the built-in `shell`. Ending there would answer a
// project with a harness configured and no default set with a shell, which is
// indistinguishable from a working setup until you are inside the sandbox
// wondering where the harness went.
// harnessConfigBySelector is the harness config a `--harness` selector names,
// wherever one is taken — create's harnessName, an import's --harness: its
// stable slug (e.g. "codex"), then its display name, then its ID. None
// answering is store.ErrNotFound.
func (s *Service) harnessConfigBySelector(ctx context.Context, projectID, selector string) (*model.HarnessConfig, error) {
	config, err := s.store.GetHarnessConfigBySlug(ctx, projectID, selector)
	if !errors.Is(err, store.ErrNotFound) {
		return config, err
	}
	config, err = s.store.GetHarnessConfigByName(ctx, projectID, selector)
	if !errors.Is(err, store.ErrNotFound) {
		return config, err
	}
	return s.store.GetHarnessConfig(ctx, projectID, selector)
}

func (s *Service) resolveHarnessConfigID(ctx context.Context, project *model.Project, harnessConfigID, harnessName services.OptString) (string, error) {
	if project == nil {
		return "", fmt.Errorf("project is required")
	}
	if id, ok := harnessConfigID.Get(); ok && id != "" {
		config, err := s.store.GetHarnessConfig(ctx, project.ID, id)
		if err != nil {
			return "", apperrors.NotFound(err, "harness config not found")
		}
		return config.ID, nil
	}
	name, ok := harnessName.Get()
	if ok && strings.TrimSpace(name) != "" {
		config, err := s.harnessConfigBySelector(ctx, project.ID, strings.TrimSpace(name))
		if err != nil {
			return "", apperrors.NotFound(err, "harness config not found")
		}
		return config.ID, nil
	}
	// No explicit selector: pin the project default so the sandbox always carries a
	// concrete harness config. Resolving the harness at create time is what makes its
	// required-secret gate and binding materialization apply to `run .`.
	if strings.TrimSpace(project.DefaultHarnessConfigID) != "" {
		config, err := s.store.GetHarnessConfig(ctx, project.ID, project.DefaultHarnessConfigID)
		if err == nil {
			return config.ID, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return "", apperrors.NotFound(err, "harness config not found")
		}
		// The default was deleted. That is an absent default like any other.
	}
	return "", apperrors.NewStatusError(http.StatusConflict,
		"no harness given and this project has no default; pass --harness, or set one with `discobox admin harness set-default <harness>`")
}

func (s *Service) GetSandbox(ctx context.Context, projectID, sandboxID string) (*model.Sandbox, error) {
	sandbox, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, apperrors.NotFound(err, "sandbox not found")
	}
	return sandbox, nil
}

// AcquireSandboxHTTPClient leases an HTTP client onto the sandbox for the
// caller's scopes. It is the choke point every route into a sandbox goes
// through, and use is what the route is for (ADR 26-10-12-711 §1).
//
// A read or an observe answers now: the sandbox exists, and its pool is up.
// Whether the sandbox is started for it is the pool agent's to carry out, by
// the mark the lease puts on every token.
//
// An action is brought to a sandbox that can take it before the lease is
// handed out (acquireForAction): it waits for one still being provisioned, has
// the pool start it, and repairs it once if it failed and has no container.
// Nothing has been forwarded at that point, so nothing has to be replayed.
func (s *Service) AcquireSandboxHTTPClient(ctx context.Context, projectID, sandboxID string, scopes []string, use services.SandboxUse) (*services.HTTPClientLease, *model.Sandbox, error) {
	// A discobox reads and types into another's terminal one call at a time,
	// and does not attach to it (ADR 26-10-01-397 §2).
	if principal, ok := auth.PrincipalFromContext(ctx); ok && use.Attach && principal.Type == auth.PrincipalTypeSandbox {
		return nil, nil, apperrors.NewStatusError(http.StatusForbidden, "a discobox does not attach to another")
	}
	if err := authorizeRequestedScopes(ctx, scopes); err != nil {
		return nil, nil, err
	}
	return s.acquireForUse(ctx, projectID, sandboxID, scopes, use)
}

// AcquireSandboxHTTPClientForServer is the same acquire for a call Discobox
// makes itself rather than on behalf of somebody: acknowledging a committed
// harness capture receipt (ADR 26-10-26-573). There are no caller scopes to
// check, because there is no caller — the scopes are this code's own, and the
// route they reach is the one they name.
//
// As an action it waits: a sandbox on a pool the server has not heard from
// since it started is the ordinary state for the first heartbeat after a
// restart. The caller bounds the wait with its context.
func (s *Service) AcquireSandboxHTTPClientForServer(ctx context.Context, projectID, sandboxID string, scopes []string, use services.SandboxUse) (*services.HTTPClientLease, *model.Sandbox, error) {
	return s.acquireForUse(ctx, projectID, sandboxID, scopes, use)
}

func (s *Service) acquireForUse(ctx context.Context, projectID, sandboxID string, scopes []string, use services.SandboxUse) (*services.HTTPClientLease, *model.Sandbox, error) {
	if use.Kind == sandbox.UseAction {
		return s.acquireForAction(ctx, projectID, sandboxID, scopes, use)
	}
	return s.acquireSandboxHTTPClient(ctx, projectID, sandboxID, scopes, use.Kind)
}

// acquireSandboxHTTPClient is one look: the row, its pool, and a lease marked
// with use. It waits for nothing and starts nothing.
func (s *Service) acquireSandboxHTTPClient(ctx context.Context, projectID, sandboxID string, scopes []string, use sandbox.Use) (*services.HTTPClientLease, *model.Sandbox, error) {
	sandboxModel, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, nil, apperrors.NotFound(err, "sandbox not found")
	}
	if sandboxModel.DesiredState == model.DesiredStateDeleted {
		return nil, sandboxModel, apperrors.NewStatusError(http.StatusConflict, "sandbox is being deleted")
	}
	// An action is not forwarded into a sandbox archived by intent, and says
	// what undoes that. A read or an observe is left to the pool agent, which
	// refuses it in its own words.
	if use == sandbox.UseAction && sandboxModel.DesiredState == model.DesiredStateArchived {
		return nil, sandboxModel, apperrors.NewStatusError(http.StatusConflict, "sandbox is archived; unarchive it first")
	}
	pool, err := s.store.GetPool(ctx, projectID, sandboxModel.PoolID)
	if err != nil {
		return nil, sandboxModel, apperrors.NotFound(err, "sandbox pool not found")
	}
	// The gate is liveness, not convergence: `offline` is the reconciler's
	// verdict that the pool agent stopped answering, and Ready is the agent's
	// own word on taking work. A pool whose reconcile is failing for other
	// reasons (State active, ErrorMessage set) still serves the sandboxes it
	// already hosts, so traffic onto them must not be refused for it.
	if !pool.IsReady() {
		return nil, sandboxModel, apperrors.StatusError{
			Status:  http.StatusConflict,
			Message: fmt.Sprintf("sandbox pool is not reachable: pool=%s state=%s ready=%t", pool.ID, pool.State, pool.IsReady()),
			// A pool that is not up yet is a condition a wait can resolve, and
			// the sentinel is how an action's wait tells it apart from the
			// refusals that are answers.
			Cause: sandbox.ErrPoolNotReachable,
		}
	}
	if s.sandboxProviders == nil {
		return nil, nil, fmt.Errorf("sandbox provider manager is required")
	}
	provider, err := s.sandboxProviders.ResolveForSandbox(ctx, sandboxModel)
	if err != nil {
		return nil, nil, err
	}
	lease, err := provider.AcquireHTTPClient(ctx, sandboxRefFromSandbox(sandboxModel), sandboxModel.ProviderState, scopes, use)
	if err != nil {
		// The row outlives the host: a pool whose container is being replaced,
		// or whose agent has not passed its healthcheck yet, still reads ready
		// until its agent is noticed gone. The driver that can see that says
		// so, and it is the same condition as the gate above.
		if errors.Is(err, sandbox.ErrPoolNotReachable) {
			return nil, sandboxModel, apperrors.StatusError{Status: http.StatusConflict, Message: err.Error(), Cause: err}
		}
		return nil, nil, err
	}
	return lease, sandboxModel, nil
}

func authorizeRequestedScopes(ctx context.Context, scopes []string) error {
	if len(scopes) == 0 {
		return nil
	}
	principal, ok := auth.PrincipalFromContext(ctx)
	if ok && principal.Type == auth.PrincipalTypeSandbox {
		// A sandbox holds no scopes. The sandbox role admits four kinds of
		// call of its that reach into a discobox in its project: the push
		// into its origin (ADR 26-09-24-630 §2), its terminals and services
		// (ADR 26-10-01-397 §1, ADR 26-10-14-320 §3), writing its meta file
		// (ADR 26-10-08-447 §1), and HTTP to one of its ports (ADR
		// 26-10-14-320 §3). Those scopes are all this allows, so a route added
		// to the role by mistake reaches no legacy terminal or tunnel.
		// Attaching is refused before this (AcquireSandboxHTTPClient).
		for _, scope := range scopes {
			switch scope {
			case poolagentauth.ScopeSandboxWrite, poolagentauth.ScopeExecRead, poolagentauth.ScopeExecWrite, poolagentauth.ScopeSandboxHTTP:
			default:
				return apperrors.NewStatusError(http.StatusForbidden, "a discobox may only push source into another, drive its terminals and services, change its meta, or reach its ports over HTTP: "+scope)
			}
		}
		return nil
	}
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
		return nil, apperrors.NotFound(err, "sandbox not found")
	}

	var change *model.SandboxHarnessChange
	if config, ok := input.Config.Get(); ok {
		change, err = s.prepareHarnessChange(ctx, sandbox, config)
		if err != nil {
			return nil, err
		}
	}
	egressChanged := false
	if config, ok := input.Config.Get(); ok {
		if name, ok := config.Name.Get(); ok {
			sandbox.Name = name
		}
		if level, ok := config.Egress.Get(); ok {
			if sandbox.Egress, err = services.EgressPolicyToModel(level); err != nil {
				return nil, err
			}
			egressChanged = true
		}
	}
	// Whether the discobox may call the discobox API is a person's to change
	// (ADR 26-10-27-880 §9); the sandbox role reaches no update anyway.
	discoboxAPI, discoboxAPIChanged := input.DiscoboxApi.Get()
	if discoboxAPIChanged {
		if principal, ok := auth.PrincipalFromContext(ctx); ok && principal.Type == auth.PrincipalTypeSandbox {
			return nil, apperrors.NewStatusError(http.StatusForbidden, "whether a discobox may call the discobox API is a person's to change")
		}
		sandbox.DiscoboxAPI = discoboxAPI
	}

	if err := s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		if change == nil {
			if err := txStore.UpdateSandbox(ctx, sandbox, store.WithGeneration(sandbox.Generation)); err != nil {
				return err
			}
		}

		if change != nil {
			if s.engine == nil {
				return errors.New("reconcile engine is required")
			}
			previous := sandbox.Generation
			sandbox.IncrementGeneration()
			sandbox.RecordIntent(model.DesiredStatePresent)
			sandbox.PendingHarnessChange = change
			sandbox.PendingHarnessConfigID = change.Manifest.HarnessConfigID
			sandbox.HarnessChangeError = ""
			if err := txStore.UpdateSandbox(ctx, sandbox, store.WithGeneration(previous)); err != nil {
				return err
			}
			if err := s.engine.MarkDirtyTx(ctx, txDB, SandboxResourceType, SandboxDirtyID(projectID, sandboxID)); err != nil {
				return err
			}
		}
		// Its own write, as the egress level's is: UpdateSandbox never writes
		// it (store.SetSandboxDiscoboxAPI). The server's sandbox role reads it
		// on every call, so nothing is delivered.
		if discoboxAPIChanged {
			if err := txStore.SetSandboxDiscoboxAPI(ctx, projectID, sandboxID, discoboxAPI); err != nil {
				return err
			}
		}
		if !egressChanged {
			return nil
		}
		// Its own write: UpdateSandbox never writes the level, so no reconcile
		// that loaded the row earlier can write it back (store.SetSandboxEgress).
		if err := txStore.SetSandboxEgress(ctx, projectID, sandboxID, sandbox.Egress); err != nil {
			return err
		}
		if sandbox.DesiredState != model.DesiredStatePresent {
			return nil
		}
		// Delivered as any level's change is, by the egress reconcile
		// (ADR 26-10-01-032 §5).
		if s.engine == nil {
			return errors.New("reconcile engine is required")
		}
		return s.engine.MarkDirtyTx(ctx, txDB, SandboxProxyPolicyResourceType, SandboxDirtyID(projectID, sandboxID))
	}); err != nil {
		return nil, err
	}
	if change != nil {
		if _, err := s.NewSandboxReconciler().Reconcile(ctx, SandboxDirtyID(projectID, sandboxID)); err != nil {
			return nil, err
		}
		updated, err := s.store.GetSandbox(ctx, projectID, sandboxID)
		if err != nil {
			return nil, err
		}
		if updated.HarnessChangeError != "" {
			return nil, apperrors.NewStatusError(http.StatusConflict, updated.HarnessChangeError)
		}
		return updated, nil
	}
	return s.store.GetSandbox(ctx, projectID, sandboxID)
}

// DeleteSandbox archives the sandbox: its runtime goes, its data is kept, and
// it can be brought back with UnarchiveSandbox until its project's retention
// runs out (ADR 0022 §2). Getting a sandbox out of the way is the common
// request and the recoverable one, so it is what the unqualified verb does.
//
// Destroying the data is PurgeSandbox, which has to be asked for.
func (s *Service) DeleteSandbox(ctx context.Context, projectID, sandboxID string) error {
	_, err := s.recordSandboxIntent(ctx, projectID, sandboxID, model.DesiredStateArchived)
	if err != nil {
		return apperrors.NotFound(err, "sandbox not found")
	}
	return nil
}

// UnarchiveSandbox asks for the sandbox to exist as a runtime again. The
// reconciler recreates its container against the data that was retained, and
// leaves it stopped: a container that has run before stays stopped until
// something uses it (ADR 0017 §13), and the pool agent starts it on demand at
// first real use. Unarchiving is not itself use.
func (s *Service) UnarchiveSandbox(ctx context.Context, projectID, sandboxID string) error {
	sandbox, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return apperrors.NotFound(err, "sandbox not found")
	}
	if sandbox.DesiredState != model.DesiredStateArchived {
		return apperrors.NewStatusError(http.StatusConflict, "sandbox is not archived")
	}
	if _, err := s.recordSandboxIntent(ctx, projectID, sandboxID, model.DesiredStatePresent); err != nil {
		return apperrors.NotFound(err, "sandbox not found")
	}
	return nil
}

// PurgeSandbox destroys the sandbox and its data, and does not return success
// until the pool agent confirms both are gone (ADR 0022 §3).
//
// It records delete intent transactionally and then drives that sandbox's
// reconcile inline, in this request. Doing the work here is not a second
// deletion path: it is the same reconcile the engine would run, called early so
// the caller learns the outcome. Whatever happens to this request, the intent
// and its dirty mark are already durable — so a purge that fails, times out, or
// loses its client still converges in the background, and the only thing lost
// is the synchronous answer.
//
// A 202 would have been a promise the server could not keep and could not later
// verify: the row it would check against is the thing being deleted.
func (s *Service) PurgeSandbox(ctx context.Context, projectID, sandboxID string) error {
	if _, err := s.recordSandboxIntent(ctx, projectID, sandboxID, model.DesiredStateDeleted); err != nil {
		return apperrors.NotFound(err, "sandbox not found")
	}
	// The Result is discarded rather than honored: a delete converges to the row
	// being gone and never arms a timer, and this call is outside the engine, so
	// there is no claimed row to arm one on.
	if _, err := s.NewSandboxReconciler().Reconcile(ctx, SandboxDirtyID(projectID, sandboxID)); err != nil {
		return fmt.Errorf("purge sandbox: %w", err)
	}
	// Reconcile settles rather than returning an error when it records a failure
	// on the resource (ADR 0017 §4), so a clean return is not by itself proof the
	// data is gone. The row is: the purge deletes it, so a sandbox still present
	// here is one whose removal did not complete.
	if _, err := s.store.GetSandbox(ctx, projectID, sandboxID); err == nil {
		return apperrors.NewStatusError(http.StatusConflict,
			"sandbox purge did not complete; it will be retried in the background")
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// StartSandbox, StopSandbox, and RestartSandbox forward an instruction to the
// pool agent hosting the sandbox and return the sandbox as it currently reads.
//
// They write no state. The returned sandbox is therefore a snapshot from
// *before* the instruction takes effect, and deliberately so: the state that
// results arrives on the pool agent's reporting channel a moment later. A
// caller that needs to know the outcome re-reads the sandbox rather than
// believing this response (ADR 0017 §§9–10).
func (s *Service) StartSandbox(ctx context.Context, projectID, sandboxID string, _ services.StartSandboxBody) (*model.Sandbox, error) {
	return s.instructSandbox(ctx, projectID, sandboxID, sandboxStart)
}

func (s *Service) StopSandbox(ctx context.Context, projectID, sandboxID string, _ services.StopSandboxBody) (*model.Sandbox, error) {
	return s.instructSandbox(ctx, projectID, sandboxID, sandboxStop)
}

func (s *Service) RestartSandbox(ctx context.Context, projectID, sandboxID string, _ services.RestartSandboxBody) (*model.Sandbox, error) {
	return s.instructSandbox(ctx, projectID, sandboxID, sandboxRestart)
}

// SuspendSandbox has the pool freeze the sandbox whole (ADR 26-10-16-420 §3).
// It is an instruction like a stop, and it never becomes one: a pool that
// cannot freeze the sandbox refuses it, and nothing changes. Any start, or any
// use, thaws it.
func (s *Service) SuspendSandbox(ctx context.Context, projectID, sandboxID string, _ services.SuspendSandboxBody) (*model.Sandbox, error) {
	return s.instructSandbox(ctx, projectID, sandboxID, sandboxSuspend)
}

// RepairSandbox rebuilds the sandbox in place, on its harness config's current
// image, and starts it (ADR 0035, ADR 0064).
//
// It is one existence intent, not a workflow: the recorded generation carries
// RepairGeneration, which makes the reconciler's ensure tear the runtime down
// (provider Archive — container and disposable state dropped, durable tree
// kept) before the ordinary create rebuilds it. Recording intent is also what
// clears a latched ErrorMessage, so repair is the way out of a settled failure
// (ADR 0017 §4).
//
// The same intent carries the re-pin an upgrade would (ADR 0064 §1). The
// teardown has already discarded everything a re-pin costs, so rebuilding on
// the older of two images buys nothing — and a stale image is itself a way for
// a sandbox to be wedged (ADR 0016), which a repair that kept the pin would
// rebuild verbatim and report as fixed. No target is not an error here, unlike
// upgrade: the re-pin is a rider on the rebuild, not the thing asked for.
//
// Like purge, the request then drives that sandbox's reconcile inline so the
// caller learns whether the rebuild landed. A request that dies loses only the
// synchronous answer — the intent and its dirty mark are already durable, the
// rebuild converges in the background, and on-demand start covers first use.
// After a clean converge the service forwards the same start instruction an
// explicit start would; power stays unorchestrated (ADR 0017 §9), so a repair
// whose rebuild landed but whose start failed is a healthy stopped sandbox.
func (s *Service) RepairSandbox(ctx context.Context, projectID, sandboxID string) (*model.Sandbox, error) {
	sandbox, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, apperrors.NotFound(err, "sandbox not found")
	}
	if sandbox.DesiredState == model.DesiredStateDeleted {
		return nil, apperrors.NewStatusError(http.StatusConflict, "sandbox is being deleted")
	}
	if sandbox.DesiredState == model.DesiredStateArchived {
		return nil, apperrors.NewStatusError(http.StatusConflict, "sandbox is archived; unarchive it instead")
	}
	repin, err := s.currentRepin(ctx, sandbox)
	if err != nil {
		return nil, err
	}
	if _, err := s.recordSandboxIntent(ctx, projectID, sandboxID, model.DesiredStatePresent, func(sb *model.Sandbox) {
		sb.RepairGeneration = sb.Generation
		repin.apply(sb)
	}); err != nil {
		return nil, apperrors.NotFound(err, "sandbox not found")
	}
	// The Result is discarded rather than honored: this call is outside the
	// engine, so there is no claimed row to arm a timer on, and the background
	// engine re-arms whatever the next reconcile of this row returns.
	if _, err := s.NewSandboxReconciler().Reconcile(ctx, SandboxDirtyID(projectID, sandboxID)); err != nil {
		return nil, fmt.Errorf("repair sandbox: %w", err)
	}
	// Reconcile settles rather than returning an error when it records a
	// failure on the resource (ADR 0017 §4), so read the verdict off the row.
	sandbox, err = s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, apperrors.NotFound(err, "sandbox not found")
	}
	if sandbox.ErrorMessage != nil {
		return nil, apperrors.NewStatusError(http.StatusConflict,
			fmt.Sprintf("sandbox repair failed: %s", *sandbox.ErrorMessage))
	}
	return s.instructSandbox(ctx, projectID, sandboxID, sandboxStart)
}

// instructSandbox forwards a power instruction and answers with the pool's
// refusal, if any.
//
// A start or a restart is a use of the sandbox like any action, so one refused
// for want of a container repairs the sandbox once, or waits for the rebuild
// already coming, and is then sent again (ADR 26-10-12-711 §3). It waits for
// nothing else: an instruction is a command that wants an answer, and a
// sandbox still being created is still refused at once. A stop is not a use,
// and nor is a suspend.
func (s *Service) instructSandbox(ctx context.Context, projectID, sandboxID string, instruction sandboxInstruction) (*model.Sandbox, error) {
	if instruction == sandboxStop || instruction == sandboxSuspend {
		sandbox, err := s.instructSandboxOnce(ctx, projectID, sandboxID, instruction)
		if err != nil {
			return nil, err
		}
		return sandbox, nil
	}
	sandbox, err := s.awaitUsable(ctx, projectID, sandboxID, services.SandboxAction(instruction.String()), waitOnceRebuilding,
		func(ctx context.Context) (*model.Sandbox, error) {
			return s.instructSandboxOnce(ctx, projectID, sandboxID, instruction)
		})
	if err != nil {
		return nil, err
	}
	return sandbox, nil
}

func (s *Service) instructSandboxOnce(ctx context.Context, projectID, sandboxID string, instruction sandboxInstruction) (*model.Sandbox, error) {
	sandbox, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, apperrors.NotFound(err, "sandbox not found")
	}
	if sandbox.DesiredState == model.DesiredStateDeleted {
		return sandbox, apperrors.NewStatusError(http.StatusConflict, "sandbox is being deleted")
	}
	if sandbox.DesiredState == model.DesiredStateArchived {
		return sandbox, apperrors.NewStatusError(http.StatusConflict, "sandbox is archived; unarchive it first")
	}
	provider, err := s.resolveProvider(ctx, sandbox)
	if err != nil {
		return sandbox, err
	}
	// A start, stop, restart or suspend from outside — a person's, a lead's,
	// a repair's — overrides the wake the sandbox asked for: it does not get
	// back the run time a stop took away (ADR 26-10-16-337 §6). Canceled
	// before the instruction is sent, so a power reconcile already in flight
	// cannot record the wake afterwards.
	//
	// A suspend cancels it only once the pool has frozen the sandbox: a
	// refused one changes nothing (ADR 26-10-16-420 §3), and the likeliest
	// refusal — a sandbox that is not running — is one a sleep stopped, which
	// is the one with a wake to lose. Canceling after is as safe for it: a
	// power reconcile reading the epoch once the sandbox is frozen finds it
	// not running, and the pool refuses its power-down.
	if instruction == sandboxSuspend {
		if err := instructSandbox(ctx, s.store, provider, sandbox, instruction); err != nil {
			return sandbox, runtimeRefusal(err, instruction.String())
		}
		return sandbox, s.cancelSandboxWake(ctx, projectID, sandboxID)
	}
	if err := s.cancelSandboxWake(ctx, projectID, sandboxID); err != nil {
		return sandbox, err
	}
	if err := instructSandbox(ctx, s.store, provider, sandbox, instruction); err != nil {
		return sandbox, runtimeRefusal(err, instruction.String())
	}
	return sandbox, nil
}

// runtimeRefusal gives a refusal for want of a runtime the status it means,
// for a power instruction or for the ensure an action makes before it is
// forwarded; verb is what was being done ("start", "use"). The row is here, so
// none of these is the sandbox missing: each says its runtime is not there,
// and why. Left as plain errors they went out as a 500 naming nothing the
// caller could do.
func runtimeRefusal(err error, verb string) error {
	var message string
	switch {
	case errors.Is(err, ErrNoContainer):
		message = fmt.Sprintf("cannot %s sandbox: it has no container on its pool; it is being rebuilt, or repair it to give it one", verb)
	case errors.Is(err, ErrArchived):
		// The row is present here — an archived row was refused before the
		// pool was asked — so this is an unarchive the pool has not caught up
		// with, or one that failed and settled, and unarchiving again would be
		// refused. Repair is the way out of the second.
		message = fmt.Sprintf("cannot %s sandbox: it is still archived on its pool; try again shortly while its unarchive converges, or repair it if the unarchive failed", verb)
	case errors.Is(err, ErrNotRunning):
		message = fmt.Sprintf("cannot %s sandbox: it is not running", verb)
	case errors.Is(err, sandbox.ErrPoolAgentUnsupported):
		message = fmt.Sprintf("cannot %s sandbox: its pool's agent does not support it; upgrade the pool", verb)
	case errors.Is(err, ErrNotFound):
		// Not only the pool's 404: a sandbox whose create has not placed it on
		// a pool yet reads the same way.
		message = fmt.Sprintf("cannot %s sandbox: it has no runtime on its pool, either not yet created or lost; if it is not being created, repair it", verb)
	default:
		return err
	}
	return apperrors.StatusError{Status: http.StatusConflict, Message: message, Cause: err}
}

func (s *Service) resolveProvider(ctx context.Context, sb *model.Sandbox) (Provider, error) {
	if s.sandboxProviders == nil {
		return nil, nil
	}
	return s.sandboxProviders.ResolveForSandbox(ctx, sb)
}

// UpgradeSandbox re-pins the sandbox to its harness config's current image
// (ADR 0021 §1).
//
// The re-pin is the whole operation. It changes the spec, so the ordinary
// reconcile delivers it, and the pool agent replaces any container that no
// longer matches — restarting it into the new image if it was running, leaving
// it stopped if it was not (ADR 0021 §3). Upgrading never powers a sandbox on.
// A separate upgrade operation, with its own generation counters, would be a
// second way to say "converge this sandbox".
//
// Availability is the only thing checked here. Whether the target image can be
// obtained on the sandbox's pool is not knowable from the control plane, and is
// deliberately left to fail where it is observable — the pool agent's create
// (ADR 0021 §5).
//
// The sandbox ID, its history, and its pool-host volumes survive; anything
// written to the container's own filesystem outside those volumes does not.
// That cost buys nothing when the sandbox is already on the current image, so
// an upgrade with no target is refused rather than performed as a recreate.
//
// The target is read here rather than supplied by the caller: the reconciler
// re-reads it anyway when it runs, and an expected-digest parameter would turn
// a continuously rebuilt dev image into a retry loop against a target that was
// never wrong.
func (s *Service) UpgradeSandbox(ctx context.Context, projectID, sandboxID string, _ services.UpgradeSandboxBody) (*model.Sandbox, error) {
	existing, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, apperrors.NotFound(err, "sandbox not found")
	}
	repin, err := s.currentRepin(ctx, existing)
	if err != nil {
		return nil, err
	}
	if !repin.Available {
		// Distinguish "nothing newer" from "nothing to move to". A config-mode
		// sandbox, one whose harness config was deleted or has no bundle, and
		// one whose bundle has no digest yet all have nothing to move to, and
		// telling their owner they are running "the current bundle" asserts
		// something that is not true of them.
		if repin.Empty() {
			return nil, apperrors.NewStatusError(http.StatusConflict,
				"sandbox has no bundle to upgrade to")
		}
		return nil, apperrors.NewStatusError(http.StatusConflict,
			"sandbox is already running its current bundles")
	}
	// The re-pin is the whole instruction: a changed pin changes the spec
	// fingerprint, and the pool host rebuilds any container that does not
	// match it (ADR 0017 §5). There is no restart counter to bump.
	sandbox, err := s.recordSandboxIntent(ctx, projectID, sandboxID, model.DesiredStatePresent, repin.apply)
	if err != nil {
		return nil, apperrors.NotFound(err, "sandbox not found")
	}
	return sandbox, nil
}

// UpgradeHarnessConfigSandboxes moves this harness config's stopped sandboxes,
// healthy or failed, onto the image and bundle it now resolves to (ADR 0082,
// ADR 0121).
//
// It is the same operation as UpgradeSandbox, performed for a caller that is
// not a person: the same sandboxRepin, applied through the same
// recordSandboxIntent, decided by the same services.SandboxUpgradeTarget the
// read path reports `upgrade.available` from. There is no automatic-upgrade
// code path — there is an automatic *author* of the upgrade every sandbox
// already had.
//
// Delivery is therefore also unchanged. The re-pin moves the pins, so the pool
// agent rebuilds the container whose spec_fingerprint label no longer matches,
// or restarts a delivered one into its moved platform
// (SandboxManifest.DeliveredFingerprint), and — because it found that
// container stopped — leaves it stopped (ADR 0021 §3). Nothing
// here starts anything. A failed sandbox takes the same path: the recorded
// intent clears its latched error, so the reconciler retries the create on the
// new image instead of treating the failure as settled (ADR 0017 §4). One whose
// client push never landed is left alone, because the retry could only park it
// again.
//
// Best-effort per sandbox: one that cannot be re-pinned is logged and the rest
// still move. A harness image landing must not be all-or-nothing across a
// project, and the sandbox that missed it is picked up the next time its
// harness image resolves, still reporting an available upgrade until then.
func (s *Service) UpgradeHarnessConfigSandboxes(ctx context.Context, projectID, harnessConfigID string) error {
	harnessConfigID = strings.TrimSpace(harnessConfigID)
	if harnessConfigID == "" {
		return nil
	}
	return s.upgradeStoppedSandboxes(ctx, projectID, harnessConfigID)
}

// UpgradeReleaseSandboxes moves every project's stopped sandboxes onto the
// bundles the release's references now resolve to (ADR 26-10-16-423 §7). It
// is UpgradeHarnessConfigSandboxes for the pins every sandbox shares, run when
// a reference resolves to a digest it had not, as a server starting on a new
// release first does.
func (s *Service) UpgradeReleaseSandboxes(ctx context.Context) error {
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, project := range projects {
		if err := s.upgradeStoppedSandboxes(ctx, project.ID, ""); err != nil {
			failures = append(failures, fmt.Errorf("project %s: %w", project.ID, err))
		}
	}
	return errors.Join(failures...)
}

// undeliveredPinInterval is how often a sandbox from before bundles that has
// stopped since the last pass is looked for (StartPinningUndelivered).
const undeliveredPinInterval = 10 * time.Minute

// StartPinningUndelivered runs PinUndeliveredSandboxes now and then every
// undeliveredPinInterval until ctx ends: a sandbox from before bundles that was
// running at the server's start is pinned once it stops, rather than at the
// next start, so its next container and an export of it have bundles to be
// built from. Once every such sandbox is pinned, each pass is one query that
// finds none.
func (s *Service) StartPinningUndelivered(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(undeliveredPinInterval)
		defer ticker.Stop()
		for {
			if err := s.PinUndeliveredSandboxes(ctx); err != nil && ctx.Err() == nil {
				slog.WarnContext(ctx, "pin sandboxes from before bundles", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// PinUndeliveredSandboxes moves every sandbox from before bundles onto its
// harness config's bundle and the bundles its server's release names (ADR
// 26-10-16-423 §§7, 10): it was pinned to a runnable harness image, which no
// release builds any more, so its pool has nothing to build its next
// container from until it is pinned to the bundles a box applies. It runs on
// every start, each time the release's bundles move, and every
// undeliveredPinInterval (StartPinningUndelivered), and takes every such
// sandbox it finds settled with no container running
// (store.ListUndeliveredSandboxes).
//
// Unlike an upgrade it is not the project's policy to refuse: the image the
// sandbox ran is gone either way. A stopped or failed one is recorded intent,
// as a re-pin is, so it is reconciled and its pool rebuilds its container
// onto the sandbox base image; an archived one is pinned in place, for the
// unarchive that builds one. A running one keeps the container it has until
// it stops, or an upgrade or a repair re-pins it, and reports its available
// upgrade until then. One whose config has no bundle with a digest to pin, or whose
// release bundles cannot be resolved now, is left for a later pass.
//
// A sandbox pinned to a platform from before the agent was a bundle of its
// own (ADR 26-10-24-953 §2), running or not, is pinned to the release's agent
// in place: neither fingerprint includes it, so nothing is rebuilt, and its
// pool places it at the sandbox's next start.
func (s *Service) PinUndeliveredSandboxes(ctx context.Context) error {
	if err := s.pinAgentlessSandboxes(ctx); err != nil {
		return err
	}
	candidates, err := s.store.ListUndeliveredSandboxes(ctx)
	if err != nil || len(candidates) == 0 {
		return err
	}
	// Asked once for the pass rather than of each sandbox: with no platform
	// or agent to pin there is nothing to do, which the next pass, or their
	// move, comes back to.
	if release, err := s.currentRelease(ctx); err != nil || release.Platform.Digest == "" || release.Agent.Digest == "" {
		slog.InfoContext(ctx, "sandboxes from before bundles wait for the release's bundles to resolve", "count", len(candidates), "error", err)
		return nil
	}
	for i := range candidates {
		sb := &candidates[i]
		if sb.HarnessMode == sandboxconfig.HarnessModeConfig || awaitingSourcePush(sb) {
			continue
		}
		repin, err := s.currentRepin(ctx, sb)
		if err != nil {
			slog.WarnContext(ctx, "skip pinning a sandbox from before bundles; cannot resolve its bundles",
				"projectId", sb.ProjectID, "sandboxId", sb.ID, "error", err)
			continue
		}
		if repin.Harness == (services.HarnessPins{}) || repin.Platform.Digest == "" || repin.Agent.Digest == "" {
			// Its harness has no bundle — registered before bundles, until
			// one is registered for it — which every pass finds again, so it
			// is no warning: create and repair say why to whoever asks.
			slog.DebugContext(ctx, "a sandbox from before bundles waits for its harness to have a bundle",
				"projectId", sb.ProjectID, "sandboxId", sb.ID)
			continue
		}
		pin := func(sb *model.Sandbox) {
			// Read again under the write: a sandbox pinned since is left.
			if sb.PlatformPin().Digest == "" {
				repin.Available = true
				repin.apply(sb)
			}
		}
		if sb.DesiredState == model.DesiredStateArchived {
			_, err = s.updateSandboxMetadata(ctx, sb.ProjectID, sb.ID, pin)
		} else {
			_, err = s.recordSandboxIntent(ctx, sb.ProjectID, sb.ID, model.DesiredStatePresent, pin)
		}
		if err != nil {
			slog.WarnContext(ctx, "skip pinning a sandbox from before bundles; cannot record it",
				"projectId", sb.ProjectID, "sandboxId", sb.ID, "error", err)
			continue
		}
		slog.InfoContext(ctx, "pinned a sandbox from before bundles to its bundles",
			"projectId", sb.ProjectID, "sandboxId", sb.ID, "previousImage", sb.Image,
			"bundle", repin.Harness.Bundle.Reference, "bundleDigest", repin.Harness.Bundle.Digest,
			"platformBundleDigest", repin.Platform.Digest, "agentBundleDigest", repin.Agent.Digest)
	}
	return nil
}

// pinAgentlessSandboxes pins every sandbox pinned to a platform and to no
// agent to the release's agent, in place (PinUndeliveredSandboxes), and to the
// release's platform with it: the platform an agent ships with is the one it
// provisions. An older platform's features predate the mark that keeps an
// optional one from blocking a box's readiness (ADR 26-10-24-953 §1), so the
// new agent applying it would hold every box's start on all of them, and
// refuse any the repository turns off.
func (s *Service) pinAgentlessSandboxes(ctx context.Context) error {
	candidates, err := s.store.ListAgentlessSandboxes(ctx)
	if err != nil || len(candidates) == 0 {
		return err
	}
	release, err := s.currentRelease(ctx)
	if err != nil || release.Agent.Digest == "" || release.Platform.Digest == "" {
		slog.InfoContext(ctx, "sandboxes pinned to no agent wait for the release's agent and platform to resolve", "count", len(candidates), "error", err)
		return nil
	}
	for i := range candidates {
		sb := &candidates[i]
		_, err := s.updateSandboxMetadata(ctx, sb.ProjectID, sb.ID, func(sb *model.Sandbox) {
			if sb.AgentBundleDigest == "" && sb.PlatformPin().Digest != "" {
				sb.SetAgentPin(release.Agent)
				sb.SetPins(release.Platform, sb.HarnessPin())
			}
		})
		if err != nil {
			slog.WarnContext(ctx, "skip pinning a sandbox to the release's agent; cannot record it",
				"projectId", sb.ProjectID, "sandboxId", sb.ID, "error", err)
			continue
		}
		slog.InfoContext(ctx, "pinned a sandbox to the release's agent and platform",
			"projectId", sb.ProjectID, "sandboxId", sb.ID, "agentBundleDigest", release.Agent.Digest,
			"platformBundleDigest", release.Platform.Digest)
	}
	return nil
}

// upgradeStoppedSandboxes re-pins a project's stopped sandboxes on one harness
// config, or on any when harnessConfigID is empty, whose pins have moved.
func (s *Service) upgradeStoppedSandboxes(ctx context.Context, projectID, harnessConfigID string) error {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	// Asked once, before any row is read: the policy is the project's answer to
	// whether this happens at all (ADR 0082 §3), not a per-sandbox condition.
	if !model.SandboxUpgradesAutomatically(project) {
		return nil
	}
	candidates, err := s.store.ListStoppedSandboxes(ctx, projectID, harnessConfigID)
	if err != nil {
		return err
	}
	for i := range candidates {
		sb := &candidates[i]
		// A sandbox still owed a client push cannot be retried into anything but
		// another wait: the create re-parks it at `awaiting_source` with a fresh
		// deadline, it reads as starting while nobody pushes, and it fails again
		// with the timeout in place of the cause it had (ADR 0121). A new image
		// delivers no source, so there is nothing for the re-pin to fix.
		if awaitingSourcePush(sb) {
			continue
		}
		repin, err := s.currentRepin(ctx, sb)
		if err != nil {
			slog.WarnContext(ctx, "skip automatic sandbox upgrade; cannot resolve target",
				"projectId", projectID, "sandboxId", sb.ID, "error", err)
			continue
		}
		if !repin.Available {
			continue
		}
		if _, err := s.recordSandboxIntent(ctx, projectID, sb.ID, model.DesiredStatePresent, repin.apply); err != nil {
			slog.WarnContext(ctx, "skip automatic sandbox upgrade; cannot record intent",
				"projectId", projectID, "sandboxId", sb.ID, "error", err)
			continue
		}
		configID := ""
		if sb.HarnessConfigID != nil {
			configID = *sb.HarnessConfigID
		}
		slog.InfoContext(ctx, "upgraded stopped sandbox to its current pins",
			"projectId", projectID, "sandboxId", sb.ID, "harnessConfigId", configID,
			"previousBundleDigest", sb.HarnessPin().Digest, "bundleDigest", repin.Harness.Bundle.Digest,
			"previousPlatformBundleDigest", sb.PlatformPin().Digest, "platformBundleDigest", repin.Platform.Digest,
			"previousAgentBundleDigest", sb.AgentBundleDigest, "agentBundleDigest", repin.Agent.Digest,
			"retryingFailure", sb.ErrorMessage != nil || sb.State == model.SandboxStateFailed)
	}
	return nil
}

// sandboxRepin is the change that moves a sandbox onto its harness config's
// current image and bundle and the platform its server's release names now,
// and the harness config a legacy sandbox adopts along with them.
//
// It exists because two operations write it — upgrade, whose whole content it
// is, and repair, which carries it on the rebuild it was already doing
// (ADR 0064 §1). They must not be able to pin differently, and the difference
// between them is only what an unavailable target means, which each decides for
// itself before calling apply.
type sandboxRepin struct {
	services.SandboxPinTarget

	// HarnessConfigID is set only when the re-pin also adopts a config: a
	// sandbox created before every sandbox carried one resolves its target
	// through the fallback `shell` config, and pinning that config's image
	// without adopting the config would leave the row describing an image no
	// config of its own names (ADR 0032 §4). This is the whole migration —
	// explicit, in place, and visible in the listing first.
	HarnessConfigID string

	// Available is whether the target differs from what the sandbox runs now.
	// When it does not, an empty target is "nothing to move to", and any
	// other "already applied".
	Available bool
}

// apply writes the re-pin onto the sandbox being recorded. An unavailable
// target writes nothing: the sandbox keeps the pin it has.
func (r sandboxRepin) apply(sb *model.Sandbox) {
	if !r.Available {
		return
	}
	r.Apply(sb)
	if r.HarnessConfigID != "" {
		adopt := r.HarnessConfigID
		sb.HarnessConfigID = &adopt
	}
}

// currentRepin resolves what moving the sandbox onto its harness config's
// current pins would change, without changing anything. It loads the config
// and the platform the shared rule needs; the rule itself lives in
// services.SandboxUpgradeTarget so this and the read path that reports an
// available upgrade cannot answer differently.
func (s *Service) currentRepin(ctx context.Context, sb *model.Sandbox) (sandboxRepin, error) {
	var config *model.HarnessConfig
	var err error
	adopt := ""
	if sb.HarnessConfigID == nil {
		// Not converged yet: it moves to the fallback, adopting it.
		if config, err = s.fallbackHarnessConfig(ctx, sb.ProjectID); err != nil {
			return sandboxRepin{}, err
		}
		if config != nil {
			adopt = config.ID
		}
	} else if config, err = s.store.GetHarnessConfig(ctx, sb.ProjectID, strings.TrimSpace(*sb.HarnessConfigID)); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return sandboxRepin{}, err
		}
		config = nil
	}
	// A release bundle that cannot be resolved now is no move of it: the
	// re-pin still carries the harness's, and the sandbox keeps the platform
	// and agent it has (SandboxPinTarget.Apply). Repair is a recovery path,
	// and a fan-out runs once per move; neither may be refused for a registry
	// being away.
	release, err := s.currentRelease(ctx)
	if err != nil {
		slog.WarnContext(ctx, "re-pin without the release's bundles; they cannot be resolved",
			"projectId", sb.ProjectID, "sandboxId", sb.ID, "error", err)
		release = services.ReleasePins{}
	}
	target, available := services.SandboxUpgradeTarget(sb, config, release)
	if target.Empty() {
		// Nothing to move to, which an empty target tells callers apart from
		// a re-pin that is merely already applied.
		return sandboxRepin{}, nil
	}
	repin := sandboxRepin{SandboxPinTarget: target, Available: available}
	// A config is adopted only with its bundle: a re-pin that moves the
	// platform alone keeps the harness the sandbox applies, and that harness
	// beside another config's command is one that does not start.
	if available && target.Harness != (services.HarnessPins{}) {
		repin.HarnessConfigID = adopt
	}
	return repin, nil
}

// currentRelease is the bundles the release's references resolve to now
// (ADR 26-10-16-423 §7, ADR 26-10-24-953 §2), each zero when the server names
// none.
func (s *Service) currentRelease(ctx context.Context) (services.ReleasePins, error) {
	if s.release == nil {
		return services.ReleasePins{}, nil
	}
	return s.release.CurrentRelease(ctx)
}

// fallbackHarnessConfig is the reserved `shell` built-in, or nil when seeding
// has not created it.
//
// It is not where create's resolution chain ends (ADR 0048): a sandbox
// names its harness or the project does. What still needs it is the migration
// of sandboxes made before every sandbox carried a harness config, which adopt
// this one on upgrade — a legacy path, not a default.
func (s *Service) fallbackHarnessConfig(ctx context.Context, projectID string) (*model.HarnessConfig, error) {
	config, err := s.store.GetHarnessConfigBySlug(ctx, projectID, harnessdefs.ShellSlug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return config, nil
}

// UpgradeBasis exposes what an upgrade is read against to the API mappers,
// which report a sandbox's available upgrade and must give the same answer this
// service applies. The release's bundles are the ones last resolved, never
// asked for afresh: a read is not held up by a registry.
func (s *Service) UpgradeBasis(ctx context.Context, projectID string) (services.SandboxUpgradeBasis, error) {
	var basis services.SandboxUpgradeBasis
	if s.release != nil {
		basis.Release = s.release.LatestRelease()
	}
	fallback, err := s.fallbackHarnessConfig(ctx, projectID)
	basis.Fallback = fallback
	return basis, err
}

// CompleteSandboxSourcePush reports that a client finished delivering a
// sandbox's sources, which is what lets the sandbox start on them.
//
// Each report may cover a subset of sources. Delivery is merged per slug;
// only the last delivered source permits the sandbox to start. Repeating a
// report at the same checkout commit is idempotent (ADR 26-10-23-948).
//
// A delivery does not wait for the sandbox (ADR 26-10-10-545 §3): a client
// delivers into its pool's cache while the image is pulled and the sandbox
// boots, so the report usually arrives before the sandbox would have parked
// in awaiting_source, and a sandbox whose source has landed by then never
// parks. One that did park is resumed by it.
//
// The commits are a confirmation, not an instruction. What to check out was
// fixed at create, in each source's Checkout.Commit: the client resolved it
// before the sandbox existed, and the delivery only makes those objects
// reachable. Accepting a different commit here would let a sandbox run
// something other than what its source says it runs, so a mismatch is refused
// rather than recorded.
//
// The server does not verify the commits are present on the pool: that means
// a round trip to the pool agent, and the reconcile that follows fails on a
// missing commit anyway, with a better message than this endpoint could
// produce.
func (s *Service) CompleteSandboxSourcePush(ctx context.Context, projectID, sandboxID string, input services.CompleteSandboxSourcePushBody) (*model.Sandbox, error) {
	existing, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, apperrors.NotFound(err, "sandbox not found")
	}
	pending := clientDeliveredSources(existing)
	if len(pending) == 0 {
		return nil, apperrors.NewStatusError(http.StatusConflict,
			"no sandbox source is delivered by a client; nothing is waiting to be delivered")
	}
	// Repeated completion confirms the immutable commit without restarting a
	// sandbox. Reports may cover a subset; only the last source resumes it.
	if len(input.Sources) == 0 {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "no sources reported")
	}
	if existing.SourceDeliveredAt == nil && !sandboxHasNeverRun(existing.State) {
		return nil, apperrors.NewStatusError(http.StatusConflict, "sandbox is not awaiting its source")
	}
	selected := []gitSourceEntry{}
	for _, entry := range pending {
		if entry.source.Slug != nil {
			if _, ok := input.Sources[*entry.source.Slug]; ok {
				selected = append(selected, entry)
			}
		}
	}
	if err := verifySourcePushCommits(selected, input.Sources); err != nil {
		return nil, err
	}
	if existing.SourceDeliveredAt != nil {
		return existing, nil
	}
	return s.recordSandboxIntent(ctx, projectID, sandboxID, model.DesiredStatePresent, func(sb *model.Sandbox) {
		if sb.SourceDeliveries == nil {
			sb.SourceDeliveries = map[string]sourcecache.Delivery{}
		}
		for slug := range input.Sources {
			sb.SourceDeliveries[slug] = sourcecache.Delivery{Status: sourcecache.Delivered}
		}
		all := true
		for _, entry := range clientDeliveredSources(sb) {
			if entry.source.Slug == nil || sb.SourceDeliveries[*entry.source.Slug].Status != sourcecache.Delivered {
				all = false
			}
		}
		if all {
			now := time.Now().UTC()
			sb.SourceDeliveredAt = &now
		}
	})
}

// verifySourcePushCommits checks the client's report against what the sandbox
// is waiting for: every push-delivered source named, each at the commit its
// checkout fixed at create, and nothing else. A slug the sandbox does not push
// is refused rather than ignored — it means the client pushed into a
// repository this sandbox does not materialize from, so its own report is
// wrong about what was delivered.
func verifySourcePushCommits(pending []gitSourceEntry, reported map[string]string) error {
	expected := make(map[string]string, len(pending))
	for _, entry := range pending {
		slug := ""
		if entry.source.Slug != nil {
			slug = strings.TrimSpace(*entry.source.Slug)
		}
		if slug == "" {
			return apperrors.NewStatusError(http.StatusConflict,
				"sandbox source has no slug to report a push for")
		}
		commit := ""
		if entry.source.Checkout != nil && entry.source.Checkout.Commit != nil {
			commit = strings.ToLower(strings.TrimSpace(*entry.source.Checkout.Commit))
		}
		if commit == "" {
			return apperrors.NewStatusError(http.StatusConflict,
				fmt.Sprintf("sandbox source %q does not name a commit to check out", slug))
		}
		expected[slug] = commit
	}
	for slug := range reported {
		if _, ok := expected[strings.TrimSpace(slug)]; !ok {
			return apperrors.NewStatusError(http.StatusConflict,
				fmt.Sprintf("source %q is not awaiting a push", strings.TrimSpace(slug)))
		}
	}
	for slug, commit := range expected {
		value, ok := reported[slug]
		if !ok {
			return apperrors.NewStatusError(http.StatusConflict,
				fmt.Sprintf("source %q was not reported as pushed", slug))
		}
		if reportedCommit := strings.ToLower(strings.TrimSpace(value)); reportedCommit != commit {
			return apperrors.NewStatusError(http.StatusConflict,
				fmt.Sprintf("pushed commit %q for source %q does not match the source's commit %q", reportedCommit, slug, commit))
		}
	}
	return nil
}

func (s *Service) ReconcileSandbox(ctx context.Context, projectID, sandboxID string) (*model.Sandbox, error) {
	sandbox, err := s.scheduleSandboxReconcile(ctx, projectID, sandboxID)
	if err != nil {
		return nil, apperrors.NotFound(err, "sandbox not found")
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

// NewSandboxReconciler returns the service-wired sandbox reconciler.
func (s *Service) NewSandboxReconciler() *SandboxReconciler {
	return NewSandboxReconciler(
		s.store,
		WithSandboxProviderManager(s.sandboxProviders),
		WithSandboxAuthenticator(s.sandboxAuth),
		WithSandboxReconcileEngine(s.engine),
		WithArchiveRetention(s.archiveRetention),
		WithServerPeerID(s.serverPeerID),
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
			Capabilities: statuses[id],
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
		return "", apperrors.NotFound(err, "sandbox not found")
	}
	return s.sandboxAuth.CreateToken(ctx, sandboxauth.TokenClaims{
		ProjectID: sb.ProjectID,
		SandboxID: sb.ID,
		UserID:    sb.CreatedByUserID,
	})
}
