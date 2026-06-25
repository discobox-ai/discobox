package workers

import (
	"context"
	"errors"
	"time"

	"github.com/obot-platform/discobox/orchestration"
	workeragentauth "github.com/obot-platform/discobox/server/internal/auth/workeragent"
	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/store"
)

type Manager struct {
	store           *store.Store
	jobs            WorkerJobManager
	workerAgentAuth *workeragentauth.Manager
}

type WorkerJobManager interface {
	CreateWorker(context.Context, *model.Worker) (*model.Worker, error)
	SubmitWorkerReconcile(context.Context, string) (*orchestration.Job, error)
	DeleteWorkerForFailedJob(context.Context, string, int64, string, string) (bool, error)
	DeleteWorkerForExpiredRegistration(context.Context, string, int64, time.Time, string) (bool, error)
	SubmitWorkerProviderReconcile(context.Context, string, string) (*orchestration.Job, error)
}

type JobRegistrar interface {
	Register(orchestration.Type, orchestration.Executor, ...orchestration.ExecutorOption) error
	OnWorkerReconcileTerminal(context.Context, *orchestration.Job, WorkerReconcilePayload) error
}

func NewManager(appStore *store.Store, jobs WorkerJobManager) *Manager {
	return &Manager{store: appStore, jobs: jobs}
}

func (s *Manager) SetWorkerAgentAuthManager(manager *workeragentauth.Manager) {
	s.workerAgentAuth = manager
}

func (s *Manager) RegisterJobs(registrar JobRegistrar, providerManager *sandbox.ProviderManager, opts ...orchestration.ExecutorOption) error {
	return registrar.Register(
		WorkerReconcileType,
		NewWorkerReconcileExecutor(
			s.store,
			WithWorkerProviderManager(providerManager),
			WithWorkerManager(s),
			WithWorkerReconcileTerminalHandler(registrar),
		),
		opts...,
	)
}

func (s *Manager) ListWorkers(ctx context.Context, projectID, providerID string) ([]model.Worker, error) {
	return s.store.ListWorkers(ctx, projectID, providerID)
}

func (s *Manager) GetWorker(ctx context.Context, workerID string) (*model.Worker, error) {
	return s.store.GetWorker(ctx, workerID)
}

func (s *Manager) GetProject(ctx context.Context, projectID string) (*model.Project, error) {
	return s.store.GetProject(ctx, projectID)
}

func (s *Manager) GetSandboxProviderInstance(ctx context.Context, projectID, providerID string) (*model.SandboxProviderInstance, error) {
	return s.store.GetSandboxProviderInstance(ctx, projectID, providerID)
}

func (s *Manager) CreateWorker(ctx context.Context, worker *model.Worker) (*model.Worker, error) {
	return s.jobs.CreateWorker(ctx, worker)
}

func (s *Manager) CreateWorkerBootstrapToken(ctx context.Context, token *model.WorkerBootstrapToken) error {
	return s.store.CreateWorkerBootstrapToken(ctx, token)
}

func (s *Manager) EnsureWorkerAgentTrustKey(ctx context.Context) (string, error) {
	if s.workerAgentAuth == nil {
		return "", nil
	}
	return s.workerAgentAuth.EnsureTrustKey(ctx)
}

func (s *Manager) CreateWorkerAgentToken(ctx context.Context, claims workeragentauth.TokenClaims) (string, error) {
	if s.workerAgentAuth == nil {
		return "", nil
	}
	return s.workerAgentAuth.CreateToken(ctx, claims)
}

func (s *Manager) CreateSandboxAgentToken(ctx context.Context, claims workeragentauth.TokenClaims) (string, error) {
	if s.workerAgentAuth == nil {
		return "", nil
	}
	return s.workerAgentAuth.CreateSandboxAgentToken(ctx, claims)
}

func (s *Manager) FindSchedulableWorker(ctx context.Context, sandbox *model.Sandbox) (*model.Worker, error) {
	return s.store.FindSchedulableWorker(ctx, sandbox)
}

func (s *Manager) GetJob(ctx context.Context, id string) (*orchestration.Job, error) {
	return s.store.GetJob(ctx, id)
}

func (s *Manager) DeleteWorkerForFailedJob(ctx context.Context, workerID string, generation int64, jobID string, message string) (bool, error) {
	return s.jobs.DeleteWorkerForFailedJob(ctx, workerID, generation, jobID, message)
}

func (s *Manager) DeleteWorkerForExpiredRegistration(ctx context.Context, workerID string, generation int64, cutoff time.Time, message string) (bool, error) {
	return s.jobs.DeleteWorkerForExpiredRegistration(ctx, workerID, generation, cutoff, message)
}

func (s *Manager) ScheduleWorkerReconciliation(ctx context.Context, workerID string) error {
	if _, err := s.store.GetWorker(ctx, workerID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	_, err := s.jobs.SubmitWorkerReconcile(ctx, workerID)
	return err
}

func (s *Manager) ScheduleWorkerProviderReconciliation(ctx context.Context, projectID, providerID string) error {
	_, err := s.jobs.SubmitWorkerProviderReconcile(ctx, projectID, providerID)
	return err
}
