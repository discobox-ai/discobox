// Package orchestration coordinates desired-state resource updates with
// durable reconcile jobs.
package orchestration

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/obot-platform/disco2/internal/model"
	"github.com/obot-platform/disco2/internal/store"
	"github.com/obot-platform/disco2/jobqueue"
)

type LifecycleResource interface {
	BeginOperation(model.OperationSpec, *string)
	IncrementGeneration()
	SetLastJobID(*string)
}

type Orchestrator struct {
	store  *store.Store
	ensure JobEnsurer
	notify func()
}

type JobEnsurer func(ctx context.Context, txDB *gorm.DB, payload jobqueue.Payload) (*jobqueue.Job, bool, error)

func New(store *store.Store, ensure JobEnsurer, notify func()) *Orchestrator {
	if ensure == nil {
		panic("orchestration job ensurer is required")
	}
	return &Orchestrator{
		store:  store,
		ensure: ensure,
		notify: notify,
	}
}

type PrepareFunc[T LifecycleResource] func(ctx context.Context, txStore *store.Store) (T, error)
type PersistFunc[T LifecycleResource] func(ctx context.Context, txStore *store.Store, resource T) error
type PayloadFunc[T LifecycleResource] func(resource T) jobqueue.Payload

func Begin[T LifecycleResource](
	ctx context.Context,
	o *Orchestrator,
	spec model.OperationSpec,
	prepare PrepareFunc[T],
	persist PersistFunc[T],
	payload PayloadFunc[T],
	mutate ...func(T),
) (T, error) {
	var zero T
	var resource T
	jobCreated := false

	if err := o.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		var err error
		resource, err = prepare(ctx, txStore)
		if err != nil {
			return err
		}

		resource.IncrementGeneration()
		resource.BeginOperation(spec, nil)
		for _, fn := range mutate {
			fn(resource)
		}

		job, created, err := o.ensure(ctx, txDB, payload(resource))
		var jobID *string
		if errors.Is(err, jobqueue.ErrJobAlreadyExists) {
			err = nil
		}
		if err != nil {
			return err
		}
		if job != nil {
			jobID = &job.ID
			resource.SetLastJobID(jobID)
		}
		if created {
			jobCreated = true
		}
		return persist(ctx, txStore, resource)
	}); err != nil {
		return zero, err
	}

	if jobCreated && o.notify != nil {
		o.notify()
	}
	return resource, nil
}
