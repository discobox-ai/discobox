package model

import (
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/targetdb"
	"github.com/discobox-ai/x/id"
)

// idPrefixTargetEntry is this package's own: nothing outside the server mints
// a target entry.
const idPrefixTargetEntry = "tgt"

// TargetEntry is a project's own entry in the target database (ADR
// 26-10-20-782 §6): what is known about a host, added by a person, or put in
// place of the built-in entry for the same host. It is trusted input, and a
// discobox never writes one.
//
// The documents' text is not here: a document is either fetched from its URL
// and checked against its digest, or stored as a TargetDocumentText row.
type TargetEntry struct {
	ID        string `gorm:"primaryKey;type:text" json:"-"`
	ProjectID string `gorm:"column:project_id;not null;type:text;index;uniqueIndex:idx_target_entry_host,priority:1" json:"-"`
	Host      string `gorm:"column:host;not null;type:text;uniqueIndex:idx_target_entry_host,priority:2" json:"host"`

	Documents  []targetdb.Document `gorm:"column:documents;type:text;serializer:json" json:"documents"`
	Terms      []targetdb.Term     `gorm:"column:terms;type:text;serializer:json" json:"terms"`
	Statements []targetdb.Shape    `gorm:"column:statements;type:text;serializer:json" json:"statements"`
	// Version is targetdb.Entry.Version of what the row holds, written with it.
	Version string `gorm:"column:version;not null;type:text" json:"version"`

	UpdatedBy string    `gorm:"column:updated_by;not null;type:text;default:''" json:"updatedBy,omitempty"`
	CreatedAt time.Time `json:"createdAt,omitzero"`
	UpdatedAt time.Time `json:"updatedAt,omitzero"`

	// Builtin and ReplacesBuiltin say where the entry a project sees comes
	// from. They are derived, never stored: a built-in entry has no row.
	Builtin         bool `gorm:"-" json:"builtin"`
	ReplacesBuiltin bool `gorm:"-" json:"replacesBuiltin"`

	Project *Project `gorm:"foreignKey:ProjectID" json:"-"`
}

func (TargetEntry) TableName() string { return "target_entries" }

func (e *TargetEntry) BeforeCreate(_ *gorm.DB) error {
	if e.ID == "" {
		var err error
		if e.ID, err = id.New(idPrefixTargetEntry); err != nil {
			return err
		}
	}
	return nil
}

// Entry is the row as the target package reads it.
func (e TargetEntry) Entry() targetdb.Entry {
	return targetdb.Entry{Host: e.Host, Documents: e.Documents, Terms: e.Terms, Statements: e.Statements}
}

// TargetDocumentText is the text of a document a person gave a project's
// entry rather than a URL to fetch it from. It lives apart from the entry so
// that reading an entry never loads megabytes of text.
type TargetDocumentText struct {
	EntryID string `gorm:"primaryKey;column:entry_id;type:text"`
	// ProjectID is the entry's, so a project's deletion reaches the text.
	ProjectID string `gorm:"column:project_id;not null;type:text;index"`
	Name      string `gorm:"primaryKey;column:name;type:text"`
	Text      []byte `gorm:"column:text;not null"`

	Entry *TargetEntry `gorm:"foreignKey:EntryID"`
}

func (TargetDocumentText) TableName() string { return "target_document_texts" }
