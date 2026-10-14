package model

import (
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/x/id"
)

// idPrefixProjectSkillsSource is this package's own: nothing outside the
// server mints a project skills source.
const idPrefixProjectSkillsSource = "skills"

// ProjectSkillsSource is one skills repository every discobox of a project is
// created with (ADR 26-10-14-894 §1): the URL[@REF][//PATH] of a --skills
// value, kept apart. The creating client reads the project's list, in the
// order its entries were added, and adds each to the create request ahead of
// its own; the server never adds them itself, since a source is pinned and
// delivered by the client.
type ProjectSkillsSource struct {
	ID        string `gorm:"primaryKey;type:text" json:"id" doc:"Stable skills source ID"`
	ProjectID string `gorm:"column:project_id;not null;type:text;index;uniqueIndex:idx_project_skills_source,priority:1" json:"projectId" doc:"Project ID"`
	URL       string `gorm:"column:url;not null;type:text;uniqueIndex:idx_project_skills_source,priority:2" json:"url" doc:"The skills repository's remote Git URL"`
	// Ref is empty for the repository's default branch.
	Ref string `gorm:"column:ref;not null;type:text;default:'';uniqueIndex:idx_project_skills_source,priority:3" json:"ref,omitempty" doc:"The branch, tag or commit to check out"`
	// Directory is sandboxconfig.RootSkillsDirectory for the repository's
	// root, never empty.
	Directory string `gorm:"column:directory;not null;type:text;uniqueIndex:idx_project_skills_source,priority:4" json:"directory" doc:"The directory inside the repository that holds the skills"`
	// Position is where the entry is in its project's list: one past the
	// last when it is added. It is what the list is ordered by, since two
	// entries can share a creation time on a coarse clock.
	Position  int64     `gorm:"column:position;not null;default:0" json:"-"`
	CreatedBy string    `gorm:"column:created_by;not null;type:text;default:''" json:"createdBy,omitempty" doc:"Principal ID that added the entry"`
	CreatedAt time.Time `json:"createdAt" doc:"Creation timestamp" format:"date-time"`

	Project *Project `gorm:"foreignKey:ProjectID" json:"-"`
}

func (ProjectSkillsSource) TableName() string { return "project_skills_sources" }

func (s *ProjectSkillsSource) BeforeCreate(_ *gorm.DB) error {
	if s.ID == "" {
		var err error
		if s.ID, err = id.New(idPrefixProjectSkillsSource); err != nil {
			return err
		}
	}
	return nil
}
