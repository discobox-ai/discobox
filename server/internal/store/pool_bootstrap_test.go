package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func TestBootstrapEvidenceSurvivesOtherWritersAndRejectsStaleReports(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, false)
	createTestPool(t, s, "project-1", "pool-bootstrap")
	pool, err := s.GetPool(ctx, "project-1", "pool-bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	b := pool.BootstrapFacts()
	if b == nil {
		t.Fatal("new pool has no attempt")
	}
	stale := *pool
	facts, err := s.RecordPoolBootstrap(ctx, pool.ID, b.Attempt, pool.Generation, model.BootstrapEvidence{Operation: "apply", Bounded: true})
	if err != nil {
		t.Fatal(err)
	}
	deadline := facts.Deadline
	if err := s.UpdatePool(ctx, &stale); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdatePoolWithGeneration(ctx, &stale, pool.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrollPool(ctx, pool.ID, "public-key"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordPoolBootstrap(ctx, pool.ID, "stale-attempt", pool.Generation, model.BootstrapEvidence{Operation: "apply", Complete: true}); !errors.Is(err, store.ErrGenerationConflict) {
		t.Fatalf("stale attempt %v", err)
	}
	facts, err = s.RecordPoolBootstrap(ctx, pool.ID, b.Attempt, pool.Generation, model.BootstrapEvidence{Operation: "apply", Bounded: true})
	if err != nil {
		t.Fatal(err)
	}
	if facts.Deadline != deadline {
		t.Fatal("deadline changed across whole-row writes/restart")
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	pool, err = s.GetPool(ctx, "project-1", pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pool.BootstrapFacts().Deadline != deadline {
		t.Fatal("migration reset evidence")
	}
	if err := db.Write.Model(&model.Pool{}).Where("id = ?", pool.ID).UpdateColumn("generation", pool.Generation+1).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordPoolBootstrap(ctx, pool.ID, b.Attempt, pool.Generation, model.BootstrapEvidence{Operation: "apply", Complete: true}); !errors.Is(err, store.ErrGenerationConflict) {
		t.Fatalf("stale generation %v", err)
	}
}

func TestBootstrapMigrationDoesNotEntitleAmbiguousLegacyRows(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, false)
	createTestPool(t, s, "project-1", "legacy")
	if err := db.Write.Model(&model.Pool{}).Where("id = ?", "legacy").UpdateColumn("bootstrap", nil).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	pool, err := s.GetPool(ctx, "project-1", "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if _, eligible, err := (&model.BootstrapWait{}).Observe(pool, time.Now()); eligible || err != nil {
		t.Fatalf("legacy: %t %v", eligible, err)
	}
}
