package secrets

import (
	"context"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// The binding expiry sweep (ADR 26-10-27-880 §3) runs as the reconcile
// resource store.BindingExpiryResourceType: one row, armed by the store at the
// expiry of every binding it writes, in the same transaction, and by the
// sweep itself with RequeueAt for the next. A lapsed binding authorizes
// nothing whether or not it has been removed — every read checks Live — so
// the sweep is what records the expiry, takes the children with it, and sends
// each discobox it covered its sentinels without it.

// BindingExpiryReconciler removes every lapsed binding, each in its own
// transaction with its expired event and the marks for the discoboxes it
// covered, then arms itself for the next.
type BindingExpiryReconciler struct {
	service *Service
}

// NewBindingExpiryReconciler returns the reconciler for
// BindingExpiryResourceType.
func (s *Service) NewBindingExpiryReconciler() *BindingExpiryReconciler {
	return &BindingExpiryReconciler{service: s}
}

func (r *BindingExpiryReconciler) Reconcile(ctx context.Context, _ string) (reconcile.Result, error) {
	st := r.service.store
	now := time.Now().UTC()
	lapsed, err := st.ListLapsedSecretBindings(ctx, now)
	if err != nil {
		return reconcile.Result{}, err
	}
	for i := range lapsed {
		binding := lapsed[i]
		if err := st.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
			deleted, err := store.ExpireSecretBindingTx(txDB, &binding, now)
			if err != nil {
				return err
			}
			return r.service.markBindingHolders(ctx, txStore, txDB, deleted...)
		}); err != nil {
			return reconcile.Result{}, err
		}
		slog.InfoContext(ctx, "secret binding lapsed", "projectId", binding.ProjectID, "bindingId", binding.ID, "secretId", binding.SecretID)
	}
	next, err := st.NextSecretBindingExpiry(ctx)
	if err != nil || next == nil {
		return reconcile.Result{}, err
	}
	return reconcile.RequeueAt(*next), nil
}

// ScanDirty finds the sweep when a binding has lapsed and nothing armed it:
// one written before this server, or one whose mark was lost.
func (r *BindingExpiryReconciler) ScanDirty(ctx context.Context) ([]string, error) {
	next, err := r.service.store.NextSecretBindingExpiry(ctx)
	if err != nil || next == nil || next.After(time.Now()) {
		return nil, err
	}
	return []string{store.BindingExpiryID}, nil
}
