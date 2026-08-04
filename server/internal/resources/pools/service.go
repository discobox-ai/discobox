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
	sandboxReporter SandboxStateReporter
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
		ProjectID: projectID,
		PoolManifest: model.PoolManifest{
			Name:               name,
			ProviderInstanceID: provider.ID,
			CPUVCPUs:           input.CpuVcpus.Or(0),
			MemoryBytes:        input.MemoryBytes.Or(0),
			StorageBytes:       input.StorageBytes.Or(0),
		},
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

// SetDefaultPool points the project's default pool at poolID, so new sandboxes
// created without an explicit pool are scheduled into it.
func (s *Service) SetDefaultPool(ctx context.Context, projectID, poolID string) (*model.Project, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, mapAPIError(err, "project not found")
	}
	pool, err := s.store.GetPool(ctx, projectID, poolID)
	if err != nil {
		return nil, mapAPIError(err, "pool not found")
	}
	project.DefaultPoolID = pool.ID
	if err := s.store.UpsertProject(ctx, project); err != nil {
		return nil, err
	}
	return s.store.GetProject(ctx, projectID)
}

// UnsetDefaultPool clears the project's default pool when it currently points
// at poolID, leaving the project with no default. New sandboxes must then name
// a pool explicitly. Clearing a pool that is not the default is rejected so the
// intent is unambiguous.
func (s *Service) UnsetDefaultPool(ctx context.Context, projectID, poolID string) (*model.Project, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, mapAPIError(err, "project not found")
	}
	pool, err := s.store.GetPool(ctx, projectID, poolID)
	if err != nil {
		return nil, mapAPIError(err, "pool not found")
	}
	if project.DefaultPoolID != pool.ID {
		return nil, apperrors.NewStatusError(http.StatusConflict, "pool is not the project default")
	}
	project.DefaultPoolID = ""
	if err := s.store.UpsertProject(ctx, project); err != nil {
		return nil, err
	}
	return s.store.GetProject(ctx, projectID)
}

// DeletePool submits delete intent for an empty pool that is not the project
// default. A pool with sandboxes cannot be deleted (pool assignment is
// immutable, so there is nothing to drain to), and the default pool must first
// be unset or replaced so new sandboxes retain a scheduling target. The
// reconciler removes the runtime host, then the row.
func (s *Service) DeletePool(ctx context.Context, projectID, poolID string) error {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return mapAPIError(err, "project not found")
	}
	pool, err := s.store.GetPool(ctx, projectID, poolID)
	if err != nil {
		return mapAPIError(err, "pool not found")
	}
	if project.DefaultPoolID == pool.ID {
		return apperrors.NewStatusError(http.StatusConflict, "pool is the project default; set a different default or unset it before deleting")
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
