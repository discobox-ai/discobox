// Package jobs contains durable job payloads and executors.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/obot-platform/disco2/jobqueue"
)

const SandboxReconcileType jobqueue.Type = "sandbox.reconcile"

type SandboxReconcilePayload struct {
	ProjectID  string `json:"projectId"`
	SandboxID  string `json:"sandboxId"`
	Generation int64  `json:"generation"`
}

func (p SandboxReconcilePayload) JobType() jobqueue.Type {
	return SandboxReconcileType
}

func (p SandboxReconcilePayload) Resource() jobqueue.Resource {
	return jobqueue.Resource{Type: "sandbox", ID: p.SandboxID}
}

func (p SandboxReconcilePayload) MaxAttempts() int {
	return 3
}

type SandboxReconciler interface {
	ReconcileSandboxJob(ctx context.Context, projectID, sandboxID, jobID string, generation int64) error
}

type SandboxReconcileExecutor struct {
	reconciler SandboxReconciler
}

func NewSandboxReconcileExecutor(reconciler SandboxReconciler) *SandboxReconcileExecutor {
	return &SandboxReconcileExecutor{reconciler: reconciler}
}

func (e *SandboxReconcileExecutor) Type() jobqueue.Type {
	return SandboxReconcileType
}

func (e *SandboxReconcileExecutor) Execute(ctx context.Context, job *jobqueue.Job) error {
	var payload SandboxReconcilePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("invalid sandbox reconcile payload: %w", err)
	}
	if payload.ProjectID == "" || payload.SandboxID == "" || payload.Generation < 1 {
		return fmt.Errorf("projectId, sandboxId, and generation are required")
	}
	return e.reconciler.ReconcileSandboxJob(ctx, payload.ProjectID, payload.SandboxID, job.ID, payload.Generation)
}
