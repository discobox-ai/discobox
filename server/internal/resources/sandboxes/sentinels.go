package sandboxes

import (
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

// SentinelsResourceType is the reconcile resource that materializes a
// sandbox's effective use bindings as its sentinels and delivers the set, with
// each sentinel's route lists, to its pool (ADR 26-10-27-880 §2, §4). It is
// keyed like the sandbox (SandboxDirtyID) and marked in the transaction that
// changes a secret's routes (MarkSecretHoldersDirtyTx) or a binding covering
// the sandbox (MarkBindingHoldersDirtyTx), and in the one that records the
// sandbox coming up (cameUp).
//
// It is a resource of its own rather than a mark on the sandbox because a
// delivery that fails is retried, and nothing else: a sandbox reconcile that
// fails on a settled generation records the sandbox as failed, which would
// turn a pool that could not be reached for a moment into a running sandbox
// reading failed, and the routes into a change nothing retries.
const SentinelsResourceType = "sandboxSentinels"

// SentinelReconciler materializes a sandbox's bindings and re-sends its
// sentinel set to its pool, with the route lists they carry now. A failure is
// returned, so the engine retries it with backoff until the pool takes it.
type SentinelReconciler struct {
	service *Service
}

// NewSentinelReconciler returns the reconciler for SentinelsResourceType.
func (s *Service) NewSentinelReconciler() *SentinelReconciler {
	return &SentinelReconciler{service: s}
}

func (r *SentinelReconciler) Reconcile(ctx context.Context, id string) (reconcile.Result, error) {
	projectID, sandboxID, err := splitSandboxDirtyID(id)
	if err != nil {
		return reconcile.Result{}, err
	}
	sb, err := r.service.store.GetSandbox(ctx, projectID, sandboxID)
	if errors.Is(err, store.ErrNotFound) {
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, err
	}
	// An archive removes the sandbox's sentinels from its pool; there is
	// nothing to deliver to one that is going away.
	if sb.DesiredState != model.DesiredStatePresent {
		return reconcile.Result{}, nil
	}
	if err := r.service.store.Transaction(ctx, func(txStore *store.Store, _ *gorm.DB) error {
		_, err := materializeSandboxSecrets(ctx, txStore, sb)
		return err
	}); err != nil {
		return reconcile.Result{}, err
	}
	// Never placed: no pool holds its sentinels yet, and the create that
	// places it sends them, then checks what it sent against the routes as
	// they are by then (markSentinelsIfStale).
	if len(sb.ProviderState) == 0 {
		return reconcile.Result{}, nil
	}
	err = r.service.pushSandboxSentinels(ctx, sb)
	if errors.Is(err, ErrNotFound) {
		// No container at its pool, so nothing there holds its sentinels: the
		// create that builds one carries them.
		return reconcile.Result{}, nil
	}
	return reconcile.Result{}, err
}

// markSentinelsIfStale marks a sandbox's sentinel delivery when the route
// lists a send carried to its pool are no longer its sentinels'. A send reads the
// routes when it starts and lands when its pool answers — for a create that
// rebuilds a container, minutes later — so a route change committed in between
// can be delivered first and then overwritten by the send. Every send checks
// after it lands, so the last delivery always follows the last send that read
// the routes.
func markSentinelsIfStale(ctx context.Context, st *store.Store, engine *reconcile.Engine, sb *model.Sandbox, sent map[string][][]secretroute.Route) error {
	if engine == nil {
		return nil
	}
	assignments, err := st.ListSandboxSecrets(ctx, sb.ProjectID, sb.ID)
	if err != nil {
		return err
	}
	current, err := st.SentinelRouteLists(ctx, sb.ProjectID, assignments)
	if err != nil {
		return err
	}
	if maps.EqualFunc(sent, current, func(a, b [][]secretroute.Route) bool { return slices.EqualFunc(a, b, model.SameRoutes) }) {
		return nil
	}
	return engine.MarkDirty(ctx, SentinelsResourceType, SandboxDirtyID(sb.ProjectID, sb.ID))
}

// ScanDirty finds nothing: there is no persisted delivery state to compare.
// The mark commits with the route change or the start that needs it and is
// kept until a delivery succeeds, so no change goes unmarked.
func (r *SentinelReconciler) ScanDirty(context.Context) ([]string, error) {
	return nil, nil
}
