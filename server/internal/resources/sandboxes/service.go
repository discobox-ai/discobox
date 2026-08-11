package sandboxes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/obot-platform/discobox/server/internal/apperrors"
	"github.com/obot-platform/discobox/server/internal/reconcile"

	"github.com/obot-platform/discobox/id"
	"github.com/obot-platform/discobox/server/internal/auth"
	"github.com/obot-platform/discobox/server/internal/model"
	services "github.com/obot-platform/discobox/server/internal/services"

	sandboxauth "github.com/obot-platform/discobox/server/internal/auth/sandbox"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/store"
)

// Service owns sandbox API behavior, orchestration, provider catalog state, and
// sandbox auth dependencies.
type Service struct {
	store              *store.Store
	engine             *reconcile.Engine
	sandboxProviders   *sandbox.ProviderManager
	providerStore      any
	sandboxAuth        *sandboxauth.Manager
	defaultUserID      string
	defaultImage       string
	defaultImageDigest string
	hostID             string
}

func NewService(store *store.Store, manager *sandbox.ProviderManager, defaultUserID string, engine *reconcile.Engine, providerStore ...any) *Service {
	svc := &Service{
		store:            store,
		engine:           engine,
		sandboxProviders: manager,
		defaultUserID:    defaultUserID,
		defaultImage:     sandbox.DefaultSandboxImageName,
	}
	if len(providerStore) > 0 {
		svc.providerStore = providerStore[0]
	}
	return svc
}

// RegisterJobs installs the sandbox reconciler on the level-triggered
// reconcile engine.
func (s *Service) RegisterJobs(opts ...reconcile.RegisterOption) error {
	if s.engine == nil {
		return errors.New("reconcile engine is required")
	}
	return s.engine.Register(SandboxResourceType, s.NewSandboxReconciler(), opts...)
}

func (s *Service) SetSandboxAuthManager(manager *sandboxauth.Manager) {
	s.sandboxAuth = manager
}

// SetDefaultSandboxImage records the image a sandbox with no harness config
// runs, and the digest identifying which build that tag currently is. They are
// set together because they are one identity: the tag says what to run, and
// only the digest says whether a sandbox already runs it.
func (s *Service) SetDefaultSandboxImage(image, digest string) {
	if image = strings.TrimSpace(image); image != "" {
		s.defaultImage = image
	}
	s.defaultImageDigest = strings.TrimSpace(digest)
}

// SetHostID records the machine this server runs on, so a create request whose
// origin reports the same host is known to come from this filesystem and can
// bind its source directory instead of pushing it.
func (s *Service) SetHostID(hostID string) {
	s.hostID = strings.TrimSpace(hostID)
}

func mapAPIError(err error, notFoundMessage string) error {
	if errors.Is(err, store.ErrNotFound) {
		return apperrors.NewStatusError(http.StatusNotFound, notFoundMessage)
	}
	return err
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

func (s *Service) ListSandboxes(ctx context.Context, projectID, sourceRoot, originKey string) ([]model.Sandbox, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, mapAPIError(err, "project not found")
	}
	return s.store.ListSandboxes(ctx, projectID, sourceRoot, originKey)
}

func (s *Service) CreateSandbox(ctx context.Context, projectID string, input services.CreateSandboxBody) (*model.Sandbox, error) {
	config := input.Config
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, mapAPIError(err, "project not found")
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
		return nil, mapAPIError(err, "pool not found")
	}
	provider, err := s.store.GetSandboxProviderInstance(ctx, projectID, pool.ProviderInstanceID)
	if err != nil {
		return nil, mapAPIError(err, "provider instance not found")
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
	// Names are unique within a project (idx_sandbox_project_name) because they
	// are an addressable handle, not just a label: `disco box ssh-config` emits
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
	userID := s.defaultUserID
	if authenticatedUserID, err := auth.UserID(ctx); err == nil {
		userID = authenticatedUserID
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
	services.DefaultGitSourceSlugs(source, sourceCodeReferences)
	var sourceRoot *string
	if root := source.Root(); root != "" {
		sourceRoot = &root
	}
	origin := services.OriginToModel(input.Origin)
	var originKey *string
	if key := origin.Key(); key != "" {
		originKey = &key
	}
	if err := s.resolveSourceDelivery(ctx, source, origin, provider); err != nil {
		return nil, err
	}
	user := services.SandboxUserToModel(config.User)
	harnessMode := "run"
	if mode, ok := config.HarnessMode.Get(); ok {
		harnessMode = string(mode)
	}
	image := strings.TrimSpace(config.Image.Or(""))
	imageDigest := ""
	if harnessConfigID != nil && strings.TrimSpace(*harnessConfigID) != "" && harnessMode != "config" {
		harnessConfig, err := s.store.GetHarnessConfig(ctx, projectID, strings.TrimSpace(*harnessConfigID))
		if err != nil {
			return nil, mapAPIError(err, "harness config not found")
		}
		// A harness is only selectable once its configure flow has succeeded.
		// harnessMode "config" is exempt: that is the configure flow itself.
		if !harnessConfig.Configured {
			return nil, apperrors.NewStatusError(http.StatusConflict,
				fmt.Sprintf("harness %q is not configured; run `disco box harness configure %s` first", harnessConfig.Slug, harnessConfig.Slug))
		}
		if strings.TrimSpace(harnessConfig.Image) != "" {
			image = strings.TrimSpace(harnessConfig.Image)
			// Pin the identity, not just the reference. The tag is rebuilt in
			// place by dev workflows, so it alone does not say which image this
			// sandbox runs; the digest does, and the pool host enforces it
			// (ADR 0016 §1).
			imageDigest = strings.TrimSpace(harnessConfig.ImageDigest)
		}
	}
	if image == "" {
		// No harness config: the sandbox runs the server's default image, which
		// is a choice of image rather than the absence of one, so pin its
		// identity the same way a harness image is pinned.
		image, imageDigest = s.defaultImage, s.defaultImageDigest
	}
	sandbox := &model.Sandbox{
		ID:              sandboxID,
		ProjectID:       projectID,
		CreatedByUserID: userID,
		PoolID:          pool.ID,
		Name:            config.Name,
		Description:     services.OptStringPtr(config.Description),
		SandboxManifest: model.SandboxManifest{
			HarnessConfigID:      harnessConfigID,
			HarnessMode:          harnessMode,
			Model:                services.OptStringPtr(config.Model),
			ModelServiceTier:     services.OptStringPtr(config.ModelServiceTier),
			ModelReasoningLevel:  services.OptStringPtr(config.ModelReasoningLevel),
			Prompt:               config.Prompt,
			Image:                image,
			ImageDigest:          imageDigest,
			Env:                  map[string]string(config.Env.Or(nil)),
			Source:               source,
			SourceCodeReferences: sourceCodeReferences,
			UserName:             user.Name,
			UserUID:              user.UID,
			UserGID:              user.GID,
			UserGroupName:        user.GroupName,
			UserAdditionalGroups: user.AdditionalGroups,
			HomeDirectory:        user.HomeDirectory,
		},
		SourceRoot: sourceRoot,
		Origin:     origin,
		OriginKey:  originKey,
	}
	assignments, err := s.prepareSandboxSecrets(ctx, projectID, sandbox, config.Secrets)
	if err != nil {
		return nil, err
	}
	// Materialize the harness config's secret bindings and enforce its required
	// secrets. Inline per-sandbox secrets take precedence over bindings.
	//
	// harnessMode "config" is exempt: the configure flow is how a harness's
	// secrets are obtained in the first place, so requiring them to already exist
	// would make an unconfigured harness impossible to configure.
	if harnessConfigID != nil && strings.TrimSpace(*harnessConfigID) != "" {
		if harnessMode == "config" {
			// Config mode instead offers the previous configuration's secrets back
			// under PREV_-prefixed names, so the configure flow can verify and keep
			// an existing credential without it ever being re-typed or re-read.
			previousAssignments, err := s.applyPreviousConfigureSecrets(ctx, projectID, sandbox, strings.TrimSpace(*harnessConfigID))
			if err != nil {
				return nil, err
			}
			assignments = append(assignments, previousAssignments...)
		} else {
			inlineEnvs := make(map[string]struct{}, len(config.Secrets))
			for _, in := range config.Secrets {
				inlineEnvs[strings.TrimSpace(in.Env)] = struct{}{}
			}
			harnessAssignments, err := s.applyHarnessConfigSecrets(ctx, projectID, sandbox, strings.TrimSpace(*harnessConfigID), inlineEnvs)
			if err != nil {
				return nil, err
			}
			assignments = append(assignments, harnessAssignments...)
		}
	}
	return s.createSandboxIntent(ctx, sandbox, assignments)
}

func (s *Service) resolveHarnessConfigID(ctx context.Context, project *model.Project, harnessConfigID, harnessName services.OptString) (*string, error) {
	if project == nil {
		return nil, fmt.Errorf("project is required")
	}
	if id, ok := harnessConfigID.Get(); ok && id != "" {
		config, err := s.store.GetHarnessConfig(ctx, project.ID, id)
		if err != nil {
			return nil, mapAPIError(err, "harness config not found")
		}
		return &config.ID, nil
	}
	name, ok := harnessName.Get()
	if ok && strings.TrimSpace(name) != "" {
		selector := strings.TrimSpace(name)
		// Prefer the stable slug (e.g. "codex"), then fall back to the display name.
		if config, err := s.store.GetHarnessConfigBySlug(ctx, project.ID, selector); err == nil {
			return &config.ID, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, mapAPIError(err, "harness config not found")
		}
		config, err := s.store.GetHarnessConfigByName(ctx, project.ID, selector)
		if err != nil {
			return nil, mapAPIError(err, "harness config not found")
		}
		return &config.ID, nil
	}
	// No explicit selector: pin the project default so the sandbox always carries a
	// concrete harness config. Resolving the harness at create time is what makes its
	// required-secret gate and binding materialization apply to `run .`.
	if strings.TrimSpace(project.DefaultHarnessConfigID) != "" {
		config, err := s.store.GetHarnessConfig(ctx, project.ID, project.DefaultHarnessConfigID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, nil // default was deleted; leave the sandbox agent-less
			}
			return nil, mapAPIError(err, "harness config not found")
		}
		return &config.ID, nil
	}
	return nil, nil
}

func (s *Service) GetSandbox(ctx context.Context, projectID, sandboxID string) (*model.Sandbox, error) {
	sandbox, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, mapAPIError(err, "sandbox not found")
	}
	return sandbox, nil
}

// AcquireSandboxHTTPClient leases an HTTP client onto the sandbox for the
// caller's scopes.
//
// It checks that the sandbox still exists and that its pool is up, and nothing
// about whether the sandbox is running: a stopped sandbox is started on demand
// by the pool agent when the request arrives (ADR 0017 §12). Refusing here
// would reject traffic the agent would have served, and would only cover the
// routes that consult the server at all — the git and HTTP proxies are served
// by the agent directly.
func (s *Service) AcquireSandboxHTTPClient(ctx context.Context, projectID, sandboxID string, scopes []string) (*services.HTTPClientLease, *model.Sandbox, error) {
	if err := authorizeRequestedScopes(ctx, scopes); err != nil {
		return nil, nil, err
	}
	sandboxModel, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, nil, mapAPIError(err, "sandbox not found")
	}
	if sandboxModel.DesiredState == model.DesiredStateDeleted {
		return nil, sandboxModel, apperrors.NewStatusError(http.StatusConflict, "sandbox is being deleted")
	}
	pool, err := s.store.GetPool(ctx, projectID, sandboxModel.PoolID)
	if err != nil {
		return nil, sandboxModel, mapAPIError(err, "sandbox pool not found")
	}
	if pool.State != model.PoolStateActive || !pool.Ready {
		return nil, sandboxModel, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf("sandbox pool is not active: pool=%s state=%s ready=%t", pool.ID, pool.State, pool.Ready))
	}
	if s.sandboxProviders == nil {
		return nil, nil, fmt.Errorf("sandbox provider manager is required")
	}
	provider, err := s.sandboxProviders.ResolveForSandbox(ctx, sandboxModel)
	if err != nil {
		return nil, nil, err
	}
	lease, err := provider.AcquireHTTPClient(ctx, sandbox.SandboxRef{ProjectID: sandboxModel.ProjectID, SandboxID: sandboxModel.ID}, sandboxModel.RuntimeState, scopes)
	if err != nil {
		return nil, nil, err
	}
	return lease, sandboxModel, nil
}

func authorizeRequestedScopes(ctx context.Context, scopes []string) error {
	if len(scopes) == 0 {
		return nil
	}
	principal, ok := auth.PrincipalFromContext(ctx)
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
		return nil, mapAPIError(err, "sandbox not found")
	}

	if config, ok := input.Config.Get(); ok {
		if name, ok := config.Name.Get(); ok {
			sandbox.Name = name
		}
	}

	if err := s.store.UpdateSandbox(ctx, sandbox); err != nil {
		return nil, err
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
		return mapAPIError(err, "sandbox not found")
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
		return mapAPIError(err, "sandbox not found")
	}
	if sandbox.DesiredState != model.DesiredStateArchived {
		return apperrors.NewStatusError(http.StatusConflict, "sandbox is not archived")
	}
	if _, err := s.recordSandboxIntent(ctx, projectID, sandboxID, model.DesiredStatePresent); err != nil {
		return mapAPIError(err, "sandbox not found")
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
		return mapAPIError(err, "sandbox not found")
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
// results arrives on the pool agent's reporting channel, which will publish it
// as a project event a moment later. A caller that needs to know the outcome
// watches for that rather than believing this response (ADR 0017 §§9–10).
func (s *Service) StartSandbox(ctx context.Context, projectID, sandboxID string, _ services.StartSandboxBody) (*model.Sandbox, error) {
	return s.instructSandbox(ctx, projectID, sandboxID, sandboxStart)
}

func (s *Service) StopSandbox(ctx context.Context, projectID, sandboxID string, _ services.StopSandboxBody) (*model.Sandbox, error) {
	return s.instructSandbox(ctx, projectID, sandboxID, sandboxStop)
}

func (s *Service) RestartSandbox(ctx context.Context, projectID, sandboxID string, _ services.RestartSandboxBody) (*model.Sandbox, error) {
	return s.instructSandbox(ctx, projectID, sandboxID, sandboxRestart)
}

func (s *Service) instructSandbox(ctx context.Context, projectID, sandboxID string, instruction sandboxInstruction) (*model.Sandbox, error) {
	sandbox, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, mapAPIError(err, "sandbox not found")
	}
	if sandbox.DesiredState == model.DesiredStateDeleted {
		return nil, apperrors.NewStatusError(http.StatusConflict, "sandbox is being deleted")
	}
	if sandbox.DesiredState == model.DesiredStateArchived {
		return nil, apperrors.NewStatusError(http.StatusConflict, "sandbox is archived; unarchive it first")
	}
	provider, err := s.resolveProvider(ctx, sandbox)
	if err != nil {
		return nil, err
	}
	if err := instructSandbox(ctx, s.store, provider, sandbox, instruction); err != nil {
		return nil, err
	}
	return sandbox, nil
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
		return nil, mapAPIError(err, "sandbox not found")
	}
	target, err := s.upgradeTarget(ctx, existing)
	if err != nil {
		return nil, err
	}
	if !target.Available {
		// Distinguish "nothing newer" from "nothing to move to". A config-mode
		// sandbox, one whose harness config was deleted or declares no image,
		// and one whose target image has no known digest all have nothing to
		// move to, and telling their owner they are running "the current image"
		// asserts something that is not true of them.
		if strings.TrimSpace(target.Digest) == "" {
			return nil, apperrors.NewStatusError(http.StatusConflict,
				"sandbox has no image to upgrade to")
		}
		return nil, apperrors.NewStatusError(http.StatusConflict,
			"sandbox is already running its current image")
	}
	// The re-pin is the whole instruction: a changed image digest changes the
	// spec fingerprint, and the pool host rebuilds any container that does not
	// match it (ADR 0017 §5). There is no restart counter to bump.
	sandbox, err := s.recordSandboxIntent(ctx, projectID, sandboxID, model.DesiredStatePresent, func(sb *model.Sandbox) {
		sb.Image = target.Image
		sb.ImageDigest = target.Digest
	})
	if err != nil {
		return nil, mapAPIError(err, "sandbox not found")
	}
	return sandbox, nil
}

// upgradeTarget reports what an upgrade would move the sandbox to, loading the
// harness config the shared rule needs. The rule itself lives in
// services.SandboxUpgradeTarget so this and the read path that reports an
// available upgrade cannot answer differently.
func (s *Service) upgradeTarget(ctx context.Context, sb *model.Sandbox) (UpgradeTarget, error) {
	var config *model.HarnessConfig
	if sb.HarnessConfigID != nil {
		loaded, err := s.store.GetHarnessConfig(ctx, sb.ProjectID, strings.TrimSpace(*sb.HarnessConfigID))
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return UpgradeTarget{}, err
		}
		config = loaded
	}
	target, available := services.SandboxUpgradeTarget(sb, config, s.defaultImageIdentity())
	if target.Digest == "" {
		// Nothing to move to: report what it runs now, so callers can tell that
		// apart from an upgrade that is merely already applied.
		return UpgradeTarget{Image: sb.Image, Digest: sb.ImageDigest}, nil
	}
	return UpgradeTarget{Image: target.Image, Digest: target.Digest, Available: available}, nil
}

// defaultImageIdentity is what a sandbox with no harness config runs, and
// therefore what it upgrades to.
func (s *Service) defaultImageIdentity() services.SandboxImageTarget {
	return services.SandboxImageTarget{Image: s.defaultImage, Digest: s.defaultImageDigest}
}

// DefaultSandboxImage exposes that identity to the API mappers, which report a
// sandbox's available upgrade and need the same answer this service applies.
func (s *Service) DefaultSandboxImage() services.SandboxImageTarget {
	return s.defaultImageIdentity()
}

// UpgradeTarget is what an upgrade would pin the sandbox to, and whether that
// differs from what it runs now.
type UpgradeTarget struct {
	Image     string
	Digest    string
	Available bool
}

// CompleteSandboxSourcePush reports that a client finished pushing into a
// push-delivered sandbox's repository, resuming the sandbox that has been
// parked in awaiting_source since it was provisioned.
//
// The commit is a confirmation, not an instruction. What to check out was fixed
// at create, in the source's Checkout.Commit: the client resolved it from its
// own repository before the sandbox existed, and the push only makes those
// objects reachable. Accepting a different commit here would let a resumed
// sandbox run something other than what its source says it runs, so a mismatch
// is refused rather than recorded.
//
// The server does not verify the commit is present in the sandbox's repository:
// that means a round trip to the worker, and the reconcile that follows fails
// on a missing commit anyway, with a better message than this endpoint could
// produce.
func (s *Service) CompleteSandboxSourcePush(ctx context.Context, projectID, sandboxID string, input services.CompleteSandboxSourcePushBody) (*model.Sandbox, error) {
	existing, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, mapAPIError(err, "sandbox not found")
	}
	if existing.Source == nil || existing.Source.Delivery != model.GitSourceDeliveryPush {
		return nil, apperrors.NewStatusError(http.StatusConflict,
			"sandbox source is not push-delivered; nothing is waiting to be pushed")
	}
	// Reject anything but a sandbox that is actually waiting. A completion for
	// an already-started sandbox would otherwise restart it out from under
	// whatever is running in it.
	if existing.State != model.SandboxStateAwaitingSource {
		return nil, apperrors.NewStatusError(http.StatusConflict,
			fmt.Sprintf("sandbox is not awaiting its source (state %q)", existing.State))
	}
	expected := ""
	if existing.Source.Checkout != nil && existing.Source.Checkout.Commit != nil {
		expected = strings.ToLower(strings.TrimSpace(*existing.Source.Checkout.Commit))
	}
	if expected == "" {
		return nil, apperrors.NewStatusError(http.StatusConflict,
			"sandbox source does not name a commit to check out")
	}
	if reported := strings.ToLower(strings.TrimSpace(input.Commit)); reported != expected {
		return nil, apperrors.NewStatusError(http.StatusConflict,
			fmt.Sprintf("pushed commit %q does not match the source's commit %q", reported, expected))
	}
	now := time.Now().UTC()
	sandbox, err := s.recordSandboxIntent(ctx, projectID, sandboxID, model.DesiredStatePresent, func(sb *model.Sandbox) {
		sb.SourceDeliveredAt = &now
	})
	if err != nil {
		return nil, mapAPIError(err, "sandbox not found")
	}
	return sandbox, nil
}

// CompleteSandboxApply records a successful `disco apply` (ADR 0014): the
// client cherry-picked a source's sandbox commits into a scratch worktree and
// fast-forwarded them onto a host working tree, and reports the result here.
// Called once per source, per apply run, only after the fast-forward has
// already landed the commits — this is a record of something that already
// happened, not a request to do anything.
//
// Like CompleteSandboxSourcePush, this does not verify the reported commits
// against the sandbox's actual Git state: that would mean a round trip to
// the pool agent for a bookkeeping call whose real work already succeeded on
// the client's side. It also carries no lifecycle intent — the sandbox's
// desired or observed runtime state does not change — so it persists via
// updateSandboxMetadata rather than recordSandboxIntent.
func (s *Service) CompleteSandboxApply(ctx context.Context, projectID, sandboxID string, input services.CompleteSandboxApplyBody) (*model.Sandbox, error) {
	existing, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, mapAPIError(err, "sandbox not found")
	}
	slug := strings.TrimSpace(input.Slug)
	if slug == "" || !sandboxHasSourceSlug(existing, slug) {
		return nil, apperrors.NewStatusError(http.StatusConflict,
			fmt.Sprintf("sandbox has no source with slug %q", slug))
	}
	commit := strings.ToLower(strings.TrimSpace(input.Commit))
	hostCommit := strings.ToLower(strings.TrimSpace(input.HostCommit))
	hostID := strings.TrimSpace(input.HostId)
	hostPath := strings.TrimSpace(input.HostPath)
	if commit == "" || hostCommit == "" || hostID == "" || hostPath == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			"commit, hostCommit, hostId, and hostPath are all required")
	}
	entry := model.AppliedSourceCommit{
		Slug:       slug,
		Commit:     commit,
		HostCommit: hostCommit,
		HostID:     hostID,
		HostPath:   hostPath,
		AppliedAt:  time.Now().UTC(),
	}
	sandbox, err := s.updateSandboxMetadata(ctx, projectID, sandboxID, func(sb *model.Sandbox) {
		sb.AppliedCommits = append(sb.AppliedCommits, entry)
	})
	if err != nil {
		return nil, mapAPIError(err, "sandbox not found")
	}
	return sandbox, nil
}

// sandboxHasSourceSlug reports whether slug names the sandbox's primary
// source or one of its secondary sources. GitSource.Slug is always populated
// by DefaultGitSourceSlugs at create, so every source has one to match
// against.
func sandboxHasSourceSlug(sandbox *model.Sandbox, slug string) bool {
	if sandbox.Source != nil && sandbox.Source.Slug != nil && *sandbox.Source.Slug == slug {
		return true
	}
	for _, source := range sandbox.SourceCodeReferences {
		if source.Slug != nil && *source.Slug == slug {
			return true
		}
	}
	return false
}

func (s *Service) ReconcileSandbox(ctx context.Context, projectID, sandboxID string) (*model.Sandbox, error) {
	sandbox, err := s.scheduleSandboxReconcile(ctx, projectID, sandboxID)
	if err != nil {
		return nil, mapAPIError(err, "sandbox not found")
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
		return "", mapAPIError(err, "sandbox not found")
	}
	return s.sandboxAuth.CreateToken(ctx, sandboxauth.TokenClaims{
		ProjectID: sb.ProjectID,
		SandboxID: sb.ID,
		UserID:    sb.CreatedByUserID,
	})
}
