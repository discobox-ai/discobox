package jobs

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/obot-platform/discobox/orchestration"
)

const WorkerReconcileType orchestration.Type = "worker.reconcile"

type WorkerReconcilePayload struct {
	ProjectID  string `json:"projectId"`
	ProviderID string `json:"providerId"`
	WorkerID   string `json:"workerId"`
	Generation int64  `json:"generation"`
}

func (p WorkerReconcilePayload) JobType() orchestration.Type {
	return WorkerReconcileType
}

func (p WorkerReconcilePayload) Resource() orchestration.Resource {
	return orchestration.Resource{Type: "worker", ID: p.WorkerID}
}

type WorkerReconciler interface {
	AssertWorkerGeneration(ctx context.Context, projectID, providerID, workerID string, generation int64) error
	ReconcileWorkerJob(ctx context.Context, projectID, providerID, workerID, jobID string, generation int64) error
}

type WorkerReconcileExecutor struct {
	reconciler WorkerReconciler
}

func NewWorkerReconcileExecutor(reconciler WorkerReconciler) *WorkerReconcileExecutor {
	return &WorkerReconcileExecutor{reconciler: reconciler}
}

func (e *WorkerReconcileExecutor) Type() orchestration.Type {
	return WorkerReconcileType
}

func (e *WorkerReconcileExecutor) AssertGeneration(ctx context.Context, job *orchestration.Job) error {
	payload, err := decodeWorkerReconcilePayload(job)
	if err != nil {
		return err
	}
	return e.reconciler.AssertWorkerGeneration(ctx, payload.ProjectID, payload.ProviderID, payload.WorkerID, payload.Generation)
}

func (e *WorkerReconcileExecutor) Execute(ctx context.Context, job *orchestration.Job) error {
	payload, err := decodeWorkerReconcilePayload(job)
	if err != nil {
		return err
	}
	return e.reconciler.ReconcileWorkerJob(ctx, payload.ProjectID, payload.ProviderID, payload.WorkerID, job.ID, payload.Generation)
}

func decodeWorkerReconcilePayload(job *orchestration.Job) (WorkerReconcilePayload, error) {
	var payload WorkerReconcilePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return payload, fmt.Errorf("invalid worker reconcile payload: %w", err)
	}
	if payload.ProjectID == "" || payload.ProviderID == "" || payload.WorkerID == "" || payload.Generation < 1 {
		return payload, fmt.Errorf("projectId, providerId, workerId, and generation are required")
	}
	return payload, nil
}
