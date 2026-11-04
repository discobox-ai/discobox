package database

import (
	"github.com/discobox-ai/discobox/server/internal/model"
	"gorm.io/gorm"
)

// Add and backfill in one transaction. The column's presence is the migration
// marker, so a crash can never leave a completed marker without its data.
func addSandboxParents(db *gorm.DB) error {
	if !db.Migrator().HasTable(&model.Sandbox{}) || db.Migrator().HasColumn(&model.Sandbox{}, "parent_sandbox_id") {
		return nil
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Migrator().AddColumn(&model.Sandbox{}, "ParentSandboxID"); err != nil {
			return err
		}
		if err := tx.Exec("UPDATE sandboxes SET parent_sandbox_id = created_by_sandbox_id").Error; err != nil {
			return err
		}
		// Old purges retained the creator as audit. A missing creator, or one in
		// another project, cannot become a live parent on upgrade.
		return tx.Exec(`UPDATE sandboxes SET parent_sandbox_id = NULL
 WHERE parent_sandbox_id IS NOT NULL AND NOT EXISTS (
 SELECT 1 FROM sandboxes AS parent WHERE parent.id = sandboxes.parent_sandbox_id
 AND parent.project_id = sandboxes.project_id)`).Error
	})
}
