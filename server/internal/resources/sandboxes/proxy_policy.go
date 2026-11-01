package sandboxes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/proxy/egress"
	"github.com/discobox-ai/discobox/proxy/gateway"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// This file delivers a sandbox's proxy policy: its effective egress policy
// (ADR 26-10-01-032), merged by store.EffectiveSandboxEgress, and its
// project's gateways (ADR 26-10-16-470), each read whenever it is needed.
//
// It reaches the pool two ways. A create carries it, so a new container is
// restricted and routed from its first request. Every change after that is
// delivered by its own reconcile (SandboxProxyPolicyResourceType), which pushes
// the policy alone through the pool's update route: it retries with the
// engine's backoff while a pool is unreachable, records nothing on the sandbox,
// and reaches a sandbox whatever its lifecycle is doing — a settled failure
// included. The sandbox reconciler marks it after every create, so a create
// that read its policy before a change committed cannot leave the old one in
// force.

// SandboxProxyPolicyResourceType is the reconcile-engine resource type that
// delivers a sandbox's proxy policy to its pool. Its IDs are SandboxDirtyID's.
const SandboxProxyPolicyResourceType = "sandboxProxyPolicy"

// sandboxProxyPolicyReconciler pushes a sandbox's proxy policy to its pool.
type sandboxProxyPolicyReconciler struct {
	store     *store.Store
	providers *ProviderManager
}

func (r *sandboxProxyPolicyReconciler) Reconcile(ctx context.Context, id string) (reconcile.Result, error) {
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
	policy, err := r.store.EffectiveSandboxEgress(ctx, sb)
	if err != nil {
		return reconcile.Result{}, err
	}
	gateways, err := sandboxGateways(ctx, r.store, sb.ProjectID)
	if err != nil {
		return reconcile.Result{}, err
	}
	provider, err := r.providers.ResolveForSandbox(ctx, sb)
	if err != nil {
		return reconcile.Result{}, err
	}
	_, _, err = provider.Update(ctx, sandboxRefFromSandbox(sb), sb.ProviderState, UpdateOptions{Egress: &policy, Gateways: gateways})
	if errors.Is(err, ErrNotFound) {
		// Never built, so nothing at the pool knows it yet: its create carries
		// the policy, and marks this again once it has run.
		return reconcile.Result{}, nil
	}
	return reconcile.Result{}, err
}

// sandboxGateways is the gateways a sandbox of projectID is routed through,
// never nil: an update carrying none replaces what the pool holds with
// nothing, where nil would leave it.
func sandboxGateways(ctx context.Context, appStore *store.Store, projectID string) ([]gateway.Route, error) {
	rows, err := appStore.ListProjectGateways(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("read project gateways: %w", err)
	}
	routes := make([]gateway.Route, 0, len(rows))
	for _, row := range rows {
		routes = append(routes, row.Route())
	}
	return routes, nil
}

// MarkProxyPolicyChangedTx marks the proxy policy delivery of every sandbox
// of projectID meant to exist — narrowed to one pool, one harness config, or
// both when they are given — in the transaction that changed an egress level
// (ADR 26-10-01-032 §5) or the project's gateways (ADR 26-10-16-470 §5). A
// sandbox archived since is not marked: its next create carries the policy
// anyway.
func (s *Service) MarkProxyPolicyChangedTx(ctx context.Context, txStore *store.Store, txDB *gorm.DB, projectID, poolID, harnessConfigID string) error {
	if s.engine == nil {
		return errors.New("reconcile engine is required")
	}
	ids, err := txStore.ListPresentSandboxIDs(ctx, projectID, poolID, harnessConfigID)
	if err != nil {
		return err
	}
	for _, sandboxID := range ids {
		if err := s.engine.MarkDirtyTx(ctx, txDB, SandboxProxyPolicyResourceType, SandboxDirtyID(projectID, sandboxID)); err != nil {
			return err
		}
	}
	return nil
}

// refuseSandboxRoleUpdate is ADR 26-10-31-819 §5's check on an update from
// the sandbox role, which reaches it only for a discobox it created and does
// not reach less than (authorizeWithinCaller): it may change that discobox's
// passthrough and nothing else, and only to entries the caller's own
// effective passthrough covers, read as it stands.
func (s *Service) refuseSandboxRoleUpdate(ctx context.Context, callerID string, target *model.Sandbox, input services.UpdateSandboxBody) error {
	onlyPassthrough := apperrors.NewStatusError(http.StatusForbidden,
		"a discobox may change only the passthrough of a discobox it created; everything else about it is a person's to change")
	// What the sandbox role may send is listed, not what it may not: a field
	// added to the update later is refused to it until someone decides
	// otherwise (server/internal/auth/REVIEW.md, no authorization by
	// exclusion). The body is read as it would be sent, field by field.
	if fields, err := onlyFields(&input, "config", "$schema"); err != nil || !fields {
		return onlyPassthrough
	}
	config, ok := input.Config.Get()
	if !ok {
		return onlyPassthrough
	}
	if fields, err := onlyFields(&config, "egress"); err != nil || !fields {
		return onlyPassthrough
	}
	requested, ok := config.Egress.Get()
	if !ok {
		return onlyPassthrough
	}
	level, err := services.EgressPolicyToModel(requested)
	if err != nil {
		return err
	}
	if level.DenyByDefault != target.Egress.DenyByDefault || !slices.Equal(level.Allow, egress.Normalize(target.Egress.Allow)) {
		return onlyPassthrough
	}
	caller, err := s.store.GetSandbox(ctx, target.ProjectID, callerID)
	if err != nil {
		return fmt.Errorf("read calling discobox: %w", err)
	}
	outer, err := s.store.EffectiveSandboxEgress(ctx, caller)
	if err != nil {
		return err
	}
	if !egress.CoversEntries(outer.Passthrough, level.Passthrough) {
		return apperrors.NewStatusError(http.StatusForbidden,
			"a discobox may set another's passthrough only to hosts its own passthrough covers (ADR 26-10-31-819 §5)")
	}
	return nil
}

// onlyFields reports whether v, encoded as the API encodes it, sets no field
// but those named.
func onlyFields(v json.Marshaler, allowed ...string) (bool, error) {
	data, err := v.MarshalJSON()
	if err != nil {
		return false, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return false, err
	}
	for name := range fields {
		if !slices.Contains(allowed, name) {
			return false, nil
		}
	}
	return true, nil
}

// refuseWiderThanCreator is §4's check on a create from the sandbox role: the
// new discobox may reach nothing its creator cannot. The creator's policy is
// read as it stands; the child's is what it would be created with.
func (s *Service) refuseWiderThanCreator(ctx context.Context, creatorID string, child *model.Sandbox) error {
	creator, err := s.store.GetSandbox(ctx, child.ProjectID, creatorID)
	if err != nil {
		return fmt.Errorf("read creating discobox: %w", err)
	}
	outer, err := s.store.EffectiveSandboxEgress(ctx, creator)
	if err != nil {
		return err
	}
	inner, err := s.store.EffectiveSandboxEgress(ctx, child)
	if err != nil {
		return err
	}
	if !egress.CoversEntries(outer.Passthrough, inner.Passthrough) {
		return apperrors.NewStatusError(http.StatusForbidden,
			"a discobox may give a new discobox only passthrough hosts its own passthrough covers (ADR 26-10-31-819 §5)")
	}
	if !egress.Covers(outer, inner) {
		return apperrors.NewStatusError(http.StatusForbidden,
			"a discobox may not create one that can reach a destination it cannot: its project, pool, harness, and own egress policy must keep the new discobox within the creator's")
	}
	return nil
}
