// Package sandboxes contains sandbox API behavior, lifecycle intent, and reconciliation.
package sandboxes

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/obot-platform/discobox/orchestration"
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

func (e *SandboxReconcileExecutor) AssertGeneration(ctx context.Context, job *orchestration.Job) error {
	payload, err := decodeSandboxReconcilePayload(job)
	if err != nil {
		return err
	}
	return e.AssertSandboxGeneration(ctx, payload.ProjectID, payload.SandboxID, payload.Generation)
}

func (e *SandboxReconcileExecutor) Execute(ctx context.Context, job *orchestration.Job) (orchestration.JobResult, error) {
	payload, err := decodeSandboxReconcilePayload(job)
	if err != nil {
		return orchestration.JobResult{}, err
	}
	return orchestration.JobResult{}, e.ReconcileSandboxJob(ctx, payload.ProjectID, payload.SandboxID, job.ID, payload.Generation)
}

func (e *SandboxReconcileExecutor) OnTerminal(ctx context.Context, job *orchestration.Job) error {
	if job.Status != orchestration.StatusFailed {
		return nil
	}
	payload, err := decodeSandboxReconcilePayload(job)
	if err != nil {
		return err
	}
	message := "sandbox reconcile failed"
	if job.Error != nil && *job.Error != "" {
		message = *job.Error
	} else if job.Message != nil && *job.Message != "" {
		message = *job.Message
	}
	return e.MarkSandboxJobFailed(ctx, payload.ProjectID, payload.SandboxID, job.ID, payload.Generation, message)
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
