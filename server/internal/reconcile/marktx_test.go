package reconcile

import (
	"context"
	"testing"
	"time"
)

// MarkTx writes the engine's own mark with no engine running: a row it finds
// queued for later is pulled forward and counted again, never pushed back,
// and the engine that starts afterwards claims it from the table.
func TestMarkTxIsTheEnginesMark(t *testing.T) {
	db := testDB(t)
	if err := db.AutoMigrate(&dirtyRow{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := MarkTx(ctx, db, "sandbox", "p/s"); err != nil {
		t.Fatalf("MarkTx: %v", err)
	}
	var first dirtyRow
	if err := db.Where("resource_type = ? AND resource_id = ?", "sandbox", "p/s").First(&first).Error; err != nil {
		t.Fatalf("no mark written: %v", err)
	}
	// A mark queued for later is pulled forward by a mark now.
	later := time.Now().Add(time.Hour)
	if err := db.Model(&dirtyRow{}).Where("resource_type = ? AND resource_id = ?", "sandbox", "p/s").Update("not_before", later).Error; err != nil {
		t.Fatal(err)
	}
	if err := MarkTx(ctx, db, "sandbox", "p/s"); err != nil {
		t.Fatalf("MarkTx: %v", err)
	}
	var pulled dirtyRow
	if err := db.Where("resource_type = ? AND resource_id = ?", "sandbox", "p/s").First(&pulled).Error; err != nil {
		t.Fatal(err)
	}
	if !pulled.NotBefore.Before(later) || pulled.Seq != first.Seq+1 {
		t.Fatalf("mark = not_before %v seq %d, want pulled before %v and counted again", pulled.NotBefore, pulled.Seq, later)
	}
	// A mark queued earlier is never pushed back.
	earlier := time.Now().Add(-time.Hour)
	if err := db.Model(&dirtyRow{}).Where("resource_type = ? AND resource_id = ?", "sandbox", "p/s").Update("not_before", earlier).Error; err != nil {
		t.Fatal(err)
	}
	if err := MarkTx(ctx, db, "sandbox", "p/s"); err != nil {
		t.Fatalf("MarkTx: %v", err)
	}
	var kept dirtyRow
	if err := db.Where("resource_type = ? AND resource_id = ?", "sandbox", "p/s").First(&kept).Error; err != nil {
		t.Fatal(err)
	}
	if kept.NotBefore.After(earlier.Add(time.Second)) {
		t.Fatalf("not_before = %v, want it left at %v", kept.NotBefore, earlier)
	}
}
