package workers

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/obot-platform/discobox/orchestration"
	"github.com/obot-platform/discobox/server/internal/apperrors"
	"github.com/obot-platform/discobox/server/internal/auth"
	"github.com/obot-platform/discobox/server/internal/model"
	services "github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
	"github.com/obot-platform/discobox/worker-agent/workerauth"
)

type Service struct {
	store *store.Store
	jobs  WorkerReconcileJobManager
}

type WorkerReconcileJobManager interface {
	SubmitWorkerReconcile(context.Context, string) (*orchestration.Job, error)
}

func NewService(store *store.Store, jobs ...WorkerReconcileJobManager) *Service {
	svc := &Service{store: store}
	if len(jobs) > 0 {
		svc.jobs = jobs[0]
	}
	return svc
}

func (s *Service) RegisterWorker(ctx context.Context, input services.RegisterWorkerBody) (*services.RegisterWorkerResponseBody, error) {
	projectID := strings.TrimSpace(input.ProjectId)
	workerID := strings.TrimSpace(input.WorkerId.Or(""))
	if projectID == "" || strings.TrimSpace(input.BootstrapToken) == "" || strings.TrimSpace(input.PublicKey) == "" {
		return nil, fmt.Errorf("projectId, bootstrapToken, and publicKey are required")
	}
	if workerID == "" {
		sandboxID := strings.TrimSpace(input.SandboxId.Or(""))
		if sandboxID == "" {
			return nil, fmt.Errorf("workerId or sandboxId is required")
		}
		sandbox, err := s.store.GetSandbox(ctx, projectID, sandboxID)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) || !strings.HasPrefix(sandboxID, "worker-") {
				return nil, apiError(err, "sandbox not found")
			}
			workerID = strings.TrimPrefix(sandboxID, "worker-")
		} else {
			if sandbox.WorkerID == nil || strings.TrimSpace(*sandbox.WorkerID) == "" {
				return nil, apperrors.NewStatusError(http.StatusBadRequest, "sandbox does not have an assigned worker")
			}
			workerID = strings.TrimSpace(*sandbox.WorkerID)
		}
	}
	if workerID == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "workerId is required")
	}
	if worker, err := s.store.GetWorker(ctx, workerID); err != nil {
		return nil, apiError(err, "worker not found")
	} else if worker.ProjectID != projectID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "worker not found")
	}
	h := sha256.Sum256([]byte(input.BootstrapToken))
	_, err := s.store.RegisterWorker(ctx, workerID, h[:], input.PublicKey, defaultString(input.KeyType.Or(""), workerauth.KeyType))
	if err != nil {
		return nil, apiError(err, "worker bootstrap token not found")
	}
	return &services.RegisterWorkerResponseBody{}, nil
}

func (s *Service) ListWorkers(ctx context.Context, projectID, providerID string) ([]model.Worker, error) {
	projectID = strings.TrimSpace(projectID)
	providerID = strings.TrimSpace(providerID)
	if projectID == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "projectId is required")
	}
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apiError(err, "project not found")
	}
	if providerID != "" {
		if _, err := s.store.GetSandboxProviderInstance(ctx, projectID, providerID); err != nil {
			return nil, apiError(err, "provider instance not found")
		}
	}
	return s.store.ListWorkers(ctx, projectID, providerID)
}

func (s *Service) UpdateWorkerStatus(ctx context.Context, workerID string, input services.UpdateWorkerStatusBody) (*model.Worker, error) {
	workerID = strings.TrimSpace(workerID)
	if workerID == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "workerId is required")
	}
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok || principal.Type != auth.PrincipalTypeWorker || principal.WorkerID != workerID {
		return nil, apperrors.NewStatusError(http.StatusForbidden, "worker is not authorized to update this status")
	}
	worker, err := s.store.UpdateWorkerStatus(ctx, workerID, input.Ready, input.Schedulable, input.Degraded, input.AvailableCpuVcpus, input.AvailableMemoryBytes, input.AvailableStorageBytes, services.RawMessage(input.Conditions))
	if err != nil {
		return nil, apiError(err, "worker not found")
	}
	return worker, nil
}

func (s *Service) ReconcileWorker(ctx context.Context, projectID, workerID string) (*model.Worker, error) {
	if s.jobs == nil {
		return nil, fmt.Errorf("job manager is required")
	}
	projectID = strings.TrimSpace(projectID)
	workerID = strings.TrimSpace(workerID)
	if projectID == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "projectId is required")
	}
	if workerID == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "workerId is required")
	}
	worker, err := s.store.GetWorker(ctx, workerID)
	if err != nil {
		return nil, apiError(err, "worker not found")
	}
	if worker.ProjectID != projectID {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "worker not found")
	}
	if _, err := s.jobs.SubmitWorkerReconcile(ctx, workerID); err != nil {
		return nil, apiError(err, "worker not found")
	}
	worker, err = s.store.GetWorker(ctx, workerID)
	if err != nil {
		return nil, apiError(err, "worker not found")
	}
	return worker, nil
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func apiError(err error, notFoundMessage string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return apperrors.NewStatusError(http.StatusNotFound, notFoundMessage)
	}
	return err
}
