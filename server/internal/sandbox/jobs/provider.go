package jobs

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/obot-platform/discobox/orchestration"
)

const ProviderReconcileType orchestration.Type = "provider.reconcile"

type ProviderReconcilePayload struct {
	ProjectID  string `json:"projectId"`
	ProviderID string `json:"providerId"`
}

func (p ProviderReconcilePayload) JobType() orchestration.Type {
	return ProviderReconcileType
}

func (p ProviderReconcilePayload) Resource() orchestration.Resource {
	return orchestration.Resource{Type: "provider", ID: p.ProviderID}
}

type ProviderReconciler interface {
	ReconcileProviderJob(ctx context.Context, projectID, providerID, jobID string) error
}

type ProviderReconcileExecutor struct {
	reconciler ProviderReconciler
}

func NewProviderReconcileExecutor(reconciler ProviderReconciler) *ProviderReconcileExecutor {
	return &ProviderReconcileExecutor{reconciler: reconciler}
}

func (e *ProviderReconcileExecutor) Execute(ctx context.Context, job *orchestration.Job) (orchestration.JobResult, error) {
	payload, err := decodeProviderReconcilePayload(job)
	if err != nil {
		return orchestration.JobResult{}, err
	}
	return orchestration.JobResult{}, e.reconciler.ReconcileProviderJob(ctx, payload.ProjectID, payload.ProviderID, job.ID)
}

func decodeProviderReconcilePayload(job *orchestration.Job) (ProviderReconcilePayload, error) {
	var payload ProviderReconcilePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return payload, fmt.Errorf("invalid provider reconcile payload: %w", err)
	}
	if payload.ProjectID == "" || payload.ProviderID == "" {
		return payload, fmt.Errorf("projectId and providerId are required")
	}
	return payload, nil
}
