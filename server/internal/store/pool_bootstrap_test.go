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

// ReplacePoolBootstrap starts a new attempt only over the ended attempt its
// caller read: a concurrent replacement, new intent, a registration, a live
// attempt and a legacy row each refuse it.
func TestReplacePoolBootstrapIsFencedOnTheEndedAttempt(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, false)
	createTestPool(t, s, "project-1", "pool-replace")
	pool, err := s.GetPool(ctx, "project-1", "pool-replace")
	if err != nil {
		t.Fatal(err)
	}
	first := pool.BootstrapFacts()
	generation := pool.Generation

	if _, err := s.ReplacePoolBootstrap(ctx, pool.ID, first.Attempt, generation); !errors.Is(err, store.ErrGenerationConflict) {
		t.Fatalf("replacing a live attempt = %v, want a conflict", err)
	}
	if _, err := s.RecordPoolBootstrap(ctx, pool.ID, first.Attempt, generation, model.BootstrapEvidence{Failure: "image not found"}); err != nil {
		t.Fatal(err)
	}
	second, err := s.ReplacePoolBootstrap(ctx, pool.ID, first.Attempt, generation)
	if err != nil {
		t.Fatalf("replace ended attempt: %v", err)
	}
	if second.Attempt == first.Attempt || second.Generation != generation || second.Failure != "" || second.ProgressAt.IsZero() {
		t.Fatalf("new attempt = %+v", second)
	}
	if _, err := s.ReplacePoolBootstrap(ctx, pool.ID, first.Attempt, generation); !errors.Is(err, store.ErrGenerationConflict) {
		t.Fatalf("second replacement of the same attempt = %v, want a conflict", err)
	}
	if _, err := s.RecordPoolBootstrap(ctx, pool.ID, first.Attempt, generation, model.BootstrapEvidence{Operation: "apply", Bounded: true}); !errors.Is(err, store.ErrGenerationConflict) {
		t.Fatalf("report for the replaced attempt = %v, want a conflict", err)
	}

	if _, err := s.RecordPoolBootstrap(ctx, pool.ID, second.Attempt, generation, model.BootstrapEvidence{Failure: "image not found"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Write.Model(&model.Pool{}).Where("id = ?", pool.ID).UpdateColumn("generation", generation+1).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplacePoolBootstrap(ctx, pool.ID, second.Attempt, generation); !errors.Is(err, store.ErrGenerationConflict) {
		t.Fatalf("replacement for superseded intent = %v, want a conflict", err)
	}
	third, err := s.ReplacePoolBootstrap(ctx, pool.ID, second.Attempt, generation+1)
	if err != nil {
		t.Fatalf("replace for the new generation: %v", err)
	}
	if third.Generation != generation+1 {
		t.Fatalf("new attempt generation = %d, want %d", third.Generation, generation+1)
	}

	if err := db.Write.Model(&model.Pool{}).Where("id = ?", pool.ID).UpdateColumn("generation", generation+2).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Write.Model(&model.Pool{}).Where("id = ?", pool.ID).UpdateColumn("registered_at", time.Now().UTC()).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplacePoolBootstrap(ctx, pool.ID, third.Attempt, generation+2); !errors.Is(err, store.ErrGenerationConflict) {
		t.Fatalf("replacement on a registered pool = %v, want a conflict", err)
	}

	createTestPool(t, s, "project-1", "pool-legacy")
	if err := db.Write.Model(&model.Pool{}).Where("id = ?", "pool-legacy").UpdateColumn("bootstrap", nil).Error; err != nil {
		t.Fatal(err)
	}
	legacy, err := s.GetPool(ctx, "project-1", "pool-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplacePoolBootstrap(ctx, legacy.ID, "", legacy.Generation); !errors.Is(err, store.ErrGenerationConflict) {
		t.Fatalf("replacement on a legacy row = %v, want a conflict", err)
	}
}
