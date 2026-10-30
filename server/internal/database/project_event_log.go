package database

import (
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// startProjectEventLog makes the event log's counter row and records the
// projection of every sandbox that has none (ADR 26-10-30-868).
//
// The projection a sandbox records is what its next write compares with, and
// a sandbox from before the log has none. Left empty, every such sandbox would
// write an event at its first write after the upgrade — the next agent status
// for every running one — announcing a change that did not happen. Its
// current projection is what it is, so it is recorded as is and no event is
// written: the log's history starts at the upgrade, as the ADR says.
//
// It runs on every start; once every sandbox has a projection it updates
// nothing, and a sandbox created since got its projection from its create.
func startProjectEventLog(db *gorm.DB) error {
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).
		Create(&model.ProjectEventCounter{ID: model.ProjectEventCounterID}).Error; err != nil {
		return fmt.Errorf("make the project event counter: %w", err)
	}
	var sandboxes []model.Sandbox
	if err := db.Model(&model.Sandbox{}).
		Select("id", "project_id", "name", "desired_state", "state", "runtime_state", "error_reason", "agent_status").
		Where("recorded_projection = ?", "").
		Find(&sandboxes).Error; err != nil {
		return fmt.Errorf("read sandboxes with no recorded projection: %w", err)
	}
	for i := range sandboxes {
		encoded, err := sandboxes[i].EncodedEventProjection()
		if err != nil {
			return err
		}
		// UpdateColumn, not Update: a migration is not an edit, and leaves
		// updated_at as it was.
		if err := db.Model(&model.Sandbox{}).
			Where("id = ? AND recorded_projection = ?", sandboxes[i].ID, "").
			UpdateColumn("recorded_projection", encoded).Error; err != nil {
			return fmt.Errorf("record the projection of sandbox %s: %w", sandboxes[i].ID, err)
		}
	}
	return nil
}
