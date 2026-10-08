package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// This file is OAuth's half of renewal: the refresh request, and what an
// OAuth credential needs to make one. renew.go owns when a token is renewed
// and how a renewed one is kept.

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
	fields := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": val.RefreshToken,
		"client_id":     clientID,
	}
	// A confidential client authenticates its refresh too (RFC 6749 §6), in
	// the body as client_secret_post, which every endpoint that takes a
	// client secret accepts. A public client sends none.
	if val.ClientSecret != "" {
		fields["client_secret"] = val.ClientSecret
	}
	// RFC 6749 §6 defines the refresh request as form-encoded, but the
	// endpoints this was first written for (Anthropic's among them) take JSON,
	// and a secret stored before the encoding was recorded was refreshed as
	// JSON. So JSON stays the default and form is what a secret asks for.
	var (
		payload     []byte
		contentType string
	)
	switch val.TokenRequestEncoding {
	case model.OAuthTokenRequestJSON:
		encoded, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		payload, contentType = encoded, "application/json"
	case model.OAuthTokenRequestForm:
		form := url.Values{}
		for key, value := range fields {
			form.Set(key, value)
		}
		payload, contentType = []byte(form.Encode()), "application/x-www-form-urlencoded"
	default:
		return nil, fmt.Errorf("oauth secret has unknown token request encoding %q", val.TokenRequestEncoding)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")

	resp, err := tokenHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth refresh request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if refusedStatus(resp.StatusCode) {
		// The endpoint answered, and its answer is about the credential: the
		// refresh token is spent, revoked, or belongs to a session somebody
		// ended elsewhere. That is a finding about the secret, and the one
		// refresh failure a person has to act on (ADR 0132 §3).
		return nil, fmt.Errorf("%w: %s", errRenewalRefused, upstreamAnswer(resp.Status, body))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// A 5xx, or anything else it chose to say. The endpoint is having a bad
		// day; the credential has not been judged.
		return nil, fmt.Errorf("oauth refresh failed: %s", upstreamAnswer(resp.Status, body))
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

// oauthRenewable reports whether a credential carries everything a refresh
// needs: the token, where to spend it, and whose client it belongs to.
//
// All three, because refreshOAuthToken requires all three — a credential
// missing any of them cannot be renewed at all, which is a different thing
// from a renewal that was refused, and the two are told apart by what gets
// recorded about them (ADR 0132 §3).
func oauthRenewable(val *model.SecretValue) bool {
	return val != nil &&
		strings.TrimSpace(val.RefreshToken) != "" &&
		strings.TrimSpace(val.TokenURL) != "" &&
		strings.TrimSpace(val.ClientID) != ""
}
