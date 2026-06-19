package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/orchestration"
	"github.com/obot-platform/discobox/server/internal/store"
)

type SandboxSubmitter struct {
	submitter *orchestration.Submitter[*model.Sandbox, model.OperationSpec, store.SandboxID, *store.SandboxStore]
}

type WorkerSubmitter struct {
	store       *store.WorkerStore
	submitter   *orchestration.Submitter[*model.Worker, model.OperationSpec, store.WorkerID, *store.WorkerStore]
	queueConfig orchestration.QueueConfig
	notify      func(context.Context)
}

type ProviderSubmitter struct {
	store       *store.SandboxProviderInstanceStore
	queueConfig orchestration.QueueConfig
	notify      func(context.Context)
}

func NewSandboxSubmitter(appStore *store.Store, queueConfig orchestration.QueueConfig, notify func(context.Context)) *SandboxSubmitter {
	sandboxStore := appStore.Sandboxes()
	return &SandboxSubmitter{
		submitter: orchestration.NewSubmitter(orchestration.SubmitterConfig[*model.Sandbox, model.OperationSpec, store.SandboxID, *store.SandboxStore]{
			Transaction: sandboxStore.Transaction,
			Resource:    sandboxStore,
			Payload:     sandboxReconcilePayload,
			QueueConfig: queueConfig,
			Notify:      notify,
		}),
	}
}

func NewWorkerSubmitter(appStore *store.Store, queueConfig orchestration.QueueConfig, notify func(context.Context)) *WorkerSubmitter {
	workerStore := appStore.Workers()
	return &WorkerSubmitter{
		store:       workerStore,
		queueConfig: queueConfig,
		notify:      notify,
		submitter: orchestration.NewSubmitter(orchestration.SubmitterConfig[*model.Worker, model.OperationSpec, store.WorkerID, *store.WorkerStore]{
			Transaction: workerStore.Transaction,
			Resource:    workerStore,
			Payload:     workerReconcilePayload,
			QueueConfig: queueConfig,
			Notify:      notify,
		}),
	}
}

func NewProviderSubmitter(appStore *store.Store, queueConfig orchestration.QueueConfig, notify func(context.Context)) *ProviderSubmitter {
	return &ProviderSubmitter{store: appStore.SandboxProviderInstances(), queueConfig: queueConfig, notify: notify}
}

func (s *SandboxSubmitter) Create(ctx context.Context, sandbox *model.Sandbox) (*model.Sandbox, error) {
	return s.submitter.Create(ctx, sandbox, model.SandboxCreateOperation)
}

func (s *SandboxSubmitter) Submit(ctx context.Context, projectID, sandboxID string, spec model.OperationSpec, mutate ...func(*model.Sandbox)) (*model.Sandbox, error) {
	return s.submitter.Submit(ctx, store.SandboxID{ProjectID: projectID, SandboxID: sandboxID}, spec, mutate...)
}

func (s *WorkerSubmitter) Create(ctx context.Context, worker *model.Worker) (*model.Worker, error) {
	return s.submitter.Create(ctx, worker, model.WorkerCreateOperation)
}

func (s *WorkerSubmitter) Submit(ctx context.Context, workerID string, spec model.OperationSpec, mutate ...func(*model.Worker)) (*model.Worker, error) {
	return s.submitter.Submit(ctx, store.WorkerID{WorkerID: workerID}, spec, mutate...)
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
	if err := s.store.Transaction(ctx, func(ctx context.Context, txStore *store.WorkerStore) error {
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
	if err := s.store.Transaction(ctx, func(ctx context.Context, txStore *store.WorkerStore) error {
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
	if err := s.store.Transaction(ctx, func(ctx context.Context, txStore *store.SandboxProviderInstanceStore) error {
		provider, err := txStore.Get(ctx, store.SandboxProviderInstanceID{ProjectID: projectID, ProviderID: providerID})
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
		job, jobCreated, err = orchestration.AppendJobWithOptions(ctx, txStore, providerReconcilePayload(provider), s.queueConfig, orchestration.WithUniqueResource())
		if err != nil {
			if errors.Is(err, orchestration.ErrJobAlreadyExists) {
				job = nil
				return nil
			}
			return err
		}
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

func appendWorkerJob(ctx context.Context, txStore *store.WorkerStore, worker *model.Worker, cfg orchestration.QueueConfig) (bool, error) {
	_, created, err := appendWorkerJobWithJob(ctx, txStore, worker, cfg)
	return created, err
}

func appendWorkerJobWithJob(ctx context.Context, txStore *store.WorkerStore, worker *model.Worker, cfg orchestration.QueueConfig) (*orchestration.Job, bool, error) {
	job, created, err := appendJob(ctx, txStore, workerReconcilePayload(worker), cfg)
	if err != nil {
		return nil, false, err
	}
	if job != nil {
		worker.SetLastJobID(&job.ID)
	}
	return job, created, nil
}

func appendJob(ctx context.Context, txStore orchestration.JobStore, payload orchestration.Payload, cfg orchestration.QueueConfig) (*orchestration.Job, bool, error) {
	return orchestration.AppendJob(ctx, txStore, payload, cfg)
}

func notifyIfCreated(ctx context.Context, notify func(context.Context), created bool) {
	if created && notify != nil {
		notify(ctx)
	}
}
