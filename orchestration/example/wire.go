package main

import (
	"context"
	"time"

	"github.com/obot-platform/discobox/orchestration"
)

type App struct {
	Store      *memoryStore
	Sandboxes  *SandboxManager
	Dispatcher *orchestration.Dispatcher
}

func NewApp() (*App, error) {
	store := newMemoryStore()

	dispatcher := orchestration.NewDispatcher(store, orchestration.DispatcherConfig{
		SingleNode:         true,
		PollInterval:       100 * time.Millisecond,
		JobTimeout:         30 * time.Second,
		StaleJobTimeout:    time.Minute,
		RetryBackoff:       time.Second,
		ImmediateExecution: true,
		DefaultConcurrency: 1,
	})

	if err := dispatcher.Register(
		SandboxReconcileType,
		NewSandboxReconcileExecutor(store),
		orchestration.WithConcurrency(2),
	); err != nil {
		return nil, err
	}

	sandboxes := NewSandboxManager(store, dispatcher)

	return &App{
		Store:      store,
		Sandboxes:  sandboxes,
		Dispatcher: dispatcher,
	}, nil
}

func NewSandboxManager(store *memoryStore, dispatcher *orchestration.Dispatcher) *SandboxManager {
	return &SandboxManager{store: store, dispatcher: dispatcher}
}

type SandboxManager struct {
	store      *memoryStore
	dispatcher *orchestration.Dispatcher
}

func (s *SandboxManager) Create(ctx context.Context, projectID, sandboxID string) (*Sandbox, error) {
	sandbox := &Sandbox{
		ProjectID: projectID,
		ID:        sandboxID,
	}
	return s.create(ctx, sandbox, OperationCreate)
}

func (s *SandboxManager) Start(ctx context.Context, projectID, sandboxID string) (*Sandbox, error) {
	return s.submit(ctx, SandboxID{ProjectID: projectID, SandboxID: sandboxID}, OperationStart)
}

func (s *SandboxManager) Stop(ctx context.Context, projectID, sandboxID string) (*Sandbox, error) {
	return s.submit(ctx, SandboxID{ProjectID: projectID, SandboxID: sandboxID}, OperationStop)
}

func (s *SandboxManager) create(ctx context.Context, sandbox *Sandbox, operation SandboxOperation) (*Sandbox, error) {
	if _, err := s.dispatcher.Submit(ctx, nil,
		orchestration.WithQueueConfig(orchestration.QueueConfig{DefaultMaxAttempts: 3}),
		orchestration.WithSubmitTransaction(func(ctx context.Context, appendJob orchestration.SubmitAppendFunc) (*orchestration.Job, error) {
			var job *orchestration.Job
			err := s.store.Transaction(ctx, func(ctx context.Context, tx *memoryStore) error {
				sandbox.IncrementGeneration()
				sandbox.BeginOperation(operation, nil)

				var err error
				job, err = appendJob(ctx, tx, sandboxPayload(sandbox))
				if err != nil {
					return err
				}
				sandbox.SetLastJobID(&job.ID)
				return tx.CreateSandbox(ctx, sandbox)
			})
			return job, err
		}),
	); err != nil {
		return nil, err
	}
	return s.store.GetSandbox(ctx, sandbox.ID)
}

func (s *SandboxManager) submit(ctx context.Context, id SandboxID, operation SandboxOperation) (*Sandbox, error) {
	if _, err := s.dispatcher.Submit(ctx, nil,
		orchestration.WithQueueConfig(orchestration.QueueConfig{DefaultMaxAttempts: 3}),
		orchestration.WithSubmitTransaction(func(ctx context.Context, appendJob orchestration.SubmitAppendFunc) (*orchestration.Job, error) {
			var job *orchestration.Job
			err := s.store.Transaction(ctx, func(ctx context.Context, tx *memoryStore) error {
				sandbox, err := tx.Get(ctx, id)
				if err != nil {
					return err
				}
				sandbox.IncrementGeneration()
				sandbox.BeginOperation(operation, nil)

				job, err = appendJob(ctx, tx, sandboxPayload(sandbox))
				if err != nil {
					return err
				}
				sandbox.SetLastJobID(&job.ID)
				return tx.Update(ctx, sandbox)
			})
			return job, err
		}),
	); err != nil {
		return nil, err
	}
	return s.store.Reload(ctx, id)
}

func sandboxPayload(sandbox *Sandbox) orchestration.Payload {
	return SandboxReconcilePayload{
		ProjectID:  sandbox.ProjectID,
		SandboxID:  sandbox.ID,
		Generation: sandbox.Generation,
	}
}
