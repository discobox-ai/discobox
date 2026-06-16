package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/obot-platform/discobox/apperrors"

	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/server/internal/api"
	"github.com/obot-platform/discobox/server/internal/auth"
)

func (s *Service) RegisterWorker(ctx context.Context, input api.RegisterWorkerBody) (*api.RegisterWorkerResponseBody, error) {
	projectID := strings.TrimSpace(input.ProjectId)
	sandboxID := strings.TrimSpace(input.SandboxId)
	if projectID == "" || sandboxID == "" || strings.TrimSpace(input.BootstrapToken) == "" || strings.TrimSpace(input.PublicKey) == "" {
		return nil, fmt.Errorf("projectId, sandboxId, bootstrapToken, and publicKey are required")
	}
	sandbox, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, apiError(err, "sandbox not found")
	}
	if sandbox.WorkerID == nil || strings.TrimSpace(*sandbox.WorkerID) == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "sandbox does not have an assigned worker")
	}
	workerID := strings.TrimSpace(*sandbox.WorkerID)
	h := sha256.Sum256([]byte(input.BootstrapToken))
	authToken, err := randomToken()
	if err != nil {
		return nil, err
	}
	authHash := sha256.Sum256([]byte(authToken))
	_, err = s.store.RegisterWorker(ctx, workerID, h[:], input.PublicKey, serviceDefaultString(input.KeyType.Or(""), "ed25519"), authHash[:], time.Now().UTC().Add(time.Hour))
	if err != nil {
		return nil, apiError(err, "worker bootstrap token not found")
	}
	return &api.RegisterWorkerResponseBody{AuthToken: authToken}, nil
}

func (s *Service) UpdateWorkerStatus(ctx context.Context, workerID string, input api.UpdateWorkerStatusBody) (*model.Worker, error) {
	workerID = strings.TrimSpace(workerID)
	if workerID == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "workerId is required")
	}
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok || principal.Type != auth.PrincipalTypeWorker || principal.WorkerID != workerID {
		return nil, apperrors.NewStatusError(http.StatusForbidden, "worker is not authorized to update this status")
	}
	worker, err := s.store.UpdateWorkerStatus(ctx, workerID, input.Ready, input.Schedulable, input.Degraded, input.AvailableCpuVcpus, input.AvailableMemoryBytes, input.AvailableStorageBytes, api.RawMessage(input.Conditions))
	if err != nil {
		return nil, apiError(err, "worker not found")
	}
	return worker, nil
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func serviceDefaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
