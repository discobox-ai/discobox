package store

import (
	"context"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// Approval rules (ADR 26-10-12-330). Grants a rule minted name it and outlive
// it. A rule goes with its project, with the secret it names (DeleteSecret),
// and with the discobox it is pinned to (DeleteSandbox): one that could never
// approve again would still count as a reason to keep a judge.

func (s *Store) CreateApprovalRule(ctx context.Context, rule *model.ApprovalRule) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Create(rule).Error
}

func (s *Store) GetApprovalRule(ctx context.Context, projectID, ruleID string) (*model.ApprovalRule, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.ApprovalRule](read.Where("project_id = ?", projectID), "id", ruleID)
}

// ListApprovalRules returns a project's rules, oldest first.
func (s *Store) ListApprovalRules(ctx context.Context, projectID string) ([]model.ApprovalRule, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.ApprovalRule
	err = read.Where("project_id = ?", projectID).Order("created_at ASC").Order("id ASC").Find(&out).Error
	return out, err
}

// UpdateApprovalRule saves a rule in place. Every column is written, so a rule
// disabled, or one whose cap was set to zero, is stored as that.
func (s *Store) UpdateApprovalRule(ctx context.Context, rule *model.ApprovalRule) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	result := write.Model(&model.ApprovalRule{}).
		Where("project_id = ? AND id = ?", rule.ProjectID, rule.ID).
		Select("*").Omit("created_at").
		Updates(rule)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteApprovalRule removes a rule. The grants it minted stay: a grant is
// independent of its rule once made.
func (s *Store) DeleteApprovalRule(ctx context.Context, projectID, ruleID string) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	result := write.Where("project_id = ? AND id = ?", projectID, ruleID).Delete(&model.ApprovalRule{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// HasEnabledApprovalRule reports whether a project has a rule switched on,
// which is a reason for it to keep a judge (ADR 26-10-12-330 §6). A rule past
// its own expiry still counts: it lapses by the clock, and a judge taken away
// and brought back by the clock is churn for nothing.
func (s *Store) HasEnabledApprovalRule(ctx context.Context, projectID string) (bool, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return false, err
	}
	var count int64
	err = read.Model(&model.ApprovalRule{}).Where("project_id = ? AND enabled = ?", projectID, true).Limit(1).Count(&count).Error
	return count > 0, err
}

// ListRuleTestableRequests returns the IDs of a project's pending requests an
// approval rule could answer: asks for a grant filed by a discobox, oldest
// first. Whether a request came through the agent credentials protocol — the
// only kind a rule is tested against — is read from its uses, which is the
// caller's to check: uses are a JSON column.
func (s *Store) ListRuleTestableRequests(ctx context.Context, projectID string) ([]string, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	err = read.Model(&model.SecretRequest{}).
		Where("project_id = ? AND status = ? AND reason = '' AND sandbox_id <> ''", projectID, model.SecretRequestStatusPending).
		Order("created_at ASC").Pluck("id", &ids).Error
	return ids, err
}

// GetSecretRequestByID reads a request by its ID alone, for the reconciler
// that tests rules against it: a dirty mark carries one ID.
func (s *Store) GetSecretRequestByID(ctx context.Context, requestID string) (*model.SecretRequest, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.SecretRequest](read, "id", requestID)
}

// QuestionRecordedUnder reports whether a rule already has a verdict of its
// own for a question, so an answer reused for it is recorded under it once.
func (s *Store) QuestionRecordedUnder(ctx context.Context, projectID, kind, sandboxID, requestID, digest, ruleID string) (bool, error) {
	db, err := s.getWrite(ctx)
	if err != nil {
		return false, err
	}
	query := db.Model(&model.CredentialVerdict{}).
		Where("project_id = ? AND kind = ? AND origin = ? AND sandbox_id = ? AND question_digest = ? AND rule_id = ?",
			projectID, kind, model.CredentialVerdictOriginJudge, sandboxID, digest, ruleID)
	if requestID != "" {
		query = query.Where("secret_request_id = ?", requestID)
	}
	var count int64
	err = query.Limit(1).Count(&count).Error
	return count > 0, err
}

// AnsweredQuestion returns the newest verdict on record for one question an
// approval rule asks — a scope read against one discobox, or a rule's uses
// read against one request — or ErrNotFound when it was never answered
// (ADR 26-10-12-330 §§2, 4). digest is the question's; a rule edited since is
// a different question. requestID is empty for a scope, which is decided once
// for a discobox whatever it asks.
//
// fresh reads the primary, for whoever would otherwise ask the judge: the row
// may have been written moments ago by the proposal a person is now choosing
// from, and a replica that has not caught up would have the judge asked again.
// A reader that only reports what is on record takes a replica.
func (s *Store) AnsweredQuestion(ctx context.Context, projectID, kind, sandboxID, requestID, digest string, fresh bool) (*model.CredentialVerdict, error) {
	db, err := s.getRead(ctx)
	if fresh {
		db, err = s.getWrite(ctx)
	}
	if err != nil {
		return nil, err
	}
	query := db.Where("project_id = ? AND kind = ? AND origin = ? AND sandbox_id = ? AND question_digest = ?",
		projectID, kind, model.CredentialVerdictOriginJudge, sandboxID, digest)
	if requestID != "" {
		query = query.Where("secret_request_id = ?", requestID)
	}
	var out []model.CredentialVerdict
	if err := query.Order("created_at DESC").Order("id DESC").Limit(1).Find(&out).Error; err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return &out[0], nil
}
