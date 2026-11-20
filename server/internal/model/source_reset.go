package model

// SandboxSourceReset records the latest reset separately from the create
// manifest. Its revision is local to the named source.
type SandboxSourceReset struct {
	SandboxID       string `gorm:"primaryKey;type:text"`
	Slug            string `gorm:"primaryKey;type:text"`
	Revision        int64  `gorm:"not null"`
	Commit          string `gorm:"type:text;not null"`
	AppliedRevision int64  `gorm:"not null;default:0"`
	Actor           string `gorm:"type:text;not null"`
}

func (SandboxSourceReset) TableName() string { return "sandbox_source_resets" }
