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
