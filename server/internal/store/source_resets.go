package store

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/server/internal/model"
)

var ErrSourceResetRevision = errors.New("the source reset revision changed")

func (s *Store) GetSourceReset(ctx context.Context, sandboxID, slug string) (model.SandboxSourceReset, error) {
	var row model.SandboxSourceReset
	read, err := s.getWrite(ctx)
	if err != nil {
		return row, err
	}
	err = read.Where("sandbox_id = ? AND slug = ?", sandboxID, slug).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = nil
	}
	return row, err
}

func (s *Store) SetSourceReset(ctx context.Context, projectID, sandboxID, slug, commit, actor string, expected int64, authorize func(*Store) error) (model.SandboxSourceReset, error) {
	var result model.SandboxSourceReset
	err := s.Transaction(ctx, func(txStore *Store, tx *gorm.DB) error {
		var box model.Sandbox
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").
			Where("project_id = ? AND id = ? AND desired_state = ?", projectID, sandboxID, model.DesiredStatePresent).
			Take(&box).Error
		if err != nil {
			return mapNotFound(err)
		}
		if err := authorize(txStore); err != nil {
			return err
		}
		result, err = txStore.GetSourceReset(ctx, sandboxID, slug)
		if err != nil {
			return err
		}
		if expected != result.Revision {
			// A lost response may be retried with the same comparison and commit.
			if result.Revision == expected+1 && result.Commit == commit && result.Actor == actor {
				return nil
			}
			return ErrSourceResetRevision
		}
		result.SandboxID, result.Slug = sandboxID, slug
		result.Revision, result.Commit, result.Actor = expected+1, commit, actor
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "sandbox_id"}, {Name: "slug"}},
			DoUpdates: clause.AssignmentColumns([]string{"revision", "commit", "actor"}),
		}).Create(&result).Error
	})
	return result, err
}

func (s *Store) HostedSourceResets(ctx context.Context, poolID, sandboxID string) (map[string]sandboxconfig.SourceReset, error) {
	read, err := s.getWrite(ctx)
	if err != nil {
		return nil, err
	}
	var rows []model.SandboxSourceReset
	err = read.Model(&model.SandboxSourceReset{}).
		Select("sandbox_source_resets.slug", "sandbox_source_resets.revision", `sandbox_source_resets."commit"`).
		Joins("JOIN sandboxes ON sandboxes.id = sandbox_source_resets.sandbox_id").
		Where("sandboxes.pool_id = ? AND sandboxes.id = ?", poolID, sandboxID).Find(&rows).Error
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	resets := make(map[string]sandboxconfig.SourceReset, len(rows))
	for _, row := range rows {
		resets[row.Slug] = sandboxconfig.SourceReset{Revision: row.Revision, Commit: row.Commit}
	}
	return resets, nil
}

func (s *Store) ObserveSourceResets(ctx context.Context, sandboxID string, revisions map[string]int64) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	for slug, revision := range revisions {
		if err := write.Model(&model.SandboxSourceReset{}).
			Where("sandbox_id = ? AND slug = ? AND applied_revision < ? AND revision >= ?", sandboxID, slug, revision, revision).
			UpdateColumn("applied_revision", revision).Error; err != nil {
			return err
		}
	}
	return nil
}
