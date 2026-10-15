package sandboxes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/proxy/egress"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// This file owns a sandbox's effective egress policy (ADR 26-10-01-032): the
// merge of its project's, pool's, harness config's, and its own levels, which
// is computed whenever it is needed and never stored, so there is no copy of
// it to go stale.
//
// It reaches the pool two ways. A create carries it, so a new container is
// restricted from its first request. Every change after that is delivered by
// its own reconcile (SandboxEgressResourceType), which pushes the policy alone
// through the pool's update route: it retries with the engine's backoff while a
// pool is unreachable, records nothing on the sandbox, and reaches a sandbox
// whatever its lifecycle is doing — a settled failure included. The sandbox
// reconciler marks it after every create, so a create that merged its policy
// before a change committed cannot leave the old one in force.

// SandboxEgressResourceType is the reconcile-engine resource type that
// delivers a sandbox's egress policy to its pool. Its IDs are SandboxDirtyID's.
const SandboxEgressResourceType = "sandbox-egress"

// sandboxEgressReconciler pushes a sandbox's merged policy to its pool.
type sandboxEgressReconciler struct {
	store     *store.Store
	providers *ProviderManager
}

func (r *sandboxEgressReconciler) Reconcile(ctx context.Context, id string) (reconcile.Result, error) {
	projectID, sandboxID, err := splitSandboxDirtyID(id)
	if err != nil {
		return reconcile.Result{}, err
	}
	sb, err := r.store.GetSandbox(ctx, projectID, sandboxID)
	if errors.Is(err, store.ErrNotFound) {
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, err
	}
	// An archived or deleted sandbox has no policy at its pool: archive takes
	// it away with the container, and the create that brings it back carries
	// the policy as it then stands.
	if sb.DesiredState != model.DesiredStatePresent {
		return reconcile.Result{}, nil
	}
	policy, err := effectiveEgress(ctx, r.store, sb)
	if err != nil {
		return reconcile.Result{}, err
	}
	provider, err := r.providers.ResolveForSandbox(ctx, sb)
	if err != nil {
		return reconcile.Result{}, err
	}
	_, _, err = provider.Update(ctx, sandboxRefFromSandbox(sb), sb.ProviderState, UpdateOptions{Egress: &policy})
	if errors.Is(err, ErrNotFound) {
		// Never built, so nothing at the pool knows it yet: its create carries
		// the policy, and marks this again once it has run.
		return reconcile.Result{}, nil
	}
	return reconcile.Result{}, err
}

// MarkEgressChangedTx marks the egress delivery of every sandbox of projectID
// meant to exist — narrowed to one pool, one harness config, or both when they
// are given — in the transaction that changed an egress level
// (ADR 26-10-01-032 §5). A sandbox archived since is not marked: its next
// create carries the policy anyway.
func (s *Service) MarkEgressChangedTx(ctx context.Context, txStore *store.Store, txDB *gorm.DB, projectID, poolID, harnessConfigID string) error {
	if s.engine == nil {
		return errors.New("reconcile engine is required")
	}
	ids, err := txStore.ListPresentSandboxIDs(ctx, projectID, poolID, harnessConfigID)
	if err != nil {
		return err
	}
	for _, sandboxID := range ids {
		if err := s.engine.MarkDirtyTx(ctx, txDB, SandboxEgressResourceType, SandboxDirtyID(projectID, sandboxID)); err != nil {
			return err
		}
	}
	return nil
}

// effectiveEgress merges the levels of sb as they stand now.
func effectiveEgress(ctx context.Context, st *store.Store, sb *model.Sandbox) (egress.Policy, error) {
	harnessConfigID := ""
	if sb.HarnessConfigID != nil {
		harnessConfigID = strings.TrimSpace(*sb.HarnessConfigID)
	}
	return mergeEgressLevels(ctx, st, sb.ProjectID, sb.PoolID, harnessConfigID, sb.Egress)
}

// mergeEgressLevels is §2's rule over a project, a pool, a harness config
// (with the hosts its image declares), and a sandbox's own level. A harness
// config that is gone contributes nothing.
func mergeEgressLevels(ctx context.Context, st *store.Store, projectID, poolID, harnessConfigID string, own egress.Policy) (egress.Policy, error) {
	project, err := st.GetProject(ctx, projectID)
	if err != nil {
		return egress.Policy{}, fmt.Errorf("read project egress: %w", err)
	}
	pool, err := st.GetPool(ctx, projectID, poolID)
	if err != nil {
		return egress.Policy{}, fmt.Errorf("read pool egress: %w", err)
	}
	levels := []egress.Policy{project.Egress, pool.Egress, own}
	if harnessConfigID != "" {
		harnessConfig, err := st.GetHarnessConfig(ctx, projectID, harnessConfigID)
		switch {
		case err == nil:
			levels = append(levels, harnessConfig.Egress, egress.Policy{Allow: harnessConfig.ImageEgressHosts})
		case !errors.Is(err, store.ErrNotFound):
			return egress.Policy{}, fmt.Errorf("read harness config egress: %w", err)
		}
	}
	return egress.Merge(levels...), nil
}

// refuseWiderThanCreator is §4's check on a create from the sandbox role: the
// new discobox may reach nothing its creator cannot. The creator's policy is
// read as it stands; the child's is what it would be created with.
func (s *Service) refuseWiderThanCreator(ctx context.Context, creatorID string, child *model.Sandbox) error {
	creator, err := s.store.GetSandbox(ctx, child.ProjectID, creatorID)
	if err != nil {
		return fmt.Errorf("read creating discobox: %w", err)
	}
	outer, err := effectiveEgress(ctx, s.store, creator)
	if err != nil {
		return err
	}
	inner, err := effectiveEgress(ctx, s.store, child)
	if err != nil {
		return err
	}
	if !egress.Covers(outer, inner) {
		return apperrors.NewStatusError(http.StatusForbidden,
			"a discobox may not create one that can reach a destination it cannot: its project, pool, harness, and own egress policy must keep the new discobox within the creator's")
	}
	return nil
}
