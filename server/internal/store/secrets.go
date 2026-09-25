package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/hostscope"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/secrets"
)

const secretValuePurpose = "secrets.value"

func secretResourceID(secret *model.Secret) string {
	return secret.ProjectID + "/" + secret.ID
}

func (s *Store) sealSecretForWrite(ctx context.Context, secret *model.Secret) (*model.Secret, error) {
	if secret.ID == "" {
		if err := secret.BeforeCreate(nil); err != nil {
			return nil, err
		}
	}
	persisted := *secret
	ciphertext, err := secrets.SealIfUnsealed(ctx, s.sealer, secretValuePurpose, secretResourceID(secret), secret.EncryptedValue)
	if err != nil {
		return nil, fmt.Errorf("encrypt secret value: %w", err)
	}
	persisted.EncryptedValue = ciphertext
	return &persisted, nil
}

// OpenSecretValue decrypts a secret's value and deserializes it.
func (s *Store) OpenSecretValue(ctx context.Context, secret *model.Secret) (*model.SecretValue, error) {
	if secret == nil || len(secret.EncryptedValue) == 0 {
		return nil, nil
	}
	// Pass through plaintext rows (written before encryption was enabled) unchanged,
	// matching the guard in OpenSandboxSecretState.
	if s.sealer == nil || !secrets.IsSealed(secret.EncryptedValue) {
		var val model.SecretValue
		if err := json.Unmarshal(secret.EncryptedValue, &val); err != nil {
			return nil, fmt.Errorf("unmarshal secret value: %w", err)
		}
		return &val, nil
	}
	plaintext, err := secrets.Open(ctx, s.sealer, secretValuePurpose, secretResourceID(secret), secret.EncryptedValue)
	if err != nil {
		return nil, fmt.Errorf("decrypt secret value: %w", err)
	}
	var val model.SecretValue
	if err := json.Unmarshal(plaintext, &val); err != nil {
		return nil, fmt.Errorf("unmarshal secret value: %w", err)
	}
	return &val, nil
}

func (s *Store) CreateSecret(ctx context.Context, secret *model.Secret) error {
	// A new secret's value is written now, whichever writer made it — the
	// secrets service, the harness configure flow, an anonymous inline value.
	if secret.ValueUpdatedAt == nil {
		secret.ValueWritten(time.Now().UTC(), secret.ValueExpiresAt)
	}
	sealed, err := s.sealSecretForWrite(ctx, secret)
	if err != nil {
		return err
	}
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	if err := write.Create(sealed).Error; err != nil {
		return err
	}
	*secret = *sealed
	return nil
}

func (s *Store) GetSecret(ctx context.Context, projectID, secretID string) (*model.Secret, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.Secret](read.Where("project_id = ?", projectID), "id", secretID)
}

// FindSecretByWellKnownID returns the project's secret marked as fulfilling a
// well-known credential, or ErrNotFound.
func (s *Store) FindSecretByWellKnownID(ctx context.Context, projectID, wellKnownID string) (*model.Secret, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.Secret
	if err := read.Where("project_id = ? AND well_known_id = ?", projectID, wellKnownID).Limit(1).Find(&out).Error; err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return &out[0], nil
}

// MarkSecretWellKnown marks a secret as fulfilling a well-known credential
// unless the project already has a secret marked with it, or the secret
// carries another ID; either way it leaves the marks as they are. It writes that one column and nothing else: the row a
// caller read may already be stale (an OAuth refresh rotates the value), and
// the mark is not a change to the credential (ADR 0132 §4).
func (s *Store) MarkSecretWellKnown(ctx context.Context, projectID, secretID, wellKnownID string) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	taken := write.Model(&model.Secret{}).Select("1").Where("project_id = ? AND well_known_id = ?", projectID, wellKnownID)
	res := write.Model(&model.Secret{}).
		Where("project_id = ? AND id = ? AND well_known_id = ''", projectID, secretID).
		Where("NOT EXISTS (?)", taken).
		UpdateColumn("well_known_id", wellKnownID)
	if res.Error != nil {
		// Two first approvals racing past NOT EXISTS: the partial unique
		// index refuses the second, and the first one's mark stands.
		if _, findErr := s.FindSecretByWellKnownID(ctx, projectID, wellKnownID); findErr == nil {
			return nil
		}
		return res.Error
	}
	return nil
}

// SetSecretLimits writes a secret's host binding, its grant limit, or both,
// and nothing else, for an approval that changes them in the transaction
// minting its grant. A nil field is left as it is: writing back what the caller
// read would revert a change made since. A full save would write back the rest
// of the row too, and the value may have been rotated since (an OAuth refresh);
// neither column is the credential, so nothing recorded about it is retracted
// (ADR 0132 §4).
func (s *Store) SetSecretLimits(ctx context.Context, projectID, secretID string, host *string, maxGrantTTL *int64) error {
	columns := map[string]any{}
	if host != nil {
		columns["host"] = *host
	}
	if maxGrantTTL != nil {
		columns["max_grant_ttl_seconds"] = *maxGrantTTL
	}
	if len(columns) == 0 {
		return nil
	}
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	res := write.Model(&model.Secret{}).
		Where("project_id = ? AND id = ?", projectID, secretID).
		Updates(columns)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListSecrets(ctx context.Context, projectID string) ([]model.Secret, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.Secret
	err = read.Where("project_id = ? AND anonymous = ?", projectID, false).Order("created_at ASC").Find(&out).Error
	return out, err
}

func (s *Store) UpdateSecret(ctx context.Context, secret *model.Secret) error {
	// Whether this write replaces the credential, which is what decides
	// whether a recorded rejection of it is still true (ADR 0132 §4).
	//
	// The test is against what is stored rather than against the shape of what
	// arrived, because it has to hold both with a sealer and without one. A
	// caller replacing a value hands over plaintext, which never equals the
	// stored ciphertext; a caller renaming hands back the row it read, whose
	// value is byte-identical to it. With no sealer both sides are plaintext
	// and the same comparison still separates them.
	//
	// It lives here rather than in each service because "the value was
	// replaced" has more than one writer — the secrets service, the harness
	// configure flow's update-in-place, and whatever is added next — and a rule
	// enforced in one of them is a rule the others walk around.
	stored, replacedValue, err := s.secretValueReplaced(ctx, secret)
	if err != nil {
		return err
	}
	// A replaced value is a new value, and its lifetime starts now
	// (ADR 26-09-25-122 §1). A writer that stamped it already — with the
	// expiry the value carries — keeps its stamp; every other writer gets one
	// here, so no path can put a new value behind an old value's clock.
	if replacedValue && !stampedSince(secret, stored) {
		secret.ValueWritten(time.Now().UTC(), nil)
	}
	sealed, err := s.sealSecretForWrite(ctx, secret)
	if err != nil {
		return err
	}
	// The write and the retraction are one act, the way DeleteSecret's cascade
	// is: a row saying a credential was refused, standing against a credential
	// that is no longer there, is the state this must not leave behind. The
	// same goes for an ask for a new value, which the write has answered.
	if err := s.Transaction(ctx, func(_ *Store, tx *gorm.DB) error {
		// When the value stays, so do the fields that describe it: they have
		// a writer of their own (MarkSecretValueStale), and a row read before
		// that write must not put the old ones back (store DESIGN, "Field
		// Ownership on Whole-Row Writes").
		save := tx
		if !replacedValue {
			save = tx.Omit("value_updated_at", "value_expires_at")
		}
		if err := save.Save(sealed).Error; err != nil {
			return err
		}
		if !replacedValue {
			return nil
		}
		if err := s.clearSecretRejections(tx, sealed.ProjectID, sealed.ID); err != nil {
			return err
		}
		return answerRefreshRequests(tx, sealed.ProjectID, sealed.ID, &model.SecretRefreshAnswer{
			AnsweredAt: time.Now().UTC(),
			Via:        model.SecretRefreshViaUpdate,
		})
	}); err != nil {
		return err
	}
	*secret = *sealed
	return nil
}

// stampedSince reports whether the writer stamped the value's write time
// itself, past what is stored.
func stampedSince(secret, stored *model.Secret) bool {
	if secret.ValueUpdatedAt == nil {
		return false
	}
	return stored == nil || stored.ValueUpdatedAt == nil || secret.ValueUpdatedAt.After(*stored.ValueUpdatedAt)
}

// secretValueReplaced reports whether an update carries a different credential
// than the row already holds, with the stored row when there is one. A secret
// that does not exist yet, or a write carrying no value at all, replaces
// nothing.
func (s *Store) secretValueReplaced(ctx context.Context, secret *model.Secret) (*model.Secret, bool, error) {
	if len(secret.EncryptedValue) == 0 || secret.ID == "" {
		return nil, false, nil
	}
	stored, err := s.GetSecret(ctx, secret.ProjectID, secret.ID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return stored, !bytes.Equal(stored.EncryptedValue, secret.EncryptedValue), nil
}

// MarkSecretValueStale makes a secret's current value stale from at, without
// touching the value: an upstream refused it, and the next resolve asks for a
// new one (ADR 26-09-25-122 §3). It reports whether it did: a value written at
// or after writtenBefore is not the one that was refused, and is left alone,
// decided in the write itself so a renewal committing in between cannot be
// marked stale by a report about the value it replaced.
func (s *Store) MarkSecretValueStale(ctx context.Context, projectID, secretID string, at, writtenBefore time.Time) (bool, error) {
	write, err := s.getWrite(ctx)
	if err != nil {
		return false, err
	}
	res := write.Model(&model.Secret{}).
		Where("project_id = ? AND id = ? AND (value_updated_at IS NULL OR value_updated_at < ?)", projectID, secretID, writtenBefore).
		UpdateColumn("value_expires_at", at)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// FindPendingRefreshRequest returns the open refresh request on a secret, or
// ErrNotFound. There is at most one: staleness belongs to the secret, not to
// the discobox that noticed it.
func (s *Store) FindPendingRefreshRequest(ctx context.Context, projectID, secretID string) (*model.SecretRequest, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var req model.SecretRequest
	err = read.Where("project_id = ? AND secret_id = ? AND reason = ? AND status = ?",
		projectID, secretID, model.SecretRequestReasonRefresh, model.SecretRequestStatusPending).
		Order("created_at DESC").First(&req).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &req, nil
}

// SetRefreshRequestSandbox records the discobox that most recently needed an
// open refresh request's value, so the window's banner follows the work.
func (s *Store) SetRefreshRequestSandbox(ctx context.Context, projectID, requestID, sandboxID string) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Model(&model.SecretRequest{}).
		Where("project_id = ? AND id = ? AND status = ?", projectID, requestID, model.SecretRequestStatusPending).
		Updates(map[string]any{"sandbox_id": sandboxID, "requested_by": "sandbox:" + sandboxID}).Error
}

// AnswerRefreshRequest closes one open refresh request with how it was
// answered. It returns ErrGenerationConflict when the request is no longer
// open: somebody answered first.
func (s *Store) AnswerRefreshRequest(ctx context.Context, projectID, requestID string, answer *model.SecretRefreshAnswer) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	res := write.Model(&model.SecretRequest{}).
		Where("project_id = ? AND id = ? AND reason = ? AND status = ?", projectID, requestID, model.SecretRequestReasonRefresh, model.SecretRequestStatusPending).
		Select("status", "refresh_answer", "closed_at").
		Updates(&model.SecretRequest{Status: model.SecretRequestStatusApproved, RefreshAnswer: answer, ClosedAt: closedAt(answer.AnsweredAt)})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrGenerationConflict
	}
	return nil
}

// answerRefreshRequests closes every open refresh request on a secret with
// one answer: a new value was written, so none of them is waiting on anything.
func answerRefreshRequests(tx *gorm.DB, projectID, secretID string, answer *model.SecretRefreshAnswer) error {
	return tx.Model(&model.SecretRequest{}).
		Where("project_id = ? AND secret_id = ? AND reason = ? AND status = ?", projectID, secretID, model.SecretRequestReasonRefresh, model.SecretRequestStatusPending).
		Select("status", "refresh_answer", "closed_at").
		Updates(&model.SecretRequest{Status: model.SecretRequestStatusApproved, RefreshAnswer: answer, ClosedAt: closedAt(answer.AnsweredAt)}).Error
}

// UpdateSecretValueIfUnchanged replaces a secret's encrypted value only if its
// row has not been updated since prevUpdatedAt. It is the atomic swap the OAuth
// refresh relies on: a concurrent refresh in another process bumps updated_at,
// so the loser sees zero rows affected (ErrGenerationConflict) and re-reads the
// winner's freshly rotated credential instead of clobbering it with a stale one.
func (s *Store) UpdateSecretValueIfUnchanged(ctx context.Context, secret *model.Secret, prevUpdatedAt time.Time) error {
	sealed, err := s.sealSecretForWrite(ctx, secret)
	if err != nil {
		return err
	}
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	result := write.Model(&model.Secret{}).
		Where("project_id = ? AND id = ? AND updated_at = ?", sealed.ProjectID, sealed.ID, prevUpdatedAt).
		Update("encrypted_value", sealed.EncryptedValue)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrGenerationConflict
	}
	*secret = *sealed
	return nil
}

func (s *Store) DeleteSecret(ctx context.Context, projectID, secretID string) error {
	return s.Transaction(ctx, func(_ *Store, tx *gorm.DB) error {
		sec, err := firstByID[model.Secret](tx.Where("project_id = ?", projectID), "id", secretID)
		if err != nil {
			return err
		}
		// Null the encrypted value before the row is deleted below, so the
		// ciphertext is overwritten rather than only unlinked with its row.
		if err := tx.Model(sec).Update("encrypted_value", nil).Error; err != nil {
			return err
		}
		// Drop harness-config bindings, standing grants, and sandbox sentinel
		// assignments that reference this secret so nothing dangles.
		if err := s.deleteHarnessConfigSecretBindingsBySecret(tx, secretID); err != nil {
			return err
		}
		if err := s.deleteSecretGrantsBySecret(tx, secretID); err != nil {
			return err
		}
		if err := s.deleteSandboxSecretsBySecret(tx, secretID); err != nil {
			return err
		}
		if err := s.clearSecretRejections(tx, projectID, secretID); err != nil {
			return err
		}
		return tx.Delete(sec).Error
	})
}

// MatchSecret finds the most specific secret for a project+type+host combination.
// An exact host match beats a wildcard (empty host) match. Returns an error if
// the result is ambiguous (multiple secrets at the same specificity).
func (s *Store) MatchSecret(ctx context.Context, projectID, secretType, host string) (*model.Secret, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	// The host is filtered in Go, for the reason FindLiveGrant is: a binding
	// covers the hosts beneath it (hostscope.Covers), which SQL equality cannot
	// express — and a secret bound to github.com is a candidate for a request
	// about api.github.com, or the reactive path would open a request for a
	// credential the project already holds.
	var rows []model.Secret
	err = read.
		Where("project_id = ? AND anonymous = ? AND type = ?", projectID, false, secretType).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	var candidates []model.Secret
	for _, row := range rows {
		if hostscope.Covers(row.Host, host) {
			candidates = append(candidates, row)
		}
	}
	if len(candidates) == 0 {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "no matching secret found for the requested type and host")
	}
	// The narrowest binding that answers wins: the host itself, then a host
	// above it, then a secret bound to nothing.
	best := hostscope.Specificity(candidates[0].Host, host)
	for _, c := range candidates[1:] {
		best = min(best, hostscope.Specificity(c.Host, host))
	}
	var pool []model.Secret
	for _, c := range candidates {
		if hostscope.Specificity(c.Host, host) == best {
			pool = append(pool, c)
		}
	}
	if len(pool) > 1 {
		return nil, apperrors.NewStatusError(http.StatusConflict, "ambiguous secret match: multiple secrets of the same type and host exist")
	}
	return &pool[0], nil
}

func (s *Store) CreateSecretRequest(ctx context.Context, req *model.SecretRequest) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Create(req).Error
}

// FindPendingSecretRequest returns the most recent pending secret request for a
// specific secret, host, and requesting principal. It powers on-demand sentinel
// resolution so repeated proxy lookups reuse an open request instead of piling
// up duplicates while it waits for approval.
func (s *Store) FindPendingSecretRequest(ctx context.Context, projectID, secretID, host, requestedBy string) (*model.SecretRequest, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var req model.SecretRequest
	// A refresh request names the same secret and discobox, and asks for
	// something else: a value, not a grant. It is never the reactive ask.
	err = read.Where("project_id = ? AND secret_id = ? AND host = ? AND requested_by = ? AND status = ? AND reason = ''",
		projectID, secretID, host, requestedBy, model.SecretRequestStatusPending).
		Order("created_at DESC").First(&req).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &req, nil
}

// FindPendingAgentCredentialRequest returns the open protocol-originated
// request for a sandbox's environment variable, destination host, well-known
// ID (empty for an ask that names none), and purpose, or ErrNotFound. The ID
// and the purpose are part of the key because each changes what approving the
// request mints: an ask to delegate is not a retry of an ask to use.
//
// It keys on (sandbox, env, host) rather than on the secret the way the
// reactive path does, because a protocol request names no secret: choosing one
// is part of the approval. An agent that retries its ask therefore reuses its
// open request instead of adding another line to the approval inbox.
func (s *Store) FindPendingAgentCredentialRequest(ctx context.Context, projectID, sandboxID, envName, host, wellKnownID, purpose string) (*model.SecretRequest, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.SecretRequest
	err = read.Where("project_id = ? AND sandbox_id = ? AND env_name = ? AND host = ? AND well_known_id = ? AND purpose = ? AND status = ?",
		projectID, sandboxID, envName, host, wellKnownID, purpose, model.SecretRequestStatusPending).
		Order("created_at DESC").Find(&out).Error
	if err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].FromProtocol() {
			return &out[i], nil
		}
	}
	return nil, ErrNotFound
}

func (s *Store) GetSecretRequest(ctx context.Context, projectID, requestID string) (*model.SecretRequest, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.SecretRequest](read.Where("project_id = ?", projectID), "id", requestID)
}

// ListSecretRequests returns a project's secret requests, optionally filtered to
// a single status.
func (s *Store) ListSecretRequests(ctx context.Context, projectID, status string) ([]model.SecretRequest, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	query := read.Where("project_id = ?", projectID)
	if status = strings.TrimSpace(status); status != "" {
		query = query.Where("status = ?", status)
	}
	var out []model.SecretRequest
	err = query.Order("created_at ASC").Find(&out).Error
	return out, err
}

// UpdateSecretRequestIfPending atomically transitions a SecretRequest out of
// pending status. It returns ErrGenerationConflict if the request is no longer
// pending (concurrent approve or deny beat this caller).
func (s *Store) UpdateSecretRequestIfPending(ctx context.Context, req *model.SecretRequest) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	if req.Status != model.SecretRequestStatusPending && req.ClosedAt == nil {
		req.ClosedAt = closedAt(time.Now())
	}
	result := write.Model(&model.SecretRequest{}).
		Where("project_id = ? AND id = ? AND status = ?", req.ProjectID, req.ID, model.SecretRequestStatusPending).
		Select("*").
		Updates(req)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrGenerationConflict
	}
	return nil
}

// AnswerOpenRefreshRequests closes every open refresh request on a secret
// with the answer given, for a writer that knows more about the value than an
// ordinary update does.
func (s *Store) AnswerOpenRefreshRequests(ctx context.Context, projectID, secretID string, answer *model.SecretRefreshAnswer) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return answerRefreshRequests(write, projectID, secretID, answer)
}

// closedAt is when a request stopped pending, in UTC: the refresh trail bounds
// its reads by it, and SQLite compares times as text (see
// SecretRequest.BeforeCreate).
func closedAt(at time.Time) *time.Time {
	at = at.UTC()
	return &at
}
