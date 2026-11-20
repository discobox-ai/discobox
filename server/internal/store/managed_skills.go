package store

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// MutateSandboxSkills orders updates under the sandbox row lock, and writes
// the accepted-change event in the transaction carrying desired intent.
// authorize runs against that same transaction before a mutation is admitted.
func (s *Store) MutateSandboxSkills(
	ctx context.Context, projectID, sandboxID, actor string,
	change sandboxconfig.SkillMutation, authorize func(*Store) error,
) (model.SandboxManagedSkills, error) {
	var result model.SandboxManagedSkills
	err := s.Transaction(ctx, func(txStore *Store, tx *gorm.DB) error {
		var sandbox model.Sandbox
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select(sandboxEventColumns).
			Where("project_id = ? AND id = ? AND desired_state <> ?", projectID, sandboxID, model.DesiredStateDeleted).
			Take(&sandbox).Error
		if err != nil {
			return mapNotFound(err)
		}
		if err := authorize(txStore); err != nil {
			return err
		}
		result.SandboxID = sandboxID
		err = tx.Where("sandbox_id = ?", sandboxID).Take(&result).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		next, err := result.Desired.Mutate(change)
		if err != nil {
			return err
		}
		if next.Revision == result.Desired.Revision {
			return nil
		}
		audit := model.SandboxSkillChange{
			Actor: actor, Operation: change.Operation, Revision: next.Revision,
			Skills: skillVersionChanges(result.Desired.Versions, next.Versions),
		}
		result.Desired = next
		result.DesiredRevision = next.Revision
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "sandbox_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"desired", "desired_revision"}),
		}).Create(&result).Error; err != nil {
			return err
		}
		if err := tx.Create(&model.SandboxSkillRevision{
			SandboxID: sandboxID, Revision: next.Revision, Versions: next.Versions,
		}).Error; err != nil {
			return err
		}
		// An unresponsive box must not retain every publication forever.
		// Recent in-flight revisions keep their provenance until observed.
		const retainedPendingSkillRevisions = 64
		if err := tx.Where("sandbox_id = ? AND revision <= ?", sandboxID, next.Revision-retainedPendingSkillRevisions).
			Delete(&model.SandboxSkillRevision{}).Error; err != nil {
			return err
		}
		data := sandboxEventData(&sandbox, time.Now().UTC())
		data.SkillChange = &audit
		txStore.sandboxEvents.skillChanges = append(txStore.sandboxEvents.skillChanges, data)
		return nil
	})
	return result, err
}

func skillVersionChanges(before, after map[string]sandboxconfig.SkillVersion) map[string]model.SkillVersionDiff {
	changes := make(map[string]model.SkillVersionDiff)
	for name, old := range before {
		current, ok := after[name]
		if !ok {
			changes[name] = model.SkillVersionDiff{Before: &old}
		} else if current != old {
			changes[name] = model.SkillVersionDiff{Before: &old, After: &current}
		}
	}
	for name, current := range after {
		if _, existed := before[name]; !existed {
			changes[name] = model.SkillVersionDiff{After: &current}
		}
	}
	return changes
}

// GetSandboxManagedSkills reads the current desired revision and last
// acknowledgement together. A sandbox without a publication has revision zero.
func (s *Store) GetSandboxManagedSkills(ctx context.Context, projectID, sandboxID string) (model.SandboxManagedSkills, error) {
	read, err := s.getWrite(ctx)
	if err != nil {
		return model.SandboxManagedSkills{}, err
	}
	var result model.SandboxManagedSkills
	err = read.Model(&model.SandboxManagedSkills{}).
		Joins("JOIN sandboxes ON sandboxes.id = sandbox_managed_skills.sandbox_id").
		Where("sandboxes.project_id = ? AND sandboxes.id = ?", projectID, sandboxID).Take(&result).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.SandboxManagedSkills{SandboxID: sandboxID}, nil
	}
	return result, err
}

// ObserveSandboxSkills records an agent's acknowledgement once. A report can
// lag the current desired revision but cannot acknowledge a future revision.
func (s *Store) ObserveSandboxSkills(ctx context.Context, projectID, poolID, sandboxID string, revision int64) error {
	if revision <= 0 {
		return nil
	}
	return s.Transaction(ctx, func(txStore *Store, tx *gorm.DB) error {
		var sandbox model.Sandbox
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select(sandboxEventColumns).
			Where("project_id = ? AND pool_id = ? AND id = ?", projectID, poolID, sandboxID).
			Take(&sandbox).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		var current model.SandboxManagedSkills
		err = tx.Where("sandbox_id = ?", sandboxID).Take(&current).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if revision <= current.Applied || revision > current.Desired.Revision {
			return nil
		}
		var applied model.SandboxSkillRevision
		err = tx.Where("sandbox_id = ? AND revision = ?", sandboxID, revision).Take(&applied).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		current.Applied = revision
		current.AppliedVersions = applied.Versions
		if revision == current.Desired.Revision {
			current.AppliedVersions = current.Desired.Versions
		}
		if err := tx.Model(&current).Select("applied", "applied_versions").Updates(&current).Error; err != nil {
			return err
		}
		if err := tx.Where("sandbox_id = ? AND revision <= ?", sandboxID, revision).Delete(&model.SandboxSkillRevision{}).Error; err != nil {
			return err
		}
		data := sandboxEventData(&sandbox, time.Now().UTC())
		data.SkillChange = &model.SandboxSkillChange{Actor: "sandbox:" + sandboxID, Operation: "apply", Revision: revision}
		txStore.sandboxEvents.skillChanges = append(txStore.sandboxEvents.skillChanges, data)
		return nil
	})
}

// GetPoolManagedSkills checks placement before reading content, and reads
// content only when the pool needs the revision.
func (s *Store) GetPoolManagedSkills(ctx context.Context, poolID, sandboxID string, known int64) (*sandboxconfig.ManagedSkills, error) {
	read, err := s.getWrite(ctx)
	if err != nil {
		return nil, err
	}
	var row struct {
		SandboxID       string
		DesiredRevision int64
	}
	err = read.Table("sandboxes").
		Select("sandboxes.id AS sandbox_id, COALESCE(sandbox_managed_skills.desired_revision, 0) AS desired_revision").
		Joins("LEFT JOIN sandbox_managed_skills ON sandbox_managed_skills.sandbox_id = sandboxes.id").
		Where("sandboxes.id = ? AND sandboxes.pool_id = ?", sandboxID, poolID).
		Take(&row).Error
	if err != nil {
		return nil, mapNotFound(err)
	}
	if known > 0 && row.DesiredRevision == known {
		return nil, nil
	}
	if row.DesiredRevision == 0 {
		return &sandboxconfig.ManagedSkills{}, nil
	}
	var current model.SandboxManagedSkills
	err = read.Model(&model.SandboxManagedSkills{}).
		Joins("JOIN sandboxes ON sandboxes.id = sandbox_managed_skills.sandbox_id").
		Where("sandboxes.pool_id = ? AND sandboxes.id = ?", poolID, sandboxID).Take(&current).Error
	if err != nil {
		return nil, mapNotFound(err)
	}
	return &current.Desired.ManagedSkills, nil
}
