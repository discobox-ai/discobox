package reconcile

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"
)

// A row armed for the future between the claim's candidate read and its
// guarded write is not claimed: it would run ahead of its timer, and read
// running with a next run a day out, as the reporter of #1728 saw.
func TestClaimRechecksNotBefore(t *testing.T) {
	e, db := testEngine(t)
	e.ctx = context.Background()
	if err := e.MarkDirty(e.ctx, "sandbox", "sbx_1"); err != nil {
		t.Fatalf("mark: %v", err)
	}

	// Arm the row for tomorrow just before the claim's write, after its read:
	// the window a complete with a RequeueAt lands in.
	armed := false
	if err := db.Callback().Update().Before("gorm:update").Register("test:arm", func(tx *gorm.DB) {
		if armed {
			return
		}
		armed = true
		if err := tx.Session(&gorm.Session{NewDB: true, SkipHooks: true}).Exec(
			"UPDATE reconcile_dirty SET not_before = ? WHERE resource_id = ?", time.Now().Add(24*time.Hour), "sbx_1").Error; err != nil {
			t.Errorf("arm: %v", err)
		}
	}); err != nil {
		t.Fatalf("register callback: %v", err)
	}
	t.Cleanup(func() { _ = db.Callback().Update().Remove("test:arm") })

	if row, ok := e.claimOne([]string{"sandbox"}); ok {
		t.Fatalf("claimed %s/%s after it was armed for the future", row.ResourceType, row.ResourceID)
	}
	if !armed {
		t.Fatal("the claim made no guarded write to race")
	}
	var row dirtyRow
	if err := db.Where("resource_id = ?", "sbx_1").First(&row).Error; err != nil {
		t.Fatalf("read row: %v", err)
	}
	if row.ClaimedBy != nil {
		t.Fatalf("row claimed by %q", *row.ClaimedBy)
	}
}

// Only the claims of rows in flight are renewed. A claim whose settle write
// failed is no longer running here, and renewing it kept the row claimed, and
// reading running, until restart; left alone it lapses at its lease and the row
// is claimed again.
func TestLeaseRenewalCoversOnlyRowsInFlight(t *testing.T) {
	e, db := testEngine(t)
	e.ctx = context.Background()
	me := e.WorkerID()
	old := time.Now().Add(time.Second).Truncate(time.Millisecond)
	for _, id := range []string{"running", "settle-failed"} {
		if err := db.Create(&dirtyRow{ResourceType: "sandbox", ResourceID: id, NotBefore: time.Now(), ClaimedBy: &me, LeaseExpires: &old}).Error; err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	e.running["sandbox"] = map[string]struct{}{"running": {}}

	e.renewLeases()

	lease := func(id string) time.Time {
		var row dirtyRow
		if err := db.Where("resource_id = ?", id).First(&row).Error; err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return *row.LeaseExpires
	}
	if got := lease("running"); !got.After(old) {
		t.Fatalf("lease of the row in flight = %v, want renewed past %v", got, old)
	}
	if got := lease("settle-failed"); !got.Equal(old) {
		t.Fatalf("lease of a row not in flight = %v, want left at %v", got, old)
	}
}
