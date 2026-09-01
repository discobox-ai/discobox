// Package service contains application services.
package service

import (
	"context"
	"errors"
	"time"

	"github.com/discobox-ai/discobox/devimage"
	"github.com/discobox-ai/discobox/endpoint"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	sandboxauth "github.com/discobox-ai/discobox/server/internal/auth/sandbox"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/resources/harnessconfigs"
	resourcejobs "github.com/discobox-ai/discobox/server/internal/resources/jobs"
	"github.com/discobox-ai/discobox/server/internal/resources/pools"
	"github.com/discobox-ai/discobox/server/internal/resources/projects"
	"github.com/discobox-ai/discobox/server/internal/resources/providers"
	sandboxes "github.com/discobox-ai/discobox/server/internal/resources/sandboxes"
	"github.com/discobox-ai/discobox/server/internal/resources/secrets"
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
	services.SSHKeyService

	store            *store.Store
	engine           *reconcile.Engine
	options          Options
	jobs             *resourcejobs.Service
	providerService  *providers.Service
	poolControlPlane *pools.ControlPlane
	harnessConfigs   *harnessconfigs.Service
}

type Options struct {
	SandboxReconcileJobConcurrency int
	DevelopmentImageSync           *dockerworker.DevelopmentImageSynchronizer
	// DevelopmentImages is the watcher-built image set. Harness seeding reads
	// build-mode metadata from it, since those images exist only as build
	// descriptions until a pool's daemon builds them.
	DevelopmentImages []devimage.Image
	// ControlPlaneStreams carries guest-initiated control-plane connections for
	// backends that cannot be dialed inward.
	ControlPlaneStreams *carrierhub.Hub
	// ListenEndpoints are the endpoints the server listens on; providers that
	// dial the control plane inward derive their address from them. Empty means
	// the local IPC endpoint an unconfigured server binds, which is the same
	// rule config applies.
	ListenEndpoints []string
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
	})
	sandboxService := sandboxes.NewService(store, manager, DefaultUserID, engine, poolControlPlane)
	providerService := providers.NewService(store, sandboxService, poolControlPlane)
	poolService := pools.NewService(store, manager, poolControlPlane)
	poolService.SetSandboxStateReporter(sandboxService)
	jobsService := resourcejobs.NewService(store, engine)
	harnessConfigService := harnessconfigs.NewService(store)
	harnessConfigService.SetDevelopmentImages(options.DevelopmentImages)
	// The configure flow runs an ephemeral sandbox and watches it through the
	// reconcile engine, so it needs both.
	harnessConfigService.SetSandboxRuntime(sandboxService)
	harnessConfigService.SetDirtier(engine)
	return &Service{
		ProjectService:                 projects.NewService(store, providerService, poolService, harnessConfigService),
		HarnessConfigService:           harnessConfigService,
		Service:                        sandboxService,
		SandboxProviderInstanceService: providerService,
		PoolService:                    poolService,
		JobService:                     jobsService,
		SecretService:                  secrets.NewService(store),
		SSHKeyService:                  sshkeys.NewService(store),

		jobs:            jobsService,
		providerService: providerService,

		store:            store,
		engine:           engine,
		options:          options,
		poolControlPlane: poolControlPlane,
		harnessConfigs:   harnessConfigService,
	}
}

func (s *Service) SetSandboxAuthManager(manager *sandboxauth.Manager) {
	s.Service.SetSandboxAuthManager(manager)
}

func (s *Service) SetDefaultSandboxImage(image, digest string) {
	s.Service.SetDefaultSandboxImage(image, digest)
	// The preloader needs the same answer. It used to read the package
	// default instead, which is only the effective image when nothing
	// overrode it — so a server told to run a different sandbox image
	// prestaged the one it was not going to use.
	s.poolControlPlane.SetDefaultSandboxImage(image)
}

func (s *Service) SetHostID(hostID string) {
	s.Service.SetHostID(hostID)
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
	if err := s.engine.Start(ctx); err != nil {
		return err
	}
	s.poolControlPlane.StartBootstrapTokenCleanup(ctx)
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
	return s.poolControlPlane.RegisterJobs(s.SandboxProviderManager())
}

func (s *Service) SandboxProviderManager() *sandbox.ProviderManager {
	return s.Service.SandboxProviderManager()
}
