package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// This file is renewal for the two types that hold a short-lived token: an
// OAuth access token renewed by its refresh token (ADR 0011), and an exchange
// secret's token renewed from the fields it stores (ADR 26-10-08-452). Both
// keep the token in SecretValue.Token and its expiry in AccessTokenExpiresAt,
// and from there everything here is the same: when to renew, one renewal at a
// time, how the renewed value is kept, and what a forced renewal settled. How
// the request is made is the type's own (oauth.go, exchange.go).

// tokenRenewSkew is how far ahead of the token's expiry a resolve will
// proactively renew it, so an outbound request never carries a token that
// expires mid-flight. It matches Claude Code's own ~5-minute proactive window.
const tokenRenewSkew = 5 * time.Minute

// tokenRenewTimeout bounds a single upstream renewal.
const tokenRenewTimeout = 30 * time.Second

// errRenewalRefused marks a token endpoint refusing to renew a credential — a
// 4xx answer, `invalid_grant` above all — as opposed to being unreachable or
// erroring. Only the refusal says anything about the credential, and only the
// refusal is worth a person's time (ADR 0132 §3).
var errRenewalRefused = errors.New("token renewal refused")

// tokenHTTPClient is the client renewals are made with. It is a package var so
// tests can point it at an httptest server.
//
// It follows no redirect. A 307 or 308 re-sends the body, and with it the key
// or refresh token, to whatever the Location names — another host, or plain
// http — past every check made on the URL the secret holds. A token endpoint
// that redirects is answered as the error it is.
var tokenHTTPClient = &http.Client{
	Timeout:       tokenRenewTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// refusedStatus reports whether a token endpoint's answer is a verdict on the
// credential: a 4xx, but not 429, which is the endpoint asking to be called
// less often and says nothing about what was sent.
func refusedStatus(status int) bool {
	return status >= 400 && status < 500 && status != http.StatusTooManyRequests
}

// upstreamAnswer is what an endpoint said, for an error a person reads: its
// status and the start of its body. The start, because the body is whatever
// the endpoint chose to send, and the error reaches whoever stored the secret.
func upstreamAnswer(status string, body []byte) string {
	const most = 256
	text := strings.TrimSpace(string(body))
	if len(text) > most {
		text = text[:most] + "…"
	}
	return status + ": " + text
}

// renews reports whether a secret of this type holds a token the server
// renews.
func renews(secretType string) bool {
	return secretType == model.SecretTypeOAuth || secretType == model.SecretTypeExchange
}

// canRenew reports whether a value carries what its type needs to renew.
func canRenew(secretType string, val *model.SecretValue) bool {
	switch secretType {
	case model.SecretTypeOAuth:
		return oauthRenewable(val)
	case model.SecretTypeExchange:
		return val != nil && len(val.Exchange) > 0
	}
	return false
}

// tokenExpiry returns the token's expiry, or the zero time when unknown.
func tokenExpiry(val *model.SecretValue) time.Time {
	if val == nil || val.AccessTokenExpiresAt == 0 {
		return time.Time{}
	}
	return time.UnixMilli(val.AccessTokenExpiresAt).UTC()
}

// needsRenewal reports whether the token is missing, of unknown expiry, or
// within the skew window of expiring — for a value that can be renewed at all.
func needsRenewal(secretType string, val *model.SecretValue, now time.Time) bool {
	if val == nil || strings.TrimSpace(val.Token) == "" {
		return true
	}
	if !canRenew(secretType, val) {
		// Nothing to renew with; serve what we have and let use-time
		// verification (a 401 upstream) be the authority on whether it still
		// works.
		return false
	}
	expiry := tokenExpiry(val)
	if expiry.IsZero() {
		return true
	}
	return !now.Before(expiry.Add(-tokenRenewSkew))
}

// ensureFresh returns a SecretValue whose token is good for at least the skew
// window, renewing it in place when needed. It is the single writer of a
// renewed credential: a per-secret singleflight collapses concurrent resolves
// onto one upstream renewal, and the persisted write is guarded by the row's
// updated_at so a renewal in another process cannot be clobbered.
//
// It never fails the resolve on a renewal error: if the renewal fails but a
// (soon-to-expire) token is still on hand, that token is served and the error
// is left for use-time verification. Only a total absence of a usable token
// surfaces as an error.
func (s *Service) ensureFresh(ctx context.Context, secret *model.Secret, val *model.SecretValue) (*model.SecretValue, error) {
	if !renews(secret.Type) || !needsRenewal(secret.Type, val, time.Now().UTC()) {
		return val, nil
	}
	renewed, err, _ := s.renewing.Do(secret.ID, func() (any, error) {
		return s.renewLocked(ctx, secret.ProjectID, secret.ID, false)
	})
	if err != nil {
		if val != nil && strings.TrimSpace(val.Token) != "" {
			// Serve the token we have; a 401 upstream is the authority on liveness.
			return val, nil
		}
		return nil, err
	}
	fresh, ok := renewed.(*model.SecretValue)
	if !ok {
		return nil, fmt.Errorf("token renewal returned unexpected type %T", renewed)
	}
	return fresh, nil
}

// renewLocked re-reads the secret under the singleflight, re-checks freshness
// (a concurrent process may have just renewed it), performs the upstream
// renewal, and persists the renewed credential with an updated_at guard. On a
// guard conflict it re-reads and returns the winner's value rather than
// renewing again: an OAuth refresh token it holds is already spent, and an
// exchange's winner is as good as its own.
// force skips the freshness check: a credential the upstream has just refused
// is stale whatever its stated expiry says, which is the whole of what a 401
// tells us that a clock cannot (ADR 0132 §3).
func (s *Service) renewLocked(ctx context.Context, projectID, secretID string, force bool) (*model.SecretValue, error) {
	secret, err := s.store.GetSecret(ctx, projectID, secretID)
	if err != nil {
		return nil, err
	}
	val, err := s.store.OpenSecretValue(ctx, secret)
	if err != nil {
		return nil, fmt.Errorf("decrypt secret: %w", err)
	}
	if val == nil {
		return nil, fmt.Errorf("%s secret %s has no value", secret.Type, secretID)
	}
	if !force && !needsRenewal(secret.Type, val, time.Now().UTC()) {
		// Someone renewed while we waited for the lock.
		return val, nil
	}
	if !canRenew(secret.Type, val) {
		return val, nil
	}

	var renewed *model.SecretValue
	switch secret.Type {
	case model.SecretTypeOAuth:
		renewed, err = refreshOAuthToken(ctx, val)
	case model.SecretTypeExchange:
		renewed, err = exchangeToken(ctx, secret, val)
	default:
		return val, nil
	}
	if err != nil {
		return nil, err
	}

	prevUpdatedAt := secret.UpdatedAt
	//nolint:gosec // Secret values are marshaled before store encryption.
	valueBytes, err := json.Marshal(renewed)
	if err != nil {
		return nil, err
	}
	secret.EncryptedValue = valueBytes
	if err := s.store.UpdateSecretValueIfUnchanged(ctx, secret, prevUpdatedAt); err != nil {
		if isGenerationConflict(err) {
			// Another process renewed first; its value is authoritative.
			fresh, ferr := s.store.GetSecret(ctx, projectID, secretID)
			if ferr != nil {
				return nil, ferr
			}
			winner, ferr := s.store.OpenSecretValue(ctx, fresh)
			if ferr != nil {
				return nil, fmt.Errorf("decrypt secret: %w", ferr)
			}
			return winner, nil
		}
		return nil, err
	}
	return renewed, nil
}

// tokenResolutionExpiry caps the grant expiry the proxy caches against by the
// token's own expiry, so the proxy re-resolves — and thus triggers the next
// renewal — right as the token ages out, without the grant itself needing to
// expire. With the token's expiry unknown, the grant's own bound stands.
func tokenResolutionExpiry(grantExpiry *time.Time, val *model.SecretValue) *time.Time {
	expiry := tokenExpiry(val)
	if expiry.IsZero() {
		return grantExpiry
	}
	if grantExpiry == nil || expiry.Before(*grantExpiry) {
		return &expiry
	}
	return grantExpiry
}

// renewal is what a forced renewal settled. Four of the five are answers; the
// fifth is the honest absence of one, and keeping it distinct is what stops a
// credential being condemned for something that was never about it.
type renewal int

const (
	// renewalRotated: a different credential came back. There is something new
	// to try, and nothing to record yet.
	renewalRotated renewal = iota
	// renewalUnchanged: the endpoint answered and handed back the token that
	// was just refused. Nothing renewed, whatever it said.
	renewalUnchanged
	// renewalRefused: the endpoint refused to renew — an OAuth refresh token
	// spent, revoked, or belonging to a session somebody ended elsewhere, or an
	// exchange's stored key revoked. This is the one that needs a person.
	renewalRefused
	// renewalImpossible: there is nothing to renew with.
	renewalImpossible
	// renewalUnavailable: the renewal could not be attempted or could not be
	// judged — the endpoint was unreachable or erroring, the value could not be
	// decrypted, or this call joined a renewal that was not forced. Nothing is
	// known about the credential, so nothing is recorded about it.
	renewalUnavailable
)

// forceRenew renews a credential an upstream refused, whatever its token's
// stated expiry says, and reports what that settled.
//
// It is the recoverable half of a rejection. A token can die before its expiry
// — a sign-out somewhere else, a plan change, a rotation performed by something
// that is not this control plane — and the credential behind it is perfectly
// good; asking a person to sign in again for that is asking them to redo work
// the renewal can do. So the renewal is tried first, and only what it cannot
// fix is recorded.
//
// What it will not do is turn its own bad day into a verdict about somebody's
// credential. A token endpoint that is unreachable or answering 500s says
// nothing about whether what is stored is still good, and recording "renewal
// refused" for it would ask a person to redo work they do not need to — and
// then suppress the retry that would have worked.
func (s *Service) forceRenew(ctx context.Context, secret *model.Secret) renewal {
	if !renews(secret.Type) {
		return renewalImpossible
	}
	before, err := s.store.OpenSecretValue(ctx, secret)
	if err != nil {
		// The stored value cannot be read. That is this side's problem, not a
		// statement about the credential.
		return renewalUnavailable
	}
	if !canRenew(secret.Type, before) {
		return renewalImpossible
	}
	// Through the same singleflight as every other renewal: an OAuth refresh
	// token rotates on use, so two rejections arriving together must not spend
	// it twice.
	//
	// Which is also why a *shared* result is not trusted. The resolve path
	// renews on this same key without forcing, and a call joining one of
	// those receives its result — which may be the unchanged value it decided
	// not to renew. Reading that as "renewed and still refused" would condemn a
	// credential nothing has tried yet, so a shared call that produced no new
	// token settles nothing and the next report tries again.
	renewed, err, shared := s.renewing.Do(secret.ID, func() (any, error) {
		return s.renewLocked(ctx, secret.ProjectID, secret.ID, true)
	})
	if err != nil {
		if errors.Is(err, errRenewalRefused) {
			return renewalRefused
		}
		// Unreachable, erroring, unparseable, or a store failure. Not a verdict.
		return renewalUnavailable
	}
	after, ok := renewed.(*model.SecretValue)
	if !ok || after == nil {
		return renewalUnavailable
	}
	if strings.TrimSpace(after.Token) != "" && after.Token != before.Token {
		return renewalRotated
	}
	if shared {
		return renewalUnavailable
	}
	return renewalUnchanged
}
