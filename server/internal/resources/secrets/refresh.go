package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"gorm.io/gorm"

	apigen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/secretformat"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/wellknown"
)

// A token may expire, and suggest the command that renews it
// (ADR 26-09-25-122). Delivery and renewal are two loops: resolve always
// serves the value on hand, and a value past its lifetime opens a refresh
// request that a person's client answers by writing a new one. The command is
// advice to that client; nothing here runs it.

const (
	// staleResolutionTTL is how long the proxy may hold a value that is past
	// its lifetime. It is served, because a lifetime is an estimate and the
	// upstream is the authority, but briefly, so a renewal written in the
	// meantime reaches the proxy soon after.
	staleResolutionTTL = 30 * time.Second
	// refreshAhead is how long before a value goes stale a resolve opens the
	// refresh request, so a client polling the inbox can answer before the
	// value is past its lifetime rather than after.
	refreshAhead = time.Minute
	// rejectionAfterRenewal is how long a value just written is not marked
	// stale by a rejection. A report can describe the value it replaced: the
	// proxy reports on a one-minute beat, and the refusal it saw may be of the
	// value this one was written to fix.
	rejectionAfterRenewal = time.Minute
	// maxRefreshCommandArgs and maxRefreshCommandBytes bound a refresh
	// command: an argument vector a person reads in a dialog, not a script.
	maxRefreshCommandArgs  = 64
	maxRefreshCommandBytes = 4096
)

// secretLifetime is what a create or update says about a token's lifetime.
type secretLifetime struct {
	ttl       *int64
	command   *[]string
	expiresAt *time.Time
}

func createLifetime(input services.CreateSecretBody) secretLifetime {
	var lifetime secretLifetime
	if v, ok := input.TtlSeconds.Get(); ok {
		lifetime.ttl = &v
	}
	if v, ok := input.RefreshCommand.Get(); ok {
		lifetime.command = &v
	}
	if v, ok := input.ValueExpiresAt.Get(); ok {
		lifetime.expiresAt = &v
	}
	return lifetime
}

func updateLifetime(input services.UpdateSecretBody) secretLifetime {
	var lifetime secretLifetime
	if v, ok := input.TtlSeconds.Get(); ok {
		lifetime.ttl = &v
	}
	if v, ok := input.RefreshCommand.Get(); ok {
		lifetime.command = &v
	}
	if v, ok := input.ValueExpiresAt.Get(); ok {
		lifetime.expiresAt = &v
	}
	return lifetime
}

// apply writes the lifetime onto the secret, refusing what a token alone may
// carry on any other type. hasValue is whether the same write carries a value,
// which is the only thing an expiry can describe.
func (l secretLifetime) apply(sec *model.Secret, hasValue bool) error {
	if l.ttl == nil && l.command == nil && l.expiresAt == nil {
		return nil
	}
	if sec.Type != model.SecretTypeToken {
		return apperrors.NewStatusError(http.StatusBadRequest,
			"a lifetime and a refresh command are for token secrets; an oauth secret renews itself")
	}
	if isGateSecret(sec) {
		return apperrors.NewStatusError(http.StatusBadRequest,
			fmt.Sprintf("%s is a gate, with no value to renew", sec.WellKnownID))
	}
	if l.ttl != nil {
		if *l.ttl < 0 {
			return apperrors.NewStatusError(http.StatusBadRequest, "a lifetime is a number of seconds; 0 never goes stale")
		}
		sec.TTL = *l.ttl
	}
	if l.command != nil {
		command, err := normalizeRefreshCommand(*l.command)
		if err != nil {
			return err
		}
		sec.RefreshCommand = command
		// A value worth fetching by command is short-lived by assumption; a
		// person who wants longer says so.
		if len(command) > 0 && l.ttl == nil && sec.TTL == 0 {
			sec.TTL = defaultRefreshTTL(sec)
		}
	}
	if l.expiresAt != nil {
		if !hasValue {
			return apperrors.NewStatusError(http.StatusBadRequest, "an expiry describes a value; send it with one")
		}
		expires := l.expiresAt.UTC()
		sec.ValueWritten(time.Now().UTC(), &expires)
	}
	return nil
}

// defaultRefreshTTL is the lifetime a token with a command takes when nobody
// names one: its well-known credential's, which knows how long the credential
// really lives, else the short default any command gets.
func defaultRefreshTTL(sec *model.Secret) int64 {
	if known, ok := wellknown.Lookup(sec.WellKnownID); ok && known.RefreshTTL > 0 {
		return int64(known.RefreshTTL / time.Second)
	}
	return model.DefaultRefreshTTL
}

// normalizeRefreshCommand checks a refresh command: an argument vector, run
// without a shell, that a person will read before it runs. Empty removes it.
func normalizeRefreshCommand(command []string) ([]string, error) {
	if len(command) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(command[0]) == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "a refresh command names a program to run first")
	}
	if len(command) > maxRefreshCommandArgs {
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			fmt.Sprintf("a refresh command is at most %d arguments", maxRefreshCommandArgs))
	}
	size := 0
	for _, arg := range command {
		if strings.ContainsRune(arg, 0) {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "a refresh command argument cannot contain a NUL byte")
		}
		size += len(arg)
	}
	if size > maxRefreshCommandBytes {
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			fmt.Sprintf("a refresh command is at most %d bytes", maxRefreshCommandBytes))
	}
	return append([]string(nil), command...), nil
}

// RefreshCommandDigest is the digest a refresh answer records a command by: the
// arguments, NUL-separated, so no two argument vectors share one.
func RefreshCommandDigest(command []string) string {
	sum := sha256.Sum256([]byte(strings.Join(command, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// withholdRenewalFromSandbox leaves a secret's refresh command out of what a
// discobox reads. The sandbox role lists secrets for their names and
// bindings, never a value (ADR 0140 §4), and a command a person wrote can name
// a vault path or carry a credential of its own; it is a person's client's to
// run, and no discobox's to see.
func withholdRenewalFromSandbox(ctx context.Context, sec *model.Secret) {
	if principal, ok := auth.PrincipalFromContext(ctx); ok && principal.Type == auth.PrincipalTypeSandbox {
		sec.RefreshCommand = nil
	}
}

// withholdAnswerFromSandbox leaves how a refresh request was answered — the
// command, the client host, who answered — out of what a discobox reads, for
// the same reason.
func withholdAnswerFromSandbox(ctx context.Context, req *model.SecretRequest) {
	if principal, ok := auth.PrincipalFromContext(ctx); ok && principal.Type == auth.PrincipalTypeSandbox {
		req.RefreshAnswer = nil
	}
}

// describeStaleness fills in when a secret's value stops being trusted.
func describeStaleness(sec *model.Secret) {
	if at, ok := sec.StaleTime(); ok {
		at = at.UTC()
		sec.StaleAt = &at
	}
}

// renewableResolution is the resolution of a token with a lifetime: the value
// on hand, always, with an expiry that brings the proxy back as the value ages
// out — and, when it has aged out or nearly has, a refresh request for a
// person's client to answer.
func (s *Service) renewableResolution(ctx context.Context, secret *model.Secret, sandboxID string, grantExpiry *time.Time) *time.Time {
	staleAt, ok := secret.StaleTime()
	if !ok {
		return grantExpiry
	}
	now := time.Now().UTC()
	if !now.Before(staleAt.Add(-refreshAhead)) {
		// A failure to open the request does not fail the resolve: the value
		// is still the best answer there is, and the next resolve asks again.
		if err := s.openRefreshRequest(ctx, secret, sandboxID, model.SecretRefreshCauseStale); err != nil {
			slog.WarnContext(ctx, "failed to open a refresh request", "projectId", secret.ProjectID, "secretId", secret.ID, "error", err)
		}
	}
	expiry := staleAt
	if !now.Before(staleAt) {
		expiry = now.Add(staleResolutionTTL)
	}
	if grantExpiry != nil && grantExpiry.Before(expiry) {
		return grantExpiry
	}
	return &expiry
}

// openRefreshRequest ensures an open refresh request stands for the secret,
// naming the discobox that most recently needed it.
func (s *Service) openRefreshRequest(ctx context.Context, secret *model.Secret, sandboxID, cause string) error {
	existing, err := s.store.FindPendingRefreshRequest(ctx, secret.ProjectID, secret.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if existing != nil {
		if existing.SandboxID == sandboxID {
			return nil
		}
		return s.store.SetRefreshRequestSandbox(ctx, secret.ProjectID, existing.ID, sandboxID)
	}
	err = s.store.CreateSecretRequest(ctx, &model.SecretRequest{
		ProjectID:    secret.ProjectID,
		RequestedBy:  "sandbox:" + sandboxID,
		SandboxID:    sandboxID,
		Type:         secret.Type,
		Host:         secret.Host,
		SecretID:     secret.ID,
		Name:         secret.Name,
		Status:       model.SecretRequestStatusPending,
		Reason:       model.SecretRequestReasonRefresh,
		RefreshCause: cause,
	})
	// Another resolve opened it between the find and the create: the index
	// holds one per secret, and that one is the answer.
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique constraint") {
		return nil
	}
	return err
}

// renewableRejection is what a refusal of a renewable token comes to: its
// value is stale from now, and a person's client is asked for another. It is
// not recorded as a rejection, because it is not a dead end (ADR 26-09-25-122
// §3).
func (s *Service) renewableRejection(ctx context.Context, secret *model.Secret, sandboxID string) error {
	now := time.Now().UTC()
	// The value just written is not the one refused; the store decides that
	// in the write, against the row as it is then, not as it was read here.
	marked, err := s.store.MarkSecretValueStale(ctx, secret.ProjectID, secret.ID, now, now.Add(-rejectionAfterRenewal))
	if err != nil || !marked {
		return err
	}
	return s.openRefreshRequest(ctx, secret, sandboxID, model.SecretRefreshCauseRejected)
}

// RefreshSecret writes a new value for a token, answering its refresh
// request. The first answer wins: one naming a request already answered writes
// nothing. The command a client reports is recorded as its account of how the
// value was made, and never checked — it could have sent any value
// (ADR 26-09-25-122 §4).
func (s *Service) RefreshSecret(ctx context.Context, projectID, secretID string, input services.RefreshSecretBody) (*model.Secret, error) {
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return nil, apperrors.NewStatusError(http.StatusUnauthorized, "authentication required")
	}
	// A discobox supplies no value and decides nothing about when one is
	// renewed. The route is not in the sandbox role; this says so here too.
	if principal.Type == auth.PrincipalTypeSandbox {
		return nil, apperrors.NewStatusError(http.StatusForbidden, "a person's client renews a credential; a discobox does not")
	}
	value := strings.TrimSpace(input.Value)
	if value == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "a refresh carries the new value")
	}
	now := time.Now().UTC()
	answer := &model.SecretRefreshAnswer{
		AnsweredAt: now,
		AnsweredBy: grantedByOf(principal),
		Via:        string(input.Via),
		Session:    input.Session.Or(false),
		ClientHost: strings.TrimSpace(input.ClientHost.Or("")),
	}
	switch answer.Via {
	case model.SecretRefreshViaCommand:
		command, _ := input.Command.Get()
		if len(command) == 0 {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "a value produced by a command names the command")
		}
		answer.Command = command
		answer.CommandDigest = RefreshCommandDigest(command)
	case model.SecretRefreshViaEntered:
	default:
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "via is command or entered")
	}
	var expiresAt *time.Time
	if v, ok := input.ExpiresAt.Get(); ok {
		v = v.UTC()
		expiresAt = &v
	}
	requestID := strings.TrimSpace(input.RequestId.Or(""))

	err := s.store.Transaction(ctx, func(txStore *store.Store, _ *gorm.DB) error {
		sec, err := txStore.GetSecret(ctx, projectID, secretID)
		if err != nil {
			return apperrors.NotFound(err, "secret not found")
		}
		if sec.Type != model.SecretTypeToken || isGateSecret(sec) {
			return apperrors.NewStatusError(http.StatusBadRequest, "only a token secret is refreshed with a new value")
		}
		if requestID != "" {
			req, err := txStore.GetSecretRequest(ctx, projectID, requestID)
			if err != nil {
				return apperrors.NotFound(err, "secret request not found")
			}
			if !req.IsRefresh() || req.SecretID != sec.ID {
				return apperrors.NewStatusError(http.StatusBadRequest, "that request does not ask for a new value of this secret")
			}
			if err := txStore.AnswerRefreshRequest(ctx, projectID, requestID, answer); err != nil {
				if errors.Is(err, store.ErrGenerationConflict) {
					return apperrors.NewStatusError(http.StatusConflict, "that refresh request was already answered")
				}
				return err
			}
		}
		// Every other open ask on the secret is answered by the same value,
		// and is recorded as such rather than as an ordinary write.
		if err := txStore.AnswerOpenRefreshRequests(ctx, projectID, sec.ID, answer); err != nil {
			return err
		}
		valueBytes, err := marshalSecretValue(apigen.SecretValue{Token: apigen.NewOptString(value)})
		if err != nil {
			return apperrors.NewStatusError(http.StatusBadRequest, "invalid secret value")
		}
		sec.EncryptedValue = valueBytes
		sec.Format = secretformat.Describe(value)
		sec.ValueWritten(now, expiresAt)
		return txStore.UpdateSecret(ctx, sec)
	})
	if err != nil {
		return nil, err
	}
	return s.GetSecret(ctx, projectID, secretID)
}
