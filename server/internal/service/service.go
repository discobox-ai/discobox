// Package service contains application services.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/discobox-ai/discobox/devimage"
	"github.com/discobox-ai/discobox/endpoint"
	"github.com/discobox-ai/discobox/judge/jev"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	sandboxauth "github.com/discobox-ai/discobox/server/internal/auth/sandbox"
	"github.com/discobox-ai/discobox/server/internal/harnessdefs"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/resources/assets"
	"github.com/discobox-ai/discobox/server/internal/resources/gateways"
	"github.com/discobox-ai/discobox/server/internal/resources/harnessconfigs"
	resourcejobs "github.com/discobox-ai/discobox/server/internal/resources/jobs"
	"github.com/discobox-ai/discobox/server/internal/resources/judges"
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
	judges           *judges.Service
	assets           *assets.Service
	// ruleRequests tests approval rules against pending credential requests.
	ruleRequests *secrets.RuleRequests
}

type Options struct {
	Harnesses                      harnessdefs.References
	SandboxReconcileJobConcurrency int
	DevelopmentImageSync           *dockerworker.DevelopmentImageSynchronizer
	// DevelopmentImages is the watcher-built image set. Harness seeding reads
	// build-mode metadata from it, since those images exist only as build
	// descriptions until a pool's daemon builds them.
	DevelopmentImages []devimage.Image
	// DevelopmentBundles are the bundles the watcher exported as OCI image
	// layouts: harness seeding reads their labels, and the asset API serves
	// them, from those layouts (ADR 26-10-16-423 §13).
	DevelopmentBundles []devimage.Bundle
	// ControlPlaneStreams carries guest-initiated control-plane connections for
	// backends that cannot be dialed inward.
	ControlPlaneStreams *carrierhub.Hub
	// ListenEndpoints are the endpoints the server listens on; providers that
	// dial the control plane inward derive their address from them. Empty means
	// the local IPC endpoint an unconfigured server binds, which is the same
	// rule config applies.
	ListenEndpoints []string
	// ServerDefaults are the provider settings the server configures rather
	// than any one provider instance: the pool-agent image override and the
	// host daemon's image retention window.
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
	// Judging is what this server judges: commands, credential-bearing
	// requests, both or neither (ADR 26-10-02-054). While it judges nothing,
	// no project has a judge and nothing is asked one.
	Judging judges.Judging
	// JudgeJev, when set, is what judges instead of a judge discobox per
	// project (ADR 26-10-01-324).
	JudgeJev *jev.Client
	// JudgeJevFallback puts every job Jev is unsure of to the project's judge
	// discobox, which every project then keeps and which decides it, rather than
	// refusing it. A refusal Jev is sure of stands either way.
	JudgeJevFallback bool
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
	harnessConfigService.SetDevelopmentImages(options.DevelopmentImages, options.DevelopmentBundles)
	// The configure flow runs an ephemeral sandbox and watches it through the
	// reconcile engine, so it needs both.
	harnessConfigService.SetSandboxRuntime(sandboxService)
	harnessConfigService.SetDirtier(engine)
	// One service answers both: host trust is the credential broker's act
	// about a different thing (ADR 0149), and shares its pool-ownership check.
	secretService := secrets.NewService(store)
	// A create's grants are prepared by the credential broker, which holds a
	// discobox giving them to what it was delegated (ADR 26-09-30-782 §1).
	sandboxService.SetSecrets(secretService)
	// And back: a change to a secret's routes is carried to the running
	// discoboxes holding it (ADR 26-10-02-924).
	secretService.SetSecretHolders(sandboxService)
	// The project's judge is converged like any other resource (ADR 26-09-22-838 §1):
	// it exists when the project has a pool for it and a configured default
	// harness, and is replaced when that harness is — unless the server judges
	// with Jev, when no project has a judge discobox (ADR 26-10-01-324 §2).
	judgeService := judges.New(store, sandboxService, nil, options.Judging, options.JudgeJev, options.JudgeJevFallback)
	// Reaching the judge's own agent is the sandbox service's to do; which
	// discobox is the judge is this one's (ADR 26-09-22-838 §2).
	judgeService.SetLeases(sandboxService)
	// What an approved use allows is the credential broker's to say, so the
	// question a judge is put is composed from the live grant rather than from
	// what the asking pool sent (ADR 26-09-22-838 §4). It is the same service that
	// answers for host trust, built above.
	judgeService.SetUses(secretService)
	// And the other way: a discobox handing a credential on is asked of the
	// project's judge — whether the uses fall within its delegation — while
	// the credential broker approves (ADR 26-09-30-782 §3).
	secretService.SetJudge(judgeService)
	// Approval rules are tested against a request when it is filed and when
	// a rule changes (ADR 26-10-12-330 §4); the engine is what runs the tests.
	secretService.SetDirtier(engine)
	// The bundles a release names are the ones the server's own registry
	// credentials pull; a project's are pulled without them.
	var releaseBundles []string
	for _, seed := range harnessdefs.Seeds(options.Harnesses, options.ServerDefaults.Release != nil) {
		releaseBundles = append(releaseBundles, seed.Bundle)
	}
	assetService := assets.New(store, assets.Options{
		Dir:            options.AssetDir,
		PlatformBundle: options.Harnesses.PlatformBundle,
		ReleaseBundles: releaseBundles,
		Development:    options.DevelopmentBundles,
	})
	// Every sandbox is pinned to the platform the release names, and a
	// stopped one follows it when the reference resolves somewhere new (ADR
	// 26-10-16-423 §7).
	sandboxService.SetPlatformBundles(assetService)
	assetService.OnPlatformMoved(func(ctx context.Context) {
		if err := sandboxService.UpgradePlatformSandboxes(ctx); err != nil {
			slog.WarnContext(ctx, "platform bundle moved but stopped sandboxes were not upgraded", "error", err)
		}
	})
	return &Service{
		ProjectService:                 projects.NewService(store, providerService, poolService, harnessConfigService, sandboxService),
		HarnessConfigService:           harnessConfigService,
		Service:                        sandboxService,
		SandboxProviderInstanceService: providerService,
		PoolService:                    poolService,
		JobService:                     jobsService,
		SecretService:                  secretService,
		HostTrustService:               secretService,
		SSHKeyService:                  sshkeys.NewService(store),
		SkillsSourceService:            skillssources.NewService(store),
		GatewayService:                 gateways.NewService(store, sandboxService),
		PeerService:                    peers.NewService(store),

		jobs:            jobsService,
		providerService: providerService,

		store:            store,
		engine:           engine,
		options:          options,
		poolControlPlane: poolControlPlane,
		harnessConfigs:   harnessConfigService,
		judges:           judgeService,
		assets:           assetService,
		ruleRequests:     secretService.RuleRequests(),
	}
}

func (s *Service) SetSandboxAuthManager(manager *sandboxauth.Manager) {
	s.Service.SetSandboxAuthManager(manager)
}

func (s *Service) SetDefaultSandboxImage(image, digest string) {
	s.Service.SetDefaultSandboxImage(image, digest)
	// The preloader needs the same answer. The package default is the
	// effective image only when nothing overrode it, so reading that instead
	// would have a server told to run a different sandbox image prestage the
	// one it is not going to use.
	s.poolControlPlane.SetDefaultSandboxImage(image)
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
	s.poolControlPlane.StartBootstrapTokenCleanup(ctx)
	s.assets.StartPruning(ctx)
	go s.assets.ResolvePlatform(ctx)
	go func() {
		if err := s.PinStoppedSandboxBundles(ctx); err != nil {
			slog.WarnContext(ctx, "pin stopped sandboxes' bundles", "error", err)
		}
	}()
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
	// Drives in-flight harness configure flows to completion, including ones that
	// outlive a server restart.
	if err := s.engine.Register(harnessconfigs.HarnessConfigResourceType, s.harnessConfigs); err != nil {
		return err
	}
	// Keeps each project's judge matching what the project says it should be,
	// and finds the ones nothing thought to mark.
	if err := s.engine.Register(judges.JudgeResourceType, s.judges); err != nil {
		return err
	}
	// Tests each project's approval rules against its pending credential
	// requests, off the path that files one (ADR 26-10-12-330 §4).
	if err := s.engine.Register(secrets.RuleRequestResourceType, s.ruleRequests); err != nil {
		return err
	}
	return s.poolControlPlane.RegisterJobs(s.SandboxProviderManager())
}

func (s *Service) SandboxProviderManager() *sandbox.ProviderManager {
	return s.Service.SandboxProviderManager()
}

// Judges answers a pool asking for a verdict from the project's judge.
func (s *Service) Judges() *judges.Service { return s.judges }

// Assets is the asset API's service, which only a pool reaches.
func (s *Service) Assets() *assets.Service { return s.assets }
