package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/obot-platform/disco2/internal/api"
	"github.com/obot-platform/disco2/internal/model"
	"github.com/obot-platform/disco2/internal/tenantctx"
)

func (s *Service) RegisterWorker(ctx context.Context, input api.RegisterWorkerBody) (*api.RegisterWorkerResponseBody, error) {
	if strings.TrimSpace(input.WorkerID) == "" || strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.BootstrapToken) == "" || strings.TrimSpace(input.PublicKey) == "" {
		return nil, fmt.Errorf("tenantId, workerId, bootstrapToken, and publicKey are required")
	}
	ctx = tenantctx.WithTenantID(ctx, input.TenantID)
	h := sha256.Sum256([]byte(input.BootstrapToken))
	authToken, err := randomToken()
	if err != nil {
		return nil, err
	}
	authHash := sha256.Sum256([]byte(authToken))
	_, err = s.store.RegisterWorker(ctx, input.TenantID, input.WorkerID, h[:], input.PublicKey, serviceDefaultString(input.KeyType, "ed25519"), authHash[:], time.Now().UTC().Add(time.Hour))
	if err != nil {
		return nil, apiError(err, "worker bootstrap token not found")
	}
	return &api.RegisterWorkerResponseBody{AuthToken: authToken}, nil
}

func (s *Service) UpdateWorkerStatus(ctx context.Context, authorization string, input api.UpdateWorkerStatusBody) (*model.Worker, error) {
	token := bearerToken(authorization)
	if token == "" {
		return nil, huma.Error401Unauthorized("worker auth token required")
	}
	if strings.TrimSpace(input.TenantID) == "" || strings.TrimSpace(input.WorkerID) == "" {
		return nil, huma.Error400BadRequest("tenantId and workerId are required")
	}
	ctx = tenantctx.WithTenantID(ctx, input.TenantID)
	h := sha256.Sum256([]byte(token))
	if err := s.store.ValidateWorkerAuthToken(ctx, input.TenantID, input.WorkerID, h[:]); err != nil {
		return nil, huma.Error401Unauthorized("invalid worker auth token")
	}
	worker, err := s.store.UpdateWorkerStatus(ctx, input.TenantID, input.WorkerID, input.Ready, input.Schedulable, input.Degraded, input.AvailableCPUVCPUs, input.AvailableMemoryBytes, input.AvailableStorageBytes, input.Conditions)
	if err != nil {
		return nil, apiError(err, "worker not found")
	}
	if worker.ProviderInstanceID != "" {
		if project, projectErr := s.store.GetProject(ctx, worker.ProjectID); projectErr == nil {
			if provider, providerErr := s.store.GetSandboxProviderInstance(ctx, worker.ProjectID, worker.ProviderInstanceID); providerErr == nil {
				_ = s.ensureSandboxProviderInstance(ctx, project, provider)
			}
		}
	}
	return worker, nil
}

func bearerToken(authorization string) string {
	authorization = strings.TrimSpace(authorization)
	if authorization == "" {
		return ""
	}
	parts := strings.Fields(authorization)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return strings.TrimSpace(parts[1])
	}
	return ""
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
