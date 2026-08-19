package reconcile

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"
)

// A drift mark must not cancel the backoff a failed reconcile is waiting out.
//
// Every observer of a broken resource marks it, and they all fire at the moment
// it fails, so a drift mark that pulled the row forward would spend the
// backoff before it was ever waited on -- which is how a failing pool collects
// thousands of attempts a minute.
func TestMarkDirtyDriftDoesNotCancelAFailureBackoff(t *testing.T) {
	e, db := testEngine(t)
	backoff := time.Now().Add(time.Hour)
	seedFailedRow(t, db, "pool", "p-1", backoff)

	if err := e.MarkDirtyDrift(context.Background(), "pool", "p-1"); err != nil {
		t.Fatal(err)
	}

	row := readRow(t, db, "p-1")
	if !row.NotBefore.Equal(backoff) {
		t.Fatalf("not_before = %v, want the backoff at %v left alone", row.NotBefore, backoff)
	}
	// The mark still registers: the resource is dirty and will be reconciled,
	// just when the backoff says rather than now.
	if row.Seq < 2 {
		t.Fatalf("seq = %d, want the mark recorded", row.Seq)
	}
}

// Intent is the case worth interrupting a backoff for, so the ordinary mark is
// unchanged.
func TestMarkDirtyStillPreemptsAFailureBackoff(t *testing.T) {
	e, db := testEngine(t)
	backoff := time.Now().Add(time.Hour)
	seedFailedRow(t, db, "pool", "p-1", backoff)

	if err := e.MarkDirty(context.Background(), "pool", "p-1"); err != nil {
		t.Fatal(err)
	}

	if row := readRow(t, db, "p-1"); !row.NotBefore.Before(backoff) {
		t.Fatalf("not_before = %v, want it pulled in ahead of %v", row.NotBefore, backoff)
	}
}

// A reconciler's own requeue timer is not a backoff: it settles attempts to 0,
// and a timer is a guess about when to look again that an observation beats.
func TestMarkDirtyDriftPullsForwardARequeueTimer(t *testing.T) {
	e, db := testEngine(t)
	timer := time.Now().Add(time.Hour)
	if err := e.MarkDirtyAt(context.Background(), "pool", "p-1", timer); err != nil {
		t.Fatal(err)
	}

	if err := e.MarkDirtyDrift(context.Background(), "pool", "p-1"); err != nil {
		t.Fatal(err)
	}

	if row := readRow(t, db, "p-1"); !row.NotBefore.Before(timer) {
		t.Fatalf("not_before = %v, want it pulled in ahead of the timer at %v", row.NotBefore, timer)
	}
}

// seedFailedRow writes the row release() leaves behind after a failed
// reconcile: attempts recorded, and not_before out at the backoff.
func seedFailedRow(t *testing.T, db *gorm.DB, resourceType, id string, notBefore time.Time) {
	t.Helper()
	now := time.Now()
	cause := "start wslc VM: session already exists"
	row := dirtyRow{
		ResourceType: resourceType,
		ResourceID:   id,
		Seq:          1,
		Attempts:     7,
		LastError:    &cause,
		NotBefore:    notBefore,
		MarkedAt:     now,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("seed row: %v", err)
	}
}

func readRow(t *testing.T, db *gorm.DB, id string) dirtyRow {
	t.Helper()
	var row dirtyRow
	if err := db.First(&row, "resource_id = ?", id).Error; err != nil {
		t.Fatalf("read row: %v", err)
	}
	return row
}
