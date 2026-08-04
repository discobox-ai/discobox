package sandboxes

import (
	"context"
	"errors"

	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/store"
	"gorm.io/gorm"
)

// This file owns sandbox intent: each accepted command bumps the generation,
// records the desired state, and marks the sandbox dirty on the reconcile
// engine — all in one transaction.
//
// Intent here means existence and spec only. Start, stop, and restart are not
// intent and do not pass through this file: they are instructions forwarded to
// the runtime, which reports what became of them (ADR 0017 §9).

// createSandboxIntent persists a new sandbox with create intent and marks it
// dirty, atomically.
func (s *Service) createSandboxIntent(ctx context.Context, sandbox *model.Sandbox) (*model.Sandbox, error) {
	if s.engine == nil {
		return nil, errors.New("reconcile engine is required")
	}
	if err := s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		sandbox.IncrementGeneration()
		sandbox.RecordIntent(model.DesiredStatePresent)
		if err := txStore.CreateSandbox(ctx, sandbox); err != nil {
			return err
		}
		return s.engine.MarkDirtyTx(ctx, txDB, SandboxResourceType, SandboxDirtyID(sandbox.ProjectID, sandbox.ID))
	}); err != nil {
		return nil, err
	}
	return s.store.GetSandbox(ctx, sandbox.ProjectID, sandbox.ID)
}

// recordSandboxIntent records intent on an existing sandbox (generation bump +
// desired state) and marks it dirty, atomically. A spec edit passes the desired
// state it already has: the generation is what versions the whole spec, so a
// re-pin or a source change is the same kind of intent as a delete.
func (s *Service) recordSandboxIntent(ctx context.Context, projectID, sandboxID, desiredState string, mutate ...func(*model.Sandbox)) (*model.Sandbox, error) {
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
		sandbox.RecordIntent(desiredState)
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

// updateSandboxMetadata persists a mutation to an existing sandbox that
// carries no lifecycle intent — client-reported bookkeeping only, such as
// CompleteSandboxApply's applied-commit record. Unlike submitSandboxOperation
// it does not bump the generation, record an operation, or mark the sandbox
// dirty for reconciliation: nothing about desired or observed runtime state
// changed, so there is nothing for the reconcile engine to act on.
func (s *Service) updateSandboxMetadata(ctx context.Context, projectID, sandboxID string, mutate func(*model.Sandbox)) (*model.Sandbox, error) {
	if err := s.store.Transaction(ctx, func(txStore *store.Store, _ *gorm.DB) error {
		sandbox, err := txStore.GetSandbox(ctx, projectID, sandboxID)
		if err != nil {
			return err
		}
		previousGeneration := sandbox.Generation
		mutate(sandbox)
		return txStore.UpdateSandbox(ctx, sandbox, store.WithGeneration(previousGeneration))
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
