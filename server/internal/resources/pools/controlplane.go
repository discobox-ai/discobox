package pools

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"time"

	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

// ControlPlane is the TRUSTED pool surface: it owns pool lifecycle intent
// (generation bumps + dirty marks, atomically) and implements
// sandbox.PoolManager, the narrow interface handed to provider drivers.
// Unlike pools.Service (the HTTP-facing API surface, which validates
// untrusted input and speaks apperrors), the control plane takes ids from
// persisted rows at face value and returns plain domain errors.
type ControlPlane struct {
	store     *store.Store
	engine    *reconcile.Engine
	agentAuth *poolagentauth.Manager
}

func NewControlPlane(appStore *store.Store, engine *reconcile.Engine) *ControlPlane {
	return &ControlPlane{store: appStore, engine: engine}
}

func (s *ControlPlane) SetAgentAuthManager(manager *poolagentauth.Manager) {
	s.agentAuth = manager
}

// RegisterJobs installs the pool reconciler on the level-triggered reconcile
// engine.
func (s *ControlPlane) RegisterJobs(providerManager *sandbox.ProviderManager) error {
	if s.engine == nil {
		return errors.New("reconcile engine is required")
	}
	return s.engine.Register(PoolResourceType, NewPoolReconciler(s.store, providerManager, s))
}

func (s *ControlPlane) GetPool(ctx context.Context, projectID, poolID string) (*model.Pool, error) {
	return s.store.GetPool(ctx, projectID, poolID)
}

func (s *ControlPlane) ListPoolsForProviderInstance(ctx context.Context, projectID, providerID string) ([]model.Pool, error) {
	return s.store.ListPoolsForProviderInstance(ctx, projectID, providerID)
}

func (s *ControlPlane) ListPools(ctx context.Context, projectID string) ([]model.Pool, error) {
	return s.store.ListPools(ctx, projectID)
}

func (s *ControlPlane) SchedulablePoolForSandbox(ctx context.Context, sb *model.Sandbox) (*model.Pool, error) {
	return s.store.SchedulablePoolForSandbox(ctx, sb)
}

func (s *ControlPlane) GetProject(ctx context.Context, projectID string) (*model.Project, error) {
	return s.store.GetProject(ctx, projectID)
}

func (s *ControlPlane) GetSandboxProviderInstance(ctx context.Context, projectID, providerID string) (*model.SandboxProviderInstance, error) {
	return s.store.GetSandboxProviderInstance(ctx, projectID, providerID)
}

func (s *ControlPlane) CountSandboxesForPool(ctx context.Context, projectID, poolID string) (int64, error) {
	return s.store.CountSandboxesForPool(ctx, projectID, poolID)
}

func (s *ControlPlane) CreatePoolBootstrapToken(ctx context.Context, token *model.PoolBootstrapToken) error {
	return s.store.CreatePoolBootstrapToken(ctx, token)
}

func (s *ControlPlane) EnsureAgentTrustKey(ctx context.Context) (string, error) {
	if s.agentAuth == nil {
		return "", nil
	}
	return s.agentAuth.EnsureTrustKey(ctx)
}

func (s *ControlPlane) CreateAgentToken(ctx context.Context, claims poolagentauth.TokenClaims) (string, error) {
	if s.agentAuth == nil {
		return "", nil
	}
	return s.agentAuth.CreateToken(ctx, claims)
}

func (s *ControlPlane) CreateSandboxAgentToken(ctx context.Context, claims poolagentauth.TokenClaims) (string, error) {
	if s.agentAuth == nil {
		return "", nil
	}
	return s.agentAuth.CreateSandboxAgentToken(ctx, claims)
}

// SchedulePoolReconciliation marks the pool dirty (drift-driven reconcile, no
// intent change).
//
// Drift-driven is why this does not shorten a failure backoff. Every caller
// here is reacting to a pool that would not serve them -- an agent client that
// would not connect, a scheduler that found no capacity -- so they all fire
// exactly when the pool is already failing, and there are as many of them as
// there are sandboxes on it. Letting each one cancel the backoff turns one
// broken pool into a retry loop that runs as fast as the failure returns. What
// they want is for the pool to be reconciled, not for it to be reconciled now;
// new intent (SchedulePoolRepair, SubmitPoolDelete) is what means now.
func (s *ControlPlane) SchedulePoolReconciliation(ctx context.Context, projectID, poolID string) error {
	if s.engine == nil {
		return errors.New("reconcile engine is required")
	}
	return s.engine.MarkDirtyDrift(ctx, PoolResourceType, PoolDirtyID(projectID, poolID))
}

// A pool's timer form is deliberately absent. "Re-check this pool at T" is the
// reconciler's own business and belongs in the Result it returns, where the
// engine can arm it on the row it already holds; routing it back through a mark
// is how a reconcile ends up marking itself (reconcile.ErrSelfMark).

// SchedulePoolRepair re-drives a failed pool as NEW INTENT: it bumps the
// generation and marks the pool dirty in one transaction.
//
// The generation bump is what makes a queued retry observable on the row:
// with it, a pending repair reads as ObservedGeneration < Generation until the
// reconciler attempts it, so schedulers can tell a pending retry from a
// settled failure. Repair is idempotent: a pool whose latest generation has
// not been attempted yet is only marked dirty.
func (s *ControlPlane) SchedulePoolRepair(ctx context.Context, poolID, reason string) error {
	if s.engine == nil {
		return errors.New("reconcile engine is required")
	}
	return s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		pool, err := txStore.GetPoolByID(ctx, poolID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		// A settled failure needs new intent to get another attempt: the
		// reconciler already gave up on the recorded generation, so a bare
		// dirty mark would be re-converged as "still failed" (ADR 0017 §4).
		if pool.ErrorMessage != nil && pool.Converged() {
			previousGeneration := pool.Generation
			pool.IncrementGeneration()
			pool.RecordIntent(pool.DesiredState)
			slog.InfoContext(ctx, "re-driving a settled pool failure", "poolId", pool.ID, "reason", reason)
			if err := txStore.UpdatePoolWithGeneration(ctx, pool, previousGeneration); err != nil {
				if errors.Is(err, store.ErrGenerationConflict) {
					return nil // concurrent intent already re-drove the pool
				}
				return err
			}
		}
		return s.engine.MarkDirtyTx(ctx, txDB, PoolResourceType, PoolDirtyID(pool.ProjectID, pool.ID))
	})
}

// SubmitPoolDelete records delete intent for the pool: generation bump,
// delete operation, and dirty mark, atomically. The reconciler removes the
// runtime and then the row.
func (s *ControlPlane) SubmitPoolDelete(ctx context.Context, projectID, poolID string) (*model.Pool, error) {
	if s.engine == nil {
		return nil, errors.New("reconcile engine is required")
	}
	if err := s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		pool, err := txStore.GetPool(ctx, projectID, poolID)
		if err != nil {
			return err
		}
		if pool.DesiredState == model.DesiredStateDeleted {
			return s.engine.MarkDirtyTx(ctx, txDB, PoolResourceType, PoolDirtyID(pool.ProjectID, pool.ID))
		}
		previousGeneration := pool.Generation
		pool.IncrementGeneration()
		pool.RecordIntent(model.DesiredStateDeleted)
		if err := txStore.UpdatePoolWithGeneration(ctx, pool, previousGeneration); err != nil {
			return err
		}
		return s.engine.MarkDirtyTx(ctx, txDB, PoolResourceType, PoolDirtyID(pool.ProjectID, pool.ID))
	}); err != nil {
		return nil, err
	}
	return s.store.GetPool(ctx, projectID, poolID)
}

const bootstrapTokenPurgeInterval = time.Hour

// StartBootstrapTokenCleanup bounds the bootstrap token table: tokens are
// single-use and minted per runtime creation; once expired, used, or revoked,
// nothing reads them again, and nothing else deletes them.
func (s *ControlPlane) StartBootstrapTokenCleanup(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(bootstrapTokenPurgeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				n, err := s.store.PurgeSpentPoolBootstrapTokens(ctx, time.Now())
				if err != nil {
					log.Printf("pool bootstrap token cleanup: %v", err)
				} else if n > 0 {
					log.Printf("pool bootstrap token cleanup: purged %d spent token(s)", n)
				}
			}
		}
	}()
}
