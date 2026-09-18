package store

import (
	"context"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// CreateCredentialVerdict persists one judge decision about an agent
// credential use (ADR 0091). It is called from the same code path that mints
// a value — before the mint, in the issuing case — so a write failure here
// must stop that path rather than let a credential out with no record of why.
func (s *Store) CreateCredentialVerdict(ctx context.Context, verdict *model.CredentialVerdict) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Create(verdict).Error
}

// CredentialVerdictFilter narrows ListCredentialVerdicts. A zero field matches
// everything, so the zero filter is the whole project.
type CredentialVerdictFilter struct {
	// ID reads the one verdict it names, which is how a caller holding an ID
	// from a listing reads that verdict in full.
	ID string
	// SandboxID is matched against the recorded ID, never resolved through the
	// sandboxes table: the trail outlives the sandbox it describes, and the
	// sandboxes most worth asking about are often already purged.
	SandboxID string
	UseID     string
	GrantID   string
	// Allow selects allowed verdicts when true and denied ones when false.
	Allow *bool
	// Since keeps verdicts recorded at or after it.
	Since time.Time
	// Ascending returns the oldest matches first, so a follower reading from
	// Since takes the rows right after its cursor rather than the newest ones.
	Ascending bool
	// Limit caps the rows returned; zero returns every match.
	Limit int
}

// ListCredentialVerdicts returns a project's recorded verdicts that match
// filter, newest first unless the filter reads forward — the read side of ADR 0091's trail, served by
// list-credential-verdicts.
func (s *Store) ListCredentialVerdicts(ctx context.Context, projectID string, filter CredentialVerdictFilter) ([]model.CredentialVerdict, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	query := read.Where("project_id = ?", projectID)
	if filter.ID != "" {
		query = query.Where("id = ?", filter.ID)
	}
	if filter.SandboxID != "" {
		query = query.Where("sandbox_id = ?", filter.SandboxID)
	}
	if filter.UseID != "" {
		query = query.Where("use_id = ?", filter.UseID)
	}
	if filter.GrantID != "" {
		query = query.Where("grant_id = ?", filter.GrantID)
	}
	if filter.Allow != nil {
		query = query.Where("allow = ?", *filter.Allow)
	}
	if !filter.Since.IsZero() {
		// UTC because created_at is written in UTC (CredentialVerdict's
		// BeforeCreate). On SQLite both sides are text with an offset and are
		// compared as text, so a bound in any other zone is off by its offset.
		query = query.Where("created_at >= ?", filter.Since.UTC())
	}
	if filter.Limit > 0 {
		query = query.Limit(filter.Limit)
	}
	var out []model.CredentialVerdict
	// id breaks ties so two verdicts recorded in the same instant come back in
	// a stable order, which is what lets a reader compare two listings. The
	// created_at order is only an order in time because every row carries the
	// same offset; see the since bound above.
	if filter.Ascending {
		query = query.Order("created_at ASC").Order("id ASC")
	} else {
		query = query.Order("created_at DESC").Order("id DESC")
	}
	err = query.Find(&out).Error
	return out, err
}
