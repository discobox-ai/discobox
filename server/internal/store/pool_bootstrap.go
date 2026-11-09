package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RecordPoolBootstrap fences executor reports by both attempt and generation.
// The locked narrow writer preserves evidence across lifecycle/manifest writes.
func (s *Store) RecordPoolBootstrap(ctx context.Context, poolID, attempt string, generation int64, evidence model.BootstrapEvidence) (*model.PoolBootstrap, error) {
	write, err := s.getWrite(ctx)
	if err != nil {
		return nil, err
	}
	var facts *model.PoolBootstrap
	err = write.Transaction(func(tx *gorm.DB) error {
		var pool model.Pool
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&pool, "id = ?", poolID).Error; err != nil {
			return mapNotFound(err)
		}
		facts = pool.BootstrapFacts()
		if facts == nil || facts.Attempt != attempt || facts.Generation != generation || pool.Generation != generation {
			return ErrGenerationConflict
		}
		if pool.RegisteredAt != nil {
			return nil
		}
		facts.Accept(evidence, time.Now().UTC())
		data, err := json.Marshal(facts)
		if err != nil {
			return err
		}
		return tx.Model(&model.Pool{}).Where("id = ?", poolID).UpdateColumn("bootstrap", model.RawJSON(data)).Error
	})
	return facts, err
}

// ReplacePoolBootstrap starts a new attempt for a never-registered pool whose
// attempt has ended (ADR 26-11-09-593 §1), owing the runtime a repair and
// carrying the failure count that paces it (model.PoolBootstrap.Next). Pacing
// is the caller's: this only fences the write. The write is fenced on the attempt
// the caller read: it is refused with ErrGenerationConflict when the pool's
// generation moved, the pool registered, the attempt was already replaced, or
// the attempt has not ended. A row with no bootstrap document never gets one
// here: an ambiguous legacy row keeps the bounded registration policy.
func (s *Store) ReplacePoolBootstrap(ctx context.Context, poolID, attempt string, generation int64) (*model.PoolBootstrap, error) {
	write, err := s.getWrite(ctx)
	if err != nil {
		return nil, err
	}
	var next model.PoolBootstrap
	err = write.Transaction(func(tx *gorm.DB) error {
		var pool model.Pool
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&pool, "id = ?", poolID).Error; err != nil {
			return mapNotFound(err)
		}
		facts := pool.BootstrapFacts()
		if facts == nil || facts.Attempt != attempt || !facts.Ended(generation) ||
			pool.Generation != generation || pool.RegisteredAt != nil {
			return ErrGenerationConflict
		}
		next = facts.Next(generation, time.Now().UTC())
		data, err := json.Marshal(next)
		if err != nil {
			return err
		}
		return tx.Model(&model.Pool{}).Where("id = ?", poolID).UpdateColumn("bootstrap", model.RawJSON(data)).Error
	})
	if err != nil {
		return nil, err
	}
	return &next, nil
}
