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
	submitter *orchestration.Submitter[*model.Sandbox, model.OperationSpec, SandboxID, *store.Store]
}

type WorkerSubmitter struct {
	store       *store.Store
	submitter   *orchestration.Submitter[*model.Worker, model.OperationSpec, WorkerID, *store.Store]
	queueConfig orchestration.QueueConfig
	notify      func(context.Context)
}

type ProviderSubmitter struct {
	store       *store.Store
	queueConfig orchestration.QueueConfig
	notify      func(context.Context)
}

type SandboxID struct {
	ProjectID string
	SandboxID string
}

type WorkerID struct {
	WorkerID string
}

func NewSandboxSubmitter(appStore *store.Store, queueConfig orchestration.QueueConfig, notify func(context.Context)) *SandboxSubmitter {
	return &SandboxSubmitter{
		submitter: orchestration.NewSubmitter(orchestration.SubmitterConfig[*model.Sandbox, model.OperationSpec, SandboxID, *store.Store]{
			Transaction: storeTransaction(appStore),
			Resource: orchestration.ResourceStore[*model.Sandbox, SandboxID, *store.Store]{
				Get: func(ctx context.Context, txStore *store.Store, id SandboxID) (*model.Sandbox, error) {
					return txStore.GetSandbox(ctx, id.ProjectID, id.SandboxID)
				},
				Create: func(ctx context.Context, txStore *store.Store, sandbox *model.Sandbox) error {
					return txStore.CreateSandbox(ctx, sandbox)
				},
				Update: func(ctx context.Context, txStore *store.Store, sandbox *model.Sandbox) error {
					return txStore.UpdateSandbox(ctx, sandbox)
				},
				ID: func(sandbox *model.Sandbox) SandboxID {
					return SandboxID{ProjectID: sandbox.ProjectID, SandboxID: sandbox.ID}
				},
				Reload: func(ctx context.Context, id SandboxID) (*model.Sandbox, error) {
					return appStore.GetSandbox(ctx, id.ProjectID, id.SandboxID)
				},
			},
			Payload:     sandboxReconcilePayload,
			QueueConfig: queueConfig,
			Notify:      notify,
		}),
	}
}

func NewWorkerSubmitter(appStore *store.Store, queueConfig orchestration.QueueConfig, notify func(context.Context)) *WorkerSubmitter {
	return &WorkerSubmitter{
		store:       appStore,
		queueConfig: queueConfig,
		notify:      notify,
		submitter: orchestration.NewSubmitter(orchestration.SubmitterConfig[*model.Worker, model.OperationSpec, WorkerID, *store.Store]{
			Transaction: storeTransaction(appStore),
			Resource: orchestration.ResourceStore[*model.Worker, WorkerID, *store.Store]{
				Get: func(ctx context.Context, txStore *store.Store, id WorkerID) (*model.Worker, error) {
					return txStore.GetWorker(ctx, id.WorkerID)
				},
				Create: func(ctx context.Context, txStore *store.Store, worker *model.Worker) error {
					return txStore.CreateWorker(ctx, worker)
				},
				Update: func(ctx context.Context, txStore *store.Store, worker *model.Worker) error {
					return txStore.UpdateWorker(ctx, worker)
				},
				ID: func(worker *model.Worker) WorkerID {
					return WorkerID{WorkerID: worker.ID}
				},
				Reload: func(ctx context.Context, id WorkerID) (*model.Worker, error) {
					return appStore.GetWorker(ctx, id.WorkerID)
				},
			},
			Payload:     workerReconcilePayload,
			QueueConfig: queueConfig,
			Notify:      notify,
		}),
	}
}

func NewProviderSubmitter(appStore *store.Store, queueConfig orchestration.QueueConfig, notify func(context.Context)) *ProviderSubmitter {
	return &ProviderSubmitter{store: appStore, queueConfig: queueConfig, notify: notify}
}

func storeTransaction(appStore *store.Store) orchestration.TransactionFunc[*store.Store] {
	return func(ctx context.Context, fn func(context.Context, *store.Store) error) error {
		return appStore.Transaction(ctx, func(txStore *store.Store, _ *gorm.DB) error {
			return fn(ctx, txStore)
		})
	}
}

func (s *SandboxSubmitter) Create(ctx context.Context, sandbox *model.Sandbox) (*model.Sandbox, error) {
	return s.submitter.Create(ctx, sandbox, model.SandboxCreateOperation)
}

func (s *SandboxSubmitter) Submit(ctx context.Context, projectID, sandboxID string, spec model.OperationSpec, mutate ...func(*model.Sandbox)) (*model.Sandbox, error) {
	return s.submitter.Submit(ctx, SandboxID{ProjectID: projectID, SandboxID: sandboxID}, spec, mutate...)
}

func (s *WorkerSubmitter) Create(ctx context.Context, worker *model.Worker) (*model.Worker, error) {
	return s.submitter.Create(ctx, worker, model.WorkerCreateOperation)
}

func (s *WorkerSubmitter) Submit(ctx context.Context, workerID string, spec model.OperationSpec, mutate ...func(*model.Worker)) (*model.Worker, error) {
	return s.submitter.Submit(ctx, WorkerID{WorkerID: workerID}, spec, mutate...)
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
