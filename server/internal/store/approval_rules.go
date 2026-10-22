package store

import (
	"context"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// Approval rules (ADR 26-10-12-330). Grants a rule minted name it and outlive
// it. A rule goes with its project and with the secret it names (DeleteSecret):
// one that could never approve again would still count as a reason to keep a
// judge. It names repositories, never a discobox, so it outlives every
// discobox it answered.

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

// GetSecretRequestByGrant reads the request a grant was minted to answer,
// which approving it recorded on the request. A grant somebody minted without
// a request — and one whose request is no longer on record — has none, and
// is ErrNotFound.
func (s *Store) GetSecretRequestByGrant(ctx context.Context, projectID, grantID string) (*model.SecretRequest, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.SecretRequest
	if err := read.Where("project_id = ? AND grant_id = ?", projectID, grantID).
		Order("created_at DESC").Limit(1).Find(&out).Error; err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return &out[0], nil
}

// QuestionRecordedUnder reports whether a rule already has a verdict of its
// own for a question, so an answer reused for it is recorded under it once.
func (s *Store) QuestionRecordedUnder(ctx context.Context, projectID, sandboxID, requestID, digest, ruleID string) (bool, error) {
	db, err := s.getWrite(ctx)
	if err != nil {
		return false, err
	}
	var count int64
	err = answeredRuleQuestions(db, projectID, sandboxID, requestID, digest).
		Where("rule_id = ?", ruleID).Limit(1).Count(&count).Error
	return count > 0, err
}

// AnsweredQuestion returns the newest verdict on record for the question an
// approval rule asks — the conditions of an allow one request asked for read
// against those of the rule's allow —
// or ErrNotFound when it was never answered (ADR 26-10-12-330 §4). digest is
// the question's; a rule edited since is a different question.
//
// fresh reads the primary, for whoever would otherwise ask the judge: the row
// may have been written moments ago by the proposal a person is now choosing
// from, and a replica that has not caught up would have the judge asked again.
// A reader that only reports what is on record takes a replica.
func (s *Store) AnsweredQuestion(ctx context.Context, projectID, sandboxID, requestID, digest string, fresh bool) (*model.CredentialVerdict, error) {
	db, err := s.getRead(ctx)
	if fresh {
		db, err = s.getWrite(ctx)
	}
	if err != nil {
		return nil, err
	}
	var out []model.CredentialVerdict
	if err := answeredRuleQuestions(db, projectID, sandboxID, requestID, digest).
		Order("created_at DESC").Order("id DESC").Limit(1).Find(&out).Error; err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return &out[0], nil
}

// answeredRuleQuestions are the project's judge's verdicts on one rule
// question about one request. A discobox's own judge's word is never one.
func answeredRuleQuestions(db *gorm.DB, projectID, sandboxID, requestID, digest string) *gorm.DB {
	return db.Model(&model.CredentialVerdict{}).
		Where("project_id = ? AND kind = ? AND origin = ? AND sandbox_id = ? AND secret_request_id = ? AND question_digest = ?",
			projectID, model.CredentialVerdictKindRule, model.CredentialVerdictOriginJudge, sandboxID, requestID, digest)
}
