package pools

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/obot-platform/discobox/pool-agent/poolauth"
	"github.com/obot-platform/discobox/server/internal/apperrors"
	"github.com/obot-platform/discobox/server/internal/auth"
	"github.com/obot-platform/discobox/server/internal/model"
	services "github.com/obot-platform/discobox/server/internal/services"
)

// SandboxRemovalReporter records agent-observed sandbox runtime loss as
// lifecycle intent owned by the sandbox control plane.
type SandboxRemovalReporter interface {
	ReportSandboxRemoved(ctx context.Context, poolID, sandboxID, containerID string) error
}

// SetSandboxRemovalReporter wires the sandbox service dependency for
// agent-reported sandbox removals.
func (s *Service) SetSandboxRemovalReporter(reporter SandboxRemovalReporter) {
	s.sandboxReporter = reporter
}

// RegisterPool redeems a bootstrap token from a starting pool agent and
// records its public key.
func (s *Service) RegisterPool(ctx context.Context, input services.RegisterPoolBody) (*services.RegisterPoolResponseBody, error) {
	projectID := strings.TrimSpace(input.ProjectId)
	poolID := strings.TrimSpace(input.PoolId)
	if projectID == "" || poolID == "" || strings.TrimSpace(input.BootstrapToken) == "" || strings.TrimSpace(input.PublicKey) == "" {
		return nil, fmt.Errorf("projectId, poolId, bootstrapToken, and publicKey are required")
	}
	pool, err := s.store.GetPool(ctx, projectID, poolID)
	if err != nil {
		return nil, mapAPIError(err, "pool not found")
	}
	h := sha256.Sum256([]byte(input.BootstrapToken))
	if _, err := s.store.RegisterPool(ctx, pool.ID, h[:], input.PublicKey, defaultString(input.KeyType.Or(""), poolauth.KeyType)); err != nil {
		return nil, mapAPIError(err, "pool bootstrap token not found")
	}
	return &services.RegisterPoolResponseBody{}, nil
}

// UpdatePoolStatus records an agent heartbeat. Only the authenticated pool
// principal for the same pool may report.
func (s *Service) UpdatePoolStatus(ctx context.Context, poolID string, input services.UpdatePoolStatusBody) (*model.Pool, error) {
	poolID = strings.TrimSpace(poolID)
	if poolID == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "poolId is required")
	}
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok || principal.Type != auth.PrincipalTypePool || principal.PoolID != poolID {
		return nil, apperrors.NewStatusError(http.StatusForbidden, "pool agent is not authorized to update this status")
	}
	pool, err := s.store.UpdatePoolStatus(ctx, poolID, input.Ready, input.Schedulable, input.Degraded, input.AvailableCpuVcpus, input.AvailableMemoryBytes, input.AvailableStorageBytes, services.RawMessage(input.Conditions))
	if err != nil {
		return nil, mapAPIError(err, "pool not found")
	}
	return pool, nil
}

// ReportPoolSandboxRemoved records an agent-observed sandbox runtime loss.
func (s *Service) ReportPoolSandboxRemoved(ctx context.Context, poolID string, input services.ReportPoolSandboxRemovedBody) error {
	poolID = strings.TrimSpace(poolID)
	sandboxID := strings.TrimSpace(input.SandboxId)
	if poolID == "" || sandboxID == "" {
		return apperrors.NewStatusError(http.StatusBadRequest, "poolId and sandboxId are required")
	}
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok || principal.Type != auth.PrincipalTypePool || principal.PoolID != poolID {
		return apperrors.NewStatusError(http.StatusForbidden, "pool agent is not authorized to report sandbox removal")
	}
	if s.sandboxReporter == nil {
		return errors.New("sandbox removal reporter is required")
	}
	if err := s.sandboxReporter.ReportSandboxRemoved(ctx, poolID, sandboxID, strings.TrimSpace(input.ContainerId.Or(""))); err != nil {
		return mapAPIError(err, "sandbox not found")
	}
	return nil
}

// ReconcilePool marks the pool dirty for a manual reconcile request.
func (s *Service) ReconcilePool(ctx context.Context, projectID, poolID string) (*model.Pool, error) {
	if s.pools == nil {
		return nil, fmt.Errorf("pool control plane is required")
	}
	pool, err := s.store.GetPool(ctx, strings.TrimSpace(projectID), strings.TrimSpace(poolID))
	if err != nil {
		return nil, mapAPIError(err, "pool not found")
	}
	if err := s.pools.SchedulePoolReconciliation(ctx, pool.ProjectID, pool.ID); err != nil {
		return nil, err
	}
	return s.store.GetPool(ctx, pool.ProjectID, pool.ID)
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
