package workers

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/store"
)

// WorkerProviderResourceType is the reconcile-engine resource type for worker
// provider instances.
const WorkerProviderResourceType = "workerprovider"

// WorkerProviderDirtyID encodes the composite identity a provider reconcile
// needs. Provider lookups are project-scoped, so the dirty id carries both.
func WorkerProviderDirtyID(projectID, providerID string) string {
	return projectID + "/" + providerID
}

func splitWorkerProviderDirtyID(id string) (projectID, providerID string, err error) {
	projectID, providerID, ok := strings.Cut(id, "/")
	if !ok || projectID == "" || providerID == "" {
		return "", "", fmt.Errorf("invalid workerprovider dirty id %q", id)
	}
	return projectID, providerID, nil
}

// WorkerProviderReconciler converges one provider instance's worker pool. It
// implements reconcile.Reconciler (and reconcile.Scanner as the drift
// backstop).
type WorkerProviderReconciler struct {
	store   *store.Store
	manager *sandbox.ProviderManager
	workers *ControlPlane
}

func NewWorkerProviderReconciler(appStore *store.Store, manager *sandbox.ProviderManager, workerManager *ControlPlane) *WorkerProviderReconciler {
	return &WorkerProviderReconciler{store: appStore, manager: manager, workers: workerManager}
}

// Reconcile loads the latest project + provider state and converges the
// provider's worker pool. Missing or disabled providers are converged trivially
// (nothing to do), settling the dirty row.
func (r *WorkerProviderReconciler) Reconcile(ctx context.Context, id string) error {
	projectID, providerID, err := splitWorkerProviderDirtyID(id)
	if err != nil {
		return err
	}
	project, err := r.store.GetProject(ctx, projectID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	provider, err := r.store.GetSandboxProviderInstance(ctx, projectID, providerID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return r.reconcileWorkerProvider(ctx, project, provider)
}

func (r *WorkerProviderReconciler) reconcileWorkerProvider(ctx context.Context, project *model.Project, provider *model.SandboxProviderInstance) error {
	if provider == nil || provider.Disabled {
		return nil
	}
	runtimeProvider, err := r.manager.ResolveInstance(ctx, provider)
	if err != nil {
		return err
	}
	workerProvider, ok := runtimeProvider.(sandbox.WorkerProviderReconciler)
	if !ok {
		return nil
	}
	if r.workers == nil {
		return fmt.Errorf("worker manager is required")
	}
	return workerProvider.ReconcileWorkerProvider(ctx, r.workers, project, provider)
}

// ScanDirty reports every enabled provider instance. It is the level-triggered
// backstop: providers whose pools drifted without an event (lost mark, crashed
// watcher, driver that forgot to reschedule) heal on the next scan.
func (r *WorkerProviderReconciler) ScanDirty(ctx context.Context) ([]string, error) {
	projects, err := r.store.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for i := range projects {
		providers, err := r.store.ListSandboxProviderInstances(ctx, projects[i].ID)
		if err != nil {
			return nil, err
		}
		for j := range providers {
			if providers[j].Disabled {
				continue
			}
			ids = append(ids, WorkerProviderDirtyID(projects[i].ID, providers[j].ID))
		}
	}
	return ids, nil
}
