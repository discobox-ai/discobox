// Package service contains application services.
package service

import (
	"context"
	"errors"
	"time"

	"github.com/obot-platform/discobox/devimage"
	poolagentauth "github.com/obot-platform/discobox/server/internal/auth/poolagent"
	sandboxauth "github.com/obot-platform/discobox/server/internal/auth/sandbox"
	eventbroker "github.com/obot-platform/discobox/server/internal/events"
	"github.com/obot-platform/discobox/server/internal/reconcile"
	resourceevents "github.com/obot-platform/discobox/server/internal/resources/events"
	"github.com/obot-platform/discobox/server/internal/resources/harnessconfigs"
	resourcejobs "github.com/obot-platform/discobox/server/internal/resources/jobs"
	"github.com/obot-platform/discobox/server/internal/resources/pools"
	"github.com/obot-platform/discobox/server/internal/resources/projects"
	"github.com/obot-platform/discobox/server/internal/resources/providers"
	sandboxes "github.com/obot-platform/discobox/server/internal/resources/sandboxes"
	"github.com/obot-platform/discobox/server/internal/resources/secrets"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	services "github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
	"github.com/obot-platform/discobox/server/internal/transport/carrierhub"
	providerregistry "github.com/obot-platform/discobox/server/providers"
	"github.com/obot-platform/discobox/server/providers/dockerworker"
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
	services.ProjectEventService
	services.SecretService

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
}

func New(store *store.Store, engine *reconcile.Engine, options Options, broker ...*eventbroker.Broker) *Service {
	var b *eventbroker.Broker
	if len(broker) > 0 {
		b = broker[0]
	}
	manager := sandbox.NewProviderManager()
	poolControlPlane := pools.NewControlPlane(store, engine)
	providerregistry.RegisterBuiltInSandboxProviderFactories(manager, poolControlPlane, providerregistry.FactoryOptions{
		DevelopmentImageSync: options.DevelopmentImageSync,
		ControlPlaneStreams:  options.ControlPlaneStreams,
	})
	sandboxService := sandboxes.NewService(store, manager, DefaultUserID, engine, poolControlPlane)
	providerService := providers.NewService(store, sandboxService, poolControlPlane)
	poolService := pools.NewService(store, poolControlPlane)
	poolService.SetSandboxRemovalReporter(sandboxService)
	jobsService := resourcejobs.NewService(store, engine)
	harnessConfigService := harnessconfigs.NewService(store)
	harnessConfigService.SetDevelopmentImages(options.DevelopmentImages)
	// The configure flow runs an ephemeral sandbox and watches it through the
	// reconcile engine, so it needs both.
	harnessConfigService.SetSandboxRuntime(sandboxService)
	harnessConfigService.SetDirtier(engine)
	return &Service{
		ProjectService:                 projects.NewService(store),
		HarnessConfigService:           harnessConfigService,
		Service:                        sandboxService,
		SandboxProviderInstanceService: providerService,
		PoolService:                    poolService,
		JobService:                     jobsService,
		ProjectEventService:            resourceevents.NewService(store, b),
		SecretService:                  secrets.NewService(store),

		jobs:            jobsService,
		providerService: providerService,

		store:            store,
		engine:           engine,
		options:          options,
		poolControlPlane: poolControlPlane,
		harnessConfigs:   harnessConfigService,
	}
}

// SetHarnessImages installs per-harness image overrides (built-in slug → image),
// used by dev builds to point the seeded built-ins at freshly tagged images.
func (s *Service) SetHarnessImages(images map[string]string) {
	s.harnessConfigs.SetHarnessImages(images)
}

func (s *Service) SetSandboxAuthManager(manager *sandboxauth.Manager) {
	s.Service.SetSandboxAuthManager(manager)
}

func (s *Service) SetDefaultSandboxImage(image string) {
	s.Service.SetDefaultSandboxImage(image)
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

// Stop shuts down the reconcile engine, waiting for in-flight reconciles.
func (s *Service) Stop(ctx context.Context) error {
	if s.engine == nil {
		return nil
	}
	return s.engine.Stop(ctx)
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
