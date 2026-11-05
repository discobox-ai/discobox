package store

import (
	"context"

	"github.com/discobox-ai/discobox/server/internal/model"
	"gorm.io/gorm/clause"
)

// LockHandoffSandboxes locks the project before the caller and target, in ID
// order, matching adoption, release and purge. It must run in a write transaction.
// The caller decides authorization from the returned current rows.
func (s *Store) LockHandoffSandboxes(ctx context.Context, projectID, holderID, targetID string) ([]model.Sandbox, error) {
	tx, err := s.getWrite(ctx)
	if err != nil {
		return nil, err
	}
	if err := lockProject(tx, projectID); err != nil {
		return nil, err
	}
	var rows []model.Sandbox
	err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("project_id = ? AND id IN ?", projectID, []string{holderID, targetID}).Order("id").Find(&rows).Error
	return rows, err
}

// LockHandoffDelegations locks the secret then the candidate binding rows in
// ID order. Revocation locks each parent before discovering its children, so
// a concurrent insert is either rejected or included in the cascade.
func (s *Store) LockHandoffDelegations(ctx context.Context, projectID, secretID string, ids []string) (*model.Secret, []model.SecretBinding, error) {
	tx, err := s.getWrite(ctx)
	if err != nil {
		return nil, nil, err
	}
	secret, err := firstByID[model.Secret](tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("project_id = ?", projectID), "id", secretID)
	if err != nil {
		return nil, nil, err
	}
	var parents []model.SecretBinding
	if len(ids) > 0 {
		err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("project_id = ? AND id IN ?", projectID, ids).Order("id").Find(&parents).Error
	}
	return secret, parents, err
}
