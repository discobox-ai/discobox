package main

import (
	"context"
	"time"

	"github.com/obot-platform/discobox/orchestration"
)

type App struct {
	Store      *memoryStore
	Sandboxes  *SandboxSubmitter
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

	sandboxes := NewSandboxSubmitter(store, dispatcher.NotifyNewJob)

	return &App{
		Store:      store,
		Sandboxes:  sandboxes,
		Dispatcher: dispatcher,
	}, nil
}

func NewSandboxSubmitter(store *memoryStore, notify func()) *SandboxSubmitter {
	submitter := orchestration.NewSubmitter(orchestration.SubmitterConfig[*Sandbox, SandboxOperation, SandboxID, *memoryStore]{
		Transaction: store.Transaction,
		Resource:    store,
		Payload: func(sandbox *Sandbox) orchestration.Payload {
			return SandboxReconcilePayload{
				ProjectID:  sandbox.ProjectID,
				SandboxID:  sandbox.ID,
				Generation: sandbox.Generation,
			}
		},
		QueueConfig: orchestration.QueueConfig{DefaultMaxAttempts: 3},
		Notify: func(context.Context) {
			if notify != nil {
				notify()
			}
		},
	})

	return &SandboxSubmitter{submitter: submitter}
}

type SandboxSubmitter struct {
	submitter *orchestration.Submitter[*Sandbox, SandboxOperation, SandboxID, *memoryStore]
}

func (s *SandboxSubmitter) Create(ctx context.Context, projectID, sandboxID string) (*Sandbox, error) {
	return s.submitter.Create(ctx, &Sandbox{
		ProjectID: projectID,
		ID:        sandboxID,
	}, OperationCreate)
}

func (s *SandboxSubmitter) Start(ctx context.Context, projectID, sandboxID string) (*Sandbox, error) {
	return s.submitter.Submit(ctx, SandboxID{ProjectID: projectID, SandboxID: sandboxID}, OperationStart)
}

func (s *SandboxSubmitter) Stop(ctx context.Context, projectID, sandboxID string) (*Sandbox, error) {
	return s.submitter.Submit(ctx, SandboxID{ProjectID: projectID, SandboxID: sandboxID}, OperationStop)
}
