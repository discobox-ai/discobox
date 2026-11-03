package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/discobox-ai/discobox/server/internal/encryption"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// PutSandboxInventory records the inventory a sandbox on poolID posted,
// replacing the one held (ADR 26-10-11-657 §3). Like the agent status it is
// an observation, so it goes through no generation check. The document is
// sealed: it holds terminal content.
//
// It answers ErrNotFound for a sandbox that pool does not host, and checks
// that in the transaction that writes the row: a report racing the sandbox's
// delete must not leave terminal content behind under a sandbox that is gone,
// where nothing would ever remove it.
func (s *Store) PutSandboxInventory(ctx context.Context, projectID, poolID, sandboxID, digest string, document []byte, observedAt time.Time) error {
	sealed, err := s.keyring.Seal(s.keyContext(ctx), encryption.SandboxInventory(projectID, sandboxID), document)
	if err != nil {
		return fmt.Errorf("encrypt sandbox inventory: %w", err)
	}
	row := model.SandboxInventory{SandboxID: sandboxID, ProjectID: projectID, Digest: digest, Document: sealed, ObservedAt: observedAt}
	return s.Transaction(ctx, func(_ *Store, tx *gorm.DB) error {
		var hosted int64
		if err := tx.Model(&model.Sandbox{}).
			Where("project_id = ? AND id = ? AND pool_id = ?", projectID, sandboxID, poolID).
			Count(&hosted).Error; err != nil {
			return err
		}
		if hosted == 0 {
			return ErrNotFound
		}
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "sandbox_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"project_id", "digest", "document", "observed_at"}),
		}).Create(&row).Error
	})
}

// ConfirmSandboxInventory records that the sandbox still had the inventory
// with this digest at observedAt, and reports whether that is the inventory
// held. False means the caller's digest names one that is not here — none was
// ever stored, or a different one is — and the full inventory is needed. The
// observation time only moves forward, so a late report cannot walk it back.
func (s *Store) ConfirmSandboxInventory(ctx context.Context, projectID, sandboxID, digest string, observedAt time.Time) (bool, error) {
	write, err := s.getWrite(ctx)
	if err != nil {
		return false, err
	}
	result := write.WithContext(ctx).Model(&model.SandboxInventory{}).
		Where("project_id = ? AND sandbox_id = ? AND digest = ?", projectID, sandboxID, digest).
		UpdateColumn("observed_at", gorm.Expr("CASE WHEN observed_at < ? THEN ? ELSE observed_at END", observedAt, observedAt))
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// GetSandboxInventory returns the inventory a sandbox last posted with its
// document opened, or ErrNotFound when it has posted none.
func (s *Store) GetSandboxInventory(ctx context.Context, projectID, sandboxID string) (*model.SandboxInventory, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var row model.SandboxInventory
	if err := read.WithContext(ctx).Where("project_id = ? AND sandbox_id = ?", projectID, sandboxID).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	document, err := s.keyring.Open(s.keyContext(ctx), encryption.SandboxInventory(projectID, sandboxID), row.Document)
	if err != nil {
		return nil, fmt.Errorf("decrypt sandbox inventory: %w", err)
	}
	row.Document = document
	return &row, nil
}
