package model

import (
	"time"
)

// Peer is a machine permitted to connect to this server (ADR 0095). It is the
// managed half of the two authorization layers, beside the server-wide
// `<data dir>/authorized_ids` file, which is not a database resource and is
// what an operator falls back to when the API is what they are trying to
// reach.
//
// An enrolled peer authenticates as the server's default user and therefore
// reaches everything that user reaches; it is not scoped to a project, because
// the connection carries the entire control-plane API (ADR 0052 §5).
type Peer struct {
	// ID is the peer ID (ADR 0097 §1), stored in its compact form, and it is
	// the primary key. There is no generated identifier: the peer ID is
	// already unique and stable, and it is the exact string an operator pasted
	// and would find in authorized_ids. A second name for it would be the only
	// one that is not the address (ADR 0095 §2).
	ID string `gorm:"primaryKey;type:text" json:"id" doc:"Peer ID"`

	Name      string    `gorm:"column:name;not null;type:text;default:''" json:"name,omitempty" doc:"Optional label for the enrollment"`
	CreatedAt time.Time `json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt time.Time `json:"updatedAt" doc:"Last update timestamp" format:"date-time"`
}

func (Peer) TableName() string { return "peers" }
