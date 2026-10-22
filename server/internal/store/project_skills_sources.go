package store

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/discobox-ai/discobox/pkg/sourceref"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// ErrSkillsSourceConflict is a skills source the project's list already has,
// or one naming a repository the list names at another ref: a discobox checks
// out one ref of a repository, so every create would fail on such a list
// (ADR 26-10-14-894 §1).
var ErrSkillsSourceConflict = errors.New("skills source conflicts with the project's list")

// CreateProjectSkillsSource appends a skills source to its project's list.
// The check against the list and the insert share a transaction that holds
// the project's row for update first, so two adds at once take turns: on a
// database with more than one writer the second sees the first's row, and
// the unique index alone would not refuse one repository at two refs.
func (s *Store) CreateProjectSkillsSource(ctx context.Context, source *model.ProjectSkillsSource) error {
	return s.Transaction(ctx, func(_ *Store, tx *gorm.DB) error {
		var project model.Project
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&project, "id = ?", source.ProjectID).Error; err != nil {
			return err
		}
		// One repository however its URL is spelled (ADR 26-10-15-664 §4):
		// the client expands owner/repo to a spelling an earlier entry may
		// not share, and a create checks every entry out alike.
		var listed []model.ProjectSkillsSource
		if err := tx.Where("project_id = ?", source.ProjectID).Find(&listed).Error; err != nil {
			return err
		}
		repository := sourceref.Ref{Kind: sourceref.Git, Location: source.URL}.RepositoryKey()
		var existing []model.ProjectSkillsSource
		for _, other := range listed {
			if (sourceref.Ref{Kind: sourceref.Git, Location: other.URL}).RepositoryKey() == repository {
				existing = append(existing, other)
			}
		}
		var last struct{ Position *int64 }
		if err := tx.Model(&model.ProjectSkillsSource{}).Select("MAX(position) AS position").Where("project_id = ?", source.ProjectID).Scan(&last).Error; err != nil {
			return err
		}
		source.Position = 1
		if last.Position != nil {
			source.Position = *last.Position + 1
		}
		for _, other := range existing {
			if other.Ref != source.Ref {
				return fmt.Errorf("%w: the project names %s at %s", ErrSkillsSourceConflict, other.URL, refOrDefault(other.Ref))
			}
			if other.Directory == source.Directory {
				return fmt.Errorf("%w: the project already names %s", ErrSkillsSourceConflict, other.ID)
			}
		}
		return tx.Create(source).Error
	})
}

func refOrDefault(ref string) string {
	if ref == "" {
		return "its default branch"
	}
	return ref
}

func (s *Store) GetProjectSkillsSource(ctx context.Context, projectID, sourceID string) (*model.ProjectSkillsSource, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.ProjectSkillsSource](read.Where("project_id = ?", projectID), "id", sourceID)
}

// ListProjectSkillsSources returns a project's skills sources in the order
// they were added, which is the order a creating client adds them in.
func (s *Store) ListProjectSkillsSources(ctx context.Context, projectID string) ([]model.ProjectSkillsSource, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.ProjectSkillsSource
	err = read.Where("project_id = ?", projectID).Order("position ASC").Find(&out).Error
	return out, err
}

// DeleteProjectSkillsSource removes a skills source. The lookup and the
// delete share a transaction because sourceID may be an ID prefix
// (firstByID), so the row the delete removes is the row the lookup resolved.
func (s *Store) DeleteProjectSkillsSource(ctx context.Context, projectID, sourceID string) error {
	return s.Transaction(ctx, func(_ *Store, tx *gorm.DB) error {
		source, err := firstByID[model.ProjectSkillsSource](tx.Where("project_id = ?", projectID), "id", sourceID)
		if err != nil {
			return err
		}
		return tx.Delete(source).Error
	})
}
