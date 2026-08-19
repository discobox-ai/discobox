package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/obot-platform/discobox/server/internal/model"
)

// oauthRefreshSkew is how far ahead of the access token's expiry a resolve will
// proactively refresh it, so an outbound request never carries a token that
// expires mid-flight. It matches Claude Code's own ~5-minute proactive window.
const oauthRefreshSkew = 5 * time.Minute

// oauthRefreshTimeout bounds a single upstream token refresh.
const oauthRefreshTimeout = 30 * time.Second

// oauthHTTPClient is the client used for token refreshes. It is a package var so
// tests can point it at an httptest server.
var oauthHTTPClient = &http.Client{Timeout: oauthRefreshTimeout}

// oauthAccessTokenExpiry returns the access token's expiry, or the zero time when
// unknown.
func oauthAccessTokenExpiry(val *model.SecretValue) time.Time {
	if val == nil || val.AccessTokenExpiresAt == 0 {
		return time.Time{}
	}
	return time.UnixMilli(val.AccessTokenExpiresAt).UTC()
}

// oauthNeedsRefresh reports whether the access token is missing, of unknown
// expiry, or within the skew window of expiring.
func oauthNeedsRefresh(val *model.SecretValue, now time.Time) bool {
	if val == nil || strings.TrimSpace(val.Token) == "" {
		return true
	}
	if strings.TrimSpace(val.RefreshToken) == "" {
		// Nothing to refresh with; serve what we have and let use-time verification
		// (a 401 from Anthropic) be the authority on whether it still works.
		return false
	}
	expiry := oauthAccessTokenExpiry(val)
	if expiry.IsZero() {
		return true
	}
	return !now.Before(expiry.Add(-oauthRefreshSkew))
}

// ensureFreshOAuth returns a SecretValue whose access token is good for at least
// the skew window, refreshing it in place when needed. It is the single writer of
// a rotated credential: a per-secret singleflight collapses concurrent resolves
// onto one upstream refresh, and the persisted write is guarded by the row's
// updated_at so a refresh in another process cannot be clobbered.
//
// It never fails the resolve on a refresh error: if the upstream refresh fails
// but a (soon-to-expire) token is still on hand, that token is served and the
// error is left for use-time verification. Only a total absence of a usable token
// surfaces as an error.
func (s *Service) ensureFreshOAuth(ctx context.Context, secret *model.Secret, val *model.SecretValue) (*model.SecretValue, error) {
	if secret.Type != model.SecretTypeOAuth {
		return val, nil
	}
	if !oauthNeedsRefresh(val, time.Now().UTC()) {
		return val, nil
	}

	refreshed, err, _ := s.oauthRefresh.Do(secret.ID, func() (any, error) {
		return s.refreshOAuthLocked(ctx, secret.ProjectID, secret.ID)
	})
	if err != nil {
		if val != nil && strings.TrimSpace(val.Token) != "" {
			// Serve the token we have; a 401 upstream is the authority on liveness.
			return val, nil
		}
		return nil, err
	}
	fresh, ok := refreshed.(*model.SecretValue)
	if !ok {
		return nil, fmt.Errorf("oauth refresh returned unexpected type %T", refreshed)
	}
	return fresh, nil
}

// refreshOAuthLocked re-reads the secret under the singleflight, re-checks
// freshness (a concurrent process may have just rotated it), performs the upstream
// refresh, and persists the rotated credential with an updated_at guard. On a
// guard conflict it re-reads and returns the winner's value rather than retrying
// the refresh, because the refresh token it holds is already spent.
func (s *Service) refreshOAuthLocked(ctx context.Context, projectID, secretID string) (*model.SecretValue, error) {
	secret, err := s.store.GetSecret(ctx, projectID, secretID)
	if err != nil {
		return nil, err
	}
	val, err := s.store.OpenSecretValue(ctx, secret)
	if err != nil {
		return nil, fmt.Errorf("decrypt secret: %w", err)
	}
	if val == nil {
		return nil, fmt.Errorf("oauth secret %s has no value", secretID)
	}
	if !oauthNeedsRefresh(val, time.Now().UTC()) {
		// Someone refreshed while we waited for the lock.
		return val, nil
	}
	if strings.TrimSpace(val.RefreshToken) == "" {
		return val, nil
	}

	rotated, err := refreshOAuthToken(ctx, val)
	if err != nil {
		return nil, err
	}

	prevUpdatedAt := secret.UpdatedAt
	//nolint:gosec // Secret values are marshaled before store encryption.
	valueBytes, err := json.Marshal(rotated)
	if err != nil {
		return nil, err
	}
	secret.EncryptedValue = valueBytes
	if err := s.store.UpdateSecretValueIfUnchanged(ctx, secret, prevUpdatedAt); err != nil {
		if isGenerationConflict(err) {
			// Another process rotated first; its value is authoritative.
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
	return rotated, nil
}

// oauthTokenResponse is the subset of the token endpoint's response we consume.
type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// refreshOAuthToken exchanges the stored refresh token for a new access/refresh
// pair against the credential's token endpoint. The refresh token rotates on each
// use, so the returned value carries the new refresh token; persisting it is the
// caller's responsibility and must be atomic.
func refreshOAuthToken(ctx context.Context, val *model.SecretValue) (*model.SecretValue, error) {
	tokenURL := strings.TrimSpace(val.TokenURL)
	clientID := strings.TrimSpace(val.ClientID)
	if tokenURL == "" || clientID == "" {
		return nil, fmt.Errorf("oauth secret is missing tokenUrl or clientId")
	}
	payload, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": val.RefreshToken,
		"client_id":     clientID,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth refresh request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("oauth refresh failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out oauthTokenResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("oauth refresh response: %w", err)
	}
	if strings.TrimSpace(out.AccessToken) == "" {
		return nil, fmt.Errorf("oauth refresh response carried no access token")
	}

	rotated := *val
	rotated.Token = out.AccessToken
	if strings.TrimSpace(out.RefreshToken) != "" {
		rotated.RefreshToken = out.RefreshToken
	}
	if out.ExpiresIn > 0 {
		rotated.AccessTokenExpiresAt = time.Now().UTC().Add(time.Duration(out.ExpiresIn) * time.Second).UnixMilli()
	} else {
		// expires_in is conventional, not guaranteed. Leaving the expiry unknown
		// is not a neutral fallback: oauthNeedsRefresh treats unknown as "refresh
		// now", so every subsequent resolve would refresh again — and since the
		// refresh token rotates on use, that spends one per request. A JWT access
		// token states its own expiry, so read it before giving up.
		rotated.AccessTokenExpiresAt = jwtExpiryMillis(out.AccessToken)
	}
	return &rotated, nil
}

// jwtExpiryMillis returns the `exp` claim of a JWT access token in unix
// milliseconds, or 0 when the token is not a JWT or carries no usable exp.
//
// The claim is read without verifying the signature, which is safe because
// nothing is authorized by it. The value only decides when the server refreshes
// ahead of expiry: a forged-early answer costs an extra refresh, and a
// forged-late one costs a use-time 401 — the same two outcomes an unknown expiry
// already produces. The token itself is one this server just received from the
// endpoint it holds the client credentials for.
func jwtExpiryMillis(token string) int64 {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 || parts[1] == "" {
		return 0
	}
	// JWT payloads are unpadded base64url, but tolerate padding rather than
	// discard an expiry over an encoder's habit.
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return 0
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return 0
	}
	return claims.Exp * 1000
}

// oauthResolutionExpiry caps the grant expiry the proxy caches against by the
// access token's own expiry, so the proxy re-resolves — and thus triggers the
// next refresh — right as the token ages out, without the grant itself needing to
// expire. A zero return means "do not cache", used when the token expiry is
// unknown.
func oauthResolutionExpiry(grantExpiry *time.Time, val *model.SecretValue) *time.Time {
	tokenExpiry := oauthAccessTokenExpiry(val)
	if tokenExpiry.IsZero() {
		// Unknown token expiry: fall back to the grant's own bound.
		return grantExpiry
	}
	if grantExpiry == nil || tokenExpiry.Before(*grantExpiry) {
		return &tokenExpiry
	}
	return grantExpiry
}
