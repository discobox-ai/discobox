package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/obot-platform/discobox/orchestration"
)

const SandboxReconcileType orchestration.Type = "sandbox.reconcile"

type SandboxReconcilePayload struct {
	SandboxID  string `json:"sandboxId"`
	ProjectID  string `json:"projectId"`
	Generation int64  `json:"generation"`
}

func (p SandboxReconcilePayload) JobType() orchestration.Type {
	return SandboxReconcileType
}

func (p SandboxReconcilePayload) Resource() orchestration.Resource {
	return orchestration.Resource{Type: "sandbox", ID: p.SandboxID}
}

func (p SandboxReconcilePayload) MaxAttempts() int {
	return 1
}

type SandboxReconcileExecutor struct {
	store *memoryStore
}

func NewSandboxReconcileExecutor(store *memoryStore) *SandboxReconcileExecutor {
	return &SandboxReconcileExecutor{store: store}
}

func (e *SandboxReconcileExecutor) Execute(ctx context.Context, job *orchestration.Job) (orchestration.JobResult, error) {
	var payload SandboxReconcilePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return orchestration.JobResult{}, fmt.Errorf("invalid sandbox reconcile payload: %w", err)
	}
	if payload.ProjectID == "" || payload.SandboxID == "" {
		return orchestration.JobResult{}, fmt.Errorf("projectId and sandboxId are required")
	}
	return orchestration.JobResult{}, e.reconcile(ctx, payload)
}

func (e *SandboxReconcileExecutor) reconcile(ctx context.Context, payload SandboxReconcilePayload) error {
	sandbox, err := e.store.GetSandbox(ctx, payload.SandboxID)
	if err != nil {
		return err
	}
	if sandbox.Generation != payload.Generation {
		return orchestration.Canceled("sandbox generation changed")
	}

	select {
	case <-time.After(100 * time.Millisecond):
	case <-ctx.Done():
		return ctx.Err()
	}

	switch sandbox.DesiredState {
	case DesiredRunning:
		sandbox.Phase = PhaseRunning
	case DesiredStopped:
		sandbox.Phase = PhaseStopped
	case DesiredDeleted:
		sandbox.Phase = PhaseDeleted
	default:
		return fmt.Errorf("unsupported desired state %q", sandbox.DesiredState)
	}
	sandbox.ObservedGeneration = payload.Generation
	return e.store.UpdateSandbox(ctx, sandbox)
}
