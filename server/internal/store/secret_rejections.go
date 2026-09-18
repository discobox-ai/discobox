package store

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// Recorded rejections: credentials an upstream refused that nothing on this
// side can renew (ADR 0132 §4).
//
// Every write here is keyed by the same pair the table is keyed by — project,
// secret, host — because that pair *is* the fact. A credential refused at one
// host says nothing about another, and the same credential refused twice at the
// same host is one thing that keeps happening rather than two things.

// RecordSecretRejection stores a rejection, or updates the one already
// standing for that secret and host.
//
// The first sighting is kept as well as the last: how long a credential has
// been dead is what tells an hour of confusion from a minute of it, and only
// the first report knows when it started.
func (s *Store) RecordSecretRejection(ctx context.Context, rejection *model.SecretRejection) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if rejection.FirstSeenAt.IsZero() {
		rejection.FirstSeenAt = now
	}
	rejection.LastSeenAt = now
	if rejection.Count <= 0 {
		rejection.Count = 1
	}
	return write.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "project_id"}, {Name: "secret_id"}, {Name: "host"}},
		DoUpdates: clause.Assignments(map[string]any{
			// first_seen_at is deliberately absent: it belongs to the standing
			// row, and an update that carried it would keep resetting the
			// answer to "how long has this been broken".
			"reason":       rejection.Reason,
			"sandbox_id":   rejection.SandboxID,
			"use_id":       rejection.UseID,
			"last_seen_at": rejection.LastSeenAt,
			"count":        gorm.Expr("secret_rejections.count + 1"),
		}),
	}).Create(rejection).Error
}

// GetSecretRejection returns the rejection standing for one secret and host, or
// nil when the credential is not known to be refused there.
func (s *Store) GetSecretRejection(ctx context.Context, projectID, secretID, host string) (*model.SecretRejection, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out model.SecretRejection
	err = read.Where("project_id = ? AND secret_id = ? AND host = ?", projectID, secretID, host).Take(&out).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &out, nil
}

// ListSecretRejections returns a project's standing rejections, worst first:
// the one that has been failing longest is the one somebody has been living
// with.
func (s *Store) ListSecretRejections(ctx context.Context, projectID string) ([]model.SecretRejection, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.SecretRejection
	err = read.Where("project_id = ?", projectID).Order("first_seen_at ASC").Find(&out).Error
	return out, err
}

// ClearSecretRejection drops the rejection standing for one secret and host.
func (s *Store) ClearSecretRejection(ctx context.Context, projectID, secretID, host string) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Where("project_id = ? AND secret_id = ? AND host = ?", projectID, secretID, host).
		Delete(&model.SecretRejection{}).Error
}

// ClearSecretRejections drops every rejection standing against a secret,
// whatever host it was refused at.
//
// It is what a replaced credential calls: the value that was refused is gone,
// so nothing recorded about it is still true. A credential that is still wrong
// is refused again within the minute, and says so again.
func (s *Store) ClearSecretRejections(ctx context.Context, projectID, secretID string) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return s.clearSecretRejections(write, projectID, secretID)
}

func (s *Store) clearSecretRejections(tx *gorm.DB, projectID, secretID string) error {
	return tx.Where("project_id = ? AND secret_id = ?", projectID, secretID).
		Delete(&model.SecretRejection{}).Error
}
