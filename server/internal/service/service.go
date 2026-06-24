// Package service contains application services.
package service

import (
	"context"
	"time"

	"github.com/obot-platform/discobox/orchestration"
	sandboxauth "github.com/obot-platform/discobox/server/internal/auth/sandbox"
	workeragentauth "github.com/obot-platform/discobox/server/internal/auth/workeragent"
	eventbroker "github.com/obot-platform/discobox/server/internal/events"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/resources/agentconfigs"
	resourceevents "github.com/obot-platform/discobox/server/internal/resources/events"
	resourcejobs "github.com/obot-platform/discobox/server/internal/resources/jobs"
	"github.com/obot-platform/discobox/server/internal/resources/projects"
	"github.com/obot-platform/discobox/server/internal/resources/providers"
	sandboxes "github.com/obot-platform/discobox/server/internal/resources/sandboxes"
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
	services.AgentConfigService
	*sandboxes.Service
	services.SandboxProviderInstanceService
	services.WorkerService
	services.JobService
	services.ProjectEventService

	store             *store.Store
	jobManager        JobManager
	jobManagerOptions JobManagerOptions
	jobs              *resourcejobs.Service
	providerService   *providers.Service
	workerManager     *workers.Manager
}

type JobManager interface {
	Register(orchestration.Type, orchestration.Executor, ...orchestration.ExecutorOption) error
	Start(context.Context) error
	Stop(context.Context) error
	NotifyNewJob(context.Context)
	CreateSandbox(context.Context, *model.Sandbox) (*model.Sandbox, error)
	StartSandbox(context.Context, string, string) (*model.Sandbox, error)
	StopSandbox(context.Context, string, string) (*model.Sandbox, error)
	RestartSandbox(context.Context, string, string) (*model.Sandbox, error)
	DeleteSandbox(context.Context, string, string) (*model.Sandbox, error)
	CreateWorker(context.Context, *model.Worker) (*model.Worker, error)
	DeleteWorkerForFailedJob(context.Context, string, int64, string, string) (bool, error)
	DeleteWorkerForExpiredRegistration(context.Context, string, int64, time.Time, string) (bool, error)
	EnqueueWorkerCurrent(context.Context, *model.Worker) (*orchestration.Job, error)
	EnqueueWorkerProviderCurrent(context.Context, string, string) (*orchestration.Job, error)
	OnWorkerReconcileTerminal(context.Context, *orchestration.Job, workers.WorkerReconcilePayload) error
}

type JobManagerOptions struct {
	SandboxReconcileJobConcurrency int
}

func New(store *store.Store, jobManager JobManager, jobManagerOptions JobManagerOptions, broker ...*eventbroker.Broker) *Service {
	var b *eventbroker.Broker
	if len(broker) > 0 {
		b = broker[0]
	}
	manager := sandbox.NewProviderManager()
	workerManager := workers.NewManager(store, jobManager)
	providerregistry.RegisterBuiltInSandboxProviderFactories(manager, workerManager)
	sandboxService := sandboxes.NewService(store, manager, DefaultUserID, jobManager, workerManager)
	providerService := providers.NewService(store, sandboxService, workerManager, jobManager)
	jobsService := resourcejobs.NewService(store, jobManager)
	return &Service{
		ProjectService:                 projects.NewService(store),
		AgentConfigService:             agentconfigs.NewService(store),
		Service:                        sandboxService,
		SandboxProviderInstanceService: providerService,
		WorkerService:                  workers.NewService(store),
		JobService:                     jobsService,
		ProjectEventService:            resourceevents.NewService(store, b),

		jobs:            jobsService,
		providerService: providerService,

		store:             store,
		jobManager:        jobManager,
		jobManagerOptions: jobManagerOptions,
		workerManager:     workerManager,
	}
}

func (s *Service) SetSandboxAuthManager(manager *sandboxauth.Manager) {
	s.Service.SetSandboxAuthManager(manager)
}

func (s *Service) SetWorkerAgentAuthManager(manager *workeragentauth.Manager) {
	s.workerManager.SetWorkerAgentAuthManager(manager)
}

func (s *Service) Start(ctx context.Context) error {
	if s.jobManager != nil {
		if err := s.registerJobExecutors(); err != nil {
			return err
		}
		if err := s.jobManager.Start(ctx); err != nil {
			return err
		}
	}
	startedJobs := s.jobManager != nil
	if err := s.EnsureExistingSandboxProviderInstances(ctx); err != nil {
		if startedJobs {
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			_ = s.jobManager.Stop(stopCtx)
		}
		return err
	}
	return nil
}

func (s *Service) registerJobExecutors() error {
	concurrency := s.jobManagerOptions.SandboxReconcileJobConcurrency
	executorOptions := []orchestration.ExecutorOption(nil)
	if concurrency > 0 {
		executorOptions = append(executorOptions, orchestration.WithConcurrency(concurrency))
	}
	if err := s.RegisterJobs(s.jobManager, executorOptions...); err != nil {
		return err
	}
	if err := s.providerService.RegisterJobs(s.jobManager, executorOptions...); err != nil {
		return err
	}
	if err := s.workerManager.RegisterJobs(s.jobManager, s.SandboxProviderManager(), executorOptions...); err != nil {
		return err
	}
	return nil
}

func (s *Service) SandboxProviderManager() *sandbox.ProviderManager {
	return s.Service.SandboxProviderManager()
}
