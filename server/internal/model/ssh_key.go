package model

import (
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/x/id"
)

// SSHKey is a project-scoped public key that authorizes SSH connections to
// that project's sandboxes (ADR 0024 §5). It is distinct from the server-wide
// `<data dir>/authorized_keys` file, which authenticates as the server's
// default user and is not a database resource.
type SSHKey struct {
	ID          string    `gorm:"primaryKey;type:text" json:"id" doc:"Stable SSH key ID"`
	ProjectID   string    `gorm:"column:project_id;not null;type:text;index;uniqueIndex:idx_ssh_key_project_fingerprint,priority:1" json:"projectId" doc:"Project ID"`
	Name        string    `gorm:"column:name;not null;type:text;default:''" json:"name,omitempty" doc:"Optional label for the key"`
	PublicKey   string    `gorm:"column:public_key;not null;type:text" json:"publicKey" doc:"Normalized authorized_keys(5) public key line (type and base64 blob only)"`
	Fingerprint string    `gorm:"column:fingerprint;not null;type:text;uniqueIndex:idx_ssh_key_project_fingerprint,priority:2" json:"fingerprint" doc:"SHA256 fingerprint of the public key"`
	Comment     string    `gorm:"column:comment;not null;type:text;default:''" json:"comment,omitempty" doc:"Trailing comment from the public key line"`
	CreatedBy   string    `gorm:"column:created_by;not null;type:text;default:''" json:"createdBy,omitempty" doc:"Principal ID that enrolled this key"`
	CreatedAt   time.Time `json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt   time.Time `json:"updatedAt" doc:"Last update timestamp" format:"date-time"`

	Project *Project `gorm:"foreignKey:ProjectID" json:"-"`
}

func (SSHKey) TableName() string { return "ssh_keys" }

func (k *SSHKey) EventProjectID() string    { return k.ProjectID }
func (k *SSHKey) EventResourceType() string { return "sshKey" }
func (k *SSHKey) EventResourceID() string   { return k.ID }

func (k *SSHKey) BeforeCreate(_ *gorm.DB) error {
	if k.ID == "" {
		var err error
		k.ID, err = id.New(id.PrefixSSHKey)
		if err != nil {
			return err
		}
	}
	return nil
}
