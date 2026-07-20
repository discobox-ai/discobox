// Package pools owns the Pool resource: the user-visible sharing boundary
// sandboxes are scheduled into. A pool binds to one provider instance at
// create time, immutably; capacity, cache, and worker sizing policy live on
// the pool, while the provider instance is backend identity only.
package pools

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/obot-platform/discobox/server/internal/apperrors"
	"github.com/obot-platform/discobox/server/internal/model"
	services "github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
)

type Service struct {
	store           *store.Store
	pools           *ControlPlane
	sandboxReporter SandboxRemovalReporter
}

func NewService(appStore *store.Store, controlPlane *ControlPlane) *Service {
	return &Service{store: appStore, pools: controlPlane}
}

func mapAPIError(err error, notFoundMessage string) error {
	if errors.Is(err, store.ErrNotFound) {
		return apperrors.NewStatusError(http.StatusNotFound, notFoundMessage)
	}
	return err
}

func (s *Service) ListPools(ctx context.Context, projectID string) ([]model.Pool, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, mapAPIError(err, "project not found")
	}
	return s.store.ListPools(ctx, projectID)
}

func (s *Service) CreatePool(ctx context.Context, projectID string, input services.CreatePoolBody) (*model.Pool, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, mapAPIError(err, "project not found")
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "pool name is required")
	}
	providerInstanceID := strings.TrimSpace(input.ProviderInstanceId)
	if providerInstanceID == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "pool provider instance is required")
	}
	provider, err := s.store.GetSandboxProviderInstance(ctx, projectID, providerInstanceID)
	if err != nil {
		return nil, mapAPIError(err, "provider instance not found")
	}
	pool := &model.Pool{
		ProjectID:          projectID,
		Name:               name,
		ProviderInstanceID: provider.ID,
		CPUVCPUs:           input.CpuVcpus.Or(0),
		MemoryBytes:        input.MemoryBytes.Or(0),
		StorageBytes:       input.StorageBytes.Or(0),
	}
	if err := s.store.CreatePool(ctx, pool); err != nil {
		return nil, err
	}
	if s.pools != nil {
		if err := s.pools.SchedulePoolReconciliation(ctx, projectID, pool.ID); err != nil {
			return nil, err
		}
	}
	return s.GetPool(ctx, projectID, pool.ID)
}

func (s *Service) GetPool(ctx context.Context, projectID, poolID string) (*model.Pool, error) {
	pool, err := s.store.GetPool(ctx, projectID, poolID)
	if err != nil {
		return nil, mapAPIError(err, "pool not found")
	}
	return pool, nil
}

func (s *Service) UpdatePool(ctx context.Context, projectID, poolID string, input services.UpdatePoolBody) (*model.Pool, error) {
	pool, err := s.store.GetPool(ctx, projectID, poolID)
	if err != nil {
		return nil, mapAPIError(err, "pool not found")
	}
	if name, ok := input.Name.Get(); ok {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "pool name is required")
		}
		pool.Name = name
	}
	if value, ok := input.CpuVcpus.Get(); ok {
		pool.CPUVCPUs = value
	}
	if value, ok := input.MemoryBytes.Get(); ok {
		pool.MemoryBytes = value
	}
	if value, ok := input.StorageBytes.Get(); ok {
		pool.StorageBytes = value
	}
	if err := s.store.UpdatePool(ctx, pool); err != nil {
		return nil, err
	}
	// Sizing policy may have changed; let the pool reconciler converge workers.
	if s.pools != nil {
		if err := s.pools.SchedulePoolReconciliation(ctx, projectID, pool.ID); err != nil {
			return nil, err
		}
	}
	return s.GetPool(ctx, projectID, poolID)
}

// DeletePool submits delete intent for an empty pool, including the built-in
// default pool. A pool with sandboxes cannot be deleted (pool assignment is
// immutable, so there is nothing to drain to). The reconciler removes the
// runtime host, then the row.
func (s *Service) DeletePool(ctx context.Context, projectID, poolID string) error {
	pool, err := s.store.GetPool(ctx, projectID, poolID)
	if err != nil {
		return mapAPIError(err, "pool not found")
	}
	sandboxCount, err := s.store.CountSandboxesForPool(ctx, projectID, pool.ID)
	if err != nil {
		return err
	}
	if sandboxCount > 0 {
		return apperrors.NewStatusError(http.StatusConflict, "pool has sandboxes")
	}
	if s.pools == nil {
		return fmt.Errorf("pool control plane is required")
	}
	if _, err := s.pools.SubmitPoolDelete(ctx, projectID, pool.ID); err != nil {
		return mapAPIError(err, "pool not found")
	}
	return nil
}
