package sandboxes

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

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

// ReportSandboxRemoved converts the loss of the runtime a sandbox is currently
// being served by into stopped intent.
//
// A sandbox whose container dies unexpectedly should stop trying, not be silently
// resurrected: the user asked for a sandbox, that sandbox is gone, and saying so
// beats rebuilding something underneath them. So this does write intent — but only
// when the report is provably about the runtime the control plane believes in, and
// only when nothing else is mid-flight on it. Two guards, each covering the other's
// gap (ADR 0016 §8):
//
//   - **An operation is in flight.** The control plane is manipulating this
//     sandbox's container right now, so a container disappearing is expected —
//     an image upgrade removes the old one to build the new one. Writing intent
//     here would bump the generation and supersede the very operation that caused
//     the report, which is exactly how an upgrade used to end with the sandbox
//     stopped and the freshly built container torn down.
//   - **The report names a container we no longer believe in.** Once the
//     operation completes, the recorded runtime is the new container, so a report
//     about the old one arriving late is stale by identity rather than by timing.
//
// An empty ContainerID skips the second guard: it comes from the level-triggered
// orphan sweep, which reports a sandbox with no live container at all rather than
// a specific removal, and which is already grace-delayed well past any replacement.
//
// Duplicate reports, reports for a sandbox that has moved pools, and reports
// superseded by stop or delete intent are harmless no-ops.
func (s *Service) ReportSandboxRemoved(ctx context.Context, poolID, sandboxID, containerID string) error {
	if s.engine == nil {
		return errors.New("reconcile engine is required")
	}
	return s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		pool, err := txStore.GetPoolByID(ctx, poolID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			return err
		}
		sandbox, err := txStore.GetSandbox(ctx, pool.ProjectID, sandboxID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			return err
		}
		if sandbox.PoolID != poolID ||
			sandbox.DesiredState == model.SandboxDesiredStateStopped ||
			sandbox.DesiredState == model.SandboxDesiredStateDeleted {
			return nil
		}
		if sandbox.LastOperationStatus == model.SandboxOperationStatusRunning {
			return nil
		}
		if !reportedRuntimeIsCurrent(sandbox, containerID) {
			return nil
		}
		previousGeneration := sandbox.Generation
		sandbox.IncrementGeneration()
		sandbox.BeginOperation(model.SandboxStopOperation)
		if err := txStore.UpdateSandbox(ctx, sandbox, store.WithGeneration(previousGeneration)); err != nil {
			return err
		}
		return s.engine.MarkDirtyTx(ctx, txDB, SandboxResourceType, SandboxDirtyID(sandbox.ProjectID, sandbox.ID))
	})
}

// reportedRuntimeIsCurrent reports whether a removal names the runtime the
// control plane currently believes is serving the sandbox.
//
// Unknown answers "yes": an empty report (the orphan sweep), a sandbox with no
// recorded runtime yet, or runtime state that will not decode are all cases where
// identity cannot rule the report out, and the in-flight guard is what protects a
// live operation. Only a decodable mismatch is treated as stale.
func reportedRuntimeIsCurrent(sandbox *model.Sandbox, containerID string) bool {
	containerID = strings.TrimSpace(containerID)
	if containerID == "" || len(sandbox.RuntimeState) == 0 {
		return true
	}
	var runtime struct {
		ID string `json:"ID"`
	}
	if err := json.Unmarshal(sandbox.RuntimeState, &runtime); err != nil {
		return true
	}
	current := strings.TrimSpace(runtime.ID)
	return current == "" || current == containerID
}
