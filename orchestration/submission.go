package orchestration

import (
	"context"
	"errors"
)

// SubmitAppendFunc appends payload to a transaction-scoped job store.
type SubmitAppendFunc func(context.Context, JobStore, Payload) (*Job, error)

// SubmitTransaction optionally wraps Dispatcher.Submit in an application
// transaction. The callback appends the job and returns it so callers can update
// and persist their own resource state before the transaction commits.
type SubmitTransaction func(context.Context, SubmitAppendFunc) (*Job, error)

type submitOptions struct {
	queueConfig      QueueConfig
	createJobOptions []CreateJobOption
	transaction      SubmitTransaction
}

// SubmitOption configures Dispatcher.Submit.
type SubmitOption func(*submitOptions)

// WithQueueConfig configures enqueue-time defaults for Dispatcher.Submit.
func WithQueueConfig(cfg QueueConfig) SubmitOption {
	return func(opts *submitOptions) {
		opts.queueConfig = cfg
	}
}

// WithCreateJobOptions passes store-level create options to the durable job
// append.
func WithCreateJobOptions(options ...CreateJobOption) SubmitOption {
	return func(opts *submitOptions) {
		opts.createJobOptions = append(opts.createJobOptions, options...)
	}
}

// WithSubmitTransaction makes Dispatcher.Submit append the durable job inside
// an application-owned transaction.
func WithSubmitTransaction(transaction SubmitTransaction) SubmitOption {
	return func(opts *submitOptions) {
		opts.transaction = transaction
	}
}

// Submit appends payload as a durable job and wakes the dispatcher after the
// append succeeds. Callers that need resource mutation and job creation to share
// a commit should pass WithSubmitTransaction and do their resource work in that
// transaction wrapper.
func (d *Dispatcher) Submit(ctx context.Context, payload Payload, opts ...SubmitOption) (*Job, error) {
	if d == nil {
		return nil, errors.New("dispatcher is nil")
	}

	cfg := submitOptions{
		transaction: func(ctx context.Context, fn SubmitAppendFunc) (*Job, error) {
			return fn(ctx, d.store, payload)
		},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.transaction == nil {
		return nil, errors.New("submit transaction is nil")
	}

	job, err := cfg.transaction(ctx, func(ctx context.Context, tx JobStore, payload Payload) (*Job, error) {
		if payload == nil {
			return nil, errors.New("payload is nil")
		}
		job, _, err := AppendJobWithOptions(ctx, tx, payload, cfg.queueConfig, cfg.createJobOptions...)
		return job, err
	})
	if err != nil {
		return nil, err
	}

	if job != nil {
		d.NotifyNewJob()
	}
	return job, nil
}
