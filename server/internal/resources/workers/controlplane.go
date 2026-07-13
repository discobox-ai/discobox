package workers

import (
	"context"
	"errors"
	"fmt"
	"time"

	workeragentauth "github.com/obot-platform/discobox/server/internal/auth/workeragent"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/reconcile"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/store"
	"gorm.io/gorm"
)

// ControlPlane is the TRUSTED worker surface: it owns worker lifecycle intent
// (generation bumps + dirty marks, atomically) and implements
// sandbox.WorkerManager, the narrow interface handed to provider drivers.
// Unlike workers.Service (the HTTP-facing API surface, which validates
// untrusted input and speaks apperrors), the control plane takes ids from
// persisted rows at face value and returns plain domain errors.
type ControlPlane struct {
	store           *store.Store
	engine          *reconcile.Engine
	workerAgentAuth *workeragentauth.Manager
}

func NewControlPlane(appStore *store.Store, engine *reconcile.Engine) *ControlPlane {
	return &ControlPlane{store: appStore, engine: engine}
}

func (s *ControlPlane) SetWorkerAgentAuthManager(manager *workeragentauth.Manager) {
	s.workerAgentAuth = manager
}

// RegisterJobs installs the worker and worker-provider reconcilers on the
// level-triggered reconcile engine. Provider chaining (re-evaluating the pool
// after every worker reconcile) happens inside the worker reconciler.
func (s *ControlPlane) RegisterJobs(providerManager *sandbox.ProviderManager) error {
	if s.engine == nil {
		return errors.New("reconcile engine is required")
	}
	if err := s.engine.Register(WorkerResourceType, NewWorkerReconciler(
		s.store,
		WithWorkerProviderManager(providerManager),
		WithWorkerManager(s),
		WithProviderChain(s.ScheduleWorkerProviderReconciliation),
	)); err != nil {
		return err
	}
	return s.engine.Register(WorkerProviderResourceType, NewWorkerProviderReconciler(s.store, providerManager, s))
}

func (s *ControlPlane) ListWorkers(ctx context.Context, projectID, providerID string) ([]model.Worker, error) {
	return s.store.ListWorkers(ctx, projectID, providerID)
}

func (s *ControlPlane) GetWorker(ctx context.Context, workerID string) (*model.Worker, error) {
	return s.store.GetWorker(ctx, workerID)
}

func (s *ControlPlane) GetProject(ctx context.Context, projectID string) (*model.Project, error) {
	return s.store.GetProject(ctx, projectID)
}

func (s *ControlPlane) GetSandboxProviderInstance(ctx context.Context, projectID, providerID string) (*model.SandboxProviderInstance, error) {
	return s.store.GetSandboxProviderInstance(ctx, projectID, providerID)
}

// CreateWorker persists a new worker with create intent and marks it dirty,
// atomically.
func (s *ControlPlane) CreateWorker(ctx context.Context, worker *model.Worker) (*model.Worker, error) {
	if s.engine == nil {
		return nil, errors.New("reconcile engine is required")
	}
	if err := s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		worker.IncrementGeneration()
		worker.BeginOperation(model.WorkerCreateOperation)
		if err := txStore.CreateWorker(ctx, worker); err != nil {
			return err
		}
		return s.engine.MarkDirtyTx(ctx, txDB, WorkerResourceType, worker.ID)
	}); err != nil {
		return nil, err
	}
	return s.store.GetWorker(ctx, worker.ID)
}

func (s *ControlPlane) DrainWorker(ctx context.Context, workerID string) (*model.Worker, error) {
	return s.submitWorker(ctx, workerID, model.WorkerDrainOperation)
}

// DeleteWorker records delete intent (generation bump) and marks the worker
// dirty, unless sandboxes are still assigned.
func (s *ControlPlane) DeleteWorker(ctx context.Context, workerID string) (*model.Worker, error) {
	if s.engine == nil {
		return nil, errors.New("reconcile engine is required")
	}
	var blocked *model.Worker
	if err := s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		worker, err := txStore.GetWorker(ctx, workerID)
		if err != nil {
			return err
		}
		assigned, err := txStore.CountSandboxesForWorker(ctx, worker.ID)
		if err != nil {
			return err
		}
		if assigned > 0 {
			blocked = worker
			return nil
		}
		previousGeneration := worker.Generation
		worker.IncrementGeneration()
		worker.BeginOperation(model.WorkerDeleteOperation)
		if err := txStore.UpdateWorker(ctx, worker, store.WithWorkerGeneration(previousGeneration)); err != nil {
			return err
		}
		return s.engine.MarkDirtyTx(ctx, txDB, WorkerResourceType, worker.ID)
	}); err != nil {
		return nil, err
	}
	if blocked != nil {
		return blocked, fmt.Errorf("worker %s has assigned sandboxes", workerID)
	}
	return s.store.GetWorker(ctx, workerID)
}

func (s *ControlPlane) submitWorker(ctx context.Context, workerID string, operation model.OperationSpec, mutate ...func(*model.Worker)) (*model.Worker, error) {
	if s.engine == nil {
		return nil, errors.New("reconcile engine is required")
	}
	if err := s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		worker, err := txStore.GetWorker(ctx, workerID)
		if err != nil {
			return err
		}
		previousGeneration := worker.Generation
		worker.IncrementGeneration()
		worker.BeginOperation(operation)
		for _, fn := range mutate {
			fn(worker)
		}
		if err := txStore.UpdateWorker(ctx, worker, store.WithWorkerGeneration(previousGeneration)); err != nil {
			return err
		}
		return s.engine.MarkDirtyTx(ctx, txDB, WorkerResourceType, worker.ID)
	}); err != nil {
		return nil, err
	}
	return s.store.GetWorker(ctx, workerID)
}

func (s *ControlPlane) CreateWorkerBootstrapToken(ctx context.Context, token *model.WorkerBootstrapToken) error {
	return s.store.CreateWorkerBootstrapToken(ctx, token)
}

func (s *ControlPlane) EnsureWorkerAgentTrustKey(ctx context.Context) (string, error) {
	if s.workerAgentAuth == nil {
		return "", nil
	}
	return s.workerAgentAuth.EnsureTrustKey(ctx)
}

func (s *ControlPlane) CreateWorkerAgentToken(ctx context.Context, claims workeragentauth.TokenClaims) (string, error) {
	if s.workerAgentAuth == nil {
		return "", nil
	}
	return s.workerAgentAuth.CreateToken(ctx, claims)
}

func (s *ControlPlane) CreateSandboxAgentToken(ctx context.Context, claims workeragentauth.TokenClaims) (string, error) {
	if s.workerAgentAuth == nil {
		return "", nil
	}
	return s.workerAgentAuth.CreateSandboxAgentToken(ctx, claims)
}

func (s *ControlPlane) FindSchedulableWorker(ctx context.Context, sandbox *model.Sandbox) (*model.Worker, error) {
	return s.store.FindSchedulableWorker(ctx, sandbox)
}

func (s *ControlPlane) CountSandboxesForWorker(ctx context.Context, workerID string) (int64, error) {
	return s.store.CountSandboxesForWorker(ctx, workerID)
}

func (s *ControlPlane) CountSandboxesForWorkers(ctx context.Context, workerIDs []string) (map[string]int64, error) {
	return s.store.CountSandboxesForWorkers(ctx, workerIDs)
}

// DeleteWorkerForExpiredRegistration deletes a worker whose registration never
// completed, guarded by generation and registration-state predicates.
func (s *ControlPlane) DeleteWorkerForExpiredRegistration(ctx context.Context, workerID string, generation int64, cutoff time.Time, message string) (bool, error) {
	if s.engine == nil {
		return false, errors.New("reconcile engine is required")
	}
	updated := false
	if err := s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		worker, err := txStore.GetWorker(ctx, workerID, store.WithWorkerGeneration(generation))
		if errors.Is(err, store.ErrGenerationConflict) || errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		expired := worker.Phase == model.WorkerPhaseRegistering &&
			worker.LastOperationStatus == model.OperationStatusSuccess &&
			worker.RegisteredAt == nil &&
			worker.LastSeenAt == nil &&
			worker.UpdatedAt.Before(cutoff)
		if !expired {
			return nil
		}
		assigned, err := txStore.CountSandboxesForWorker(ctx, worker.ID)
		if err != nil {
			return err
		}
		if assigned > 0 {
			return nil
		}
		previousGeneration := worker.Generation
		worker.IncrementGeneration()
		worker.BeginOperation(model.WorkerDeleteOperation)
		worker.StatusMessage = &message
		if err := txStore.UpdateWorker(ctx, worker, store.WithWorkerGeneration(previousGeneration)); err != nil {
			return err
		}
		if err := s.engine.MarkDirtyTx(ctx, txDB, WorkerResourceType, worker.ID); err != nil {
			return err
		}
		updated = true
		return nil
	}); err != nil {
		if errors.Is(err, store.ErrGenerationConflict) {
			return false, nil
		}
		return false, err
	}
	return updated, nil
}

// ScheduleWorkerReconciliation marks the worker dirty (drift-driven reconcile,
// no intent change).
func (s *ControlPlane) ScheduleWorkerReconciliation(ctx context.Context, workerID string) error {
	if s.engine == nil {
		return errors.New("reconcile engine is required")
	}
	if _, err := s.store.GetWorker(ctx, workerID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	return s.engine.MarkDirty(ctx, WorkerResourceType, workerID)
}

// ScheduleWorkerProviderReconciliation marks the provider's worker pool dirty.
func (s *ControlPlane) ScheduleWorkerProviderReconciliation(ctx context.Context, projectID, providerID string) error {
	if s.engine == nil {
		return errors.New("reconcile engine is required")
	}
	return s.engine.MarkDirty(ctx, WorkerProviderResourceType, WorkerProviderDirtyID(projectID, providerID))
}

// ScheduleWorkerProviderReconciliationAt marks the provider's worker pool
// dirty no earlier than scheduledAt (the timer form: "re-check the pool at T").
func (s *ControlPlane) ScheduleWorkerProviderReconciliationAt(ctx context.Context, projectID, providerID string, scheduledAt time.Time) error {
	if s.engine == nil {
		return errors.New("reconcile engine is required")
	}
	if scheduledAt.IsZero() {
		scheduledAt = time.Now()
	}
	return s.engine.MarkDirtyAt(ctx, WorkerProviderResourceType, WorkerProviderDirtyID(projectID, providerID), scheduledAt)
}
