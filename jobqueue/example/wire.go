package main

import (
	"context"
	"time"

	"github.com/obot-platform/disco2/jobqueue"
)

type App struct {
	Queue      *jobqueue.Queue
	Dispatcher *jobqueue.Dispatcher
}

func NewApp(store jobqueue.Store, sandboxes SandboxService) (*App, error) {
	queue := jobqueue.NewQueue(store, jobqueue.QueueConfig{
		DefaultPriority:    10,
		DefaultMaxAttempts: 3,
	})

	dispatcher := jobqueue.NewDispatcher(store, jobqueue.DispatcherConfig{
		SingleNode:         true,
		PollInterval:       100 * time.Millisecond,
		JobTimeout:         30 * time.Second,
		StaleJobTimeout:    time.Minute,
		RetryBackoff:       time.Second,
		ImmediateExecution: true,
		DefaultConcurrency: 1,
	})

	if err := dispatcher.Register(
		NewSandboxProvisionExecutor(sandboxes),
		jobqueue.WithConcurrency(2),
	); err != nil {
		return nil, err
	}

	if err := dispatcher.Register(
		NewSandboxDeleteExecutor(sandboxes),
		jobqueue.WithConcurrency(1),
	); err != nil {
		return nil, err
	}

	queue.SetNotifyFunc(dispatcher.NotifyNewJob)

	return &App{
		Queue:      queue,
		Dispatcher: dispatcher,
	}, nil
}

func ProvisionSandbox(ctx context.Context, queue *jobqueue.Queue, projectID, sandboxID string) (*jobqueue.Job, error) {
	return queue.Enqueue(ctx, SandboxProvisionPayload{
		ProjectID: projectID,
		SandboxID: sandboxID,
	})
}

func DeleteSandboxLater(
	ctx context.Context,
	queue *jobqueue.Queue,
	projectID string,
	sandboxID string,
	deleteAt time.Time,
) (*jobqueue.Job, error) {
	return queue.Enqueue(ctx, SandboxDeletePayload{
		ProjectID: projectID,
		SandboxID: sandboxID,
		DeleteAt:  deleteAt,
	})
}
