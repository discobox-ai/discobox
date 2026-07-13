package sandboxes

import (
	"context"
	"errors"

	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/store"
	"gorm.io/gorm"
)

// This file owns sandbox lifecycle intent: each accepted command bumps the
// generation, records the operation, and marks the sandbox dirty on the
// reconcile engine — all in one transaction.

// createSandboxIntent persists a new sandbox with create intent and marks it
// dirty, atomically.
func (s *Service) createSandboxIntent(ctx context.Context, sandbox *model.Sandbox) (*model.Sandbox, error) {
	if s.engine == nil {
		return nil, errors.New("reconcile engine is required")
	}
	if err := s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		sandbox.IncrementGeneration()
		sandbox.BeginOperation(model.SandboxCreateOperation)
		if err := txStore.CreateSandbox(ctx, sandbox); err != nil {
			return err
		}
		return s.engine.MarkDirtyTx(ctx, txDB, SandboxResourceType, SandboxDirtyID(sandbox.ProjectID, sandbox.ID))
	}); err != nil {
		return nil, err
	}
	return s.store.GetSandbox(ctx, sandbox.ProjectID, sandbox.ID)
}

// submitSandboxOperation records lifecycle intent on an existing sandbox
// (generation bump + operation) and marks it dirty, atomically.
func (s *Service) submitSandboxOperation(ctx context.Context, projectID, sandboxID string, operation model.OperationSpec, mutate ...func(*model.Sandbox)) (*model.Sandbox, error) {
	if s.engine == nil {
		return nil, errors.New("reconcile engine is required")
	}
	if err := s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		sandbox, err := txStore.GetSandbox(ctx, projectID, sandboxID)
		if err != nil {
			return err
		}
		previousGeneration := sandbox.Generation
		sandbox.IncrementGeneration()
		sandbox.BeginOperation(operation)
		for _, fn := range mutate {
			fn(sandbox)
		}
		if err := txStore.UpdateSandbox(ctx, sandbox, store.WithGeneration(previousGeneration)); err != nil {
			return err
		}
		return s.engine.MarkDirtyTx(ctx, txDB, SandboxResourceType, SandboxDirtyID(projectID, sandboxID))
	}); err != nil {
		return nil, err
	}
	return s.store.GetSandbox(ctx, projectID, sandboxID)
}

// scheduleSandboxReconcile marks the sandbox dirty (drift-driven reconcile, no
// intent change).
func (s *Service) scheduleSandboxReconcile(ctx context.Context, projectID, sandboxID string) (*model.Sandbox, error) {
	if s.engine == nil {
		return nil, errors.New("reconcile engine is required")
	}
	sandbox, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	if err := s.engine.MarkDirty(ctx, SandboxResourceType, SandboxDirtyID(projectID, sandboxID)); err != nil {
		return nil, err
	}
	return sandbox, nil
}
