package store

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// ListTargetEntries returns a project's own target entries, by host.
func (s *Store) ListTargetEntries(ctx context.Context, projectID string) ([]model.TargetEntry, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.TargetEntry
	err = read.Where("project_id = ?", projectID).Order("host ASC").Find(&out).Error
	return out, err
}

// GetTargetEntry returns the project's own entry for host, exactly.
func (s *Store) GetTargetEntry(ctx context.Context, projectID, host string) (*model.TargetEntry, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var entry model.TargetEntry
	if err := read.Where("project_id = ? AND host = ?", projectID, host).First(&entry).Error; err != nil {
		return nil, mapNotFound(err)
	}
	return &entry, nil
}

// PutTargetEntry writes the project's entry for entry.Host, in place of any it
// had, with texts as the stored text of its documents by name. Whatever text
// the entry held before goes with it, so a document that is now fetched
// leaves no stale text behind. It reports whether the entry is new.
//
// The project's row is held for update first, so two puts for one host take
// turns rather than both inserting.
func (s *Store) PutTargetEntry(ctx context.Context, entry *model.TargetEntry, texts map[string][]byte) (bool, error) {
	created := false
	err := s.Transaction(ctx, func(_ *Store, tx *gorm.DB) error {
		var project model.Project
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&project, "id = ?", entry.ProjectID).Error; err != nil {
			return mapNotFound(err)
		}
		var existing model.TargetEntry
		err := tx.Where("project_id = ? AND host = ?", entry.ProjectID, entry.Host).First(&existing).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			created = true
			if err := tx.Create(entry).Error; err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			entry.ID, entry.CreatedAt = existing.ID, existing.CreatedAt
			if err := tx.Model(&existing).Select("documents", "terms", "statements", "version", "updated_by", "updated_at").Updates(entry).Error; err != nil {
				return err
			}
			if err := tx.Where("entry_id = ?", entry.ID).Delete(&model.TargetDocumentText{}).Error; err != nil {
				return err
			}
		}
		for name, text := range texts {
			row := model.TargetDocumentText{EntryID: entry.ID, ProjectID: entry.ProjectID, Name: name, Text: text}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return created, err
}

// DeleteTargetEntry removes the project's entry for host, and its text.
func (s *Store) DeleteTargetEntry(ctx context.Context, projectID, host string) error {
	return s.Transaction(ctx, func(_ *Store, tx *gorm.DB) error {
		var entry model.TargetEntry
		if err := tx.Where("project_id = ? AND host = ?", projectID, host).First(&entry).Error; err != nil {
			return mapNotFound(err)
		}
		if err := tx.Where("entry_id = ?", entry.ID).Delete(&model.TargetDocumentText{}).Error; err != nil {
			return err
		}
		return tx.Delete(&entry).Error
	})
}

// GetTargetDocumentText returns the stored text of the named document of the
// project's entry for host.
func (s *Store) GetTargetDocumentText(ctx context.Context, projectID, host, name string) ([]byte, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var row model.TargetDocumentText
	err = read.Joins("JOIN target_entries ON target_entries.id = target_document_texts.entry_id").
		Where("target_entries.project_id = ? AND target_entries.host = ? AND target_document_texts.name = ?", projectID, host, name).
		First(&row).Error
	if err != nil {
		return nil, mapNotFound(err)
	}
	return row.Text, nil
}
