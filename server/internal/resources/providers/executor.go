package providers

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/obot-platform/discobox/orchestration"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/resources/workers"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/store"
)

const WorkerProviderReconcileType orchestration.Type = "workerprovider.reconcile"

type WorkerProviderReconcilePayload struct {
	ProjectID  string `json:"projectId"`
	ProviderID string `json:"providerId"`
}

func (p WorkerProviderReconcilePayload) JobType() orchestration.Type {
	return WorkerProviderReconcileType
}

func (p WorkerProviderReconcilePayload) Resource() orchestration.Resource {
	return orchestration.Resource{Type: "workerprovider", ID: p.ProviderID}
}

type WorkerProviderReconcileExecutor struct {
	store   *store.Store
	manager *sandbox.ProviderManager
	workers *workers.Manager
}

func NewWorkerProviderReconcileExecutor(store *store.Store, manager *sandbox.ProviderManager, workers *workers.Manager) *WorkerProviderReconcileExecutor {
	return &WorkerProviderReconcileExecutor{store: store, manager: manager, workers: workers}
}

func (e *WorkerProviderReconcileExecutor) Execute(ctx context.Context, job *orchestration.Job) (orchestration.JobResult, error) {
	payload, err := decodeWorkerProviderReconcilePayload(job)
	if err != nil {
		return orchestration.JobResult{}, err
	}
	project, err := e.store.GetProject(ctx, payload.ProjectID)
	if err != nil {
		return orchestration.JobResult{}, err
	}
	provider, err := e.store.GetSandboxProviderInstance(ctx, payload.ProjectID, payload.ProviderID)
	if err != nil {
		return orchestration.JobResult{}, err
	}
	return orchestration.JobResult{}, e.ReconcileWorkerProvider(ctx, project, provider)
}

func (e *WorkerProviderReconcileExecutor) ReconcileWorkerProvider(ctx context.Context, project *model.Project, provider *model.SandboxProviderInstance) error {
	if provider == nil || provider.Disabled {
		return nil
	}
	runtimeProvider, err := e.manager.ResolveInstance(ctx, provider)
	if err != nil {
		return err
	}
	workerProvider, ok := runtimeProvider.(sandbox.WorkerProviderReconciler)
	if !ok {
		return nil
	}
	if e.workers == nil {
		return fmt.Errorf("worker manager is required")
	}
	return workerProvider.ReconcileWorkerProvider(ctx, e.workers, project, provider)
}

func decodeWorkerProviderReconcilePayload(job *orchestration.Job) (WorkerProviderReconcilePayload, error) {
	var payload WorkerProviderReconcilePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return payload, fmt.Errorf("invalid worker provider reconcile payload: %w", err)
	}
	if payload.ProjectID == "" || payload.ProviderID == "" {
		return payload, fmt.Errorf("projectId and providerId are required")
	}
	return payload, nil
}
