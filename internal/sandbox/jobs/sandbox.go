// Package jobs contains durable job payloads and executors.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/obot-platform/disco2/orchestration"
)

const SandboxReconcileType orchestration.Type = "sandbox.reconcile"

type SandboxReconcilePayload struct {
	ProjectID  string `json:"projectId"`
	SandboxID  string `json:"sandboxId"`
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

type SandboxReconciler interface {
	AssertSandboxGeneration(ctx context.Context, projectID, sandboxID string, generation int64) error
	ReconcileSandboxJob(ctx context.Context, projectID, sandboxID, jobID string, generation int64) error
}

type SandboxReconcileExecutor struct {
	reconciler SandboxReconciler
}

func NewSandboxReconcileExecutor(reconciler SandboxReconciler) *SandboxReconcileExecutor {
	return &SandboxReconcileExecutor{reconciler: reconciler}
}

func (e *SandboxReconcileExecutor) Type() orchestration.Type {
	return SandboxReconcileType
}

func (e *SandboxReconcileExecutor) AssertGeneration(ctx context.Context, job *orchestration.Job) error {
	payload, err := decodeSandboxReconcilePayload(job)
	if err != nil {
		return err
	}
	return e.reconciler.AssertSandboxGeneration(ctx, payload.ProjectID, payload.SandboxID, payload.Generation)
}

func (e *SandboxReconcileExecutor) Execute(ctx context.Context, job *orchestration.Job) error {
	payload, err := decodeSandboxReconcilePayload(job)
	if err != nil {
		return err
	}
	return e.reconciler.ReconcileSandboxJob(ctx, payload.ProjectID, payload.SandboxID, job.ID, payload.Generation)
}

func decodeSandboxReconcilePayload(job *orchestration.Job) (SandboxReconcilePayload, error) {
	var payload SandboxReconcilePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return payload, fmt.Errorf("invalid sandbox reconcile payload: %w", err)
	}
	if payload.ProjectID == "" || payload.SandboxID == "" || payload.Generation < 1 {
		return payload, fmt.Errorf("projectId, sandboxId, and generation are required")
	}
	return payload, nil
}
