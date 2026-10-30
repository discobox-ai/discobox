package events

import (
	"context"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// The sweep removes what is past retention, arms itself for the next event to
// expire, and is found by its scan only when something is overdue.
func TestRetentionPrunesAndArmsForTheNext(t *testing.T) {
	ctx := context.Background()
	s, st, db := newTestServiceWithDB(t, subscriptionBuffer)
	createSandboxes(t, st, "project-1", "old", "new")
	reconciler := s.NewRetentionReconciler()

	if ids, err := reconciler.ScanDirty(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("scan of a fresh log = %v, %v; want nothing", ids, err)
	}
	aged := time.Now().Add(-Retention - time.Hour).UTC()
	if err := db.Write.Model(&model.ProjectEvent{}).Where("resource_id = ?", "project-1-old").UpdateColumn("at", aged).Error; err != nil {
		t.Fatal(err)
	}
	if ids, err := reconciler.ScanDirty(ctx); err != nil || len(ids) != 1 || ids[0] != RetentionID {
		t.Fatalf("scan of an overdue log = %v, %v; want the sweep", ids, err)
	}

	result, err := reconciler.Reconcile(ctx, RetentionID)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	left, err := st.ListProjectEventsAfter(ctx, "", 0, 10)
	if err != nil || len(left) != 1 || left[0].ResourceID != "project-1-new" {
		t.Fatalf("left = %v (%v), want the new event only", names(left), err)
	}
	if want := left[0].At.Add(Retention); !result.RequeueAt.Equal(want) {
		t.Fatalf("requeue at %v, want %v", result.RequeueAt, want)
	}
}
