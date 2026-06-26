package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/obot-platform/discobox/orchestration"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/resources/providers"
	"github.com/obot-platform/discobox/server/internal/resources/sandboxes"
	"github.com/obot-platform/discobox/server/internal/resources/workers"
	"github.com/obot-platform/discobox/server/internal/store"
)

type lifecycleResource interface {
	IncrementGeneration()
	BeginOperation(model.OperationSpec, *string)
	SetLastJobID(*string)
}

type lifecycleStore[Resource lifecycleResource, ID any] interface {
	orchestration.JobStore
	Get(context.Context, ID) (Resource, error)
	Create(context.Context, Resource) error
	UpdateWithGeneration(context.Context, Resource, int64) error
	Reload(context.Context, ID) (Resource, error)
	Generation(Resource) int64
}

type lifecycleTransaction[Store any] func(context.Context, func(context.Context, Store) error) error

func submitExistingLifecycle[Resource lifecycleResource, ID any, Store lifecycleStore[Resource, ID]](
	ctx context.Context,
	dispatcher *orchestration.Dispatcher,
	queueConfig orchestration.QueueConfig,
	resourceStore Store,
	transaction lifecycleTransaction[Store],
	id ID,
	operation model.OperationSpec,
	payload func(Resource) orchestration.Payload,
	mutate ...func(Resource),
) (Resource, error) {
	var zero Resource

	if _, err := dispatcher.Submit(ctx, nil,
		orchestration.WithQueueConfig(queueConfig),
		orchestration.WithSubmitTransaction(func(ctx context.Context, appendJob orchestration.SubmitAppendFunc) (*orchestration.Job, error) {
			var job *orchestration.Job
			err := transaction(ctx, func(ctx context.Context, tx Store) error {
				resource, err := tx.Get(ctx, id)
				if err != nil {
					return err
				}
				previousGeneration := tx.Generation(resource)

				resource.IncrementGeneration()
				resource.BeginOperation(operation, nil)
				for _, fn := range mutate {
					fn(resource)
				}

				job, err = appendJob(ctx, tx, payload(resource))
				if err != nil {
					return err
				}
				if job == nil {
					return orchestration.ErrJobAlreadyExists
				}
				resource.SetLastJobID(&job.ID)
				if err := tx.UpdateWithGeneration(ctx, resource, previousGeneration); err != nil {
					return err
				}
				return nil
			})
			return job, err
		}),
	); err != nil {
		return zero, err
	}
	return resourceStore.Reload(ctx, id)
}

func createLifecycle[Resource lifecycleResource, ID any, Store lifecycleStore[Resource, ID]](
	ctx context.Context,
	dispatcher *orchestration.Dispatcher,
	queueConfig orchestration.QueueConfig,
	resourceStore Store,
	transaction lifecycleTransaction[Store],
	resource Resource,
	operation model.OperationSpec,
	payload func(Resource) orchestration.Payload,
	id func(Resource) ID,
) (Resource, error) {
	var zero Resource

	if _, err := dispatcher.Submit(ctx, nil,
		orchestration.WithQueueConfig(queueConfig),
		orchestration.WithSubmitTransaction(func(ctx context.Context, appendJob orchestration.SubmitAppendFunc) (*orchestration.Job, error) {
			var job *orchestration.Job
			err := transaction(ctx, func(ctx context.Context, tx Store) error {
				resource.IncrementGeneration()
				resource.BeginOperation(operation, nil)

				var err error
				job, err = appendJob(ctx, tx, payload(resource))
				if err != nil {
					return err
				}
				if job == nil {
					return orchestration.ErrJobAlreadyExists
				}
				resource.SetLastJobID(&job.ID)
				return tx.Create(ctx, resource)
			})
			return job, err
		}),
	); err != nil {
		return zero, err
	}
	return resourceStore.Reload(ctx, id(resource))
}

func sandboxReconcilePayload(sandbox *model.Sandbox) orchestration.Payload {
	return sandboxes.SandboxReconcilePayload{
		ProjectID:  sandbox.ProjectID,
		SandboxID:  sandbox.ID,
		Generation: sandbox.Generation,
	}
}

func workerReconcilePayload(worker *model.Worker) orchestration.Payload {
	return workers.WorkerReconcilePayload{
		ProjectID:  worker.ProjectID,
		ProviderID: worker.ProviderInstanceID,
		WorkerID:   worker.ID,
		Generation: worker.Generation,
	}
}

func workerProviderReconcilePayload(provider *model.SandboxProviderInstance) orchestration.Payload {
	return providers.WorkerProviderReconcilePayload{
		ProjectID:  provider.ProjectID,
		ProviderID: provider.ID,
	}
}

func (m *Manager) CreateSandbox(ctx context.Context, sandbox *model.Sandbox) (*model.Sandbox, error) {
	dispatcher, err := m.dispatcherForSubmit()
	if err != nil {
		return nil, err
	}
	sandboxStore := m.store.Sandboxes()
	return createLifecycle(ctx, dispatcher, m.cfg.QueueConfig, sandboxStore, sandboxStore.Transaction, sandbox, model.SandboxCreateOperation, sandboxReconcilePayload, sandboxStore.ID)
}

func (m *Manager) StartSandbox(ctx context.Context, projectID, sandboxID string) (*model.Sandbox, error) {
	return m.submitSandbox(ctx, projectID, sandboxID, model.SandboxStartOperation)
}

func (m *Manager) StopSandbox(ctx context.Context, projectID, sandboxID string) (*model.Sandbox, error) {
	return m.submitSandbox(ctx, projectID, sandboxID, model.SandboxStopOperation)
}

func (m *Manager) RestartSandbox(ctx context.Context, projectID, sandboxID string) (*model.Sandbox, error) {
	return m.submitSandbox(ctx, projectID, sandboxID, model.SandboxRestartOperation, func(sandbox *model.Sandbox) {
		sandbox.RestartGeneration++
	})
}

func (m *Manager) DeleteSandbox(ctx context.Context, projectID, sandboxID string) (*model.Sandbox, error) {
	return m.submitSandbox(ctx, projectID, sandboxID, model.SandboxDeleteOperation)
}

func (m *Manager) SubmitSandboxReconcile(ctx context.Context, projectID, sandboxID string) (*model.Sandbox, error) {
	dispatcher, err := m.dispatcherForSubmit()
	if err != nil {
		return nil, err
	}
	sandboxStore := m.store.Sandboxes()
	if _, err := dispatcher.Submit(ctx, nil,
		orchestration.WithQueueConfig(m.cfg.QueueConfig),
		orchestration.WithSubmitTransaction(func(ctx context.Context, appendJob orchestration.SubmitAppendFunc) (*orchestration.Job, error) {
			var job *orchestration.Job
			err := sandboxStore.Transaction(ctx, func(ctx context.Context, txStore *store.SandboxStore) error {
				current, err := txStore.GetSandbox(ctx, projectID, sandboxID)
				if err != nil {
					return err
				}
				job, err = appendJob(ctx, txStore, sandboxReconcilePayload(current))
				if err != nil {
					return err
				}
				if job != nil {
					current.SetLastJobID(&job.ID)
				}
				return txStore.UpdateWithGeneration(ctx, current, current.Generation)
			})
			return job, err
		}),
	); err != nil {
		return nil, err
	}
	return sandboxStore.Reload(ctx, store.SandboxID{ProjectID: projectID, SandboxID: sandboxID})
}

func (m *Manager) submitSandbox(ctx context.Context, projectID, sandboxID string, operation model.OperationSpec, mutate ...func(*model.Sandbox)) (*model.Sandbox, error) {
	dispatcher, err := m.dispatcherForSubmit()
	if err != nil {
		return nil, err
	}
	sandboxStore := m.store.Sandboxes()
	return submitExistingLifecycle(ctx, dispatcher, m.cfg.QueueConfig, sandboxStore, sandboxStore.Transaction, store.SandboxID{ProjectID: projectID, SandboxID: sandboxID}, operation, sandboxReconcilePayload, mutate...)
}

func (m *Manager) CreateWorker(ctx context.Context, worker *model.Worker) (*model.Worker, error) {
	dispatcher, err := m.dispatcherForSubmit()
	if err != nil {
		return nil, err
	}
	workerStore := m.store.Workers()
	return createLifecycle(ctx, dispatcher, m.cfg.QueueConfig, workerStore, workerStore.Transaction, worker, model.WorkerCreateOperation, workerReconcilePayload, workerStore.ID)
}

func (m *Manager) DrainWorker(ctx context.Context, workerID string) (*model.Worker, error) {
	return m.submitWorker(ctx, workerID, model.WorkerDrainOperation)
}

func (m *Manager) DeleteWorker(ctx context.Context, workerID string) (*model.Worker, error) {
	dispatcher, err := m.dispatcherForSubmit()
	if err != nil {
		return nil, err
	}
	workerStore := m.store.Workers()
	var blocked *model.Worker
	if _, err := dispatcher.Submit(ctx, nil,
		orchestration.WithQueueConfig(m.cfg.QueueConfig),
		orchestration.WithSubmitTransaction(func(ctx context.Context, appendJob orchestration.SubmitAppendFunc) (*orchestration.Job, error) {
			var job *orchestration.Job
			err := workerStore.Transaction(ctx, func(ctx context.Context, txStore *store.WorkerStore) error {
				worker, err := txStore.Get(ctx, store.WorkerID{WorkerID: workerID})
				if err != nil {
					return err
				}
				assigned, err := txStore.CountSandboxesForWorker(ctx, worker.ID)
				if err != nil {
					return err
				}
				if assigned > 0 {
					blocked = worker
					return nil
				}
				previousGeneration := worker.Generation
				worker.IncrementGeneration()
				worker.BeginOperation(model.WorkerDeleteOperation, nil)
				job, err = appendJob(ctx, txStore, workerReconcilePayload(worker))
				if err != nil {
					return err
				}
				if job == nil {
					return orchestration.ErrJobAlreadyExists
				}
				worker.SetLastJobID(&job.ID)
				return txStore.UpdateWithGeneration(ctx, worker, previousGeneration)
			})
			return job, err
		}),
	); err != nil {
		return nil, err
	}
	if blocked != nil {
		return blocked, fmt.Errorf("worker %s has assigned sandboxes", workerID)
	}
	return workerStore.Reload(ctx, store.WorkerID{WorkerID: workerID})
}

func (m *Manager) submitWorker(ctx context.Context, workerID string, operation model.OperationSpec, mutate ...func(*model.Worker)) (*model.Worker, error) {
	dispatcher, err := m.dispatcherForSubmit()
	if err != nil {
		return nil, err
	}
	workerStore := m.store.Workers()
	return submitExistingLifecycle(ctx, dispatcher, m.cfg.QueueConfig, workerStore, workerStore.Transaction, store.WorkerID{WorkerID: workerID}, operation, workerReconcilePayload, mutate...)
}

func (m *Manager) MarkWorkerFailedForJob(ctx context.Context, workerID string, generation int64, jobID string, message string) (bool, error) {
	return m.store.MarkWorkerFailedForJob(ctx, workerID, generation, jobID, message)
}

func (m *Manager) DeleteWorkerForExpiredRegistration(ctx context.Context, workerID string, generation int64, cutoff time.Time, message string) (bool, error) {
	return m.deleteWorkerIfCurrent(ctx, workerID, generation, message, func(worker *model.Worker) bool {
		return worker.Phase == model.WorkerPhaseRegistering &&
			worker.LastOperationStatus == model.OperationStatusSuccess &&
			worker.RegisteredAt == nil &&
			worker.LastSeenAt == nil &&
			worker.UpdatedAt.Before(cutoff)
	})
}

func (m *Manager) deleteWorkerIfCurrent(ctx context.Context, workerID string, generation int64, message string, shouldDelete func(*model.Worker) bool) (bool, error) {
	dispatcher, err := m.dispatcherForSubmit()
	if err != nil {
		return false, err
	}
	workerStore := m.store.Workers()
	updated := false
	if _, err := dispatcher.Submit(ctx, nil,
		orchestration.WithQueueConfig(m.cfg.QueueConfig),
		orchestration.WithSubmitTransaction(func(ctx context.Context, appendJob orchestration.SubmitAppendFunc) (*orchestration.Job, error) {
			var job *orchestration.Job
			err := workerStore.Transaction(ctx, func(ctx context.Context, txStore *store.WorkerStore) error {
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
				assigned, err := txStore.CountSandboxesForWorker(ctx, worker.ID)
				if err != nil {
					return err
				}
				if assigned > 0 {
					return nil
				}
				previousGeneration := worker.Generation
				worker.IncrementGeneration()
				worker.BeginOperation(model.WorkerDeleteOperation, nil)
				worker.StatusMessage = &message

				job, err = appendJob(ctx, txStore, workerReconcilePayload(worker))
				if err != nil {
					return err
				}
				if job == nil {
					return nil
				}
				worker.SetLastJobID(&job.ID)
				if err := txStore.UpdateWorker(ctx, worker, store.WithWorkerGeneration(previousGeneration)); err != nil {
					if errors.Is(err, store.ErrGenerationConflict) {
						return err
					}
					return err
				}
				updated = true
				return nil
			})
			return job, err
		}),
	); err != nil {
		if errors.Is(err, store.ErrGenerationConflict) {
			return false, nil
		}
		return false, err
	}
	return updated, nil
}

func (m *Manager) SubmitWorkerReconcile(ctx context.Context, workerID string) (*orchestration.Job, error) {
	dispatcher, err := m.dispatcherForSubmit()
	if err != nil {
		return nil, err
	}
	workerStore := m.store.Workers()
	return dispatcher.Submit(ctx, nil,
		orchestration.WithQueueConfig(m.cfg.QueueConfig),
		orchestration.WithSubmitTransaction(func(ctx context.Context, appendJob orchestration.SubmitAppendFunc) (*orchestration.Job, error) {
			var job *orchestration.Job
			err := workerStore.Transaction(ctx, func(ctx context.Context, txStore *store.WorkerStore) error {
				current, err := txStore.GetWorker(ctx, workerID)
				if err != nil {
					return err
				}
				job, err = appendJob(ctx, txStore, workerReconcilePayload(current))
				if err != nil {
					return err
				}
				if job != nil {
					current.SetLastJobID(&job.ID)
				}
				return txStore.UpdateWorker(ctx, current, store.WithWorkerGeneration(current.Generation))
			})
			return job, err
		}),
	)
}

func (m *Manager) SubmitWorkerProviderReconcile(ctx context.Context, projectID, providerID string) (*orchestration.Job, error) {
	dispatcher, err := m.dispatcherForSubmit()
	if err != nil {
		return nil, err
	}
	providerStore := m.store.SandboxProviderInstances()
	return dispatcher.Submit(ctx, nil,
		orchestration.WithQueueConfig(m.cfg.QueueConfig),
		orchestration.WithCreateJobOptions(orchestration.WithUniqueResource()),
		orchestration.WithSubmitTransaction(func(ctx context.Context, appendJob orchestration.SubmitAppendFunc) (*orchestration.Job, error) {
			var job *orchestration.Job
			err := providerStore.Transaction(ctx, func(ctx context.Context, txStore *store.SandboxProviderInstanceStore) error {
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
				resource := orchestration.Resource{Type: "workerprovider", ID: provider.ID}
				active, err := txStore.HasActiveJobForResource(ctx, resource)
				if err != nil {
					return err
				}
				if active {
					return nil
				}
				job, err = appendJob(ctx, txStore, workerProviderReconcilePayload(provider))
				if errors.Is(err, orchestration.ErrJobAlreadyExists) {
					job = nil
					return nil
				}
				return err
			})
			return job, err
		}),
	)
}

func (m *Manager) OnWorkerReconcileTerminal(ctx context.Context, job *orchestration.Job, payload workers.WorkerReconcilePayload) error {
	if job.Status != orchestration.StatusCompleted && job.Status != orchestration.StatusFailed {
		return nil
	}
	_, err := m.SubmitWorkerProviderReconcile(ctx, payload.ProjectID, payload.ProviderID)
	return err
}
