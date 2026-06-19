package jobs

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/orchestration"
	"github.com/obot-platform/discobox/server/internal/store"
)

type SandboxSubmitter struct {
	store       *store.Store
	queueConfig orchestration.QueueConfig
	notify      func(context.Context)
}

type WorkerSubmitter struct {
	store       *store.Store
	queueConfig orchestration.QueueConfig
	notify      func(context.Context)
}

type ProviderSubmitter struct {
	store       *store.Store
	queueConfig orchestration.QueueConfig
	notify      func(context.Context)
}

func NewSandboxSubmitter(appStore *store.Store, queueConfig orchestration.QueueConfig, notify func(context.Context)) *SandboxSubmitter {
	return &SandboxSubmitter{store: appStore, queueConfig: queueConfig, notify: notify}
}

func NewWorkerSubmitter(appStore *store.Store, queueConfig orchestration.QueueConfig, notify func(context.Context)) *WorkerSubmitter {
	return &WorkerSubmitter{store: appStore, queueConfig: queueConfig, notify: notify}
}

func NewProviderSubmitter(appStore *store.Store, queueConfig orchestration.QueueConfig, notify func(context.Context)) *ProviderSubmitter {
	return &ProviderSubmitter{store: appStore, queueConfig: queueConfig, notify: notify}
}

func (s *SandboxSubmitter) Create(ctx context.Context, sandbox *model.Sandbox) (*model.Sandbox, error) {
	jobCreated := false
	if err := s.store.Transaction(ctx, func(txStore *store.Store, _ *gorm.DB) error {
		sandbox.IncrementGeneration()
		sandbox.BeginOperation(model.SandboxCreateOperation, nil)
		created, err := appendSandboxJob(ctx, txStore, sandbox, s.queueConfig)
		if err != nil {
			return err
		}
		jobCreated = created
		return txStore.CreateSandbox(ctx, sandbox)
	}); err != nil {
		return nil, err
	}
	notifyIfCreated(ctx, s.notify, jobCreated)
	return s.store.GetSandbox(ctx, sandbox.ProjectID, sandbox.ID)
}

func (s *SandboxSubmitter) Submit(ctx context.Context, projectID, sandboxID string, spec model.OperationSpec, mutate ...func(*model.Sandbox)) (*model.Sandbox, error) {
	jobCreated := false
	if err := s.store.Transaction(ctx, func(txStore *store.Store, _ *gorm.DB) error {
		sandbox, err := txStore.GetSandbox(ctx, projectID, sandboxID)
		if err != nil {
			return err
		}
		sandbox.IncrementGeneration()
		sandbox.BeginOperation(spec, nil)
		for _, fn := range mutate {
			fn(sandbox)
		}
		created, err := appendSandboxJob(ctx, txStore, sandbox, s.queueConfig)
		if err != nil {
			return err
		}
		jobCreated = created
		return txStore.UpdateSandbox(ctx, sandbox)
	}); err != nil {
		return nil, err
	}
	notifyIfCreated(ctx, s.notify, jobCreated)
	return s.store.GetSandbox(ctx, projectID, sandboxID)
}

func (s *WorkerSubmitter) Create(ctx context.Context, worker *model.Worker) (*model.Worker, error) {
	jobCreated := false
	if err := s.store.Transaction(ctx, func(txStore *store.Store, _ *gorm.DB) error {
		worker.IncrementGeneration()
		worker.BeginOperation(model.WorkerCreateOperation, nil)
		created, err := appendWorkerJob(ctx, txStore, worker, s.queueConfig)
		if err != nil {
			return err
		}
		jobCreated = created
		return txStore.CreateWorker(ctx, worker)
	}); err != nil {
		return nil, err
	}
	notifyIfCreated(ctx, s.notify, jobCreated)
	return s.store.GetWorker(ctx, worker.ID)
}

func (s *WorkerSubmitter) Submit(ctx context.Context, workerID string, spec model.OperationSpec, mutate ...func(*model.Worker)) (*model.Worker, error) {
	jobCreated := false
	if err := s.store.Transaction(ctx, func(txStore *store.Store, _ *gorm.DB) error {
		worker, err := txStore.GetWorker(ctx, workerID)
		if err != nil {
			return err
		}
		worker.IncrementGeneration()
		worker.BeginOperation(spec, nil)
		for _, fn := range mutate {
			fn(worker)
		}
		created, err := appendWorkerJob(ctx, txStore, worker, s.queueConfig)
		if err != nil {
			return err
		}
		jobCreated = created
		return txStore.UpdateWorker(ctx, worker)
	}); err != nil {
		return nil, err
	}
	notifyIfCreated(ctx, s.notify, jobCreated)
	return s.store.GetWorker(ctx, workerID)
}

func (s *WorkerSubmitter) DeleteForFailedJob(ctx context.Context, workerID string, generation int64, jobID string, message string) (bool, error) {
	return s.deleteIfCurrent(ctx, workerID, generation, message, func(worker *model.Worker) bool {
		return worker.LastJobID != nil &&
			*worker.LastJobID == jobID &&
			worker.LastOperationStatus != model.OperationStatusFailed &&
			worker.LastOperationStatus != model.OperationStatusSuccess
	})
}

func (s *WorkerSubmitter) DeleteForExpiredRegistration(ctx context.Context, workerID string, generation int64, cutoff time.Time, message string) (bool, error) {
	return s.deleteIfCurrent(ctx, workerID, generation, message, func(worker *model.Worker) bool {
		return worker.Phase == model.WorkerPhaseRegistering &&
			worker.LastOperationStatus == model.OperationStatusSuccess &&
			worker.RegisteredAt == nil &&
			worker.LastSeenAt == nil &&
			worker.UpdatedAt.Before(cutoff)
	})
}

func (s *WorkerSubmitter) deleteIfCurrent(ctx context.Context, workerID string, generation int64, message string, shouldDelete func(*model.Worker) bool) (bool, error) {
	jobCreated := false
	updated := false
	if err := s.store.Transaction(ctx, func(txStore *store.Store, _ *gorm.DB) error {
		worker, err := txStore.GetWorker(ctx, workerID, store.WithWorkerGeneration(generation))
		if errors.Is(err, store.ErrGenerationConflict) || errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if !shouldDelete(worker) {
			return nil
		}
		previousGeneration := worker.Generation
		worker.IncrementGeneration()
		worker.BeginOperation(model.WorkerDeleteOperation, nil)
		worker.StatusMessage = &message
		created, err := appendWorkerJob(ctx, txStore, worker, s.queueConfig)
		if err != nil {
			return err
		}
		if created {
			jobCreated = true
		}
		if err := txStore.UpdateWorker(ctx, worker, store.WithWorkerGeneration(previousGeneration)); err != nil {
			if errors.Is(err, store.ErrGenerationConflict) {
				return err
			}
			return err
		}
		updated = true
		return nil
	}); err != nil {
		if errors.Is(err, store.ErrGenerationConflict) {
			return false, nil
		}
		return false, err
	}
	notifyIfCreated(ctx, s.notify, jobCreated)
	return updated, nil
}

func (s *WorkerSubmitter) EnqueueCurrent(ctx context.Context, worker *model.Worker) (*orchestration.Job, error) {
	var job *orchestration.Job
	jobCreated := false
	if err := s.store.Transaction(ctx, func(txStore *store.Store, _ *gorm.DB) error {
		current, err := txStore.GetWorker(ctx, worker.ID)
		if err != nil {
			return err
		}
		job, jobCreated, err = appendWorkerJobWithJob(ctx, txStore, current, s.queueConfig)
		if err != nil {
			return err
		}
		return txStore.UpdateWorker(ctx, current, store.WithWorkerGeneration(current.Generation))
	}); err != nil {
		return nil, err
	}
	notifyIfCreated(ctx, s.notify, jobCreated)
	return job, nil
}

func (s *ProviderSubmitter) EnqueueCurrent(ctx context.Context, projectID, providerID string) (*orchestration.Job, error) {
	if s == nil {
		return nil, nil
	}
	var job *orchestration.Job
	jobCreated := false
	if err := s.store.Transaction(ctx, func(txStore *store.Store, _ *gorm.DB) error {
		provider, err := txStore.GetSandboxProviderInstance(ctx, projectID, providerID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if provider.Disabled {
			return nil
		}
		resource := orchestration.Resource{Type: "provider", ID: provider.ID}
		active, err := txStore.HasActiveJobForResource(ctx, resource)
		if err != nil {
			return err
		}
		if active {
			return nil
		}
		job, err = orchestration.JobFromPayload(providerReconcilePayload(provider), s.queueConfig)
		if err != nil {
			return err
		}
		if err := txStore.CreateJob(ctx, job, orchestration.WithUniqueResource()); err != nil {
			if errors.Is(err, orchestration.ErrJobAlreadyExists) {
				job = nil
				return nil
			}
			return err
		}
		jobCreated = true
		return nil
	}); err != nil {
		return nil, err
	}
	notifyIfCreated(ctx, s.notify, jobCreated)
	return job, nil
}

func (s *ProviderSubmitter) OnWorkerReconcileTerminal(ctx context.Context, job *orchestration.Job, payload WorkerReconcilePayload) error {
	if job.Status != orchestration.StatusCompleted && job.Status != orchestration.StatusFailed {
		return nil
	}
	_, err := s.EnqueueCurrent(ctx, payload.ProjectID, payload.ProviderID)
	return err
}

func sandboxReconcilePayload(sandbox *model.Sandbox) orchestration.Payload {
	return SandboxReconcilePayload{
		ProjectID:  sandbox.ProjectID,
		SandboxID:  sandbox.ID,
		Generation: sandbox.Generation,
	}
}

func workerReconcilePayload(worker *model.Worker) orchestration.Payload {
	return WorkerReconcilePayload{
		ProjectID:  worker.ProjectID,
		ProviderID: worker.ProviderInstanceID,
		WorkerID:   worker.ID,
		Generation: worker.Generation,
	}
}

func providerReconcilePayload(provider *model.SandboxProviderInstance) orchestration.Payload {
	return ProviderReconcilePayload{
		ProjectID:  provider.ProjectID,
		ProviderID: provider.ID,
	}
}

func appendSandboxJob(ctx context.Context, txStore *store.Store, sandbox *model.Sandbox, cfg orchestration.QueueConfig) (bool, error) {
	job, created, err := appendJob(ctx, txStore, sandboxReconcilePayload(sandbox), cfg)
	if err != nil {
		return false, err
	}
	if job != nil {
		sandbox.SetLastJobID(&job.ID)
	}
	return created, nil
}

func appendWorkerJob(ctx context.Context, txStore *store.Store, worker *model.Worker, cfg orchestration.QueueConfig) (bool, error) {
	_, created, err := appendWorkerJobWithJob(ctx, txStore, worker, cfg)
	return created, err
}

func appendWorkerJobWithJob(ctx context.Context, txStore *store.Store, worker *model.Worker, cfg orchestration.QueueConfig) (*orchestration.Job, bool, error) {
	job, created, err := appendJob(ctx, txStore, workerReconcilePayload(worker), cfg)
	if err != nil {
		return nil, false, err
	}
	if job != nil {
		worker.SetLastJobID(&job.ID)
	}
	return job, created, nil
}

func appendJob(ctx context.Context, txStore *store.Store, payload orchestration.Payload, cfg orchestration.QueueConfig) (*orchestration.Job, bool, error) {
	return orchestration.AppendJob(ctx, txStore, payload, cfg)
}

func notifyIfCreated(ctx context.Context, notify func(context.Context), created bool) {
	if created && notify != nil {
		notify(ctx)
	}
}
