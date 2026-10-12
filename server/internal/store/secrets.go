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
	"github.com/discobox-ai/discobox/secretformat"
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
	// A format nobody set is the value's shape, read here on every write, for
	// every writer at once — the secrets service, the harness configure flow,
	// an anonymous inline value, an OAuth refresh — so none of them can store
	// a credential whose sentinel says nothing about what it stands for. An
	// OAuth secret's access token is in Token too, and is what a sentinel for
	// it has to look like. It is read on a write that keeps the value as well,
	// not only one that replaces it: a stored shape is only as current as the
	// provider table was when it was read, and a rename is as good a moment as
	// any to bring it up to date. A format a person set is theirs, and no
	// write replaces it.
	if !secret.FormatSet {
		persisted.Format = s.valueFormat(ctx, secret, secret.Format)
	}
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

// valueFormat is the shape of secret's value, or fallback when there is no
// value to read one from — a gate, or a row whose key has moved on.
func (s *Store) valueFormat(ctx context.Context, secret *model.Secret, fallback string) string {
	val, err := s.OpenSecretValue(ctx, secret)
	if err != nil || val == nil {
		return fallback
	}
	if token := strings.TrimSpace(val.Token); token != "" {
		return secretformat.Describe(token)
	}
	return fallback
}

// RefreshSecretFormats re-reads the format of every secret whose format
// nobody set, and stores it where it changed. Run at startup, it is the
// upgrade path for the shapes already stored: rows written before every value
// write recorded one, and rows whose shape was read under an older provider
// table — an Anthropic key stored as sk-ant-{alnum:5}- before the kind marker
// was kept — would otherwise go on minting that shape until somebody replaced
// the value. It is idempotent, so it runs every time rather than once, and a
// row it cannot read is left as it is rather than failing the start.
//
// Only the format column is written: this is a correction of what the store
// says about the value, not a write of the secret, and it must not move the
// updated_at an OAuth refresh uses as its generation guard.
func (s *Store) RefreshSecretFormats(ctx context.Context) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	var rows []model.Secret
	if err := write.Where("format_set = ?", false).Find(&rows).Error; err != nil {
		return err
	}
	for i := range rows {
		secret := &rows[i]
		format := s.valueFormat(ctx, secret, secret.Format)
		if format == secret.Format {
			continue
		}
		if err := write.Model(&model.Secret{}).
			Where("project_id = ? AND id = ? AND format_set = ?", secret.ProjectID, secret.ID, false).
			UpdateColumn("format", format).Error; err != nil {
			return err
		}
	}
	return nil
}

// SentinelFormat is the template a sentinel for secret is minted from: the
// format stored with its value, or, for a row written before every value
// write recorded one, the shape read from the value itself. Every minter —
// a sandbox's stable sentinel, an agent credential's binding, the format a
// pool agent mints ephemeral ones from — asks here, so a secret's sentinels
// cannot differ by which path minted them.
func (s *Store) SentinelFormat(ctx context.Context, secret *model.Secret) string {
	if format := strings.TrimSpace(secret.Format); format != "" {
		return format
	}
	return s.valueFormat(ctx, secret, secretformat.DefaultSentinelFormat)
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

// SecretListOption narrows what a listing of secrets answers with.
type SecretListOption func(*secretListOptions)

type secretListOptions struct {
	delegatedTo string
}

// DelegatedTo lists only the secrets a discobox holds a live delegation grant
// of: the ones it may hand on, and so the only ones it has reason to see or
// name (ADR 26-09-30-782 §3).
func DelegatedTo(sandboxID string) SecretListOption {
	return func(options *secretListOptions) { options.delegatedTo = sandboxID }
}

func (s *Store) ListSecrets(ctx context.Context, projectID string, listOptions ...SecretListOption) ([]model.Secret, error) {
	var options secretListOptions
	for _, option := range listOptions {
		if option != nil {
			option(&options)
		}
	}
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	query := read.Where("project_id = ? AND anonymous = ?", projectID, false)
	if options.delegatedTo != "" {
		query = query.Where("id IN (?)", read.Model(&model.SecretGrant{}).Select("secret_id").
			Where("project_id = ? AND purpose = ? AND scope = ? AND scope_key = ?",
				projectID, model.SecretGrantPurposeDelegate, model.SecretGrantScopeSandbox, options.delegatedTo).
			Where("expires_at IS NULL OR expires_at > ?", time.Now().UTC()))
	}
	var out []model.Secret
	err = query.Order("created_at ASC").Find(&out).Error
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
	// The format rides with the value, as on every other value write; one a
	// person set is left alone.
	columns := map[string]any{"encrypted_value": sealed.EncryptedValue}
	if !sealed.FormatSet {
		columns["format"] = sealed.Format
	}
	result := write.Model(&model.Secret{}).
		Where("project_id = ? AND id = ? AND updated_at = ?", sealed.ProjectID, sealed.ID, prevUpdatedAt).
		Updates(columns)
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
		// A rule that names the secret could never approve again, and would
		// go on counting as a reason for the project to keep a judge.
		if err := tx.Where("project_id = ? AND secret_id = ?", projectID, secretID).Delete(&model.ApprovalRule{}).Error; err != nil {
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
	var found []model.SecretRequest
	// A refresh request names the same secret and discobox, and asks for
	// something else: a value, not a grant. It is never the reactive ask.
	//
	// The host is compared in Go: the hosts are a JSON list, and a reactive
	// request's is the one destination the proxy observed.
	err = read.Where("project_id = ? AND secret_id = ? AND requested_by = ? AND status = ? AND reason = ''",
		projectID, secretID, requestedBy, model.SecretRequestStatusPending).
		Order("created_at DESC").Find(&found).Error
	if err != nil {
		return nil, err
	}
	for i := range found {
		if hostscope.SameSet(found[i].Hosts, hostscope.List(host)) {
			return &found[i], nil
		}
	}
	return nil, ErrNotFound
}

// FindPendingAgentCredentialRequests returns the open protocol-originated
// requests for a sandbox's environment variable, destination hosts, well-known
// ID (empty for an ask that names none), and purpose, newest first. The ID and
// the purpose are part of the key because each changes what approving the
// request mints: an ask to delegate is not a retry of an ask to use. The hosts
// are compared as a set (ADR 26-10-02-393 §3): approving either of two asks
// listing them in another order grants the same thing.
//
// It keys on (sandbox, env, hosts) rather than on the secret the way the
// reactive path does, because a protocol request names no secret: choosing one
// is part of the approval. Which of them, if any, a new ask repeats is the
// caller's to decide from what each asks for.
func (s *Store) FindPendingAgentCredentialRequests(ctx context.Context, projectID, sandboxID, envName string, hosts []string, wellKnownID, purpose string) ([]model.SecretRequest, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var found []model.SecretRequest
	err = read.Where("project_id = ? AND sandbox_id = ? AND env_name = ? AND well_known_id = ? AND purpose = ? AND status = ?",
		projectID, sandboxID, envName, wellKnownID, purpose, model.SecretRequestStatusPending).
		Order("created_at DESC").Find(&found).Error
	if err != nil {
		return nil, err
	}
	out := found[:0]
	for _, req := range found {
		if req.FromProtocol() && hostscope.SameSet(req.Hosts, hosts) {
			out = append(out, req)
		}
	}
	return out, nil
}

func (s *Store) GetSecretRequest(ctx context.Context, projectID, requestID string) (*model.SecretRequest, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.SecretRequest](read.Where("project_id = ?", projectID), "id", requestID)
}

// SecretRequestListOption narrows what a listing of secret requests answers
// with.
type SecretRequestListOption func(*secretRequestListOptions)

type secretRequestListOptions struct {
	owner string
}

// OwnedBy lists only the requests a discobox owns: those filed by a discobox
// it created (ADR 26-09-30-782 §2). A request no discobox filed, or whose
// discobox is gone, has no owner and is never listed.
func OwnedBy(sandboxID string) SecretRequestListOption {
	return func(options *secretRequestListOptions) { options.owner = sandboxID }
}

// ListSecretRequests returns a project's secret requests, optionally filtered to
// a single status.
func (s *Store) ListSecretRequests(ctx context.Context, projectID, status string, listOptions ...SecretRequestListOption) ([]model.SecretRequest, error) {
	var options secretRequestListOptions
	for _, option := range listOptions {
		if option != nil {
			option(&options)
		}
	}
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	query := read.Where("project_id = ?", projectID)
	if status = strings.TrimSpace(status); status != "" {
		query = query.Where("status = ?", status)
	}
	if options.owner != "" {
		query = query.Where("sandbox_id IN (?)", read.Model(&model.Sandbox{}).Select("id").
			Where("project_id = ? AND created_by_sandbox_id = ?", projectID, options.owner))
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
// denySandboxSecretRequestsTx closes a deleted sandbox's open requests as
// denied. The agent that asked is gone, so nothing will poll for the answer,
// and approving one would grant a credential to nothing. A refresh request is
// left open: it asks for a new value of a secret every other sandbox using it
// still needs, and only names the sandbox that noticed.
func denySandboxSecretRequestsTx(tx *gorm.DB, projectID, sandboxID string) error {
	return tx.Model(&model.SecretRequest{}).
		Where("project_id = ? AND sandbox_id = ? AND status = ? AND reason <> ?",
			projectID, sandboxID, model.SecretRequestStatusPending, model.SecretRequestReasonRefresh).
		Updates(map[string]any{"status": model.SecretRequestStatusDenied, "closed_at": closedAt(time.Now())}).Error
}

func closedAt(at time.Time) *time.Time {
	at = at.UTC()
	return &at
}
