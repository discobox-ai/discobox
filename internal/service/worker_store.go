package service

import (
	"context"

	"github.com/obot-platform/discobox/internal/model"
	"github.com/obot-platform/discobox/internal/sandbox/jobs"
	"github.com/obot-platform/discobox/internal/store"
)

type workerStore struct {
	store   *store.Store
	workers *jobs.WorkerSubmitter
}

func newWorkerStore(appStore *store.Store, workers *jobs.WorkerSubmitter) *workerStore {
	return &workerStore{store: appStore, workers: workers}
}

func (s *workerStore) ListWorkers(ctx context.Context, projectID, providerID string) ([]model.Worker, error) {
	return s.store.ListWorkers(ctx, projectID, providerID)
}

func (s *workerStore) GetProject(ctx context.Context, projectID string) (*model.Project, error) {
	return s.store.GetProject(ctx, projectID)
}

func (s *workerStore) GetSandboxProviderInstance(ctx context.Context, projectID, providerID string) (*model.SandboxProviderInstance, error) {
	return s.store.GetSandboxProviderInstance(ctx, projectID, providerID)
}

func (s *workerStore) CreateWorker(ctx context.Context, worker *model.Worker) (*model.Worker, error) {
	return s.workers.Create(ctx, worker)
}

func (s *workerStore) CreateWorkerBootstrapToken(ctx context.Context, token *model.WorkerBootstrapToken) error {
	return s.store.CreateWorkerBootstrapToken(ctx, token)
}

func (s *workerStore) FindSchedulableWorker(ctx context.Context, sandbox *model.Sandbox) (*model.Worker, error) {
	return s.store.FindSchedulableWorker(ctx, sandbox)
}
