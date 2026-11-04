package database

import (
	"fmt"
	"log"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// retiredConfigureHarnessMode is the harness mode a configure sandbox ran in
// (ADR 0002), before configure was removed (ADR 26-11-02-152 §2).
const retiredConfigureHarnessMode = "config"

// retiredConfigureColumns are the harness_configs columns only configure
// wrote or read: the enable flag, the in-flight run and its failure, the
// revisions it committed against, and the bundle's configure declaration.
var retiredConfigureColumns = []string{
	"configured",
	"configure_error",
	"configure_sandbox_id",
	"configure_revision",
	"configure_secret_revisions",
	"config_command",
	"config_reminder",
	"config_ports",
}

// retireConfigureFlow takes configure out of the database (ADR 26-11-02-152):
//
//   - each configure sandbox still recorded — an unfinished configure run
//     nothing can commit any more — is marked for deletion, its dirty mark in
//     the same transaction, so the reconciler removes it from its pool the way
//     it removes any other;
//   - sandboxes.harness_mode is dropped: with configure gone every sandbox is
//     a run sandbox, and nothing reads it;
//   - the harness_configs columns only configure used are dropped. The saved
//     overlay, configured_files and configured_secret_ids, is what save-back
//     writes and stays.
//
// It runs after retireCredentialJudge, which finds a judge by the mode this
// drops. Every part is guarded by the column it reads or drops, so it runs on
// every start and does nothing once they are gone; one interrupted after the
// marks and before the drop marks nothing again, since the sandboxes it found
// are already to be deleted.
func retireConfigureFlow(db *gorm.DB) error {
	migrator := db.Migrator()
	if migrator.HasTable("sandboxes") && migrator.HasColumn("sandboxes", "harness_mode") {
		var configures []sandboxRef
		if err := db.Table("sandboxes").Select("project_id, id").
			Where("harness_mode = ? AND desired_state <> ?", retiredConfigureHarnessMode, model.DesiredStateDeleted).
			Scan(&configures).Error; err != nil {
			return err
		}
		for _, configure := range configures {
			log.Printf("removing configure sandbox %s of project %s: configure is removed, and a harness is set up in the discobox it runs in (ADR 26-11-02-152)", configure.ID, configure.ProjectID)
			if err := db.Transaction(func(tx *gorm.DB) error {
				if err := tx.Table("sandboxes").Where("id = ?", configure.ID).UpdateColumns(map[string]any{
					"desired_state": model.DesiredStateDeleted,
					"generation":    gorm.Expr("generation + 1"),
					"error_message": nil,
					"error_reason":  "",
				}).Error; err != nil {
					return err
				}
				return markDirty(tx, sandboxResourceType, []sandboxRef{configure})
			}); err != nil {
				return err
			}
		}
		if err := dropRetiredColumn(db, "sandboxes", "sandboxes", "harness_mode"); err != nil {
			return fmt.Errorf("drop sandboxes.harness_mode: %w", err)
		}
	}
	if !migrator.HasTable(&model.HarnessConfig{}) {
		return nil
	}
	for _, column := range retiredConfigureColumns {
		if err := dropRetiredColumn(db, &model.HarnessConfig{}, "harness_configs", column); err != nil {
			return fmt.Errorf("drop harness_configs.%s: %w", column, err)
		}
	}
	return nil
}
