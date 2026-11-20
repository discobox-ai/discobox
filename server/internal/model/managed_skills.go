package model

import "github.com/discobox-ai/discobox/pkg/sandboxconfig"

// SandboxManagedSkills is live intent, stored outside the create manifest and
// ordinary sandbox projections. Only skills operations read its content.
type SandboxManagedSkills struct {
	DesiredRevision int64                         `gorm:"not null;default:0"`
	SandboxID       string                        `gorm:"primaryKey;type:text"`
	Desired         sandboxconfig.ManagedSkillSet `gorm:"type:text;serializer:json;not null"`
	// AppliedVersions keeps provenance while the next desired revision is pending.
	AppliedVersions map[string]sandboxconfig.SkillVersion `gorm:"type:text;serializer:json"`
	Installed       map[string]sandboxconfig.SkillVersion `gorm:"-"`
	Application     string                                `gorm:"-"`
	Applied         int64                                 `gorm:"not null;default:0"`
}

func (SandboxManagedSkills) TableName() string { return "sandbox_managed_skills" }

// SandboxSkillChange contains audit metadata only. Skill bodies belong to
// desired intent and never enter the event stream.
type SandboxSkillChange struct {
	Actor     string                      `json:"actor"`
	Operation string                      `json:"operation"`
	Revision  int64                       `json:"revision"`
	Skills    map[string]SkillVersionDiff `json:"skills,omitempty"`
}

type SkillVersionDiff struct {
	Before *sandboxconfig.SkillVersion `json:"before,omitempty"`
	After  *sandboxconfig.SkillVersion `json:"after,omitempty"`
}

// SandboxSkillRevision keeps metadata for a revision that can still be
// acknowledged. It contains no skill bodies.
type SandboxSkillRevision struct {
	SandboxID string                                `gorm:"primaryKey;type:text"`
	Revision  int64                                 `gorm:"primaryKey;autoIncrement:false"`
	Versions  map[string]sandboxconfig.SkillVersion `gorm:"type:text;serializer:json"`
}

func (SandboxSkillRevision) TableName() string { return "sandbox_skill_revisions" }
