package store

import (
	"context"

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

// ListCredentialVerdicts returns a sandbox's recorded verdicts, newest first —
// the read side of ADR 0091's trail. Nothing in this repository serves it over
// HTTP yet; it exists so the rows a credential use writes are checkable from a
// test today and queryable by whatever needs them later, without a second
// migration to add the ability to read back what the first one started
// writing.
func (s *Store) ListCredentialVerdicts(ctx context.Context, projectID, sandboxID string) ([]model.CredentialVerdict, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.CredentialVerdict
	err = read.Where("project_id = ? AND sandbox_id = ?", projectID, sandboxID).
		Order("created_at DESC").
		Find(&out).Error
	return out, err
}
