// Package service contains application services.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/discobox-ai/discobox/pkg/endpoint"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	sandboxauth "github.com/discobox-ai/discobox/server/internal/auth/sandbox"
	"github.com/discobox-ai/discobox/server/internal/devbundles"
	"github.com/discobox-ai/discobox/server/internal/harnessdefs"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/resources/assets"
	"github.com/discobox-ai/discobox/server/internal/resources/events"
	"github.com/discobox-ai/discobox/server/internal/resources/gateways"
	"github.com/discobox-ai/discobox/server/internal/resources/harnessconfigs"
	resourcejobs "github.com/discobox-ai/discobox/server/internal/resources/jobs"
	"github.com/discobox-ai/discobox/server/internal/resources/peers"
	"github.com/discobox-ai/discobox/server/internal/resources/pools"
	"github.com/discobox-ai/discobox/server/internal/resources/projects"
	"github.com/discobox-ai/discobox/server/internal/resources/providers"
	sandboxes "github.com/discobox-ai/discobox/server/internal/resources/sandboxes"
	"github.com/discobox-ai/discobox/server/internal/resources/secrets"
	"github.com/discobox-ai/discobox/server/internal/resources/skillssources"
	"github.com/discobox-ai/discobox/server/internal/resources/sshkeys"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/server/internal/transport/carrierhub"
	providerregistry "github.com/discobox-ai/discobox/server/providers"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
)

const (
	DefaultUserID = "user_default"
)

// Service implements the API service interfaces using the database store.
type Service struct {
	services.ProjectService
	services.HarnessConfigService
	*sandboxes.Service
	services.SandboxProviderInstanceService
	services.PoolService
	services.JobService
	services.SecretService
	services.HostTrustService
	services.SSHKeyService
	services.SkillsSourceService
	services.GatewayService
	services.PeerService

	store            *store.Store
	engine           *reconcile.Engine
	options          Options
	jobs             *resourcejobs.Service
	providerService  *providers.Service
	poolControlPlane *pools.ControlPlane
	harnessConfigs   *harnessconfigs.Service
	secrets          *secrets.Service
	assets           *assets.Service
	gateways         *gateways.Service
	events           *events.Service
}

type Options struct {
	Harnesses                      harnessdefs.References
	SandboxReconcileJobConcurrency int
	DevelopmentImageSync           *dockerworker.DevelopmentImageSynchronizer
	// DevelopmentBundles are the development bundles: the ones the watcher
	// exported as OCI image layouts, and in build-mode the ones a pool's
	// daemon builds. Harness seeding reads their labels, and the asset API
	// serves them, from their layouts (ADR 26-10-16-423 §13).
	DevelopmentBundles *devbundles.Set
	// ControlPlaneStreams carries guest-initiated control-plane connections for
	// backends that cannot be dialed inward.
	ControlPlaneStreams *carrierhub.Hub
	// ListenEndpoints are the endpoints the server listens on; providers that
	// dial the control plane inward derive their address from them. Empty means
	// the local IPC endpoint an unconfigured server binds, which is the same
	// rule config applies.
	ListenEndpoints []string
	// ServerDefaults are the provider settings the server configures rather
	// than any one provider instance: the sandbox base image and bundle
	// overrides, and the host daemon's image retention window.
	ServerDefaults dockerworker.ServerDefaults
	// WSLCCommand overrides the WSL Containers program the Windows host is
	// checked for.
	WSLCCommand string
	// OverlayDir is the one directory a harness's file:// manifest reference
	// may name a file in. Empty refuses every such reference.
	OverlayDir string
	// AssetDir is the blob store bundles are served to pools from (ADR
	// 26-10-16-423 §13).
	AssetDir string
	// ArchiveRetention is the server-wide default an archived sandbox is kept
	// for, which a project follows until it sets its own. Zero leaves the
	// package default (24h) in force.
	ArchiveRetention time.Duration
	// ServerPeerID is this server's own peer ID, which names it in the address
	// a sandbox is started with. Empty when the server does not listen on iroh,
	// where that address would reach nothing; sandboxes then get none.
	ServerPeerID string
	// ConfiguredGateways is the server configuration file's gateways
	// (ADR 26-10-17-271).
	ConfiguredGateways gateways.Configured
}

func New(store *store.Store, engine *reconcile.Engine, options Options) *Service {
	if len(options.ListenEndpoints) == 0 {
		options.ListenEndpoints = []string{endpoint.DefaultEndpoint()}
	}
	manager := sandbox.NewProviderManager()
	poolControlPlane := pools.NewControlPlane(store, engine)
	providerregistry.RegisterBuiltInSandboxProviderFactories(manager, poolControlPlane, providerregistry.FactoryOptions{
		DevelopmentImageSync: options.DevelopmentImageSync,
		ControlPlaneStreams:  options.ControlPlaneStreams,
		ListenEndpoints:      options.ListenEndpoints,
		ServerDefaults:       options.ServerDefaults,
		WSLCCommand:          options.WSLCCommand,
	})
	sandboxService := sandboxes.NewService(store, manager, DefaultUserID, engine)
	providerService := providers.NewService(store, sandboxService, poolControlPlane)
	poolService := pools.NewService(store, manager, poolControlPlane, sandboxService)
	poolService.SetSandboxStateReporter(sandboxService)
	jobsService := resourcejobs.NewService(store, engine)
	sandboxService.SetArchiveRetention(options.ArchiveRetention)
	sandboxService.SetServerPeerID(options.ServerPeerID)
	harnessConfigService := harnessconfigs.NewService(store, options.Harnesses, options.ServerDefaults.Release != nil, options.OverlayDir)
	harnessConfigService.SetDevelopmentBundles(options.DevelopmentBundles)
	// The configure flow runs an ephemeral sandbox and watches it through the
	// reconcile engine, so it needs both.
	harnessConfigService.SetSandboxRuntime(sandboxService)
	harnessConfigService.SetDirtier(engine)
	// One service answers both: host trust is the credential broker's act
	// about a different thing (ADR 0149), and shares its pool-ownership check.
	secretService := secrets.NewService(store)
	// A create's bindings are prepared by the credential broker, which holds
	// a discobox giving them to what it was delegated (ADR 26-10-27-880 §3).
	sandboxService.SetSecrets(secretService)
	// And back: a change to a secret's routes, or to a binding, is carried to
	// the discoboxes it reaches (ADR 26-10-27-880 §2).
	secretService.SetSecretHolders(sandboxService)
	// The bundles a release names are the ones the server's own registry
	// credentials pull; a project's are pulled without them.
	var releaseBundles []string
	for _, seed := range harnessdefs.Seeds(options.Harnesses, options.ServerDefaults.Release != nil) {
		releaseBundles = append(releaseBundles, seed.Bundle)
	}
	assetService := assets.New(store, assets.Options{
		Dir:            options.AssetDir,
		PlatformBundle: options.Harnesses.PlatformBundle,
		AgentBundle:    dockerworker.EffectiveAgentBundle(options.ServerDefaults),
		// The pool bundle: what makes a pool's box a pool's, served from the
		// blob store to the box that fetches it (ADR 26-10-24-953 §2).
		PoolBundle:     dockerworker.EffectivePoolAgentBundle(options.ServerDefaults),
		ReleaseBundles: releaseBundles,
		Development:    options.DevelopmentBundles,
	})
	// A development bundle a pool's daemon built is a new digest for what it
	// names: the built-in harnesses are seeded again onto it, and the platform
	// resolved again (ADR 26-10-16-423 §13).
	options.DevelopmentBundles.OnBuilt(func() {
		ctx := context.Background()
		projects, err := store.ListProjects(ctx)
		if err != nil {
			slog.WarnContext(ctx, "development bundle built but the harnesses were not seeded onto it", "error", err)
		}
		for _, project := range projects {
			if err := harnessConfigService.SeedBuiltIns(ctx, project.ID); err != nil {
				slog.WarnContext(ctx, "development bundle built but a project's harnesses were not seeded onto it", "projectId", project.ID, "error", err)
			}
		}
		assetService.ResolveRelease(ctx)
	})
	// Every sandbox is pinned to the platform and the agent the release
	// names, and a stopped one follows them when a reference resolves
	// somewhere new (ADR 26-10-16-423 §7, ADR 26-10-24-953 §2). A pool's box
	// is placed with the agent and its bundles, and applies a moved one in
	// place at its next reconcile, which a move asks for.
	sandboxService.SetReleaseBundles(assetService)
	poolControlPlane.SetPoolBoxes(assetService)
	assetService.OnReleaseMoved(func(ctx context.Context) {
		reconcileEveryPool(ctx, store, poolControlPlane)
		if err := sandboxService.UpgradeReleaseSandboxes(ctx); err != nil {
			slog.WarnContext(ctx, "a release bundle moved but stopped sandboxes were not upgraded", "error", err)
		}
		// A sandbox from before bundles waits on them to be pinned to.
		if err := sandboxService.PinUndeliveredSandboxes(ctx); err != nil {
			slog.WarnContext(ctx, "pin sandboxes from before bundles", "error", err)
		}
	})
	// The server configuration file's gateways (ADR 26-10-17-271), which
	// follow the default project when it moves.
	gatewayService := gateways.NewService(store, sandboxService, options.ConfiguredGateways, DefaultUserID)
	return &Service{
		ProjectService:                 projects.NewService(store, providerService, poolService, harnessConfigService, sandboxService, gatewayService),
		HarnessConfigService:           harnessConfigService,
		Service:                        sandboxService,
		SandboxProviderInstanceService: providerService,
		PoolService:                    poolService,
		JobService:                     jobsService,
		SecretService:                  secretService,
		HostTrustService:               secretService,
		SSHKeyService:                  sshkeys.NewService(store),
		SkillsSourceService:            skillssources.NewService(store),
		GatewayService:                 gatewayService,
		PeerService:                    peers.NewService(store),

		jobs:            jobsService,
		providerService: providerService,

		store:            store,
		engine:           engine,
		options:          options,
		poolControlPlane: poolControlPlane,
		harnessConfigs:   harnessConfigService,
		secrets:          secretService,
		assets:           assetService,
		gateways:         gatewayService,
		events:           events.NewService(store),
	}
}

func (s *Service) SetSandboxAuthManager(manager *sandboxauth.Manager) {
	s.Service.SetSandboxAuthManager(manager)
}

func (s *Service) SetWorkerAgentAuthManager(manager *poolagentauth.Manager) {
	s.poolControlPlane.SetAgentAuthManager(manager)
}

func (s *Service) Start(ctx context.Context) error {
	if s.engine == nil {
		return errors.New("reconcile engine is required")
	}
	if err := s.registerReconcilers(); err != nil {
		return err
	}
	if err := s.store.BeginPoolHealthChecks(ctx); err != nil {
		return err
	}
	// Before anything mints a sentinel: a stored shape read under an older
	// provider table is brought up to date, so the first sandbox after an
	// upgrade already gets the current one.
	if err := s.store.RefreshSecretFormats(ctx); err != nil {
		return fmt.Errorf("refresh secret formats: %w", err)
	}
	if err := s.engine.Start(ctx); err != nil {
		return err
	}
	s.assets.StartPruning(ctx)
	go s.assets.ResolveRelease(ctx)
	s.StartPinningUndelivered(ctx)
	if err := s.EnsureExistingSandboxProviderInstances(ctx); err != nil {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = s.engine.Stop(stopCtx)
		return err
	}
	return nil
}

// Stop shuts down the reconcile engine, waiting for in-flight reconciles, then
// closes every provider.
//
// Provider close is what releases a backend's own resources -- for wslc, the
// COM session whose lifetime is the pool VM's. Losing the handle when the
// process dies happens to tear that VM down too, but only after the service
// notices, and only for a backend whose resources are bound to this process at
// all. Closing them here makes teardown deterministic rather than incidental.
//
// The engine stops first: a reconcile still in flight may be talking to a
// provider, and closing underneath it would fail the operation rather than let
// it finish.
func (s *Service) Stop(ctx context.Context) error {
	var err error
	if s.engine != nil {
		err = s.engine.Stop(ctx)
	}
	s.SandboxProviderManager().Shutdown()
	return err
}

func (s *Service) registerReconcilers() error {
	concurrency := s.options.SandboxReconcileJobConcurrency
	sandboxOptions := []reconcile.RegisterOption(nil)
	if concurrency > 0 {
		sandboxOptions = append(sandboxOptions, reconcile.WithConcurrency(concurrency))
	}
	if err := s.RegisterJobs(sandboxOptions...); err != nil {
		return err
	}
	// Delivers committed harness capture acknowledgments and reaps configure
	// flows, including accepted intent that outlives a server restart.
	if err := s.engine.Register(harnessconfigs.HarnessConfigResourceType, s.harnessConfigs); err != nil {
		return err
	}
	// Removes lapsed bindings with their expired events (ADR 26-10-27-880 §3).
	if err := s.engine.Register(store.BindingExpiryResourceType, s.secrets.NewBindingExpiryReconciler()); err != nil {
		return err
	}
	// Prunes the project event log past retention (ADR 26-10-30-868 §6).
	if err := s.engine.Register(events.RetentionResourceType, s.events.NewRetentionReconciler()); err != nil {
		return err
	}
	return s.poolControlPlane.RegisterJobs(s.SandboxProviderManager())
}

func (s *Service) SandboxProviderManager() *sandbox.ProviderManager {
	return s.Service.SandboxProviderManager()
}

// Assets is the asset API's service, which only a pool reaches.
func (s *Service) Assets() *assets.Service { return s.assets }

// Events is the project event stream's service (ADR 26-10-30-868).
func (s *Service) Events() *events.Service { return s.events }

// reconcileEveryPool marks every pool for a reconcile, which provisions a
// pool's box again in place when its bundles moved (ADR 26-10-24-953 §5).
func reconcileEveryPool(ctx context.Context, store *store.Store, pools *pools.ControlPlane) {
	projects, err := store.ListProjects(ctx)
	if err != nil {
		slog.WarnContext(ctx, "a release bundle moved but pools were not reconciled", "error", err)
		return
	}
	for _, project := range projects {
		listed, err := store.ListPools(ctx, project.ID)
		if err != nil {
			slog.WarnContext(ctx, "a release bundle moved but a project's pools were not reconciled", "projectId", project.ID, "error", err)
			continue
		}
		for _, pool := range listed {
			if err := pools.SchedulePoolReconciliation(ctx, pool.ProjectID, pool.ID); err != nil {
				slog.WarnContext(ctx, "a release bundle moved but a pool was not reconciled", "poolId", pool.ID, "error", err)
			}
		}
	}
}
