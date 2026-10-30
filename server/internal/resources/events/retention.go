package events

import (
	"context"
	"log/slog"
	"time"

	"github.com/discobox-ai/discobox/server/internal/reconcile"
)

// Retention is how long the log keeps an event (ADR 26-10-30-868 §6). The log
// is for resuming a dropped stream, not for audit.
const Retention = 24 * time.Hour

// RetentionResourceType is the reconcile resource that prunes the log: one
// row, RetentionID, armed by the sweep itself for the next event to expire
// and found by its scan when nothing armed it.
const (
	RetentionResourceType = "projectEventRetention"
	RetentionID           = "log"
)

// RetentionReconciler removes the events older than Retention.
type RetentionReconciler struct {
	service *Service
}

// NewRetentionReconciler returns the reconciler for RetentionResourceType.
func (s *Service) NewRetentionReconciler() *RetentionReconciler {
	return &RetentionReconciler{service: s}
}

func (r *RetentionReconciler) Reconcile(ctx context.Context, _ string) (reconcile.Result, error) {
	st := r.service.store
	removed, err := st.PruneProjectEvents(ctx, time.Now().Add(-Retention))
	if err != nil {
		return reconcile.Result{}, err
	}
	if removed > 0 {
		slog.InfoContext(ctx, "project events past retention removed", "removed", removed)
	}
	oldest, err := st.OldestProjectEventAt(ctx)
	if err != nil || oldest == nil {
		return reconcile.Result{}, err
	}
	return reconcile.RequeueAt(oldest.Add(Retention)), nil
}

// ScanDirty finds the sweep when the log holds an event past retention and
// nothing armed it: the first event ever written, or a mark that was lost.
func (r *RetentionReconciler) ScanDirty(ctx context.Context) ([]string, error) {
	oldest, err := r.service.store.OldestProjectEventAt(ctx)
	if err != nil || oldest == nil || time.Since(*oldest) < Retention {
		return nil, err
	}
	return []string{RetentionID}, nil
}
