package model

import (
	"time"

	"github.com/discobox-ai/x/id"
	"gorm.io/gorm"
)

// AdoptionRequest records a person's decision about who leads a sandbox.
// IDs are retained after either sandbox is purged, preserving the audit.
type AdoptionRequest struct {
	CurrentParentSandboxID  *string    `gorm:"-" json:"currentParentSandboxId,omitempty"`
	ChildName               string     `gorm:"-" json:"childName,omitempty"`
	RequesterName           string     `gorm:"-" json:"requesterName,omitempty"`
	ReadOnly                bool       `gorm:"-" json:"readOnly"`
	ID                      string     `gorm:"primaryKey;type:text" json:"id"`
	ProjectID               string     `gorm:"not null;type:text;index" json:"projectId"`
	RequesterSandboxID      string     `gorm:"not null;type:text;index" json:"requesterSandboxId"`
	ChildSandboxID          string     `gorm:"not null;type:text;index" json:"childSandboxId"`
	Justification           string     `gorm:"not null;type:text" json:"justification"`
	ReplacedParentSandboxID *string    `gorm:"type:text" json:"replacedParentSandboxId,omitempty"`
	Status                  string     `gorm:"not null;type:text;default:'pending'" json:"status" enum:"pending,approved,denied"`
	DecidedBy               string     `gorm:"not null;type:text;default:''" json:"decidedBy,omitempty"`
	DecidedAt               *time.Time `json:"decidedAt,omitempty"`
	CreatedAt               time.Time  `json:"createdAt"`
	UpdatedAt               time.Time  `json:"updatedAt"`
}

func (r *AdoptionRequest) BeforeCreate(_ *gorm.DB) error {
	if r.ID != "" {
		return nil
	}
	var err error
	r.ID, err = id.New("adopt")
	return err
}
