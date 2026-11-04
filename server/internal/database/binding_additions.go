package database

import (
	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// retiredPlainUseIndex is the unique index over a target's use bindings by
// variable from before additions (ADR 26-11-03-329 §1). It covered every use
// binding; idx_secret_binding_plain_use covers only those that are not
// additions, so a target may hold several additions in one variable beside
// its one plain binding.
const retiredPlainUseIndex = "idx_secret_binding_use"

// dropRetiredPlainUseIndex drops the old use index before AutoMigrate makes
// its narrower successor. A partial index's condition cannot be changed in
// place, and AutoMigrate never revisits one that exists by name, so the
// successor has a name of its own. It rewrites no row: every binding stored
// before additions is plain, and the new index holds them as the old one did.
func dropRetiredPlainUseIndex(db *gorm.DB) error {
	migrator := db.Migrator()
	if !migrator.HasTable(&model.SecretBinding{}) || !migrator.HasIndex(&model.SecretBinding{}, retiredPlainUseIndex) {
		return nil
	}
	return migrator.DropIndex(&model.SecretBinding{}, retiredPlainUseIndex)
}

// recordReplacingApprovals records an approved use part from before
// additions as the replacement it was (ADR 26-11-03-329 §4): every approval
// then replaced. Idempotent: a part already recorded is not matched again.
func recordReplacingApprovals(db *gorm.DB) error {
	return db.Model(&model.SecretRequest{}).
		Where("use_outcome = ? AND use_approved_as = ?", model.SecretRequestPartApproved, "").
		Update("use_approved_as", model.SecretRequestApprovedAsReplace).Error
}
