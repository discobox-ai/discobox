package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/obot-platform/disco2/internal/model"
	"github.com/obot-platform/disco2/internal/store"
	"github.com/obot-platform/disco2/jobqueue"
)

type SandboxReconciler struct {
	store      *store.Store
	operations *SandboxOperations
}

func NewSandboxReconciler(store *store.Store, operations *SandboxOperations) *SandboxReconciler {
	if operations == nil {
		operations = NewSandboxOperations()
	}
	return &SandboxReconciler{
		store:      store,
		operations: operations,
	}
}

func (r *SandboxReconciler) ReconcileSandboxJob(ctx context.Context, projectID, sandboxID, jobID string, generation int64) error {
	sandbox, err := r.store.GetSandbox(ctx, projectID, sandboxID, store.WithGeneration(generation))
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if errors.Is(err, store.ErrGenerationConflict) {
		return jobqueue.Canceled("sandbox generation changed")
	}
	if err != nil {
		return err
	}

	run := sandboxRun{
		store:      r.store,
		operations: r.operations,
		sandbox:    sandbox,
		generation: generation,
	}
	run.sandbox.LastJobID = &jobID

	switch sandbox.DesiredState {
	case model.SandboxDesiredStateRunning:
		if sandbox.RestartGeneration > sandbox.RestartedGeneration {
			return run.Restart(ctx)
		}
		return run.Start(ctx)
	case model.SandboxDesiredStateStopped:
		return run.Stop(ctx)
	case model.SandboxDesiredStateDeleted:
		return run.Delete(ctx)
	default:
		return fmt.Errorf("unsupported sandbox desired state %q", sandbox.DesiredState)
	}
}

type sandboxRun struct {
	store      *store.Store
	operations *SandboxOperations
	sandbox    *model.Sandbox
	generation int64
}

func (r *sandboxRun) Start(ctx context.Context) error {
	if r.sandbox.Phase == model.SandboxPhaseRunning && r.sandbox.ObservedGeneration == r.generation && r.sandbox.LastOperationStatus == model.SandboxOperationStatusSuccess {
		return nil
	}

	status := "starting sandbox"
	r.sandbox.MarkOperationRunning(&status)
	if err := r.update(ctx); err != nil {
		return err
	}
	if err := r.operations.Start(ctx, r.sandbox); err != nil {
		return err
	}
	r.sandbox.ObservedGeneration = r.generation
	r.sandbox.CompleteOperation(model.SandboxPhaseRunning, nil)
	return r.update(ctx)
}

func (r *sandboxRun) Restart(ctx context.Context) error {
	status := "restarting sandbox"
	r.sandbox.MarkOperationRunning(&status)
	if err := r.update(ctx); err != nil {
		return err
	}
	if err := r.operations.Stop(ctx, r.sandbox); err != nil {
		return err
	}
	if err := r.operations.Start(ctx, r.sandbox); err != nil {
		return err
	}
	r.sandbox.RestartedGeneration = r.sandbox.RestartGeneration
	r.sandbox.ObservedGeneration = r.generation
	r.sandbox.CompleteOperation(model.SandboxPhaseRunning, nil)
	return r.update(ctx)
}

func (r *sandboxRun) Stop(ctx context.Context) error {
	if r.sandbox.Phase == model.SandboxPhaseStopped && r.sandbox.ObservedGeneration == r.generation && r.sandbox.LastOperationStatus == model.SandboxOperationStatusSuccess {
		return nil
	}

	status := "stopping sandbox"
	r.sandbox.MarkOperationRunning(&status)
	if err := r.update(ctx); err != nil {
		return err
	}
	if err := r.operations.Stop(ctx, r.sandbox); err != nil {
		return err
	}
	r.sandbox.ObservedGeneration = r.generation
	r.sandbox.CompleteOperation(model.SandboxPhaseStopped, nil)
	return r.update(ctx)
}

func (r *sandboxRun) Delete(ctx context.Context) error {
	if r.sandbox.Phase == model.SandboxPhaseDeleted && r.sandbox.ObservedGeneration == r.generation && r.sandbox.LastOperationStatus == model.SandboxOperationStatusSuccess {
		return nil
	}

	status := "deleting sandbox"
	r.sandbox.MarkOperationRunning(&status)
	if err := r.update(ctx); err != nil {
		return err
	}
	if err := r.operations.Delete(ctx, r.sandbox); err != nil {
		return err
	}
	r.sandbox.ObservedGeneration = r.generation
	r.sandbox.CompleteOperation(model.SandboxPhaseDeleted, nil)
	return r.update(ctx)
}

func (r *sandboxRun) update(ctx context.Context) error {
	if err := r.store.UpdateSandbox(ctx, r.sandbox, store.WithGeneration(r.generation)); err != nil {
		if errors.Is(err, store.ErrGenerationConflict) {
			return jobqueue.Canceled("sandbox generation changed")
		}
		return err
	}
	return nil
}
