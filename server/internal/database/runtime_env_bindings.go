package database

import (
	"log"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// markRuntimeEnvHolders marks the sentinel delivery of every discobox that
// still holds a row in a variable it runs on (sandboxconfig.RuntimeEnvVar),
// delivered under a binding stored before such a variable was refused
// (#1545). Its binding no longer applies, so the materialization the mark
// starts takes the row away and the pool is sent the set without it: a running
// discobox whose PATH was a sentinel gets it back without a restart.
//
// It runs on every start and marks nothing once no such row is left. Only a
// discobox meant to be present is marked: an archived one is materialized when
// it comes back.
func markRuntimeEnvHolders(db *gorm.DB) error {
	migrator := db.Migrator()
	if !migrator.HasTable("sandbox_secrets") || !migrator.HasTable("sandboxes") {
		return nil
	}
	var rows []struct {
		ProjectID string
		ID        string
		EnvName   string
	}
	if err := db.Table("sandbox_secrets").
		Select("sandbox_secrets.project_id, sandbox_secrets.sandbox_id AS id, sandbox_secrets.env_name").
		Joins("JOIN sandboxes ON sandboxes.id = sandbox_secrets.sandbox_id").
		Where("sandboxes.desired_state = ?", model.DesiredStatePresent).
		Scan(&rows).Error; err != nil {
		return err
	}
	var held []sandboxRef
	seen := map[sandboxRef]bool{}
	for _, row := range rows {
		ref := sandboxRef{ProjectID: row.ProjectID, ID: row.ID}
		if !sandboxconfig.RuntimeEnvVar(row.EnvName) || seen[ref] {
			continue
		}
		seen[ref] = true
		log.Printf("discobox %s of project %s holds a credential in %s, a variable it runs on; it is no longer delivered there", row.ID, row.ProjectID, row.EnvName)
		held = append(held, ref)
	}
	if len(held) == 0 {
		return nil
	}
	return db.Transaction(func(tx *gorm.DB) error {
		return markDirty(tx, sentinelsResourceType, held)
	})
}
