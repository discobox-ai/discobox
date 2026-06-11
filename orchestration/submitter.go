package orchestration

import (
	"context"
)

// LifecycleResource is implemented by application resources whose desired state
// is reconciled by durable jobs. The operation type is application-defined so
// this module does not need to know resource-specific lifecycle fields.
type LifecycleResource[Operation any] interface {
	BeginOperation(Operation, *string)
	IncrementGeneration()
	SetLastJobID(*string)
}

// TransactionFunc runs fn in a store transaction and passes the transaction-
// scoped store value to fn. The store type is application-defined.
type TransactionFunc[Store any] func(ctx context.Context, fn func(ctx context.Context, tx Store) error) error

// ResourceStore contains the persistence operations needed to accept lifecycle
// intent for one resource type.
type ResourceStore[Resource any, ID any, Store any] struct {
	Get    func(context.Context, Store, ID) (Resource, error)
	Create func(context.Context, Store, Resource) error
	Update func(context.Context, Store, Resource) error
}

// PayloadFunc builds the durable job payload for a resource after its accepted
// lifecycle operation has incremented generation and updated desired state.
type PayloadFunc[Resource any] func(Resource) Payload

// Submitter accepts desired-state operations by atomically updating a resource
// and appending the durable job that will reconcile it.
type Submitter[Resource LifecycleResource[Operation], Operation any, ID any, Store JobStore] struct {
	transaction TransactionFunc[Store]
	resource    ResourceStore[Resource, ID, Store]
	payload     PayloadFunc[Resource]
	queueConfig QueueConfig
	notify      func(context.Context)
}

// SubmitterConfig configures a Submitter for one reconciled resource type.
type SubmitterConfig[Resource LifecycleResource[Operation], Operation any, ID any, Store JobStore] struct {
	Transaction TransactionFunc[Store]
	Resource    ResourceStore[Resource, ID, Store]
	Payload     PayloadFunc[Resource]
	QueueConfig QueueConfig
	Notify      func(context.Context)
}

func NewSubmitter[Resource LifecycleResource[Operation], Operation any, ID any, Store JobStore](cfg SubmitterConfig[Resource, Operation, ID, Store]) *Submitter[Resource, Operation, ID, Store] {
	if cfg.Transaction == nil {
		panic("orchestration transaction function is required")
	}
	if cfg.Payload == nil {
		panic("orchestration payload function is required")
	}
	if cfg.Resource.Get == nil {
		panic("orchestration resource get function is required")
	}
	if cfg.Resource.Create == nil {
		panic("orchestration resource create function is required")
	}
	if cfg.Resource.Update == nil {
		panic("orchestration resource update function is required")
	}
	return &Submitter[Resource, Operation, ID, Store]{
		transaction: cfg.Transaction,
		resource:    cfg.Resource,
		payload:     cfg.Payload,
		queueConfig: cfg.QueueConfig,
		notify:      cfg.Notify,
	}
}

// Create accepts intent for a new resource and persists the resource and its
// durable reconcile job in one transaction.
func (s *Submitter[Resource, Operation, ID, Store]) Create(ctx context.Context, resource Resource, operation Operation, mutate ...func(Resource)) (Resource, error) {
	return s.accept(ctx, operation, func(context.Context, Store) (Resource, error) {
		return resource, nil
	}, s.resource.Create, mutate...)
}

// Submit accepts intent for an existing resource and persists the resource
// update and durable reconcile job in one transaction.
func (s *Submitter[Resource, Operation, ID, Store]) Submit(ctx context.Context, id ID, operation Operation, mutate ...func(Resource)) (Resource, error) {
	return s.accept(ctx, operation, func(ctx context.Context, tx Store) (Resource, error) {
		return s.resource.Get(ctx, tx, id)
	}, s.resource.Update, mutate...)
}

func (s *Submitter[Resource, Operation, ID, Store]) accept(ctx context.Context, operation Operation, prepare func(context.Context, Store) (Resource, error), persist func(context.Context, Store, Resource) error, mutate ...func(Resource)) (Resource, error) {
	var zero Resource
	var resource Resource
	jobCreated := false

	if err := s.transaction(ctx, func(ctx context.Context, tx Store) error {
		var err error
		resource, err = prepare(ctx, tx)
		if err != nil {
			return err
		}

		resource.IncrementGeneration()
		resource.BeginOperation(operation, nil)
		for _, fn := range mutate {
			fn(resource)
		}

		job, created, err := AppendJob(ctx, tx, s.payload(resource), s.queueConfig)
		if err != nil {
			return err
		}
		if job != nil {
			jobID := job.ID
			resource.SetLastJobID(&jobID)
		}
		if created {
			jobCreated = true
		}
		return persist(ctx, tx, resource)
	}); err != nil {
		return zero, err
	}

	if jobCreated && s.notify != nil {
		s.notify(ctx)
	}
	return resource, nil
}
