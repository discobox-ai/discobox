package workers

import (
	"context"
	"time"

	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/orchestration"
	"github.com/obot-platform/discobox/server/internal/store"
)

type Manager struct {
	store *store.Store
	jobs  WorkerJobManager
}

type WorkerJobManager interface {
	CreateWorker(context.Context, *model.Worker) (*model.Worker, error)
	DeleteWorkerForFailedJob(context.Context, string, int64, string, string) (bool, error)
	DeleteWorkerForExpiredRegistration(context.Context, string, int64, time.Time, string) (bool, error)
}

func NewManager(appStore *store.Store) *Manager {
	return &Manager{store: appStore}
}

func (s *Manager) SetJobManager(manager WorkerJobManager) {
	s.jobs = manager
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
