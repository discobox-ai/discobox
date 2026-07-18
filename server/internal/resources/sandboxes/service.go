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
	store            *store.Store
	engine           *reconcile.Engine
	sandboxProviders *sandbox.ProviderManager
	providerStore    any
	sandboxAuth      *sandboxauth.Manager
	defaultUserID    string
	defaultImage     string
	hostID           string
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

func (s *Service) SetDefaultSandboxImage(image string) {
	if image = strings.TrimSpace(image); image != "" {
		s.defaultImage = image
	}
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
	Available    bool
	BuiltIn      bool
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
	userName, userUID, userGID, homeDirectory := services.SandboxUserToModel(config.User)
	harnessMode := "run"
	if mode, ok := config.HarnessMode.Get(); ok {
		harnessMode = string(mode)
	}
	image := strings.TrimSpace(config.Image.Or(""))
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
		}
	}
	if image == "" {
		image = s.defaultImage
	}
	sandbox := &model.Sandbox{
		ID:                   sandboxID,
		ProjectID:            projectID,
		CreatedByUserID:      userID,
		PoolID:               pool.ID,
		HarnessConfigID:      harnessConfigID,
		HarnessMode:          harnessMode,
		Name:                 config.Name,
		Description:          services.OptStringPtr(config.Description),
		ResourceLifecycle:    model.NewResourceLifecycle(model.SandboxCreateOperation),
		Model:                services.OptStringPtr(config.Model),
		ModelServiceTier:     services.OptStringPtr(config.ModelServiceTier),
		ModelReasoningLevel:  services.OptStringPtr(config.ModelReasoningLevel),
		Prompt:               config.Prompt,
		Image:                image,
		Env:                  map[string]string(config.Env.Or(nil)),
		Source:               source,
		SourceRoot:           sourceRoot,
		SourceCodeReferences: sourceCodeReferences,
		Origin:               origin,
		OriginKey:            originKey,
		UserName:             userName,
		UserUID:              userUID,
		UserGID:              userGID,
		HomeDirectory:        homeDirectory,
		CPUVCPUs:             config.CpuVcpus.Or(0),
		MemoryBytes:          config.MemoryBytes.Or(0),
		StorageBytes:         config.StorageBytes.Or(0),
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
	if harnessConfigID != nil && strings.TrimSpace(*harnessConfigID) != "" && harnessMode != "config" {
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
	created, err := s.createSandboxIntent(ctx, sandbox)
	if err != nil {
		return nil, err
	}
	for _, assignment := range assignments {
		if err := s.store.CreateSandboxSecret(ctx, assignment); err != nil {
			return nil, fmt.Errorf("persist sandbox secret assignment: %w", err)
		}
	}
	return created, nil
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

func (s *Service) AcquireSandboxHTTPClient(ctx context.Context, projectID, sandboxID string, scopes []string) (*services.HTTPClientLease, *model.Sandbox, error) {
	if err := authorizeRequestedScopes(ctx, scopes); err != nil {
		return nil, nil, err
	}
	sandboxModel, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, nil, mapAPIError(err, "sandbox not found")
	}
	if sandboxModel.Phase != model.SandboxPhaseRunning {
		return nil, sandboxModel, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf("sandbox is not running: phase=%s", sandboxModel.Phase))
	}
	pool, err := s.store.GetPool(ctx, projectID, sandboxModel.PoolID)
	if err != nil {
		return nil, sandboxModel, mapAPIError(err, "sandbox pool not found")
	}
	if pool.Phase != model.PoolPhaseActive || !pool.Ready {
		return nil, sandboxModel, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf("sandbox pool is not active: pool=%s phase=%s ready=%t", pool.ID, pool.Phase, pool.Ready))
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

func (s *Service) DeleteSandbox(ctx context.Context, projectID, sandboxID string) error {
	_, err := s.submitSandboxOperation(ctx, projectID, sandboxID, model.SandboxDeleteOperation)
	if err != nil {
		return mapAPIError(err, "sandbox not found")
	}
	return nil
}

func (s *Service) StartSandbox(ctx context.Context, projectID, sandboxID string, _ services.StartSandboxBody) (*model.Sandbox, error) {
	sandbox, err := s.submitSandboxOperation(ctx, projectID, sandboxID, model.SandboxStartOperation)
	if err != nil {
		return nil, mapAPIError(err, "sandbox not found")
	}
	return sandbox, nil
}

func (s *Service) StopSandbox(ctx context.Context, projectID, sandboxID string, _ services.StopSandboxBody) (*model.Sandbox, error) {
	sandbox, err := s.submitSandboxOperation(ctx, projectID, sandboxID, model.SandboxStopOperation)
	if err != nil {
		return nil, mapAPIError(err, "sandbox not found")
	}
	return sandbox, nil
}

func (s *Service) RestartSandbox(ctx context.Context, projectID, sandboxID string, _ services.RestartSandboxBody) (*model.Sandbox, error) {
	sandbox, err := s.submitSandboxOperation(ctx, projectID, sandboxID, model.SandboxRestartOperation, func(sandbox *model.Sandbox) {
		sandbox.RestartGeneration++
	})
	if err != nil {
		return nil, mapAPIError(err, "sandbox not found")
	}
	return sandbox, nil
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
	if existing.Phase != model.SandboxPhaseAwaitingSource {
		return nil, apperrors.NewStatusError(http.StatusConflict,
			fmt.Sprintf("sandbox is not awaiting its source (phase %q)", existing.Phase))
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
	sandbox, err := s.submitSandboxOperation(ctx, projectID, sandboxID, model.SandboxStartOperation, func(sb *model.Sandbox) {
		sb.SourceDeliveredAt = &now
		sb.StatusMessage = nil
	})
	if err != nil {
		return nil, mapAPIError(err, "sandbox not found")
	}
	return sandbox, nil
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
		status, registered := statuses[id]
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
			Available:    !registered || status.Available,
			BuiltIn:      registered,
			Capabilities: status,
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
