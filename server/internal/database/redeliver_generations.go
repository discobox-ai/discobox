package database

import (
	"errors"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// The value generation a pool's proxy holds each cached value by names its
// secret since ADR 26-11-19-927 §5 (model.Secret.ValueGeneration). Every
// generation a pool was sent before that is of the old shape, so every value
// its proxy resolves after the upgrade reads as replaced and is used once,
// never kept, until the box's sentinels are delivered again; nothing else
// delivers them on an upgrade alone.

// generationsNamedStateKey is the server_state row recording that every
// sentinel holder was marked for the generation's new shape: written in the
// transaction that marks them, so a start that fails before it commits marks
// them again, and one after never does.
const generationsNamedStateKey = "sentinel-generations-name-secret"

// redeliverSentinelGenerations marks the sentinel delivery of every discobox
// holding a sentinel, once, so each pool is sent every generation in its new
// shape and its proxy caches again. A database new to this release marks
// nothing and records that it is done.
func redeliverSentinelGenerations(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var done model.ServerState
		err := tx.Where("key = ?", generationsNamedStateKey).First(&done).Error
		if err == nil {
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var holders []sandboxRef
		if err := tx.Table("sandbox_secrets").Select("DISTINCT project_id, sandbox_id AS id").Scan(&holders).Error; err != nil {
			return err
		}
		if err := markDirty(tx, sentinelsResourceType, holders); err != nil {
			return err
		}
		return tx.Create(&model.ServerState{Key: generationsNamedStateKey, Value: model.RawJSON(`true`)}).Error
	})
}
