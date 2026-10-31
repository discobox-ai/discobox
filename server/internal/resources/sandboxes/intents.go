package sandboxes

import (
	"context"
	"errors"

	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

// This file owns sandbox intent: each accepted command bumps the generation,
// records the desired state, and marks the sandbox dirty on the reconcile
// engine — all in one transaction.
//
// Each returns the sandbox as read inside its own transaction, never re-read
// after the commit. The dirty mark wakes the engine the moment the transaction
// commits, and a reconcile that finishes first — a delete drops the row — would
// turn an accepted intent into a not-found (issue #204).
//
// Intent here means existence and spec only. Start, stop, and restart are not
// intent and do not pass through this file: they are instructions forwarded to
// the runtime, which reports what became of them (ADR 0017 §9).

// createSandboxIntent persists a new sandbox with create intent, the inline
// secrets and bindings it is created with, its sentinels materialized from every binding covering
// it, and the dirty mark, atomically. The sentinels must commit with the
// sandbox row: the dirty mark wakes the reconciler on commit, and a
// reconciler that can observe the sandbox without them launches it with no
// secrets.
func (s *Service) createSandboxIntent(ctx context.Context, sandbox *model.Sandbox, inlineSecrets []*model.Secret, own []*model.SecretBinding, prepared resourcesecrets.SandboxBindings) (*model.Sandbox, error) {
	if s.engine == nil {
		return nil, errors.New("reconcile engine is required")
	}
	var created *model.Sandbox
	if err := s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		sandbox.IncrementGeneration()
		sandbox.RecordIntent(model.DesiredStatePresent)
		if err := txStore.CreateSandbox(ctx, sandbox); err != nil {
			return err
		}
		// An inline value is stored with the sandbox it was given to, so a
		// create that fails leaves none behind.
		for _, secret := range inlineSecrets {
			if err := txStore.CreateSecret(ctx, secret); err != nil {
				return err
			}
		}
		for _, binding := range own {
			if err := txStore.CreateSecretBinding(ctx, binding, store.BindingChange{Actor: binding.CreatedBy}); err != nil {
				return err
			}
		}
		// The bindings a create gives the new discobox, stored with it or not
		// at all (ADR 0140 §4) — and, given by a discobox, only while the
		// delegate bindings they are handed on under still cover it (ADR
		// 26-10-27-880 §3).
		if err := resourcesecrets.StoreSandboxBindingsTx(ctx, txStore, prepared); err != nil {
			return err
		}
		if _, err := materializeSandboxSecrets(ctx, txStore, sandbox); err != nil {
			return err
		}
		if err := s.engine.MarkDirtyTx(ctx, txDB, SandboxResourceType, SandboxDirtyID(sandbox.ProjectID, sandbox.ID)); err != nil {
			return err
		}
		var err error
		created, err = txStore.GetSandbox(ctx, sandbox.ProjectID, sandbox.ID)
		return err
	}); err != nil {
		return nil, err
	}
	return created, nil
}

// recordSandboxIntent records intent on an existing sandbox (generation bump +
// desired state) and marks it dirty, atomically. A spec edit passes the desired
// state it already has: the generation is what versions the whole spec, so a
// re-pin or a source change is the same kind of intent as a delete.
func (s *Service) recordSandboxIntent(ctx context.Context, projectID, sandboxID, desiredState string, mutate ...func(*model.Sandbox)) (*model.Sandbox, error) {
	if s.engine == nil {
		return nil, errors.New("reconcile engine is required")
	}
	var recorded *model.Sandbox
	if err := s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		sandbox, err := txStore.GetSandbox(ctx, projectID, sandboxID)
		if err != nil {
			return err
		}
		previousGeneration := sandbox.Generation
		sandbox.IncrementGeneration()
		sandbox.RecordIntent(desiredState)
		if desiredState != model.DesiredStatePresent {
			sandbox.PendingHarnessChange = nil
			sandbox.PendingHarnessConfigID = nil
		}
		for _, fn := range mutate {
			fn(sandbox)
		}
		if err := txStore.UpdateSandbox(ctx, sandbox, store.WithGeneration(previousGeneration)); err != nil {
			return err
		}
		if err := s.engine.MarkDirtyTx(ctx, txDB, SandboxResourceType, SandboxDirtyID(projectID, sandboxID)); err != nil {
			return err
		}
		// An archive or a delete is an outside power instruction too: the
		// sandbox will not come back on a wake it set before (ADR 26-10-16-337
		// §6).
		if desiredState != model.DesiredStatePresent {
			if err := txStore.CancelSandboxWakeTx(ctx, txDB, projectID, sandboxID); err != nil {
				return err
			}
			if err := s.engine.MarkDirtyTx(ctx, txDB, SandboxPowerResourceType, SandboxDirtyID(projectID, sandboxID)); err != nil {
				return err
			}
		}
		recorded, err = txStore.GetSandbox(ctx, projectID, sandboxID)
		return err
	}); err != nil {
		return nil, err
	}
	return recorded, nil
}

// updateSandboxMetadata persists a mutation to an existing sandbox that
// carries no lifecycle intent — client-reported bookkeeping only, such as
// source metadata updates. Unlike recordSandboxIntent
// it does not bump the generation or mark the sandbox dirty for
// reconciliation: nothing about desired or observed runtime state
// changed, so there is nothing for the reconcile engine to act on.
func (s *Service) updateSandboxMetadata(ctx context.Context, projectID, sandboxID string, mutate func(*model.Sandbox)) (*model.Sandbox, error) {
	var updated *model.Sandbox
	if err := s.store.Transaction(ctx, func(txStore *store.Store, _ *gorm.DB) error {
		sandbox, err := txStore.GetSandbox(ctx, projectID, sandboxID)
		if err != nil {
			return err
		}
		previousGeneration := sandbox.Generation
		mutate(sandbox)
		if err := txStore.UpdateSandbox(ctx, sandbox, store.WithGeneration(previousGeneration)); err != nil {
			return err
		}
		updated, err = txStore.GetSandbox(ctx, projectID, sandboxID)
		return err
	}); err != nil {
		return nil, err
	}
	return updated, nil
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
