// Package service contains application services.
package service

import (
	"context"
	"errors"
	"time"

	sandboxauth "github.com/obot-platform/discobox/server/internal/auth/sandbox"
	workeragentauth "github.com/obot-platform/discobox/server/internal/auth/workeragent"
	eventbroker "github.com/obot-platform/discobox/server/internal/events"
	"github.com/obot-platform/discobox/server/internal/reconcile"
	resourceevents "github.com/obot-platform/discobox/server/internal/resources/events"
	"github.com/obot-platform/discobox/server/internal/resources/harnessconfigs"
	resourcejobs "github.com/obot-platform/discobox/server/internal/resources/jobs"
	"github.com/obot-platform/discobox/server/internal/resources/projects"
	"github.com/obot-platform/discobox/server/internal/resources/providers"
	sandboxes "github.com/obot-platform/discobox/server/internal/resources/sandboxes"
	"github.com/obot-platform/discobox/server/internal/resources/secrets"
	workers "github.com/obot-platform/discobox/server/internal/resources/workers"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	services "github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
	providerregistry "github.com/obot-platform/discobox/server/providers"
)

const (
	DefaultUserID = "usr_default"
)

// Service implements the API service interfaces using the database store.
type Service struct {
	services.ProjectService
	services.HarnessConfigService
	*sandboxes.Service
	services.SandboxProviderInstanceService
	services.WorkerService
	services.JobService
	services.ProjectEventService
	services.SecretService

	store             *store.Store
	engine            *reconcile.Engine
	jobManagerOptions JobManagerOptions
	jobs              *resourcejobs.Service
	providerService   *providers.Service
	workerManager     *workers.ControlPlane
}

type JobManagerOptions struct {
	SandboxReconcileJobConcurrency int
}

func New(store *store.Store, engine *reconcile.Engine, jobManagerOptions JobManagerOptions, broker ...*eventbroker.Broker) *Service {
	var b *eventbroker.Broker
	if len(broker) > 0 {
		b = broker[0]
	}
	manager := sandbox.NewProviderManager()
	workerManager := workers.NewControlPlane(store, engine)
	providerregistry.RegisterBuiltInSandboxProviderFactories(manager, workerManager)
	sandboxService := sandboxes.NewService(store, manager, DefaultUserID, engine, workerManager)
	providerService := providers.NewService(store, sandboxService, workerManager)
	jobsService := resourcejobs.NewService(store, engine)
	return &Service{
		ProjectService:                 projects.NewService(store),
		HarnessConfigService:           harnessconfigs.NewService(store),
		Service:                        sandboxService,
		SandboxProviderInstanceService: providerService,
		WorkerService:                  workers.NewService(store, workerManager),
		JobService:                     jobsService,
		ProjectEventService:            resourceevents.NewService(store, b),
		SecretService:                  secrets.NewService(store),

		jobs:            jobsService,
		providerService: providerService,

		store:             store,
		engine:            engine,
		jobManagerOptions: jobManagerOptions,
		workerManager:     workerManager,
	}
}

func (s *Service) SetSandboxAuthManager(manager *sandboxauth.Manager) {
	s.Service.SetSandboxAuthManager(manager)
}

func (s *Service) SetDefaultSandboxImage(image string) {
	s.Service.SetDefaultSandboxImage(image)
}

func (s *Service) SetWorkerAgentAuthManager(manager *workeragentauth.Manager) {
	s.workerManager.SetWorkerAgentAuthManager(manager)
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
	s.workerManager.StartDeletedWorkerCleanup(ctx)
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
	concurrency := s.jobManagerOptions.SandboxReconcileJobConcurrency
	sandboxOptions := []reconcile.RegisterOption(nil)
	if concurrency > 0 {
		sandboxOptions = append(sandboxOptions, reconcile.WithConcurrency(concurrency))
	}
	if err := s.RegisterJobs(sandboxOptions...); err != nil {
		return err
	}
	return s.workerManager.RegisterJobs(s.SandboxProviderManager())
}

func (s *Service) SandboxProviderManager() *sandbox.ProviderManager {
	return s.Service.SandboxProviderManager()
}
