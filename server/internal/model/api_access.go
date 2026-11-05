package model

import (
	"time"

	"github.com/discobox-ai/x/id"
	"gorm.io/gorm"
)

// APIAccessRequest records a person's decision about a sandbox's API access.
// IDs are retained after either sandbox is purged, preserving the audit.
type APIAccessRequest struct {
	ID                 string     `gorm:"primaryKey;type:text" json:"id"`
	ProjectID          string     `gorm:"not null;type:text;index" json:"projectId"`
	RequesterSandboxID string     `gorm:"not null;type:text;index" json:"requesterSandboxId"`
	Justification      string     `gorm:"not null;type:text" json:"justification"`
	Status             string     `gorm:"not null;type:text;default:'pending'" json:"status" enum:"pending,approved,denied"`
	DecidedBy          string     `gorm:"not null;type:text;default:''" json:"decidedBy,omitempty"`
	DecidedAt          *time.Time `json:"decidedAt,omitempty"`
	CreatedAt          time.Time  `json:"createdAt"`
	UpdatedAt          time.Time  `json:"updatedAt"`
}

func (r *APIAccessRequest) BeforeCreate(_ *gorm.DB) error {
	if r.ID != "" {
		return nil
	}
	var err error
	r.ID, err = id.New("apireq")
	return err
}
