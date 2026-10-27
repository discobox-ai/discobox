package model

import (
	"time"

	"github.com/discobox-ai/discobox/pkg/harnesscapture"
)

// HarnessCaptureReceipt records a committed candidate without retaining its
// credential material. Its composite key scopes an agent-generated opaque ID.
type HarnessCaptureReceipt struct {
	ProjectID       string                  `gorm:"primaryKey;type:text"`
	SandboxID       string                  `gorm:"primaryKey;type:text"`
	HarnessConfigID string                  `gorm:"primaryKey;type:text"`
	CandidateID     string                  `gorm:"primaryKey;type:text"`
	Baseline        harnesscapture.Baseline `gorm:"type:text;serializer:json"`
	AcknowledgedAt  *time.Time
	Superseded      bool `gorm:"not null;default:false"`
	CreatedAt       time.Time
	Project         *Project       `gorm:"foreignKey:ProjectID;constraint:OnDelete:CASCADE" json:"-"`
	Sandbox         *Sandbox       `gorm:"foreignKey:SandboxID;constraint:OnDelete:CASCADE" json:"-"`
	HarnessConfig   *HarnessConfig `gorm:"foreignKey:HarnessConfigID;constraint:OnDelete:CASCADE" json:"-"`
}
