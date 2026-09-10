package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/discobox-ai/discobox/secretformat"
	"github.com/discobox-ai/discobox/server/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (s *Store) GetPoolJudge(ctx context.Context, poolID string) (*model.PoolJudge, error) {
	db, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var row model.PoolJudge
	err = db.Where("pool_id = ?", poolID).First(&row).Error
	return &row, mapNotFound(err)
}

// EnsurePoolJudge atomically replaces identity and bindings when configuration
// changes. Retiring a revision removes its bindings without altering grants.
func (s *Store) EnsurePoolJudge(ctx context.Context, row model.PoolJudge, bindings []model.HarnessConfigSecretBinding) (map[string]string, error) {
	env := map[string]string{}
	err := s.Transaction(ctx, func(tx *Store, db *gorm.DB) error {
		project, err := tx.GetProject(ctx, row.ProjectID)
		if err != nil {
			return err
		}
		selected := project.JudgeHarnessConfigID
		if selected == "" {
			selected = project.DefaultHarnessConfigID
		}
		if selected != row.HarnessConfigID {
			return fmt.Errorf("judge selection changed")
		}
		hc, err := tx.GetHarnessConfig(ctx, row.ProjectID, selected)
		if err != nil {
			return err
		}
		currentBindings, err := tx.ListHarnessConfigSecretBindings(ctx, row.ProjectID, selected)
		if err != nil {
			return err
		}
		revision, err := model.JudgeRevision(hc, currentBindings)
		if err != nil {
			return err
		}
		if !hc.Configured || hc.Slug == "shell" || revision != row.Revision {
			return fmt.Errorf("judge configuration changed")
		}
		var old model.PoolJudge
		err = db.Clauses(clause.Locking{Strength: "UPDATE"}).Where("pool_id = ?", row.PoolID).First(&old).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if old.SandboxID != "" && old.SandboxID != row.SandboxID {
			if err := db.Where("sandbox_id = ? AND project_id = ?", old.SandboxID, row.ProjectID).Delete(&model.SandboxSecret{}).Error; err != nil {
				return err
			}
		}
		if err := db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "pool_id"}}, UpdateAll: true}).Create(&row).Error; err != nil {
			return err
		}
		for _, b := range bindings {
			var assignment model.SandboxSecret
			err := db.Where("sandbox_id = ? AND env_name = ? AND agent_requested = ?", row.SandboxID, b.EnvName, false).First(&assignment).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				secret, err := tx.GetSecret(ctx, row.ProjectID, b.SecretID)
				if err != nil {
					return err
				}
				format := secret.Format
				if format == "" {
					format = secretformat.DefaultSentinelFormat
				}
				sentinel, err := secretformat.MintSentinel(format)
				if err != nil {
					return err
				}
				assignment = model.SandboxSecret{ProjectID: row.ProjectID, SandboxID: row.SandboxID, SecretID: b.SecretID, EnvName: b.EnvName, Sentinel: sentinel, Format: format}
				if err := db.Create(&assignment).Error; err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
			env[b.EnvName] = assignment.Sentinel
		}
		return nil
	})
	return env, err
}
