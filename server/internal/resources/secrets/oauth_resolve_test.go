package secrets_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// oauthTokenServer stands in for the upstream token endpoint. It records how many
// times it was called and the last request body, and returns a rotated pair.
type oauthTokenServer struct {
	server   *httptest.Server
	calls    atomic.Int32
	lastBody map[string]string
}

func newOAuthTokenServer(t *testing.T, access, refresh string, expiresIn int64) *oauthTokenServer {
	t.Helper()
	ts := &oauthTokenServer{}
	ts.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]string
		_ = json.Unmarshal(body, &parsed)
		ts.lastBody = parsed
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  access,
			"refresh_token": refresh,
			"expires_in":    expiresIn,
			"token_type":    "Bearer",
		})
	}))
	t.Cleanup(ts.server.Close)
	return ts
}

func mustOAuthSecret(t *testing.T, st *store.Store, name string, val model.SecretValue) *model.Secret {
	t.Helper()
	//nolint:gosec // Test marshals a secret value before store encryption.
	b, err := json.Marshal(val)
	if err != nil {
		t.Fatal(err)
	}
	sec := &model.Secret{
		ProjectID: "project-1", Name: name, Type: model.SecretTypeOAuth,
		MaxGrantTTL: 3600, EncryptedValue: b,
	}
	if err := st.CreateSecret(context.Background(), sec); err != nil {
		t.Fatalf("create oauth secret: %v", err)
	}
	return sec
}

func TestResolveOAuthRefreshesExpiredToken(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	tokenSrv := newOAuthTokenServer(t, "new-access", "rt-2", 28800)
	sec := mustOAuthSecret(t, st, "claude", model.SecretValue{
		Token:                "old-access",
		RefreshToken:         "rt-1",
		TokenURL:             tokenSrv.server.URL,
		ClientID:             "client-x",
		AccessTokenExpiresAt: time.Now().UTC().Add(-time.Minute).UnixMilli(), // expired
	})
	mustGrant(t, st, sec.ID, model.SecretGrantScopeProject, "project-1")
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-OA")

	res, err := svc.ResolveSandboxSecret(ctx, "pool-1", "sb-1", "SENTINEL-OA", "api.anthropic.com")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Value == nil || res.Value.Token != "new-access" {
		t.Fatalf("resolved token = %#v, want new-access", res.Value)
	}
	if tokenSrv.calls.Load() != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", tokenSrv.calls.Load())
	}
	// The refresh request must carry the rotating grant, not re-use anything else.
	if tokenSrv.lastBody["grant_type"] != "refresh_token" ||
		tokenSrv.lastBody["refresh_token"] != "rt-1" ||
		tokenSrv.lastBody["client_id"] != "client-x" {
		t.Fatalf("refresh request body = %#v", tokenSrv.lastBody)
	}

	// The rotated pair must be persisted so the spent refresh token is never reused.
	reloaded, err := st.GetSecret(ctx, "project-1", sec.ID)
	if err != nil {
		t.Fatalf("reload secret: %v", err)
	}
	val, err := st.OpenSecretValue(ctx, reloaded)
	if err != nil {
		t.Fatalf("open value: %v", err)
	}
	if val.Token != "new-access" || val.RefreshToken != "rt-2" {
		t.Fatalf("persisted value = %#v, want new-access/rt-2", val)
	}
	if val.AccessTokenExpiresAt == 0 || time.UnixMilli(val.AccessTokenExpiresAt).Before(time.Now()) {
		t.Fatalf("persisted expiry not in the future: %d", val.AccessTokenExpiresAt)
	}

	// The proxy cache lifetime is capped by the token expiry, not the grant.
	if res.ExpiresAt == nil {
		t.Fatalf("resolution expiresAt = nil, want token expiry")
	}
	if got := res.ExpiresAt.UnixMilli(); got != val.AccessTokenExpiresAt {
		t.Fatalf("resolution expiresAt = %d, want token expiry %d", got, val.AccessTokenExpiresAt)
	}
}

func TestResolveOAuthKeepsFreshTokenWithoutRefresh(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	tokenSrv := newOAuthTokenServer(t, "should-not-be-used", "rt-2", 28800)
	sec := mustOAuthSecret(t, st, "claude", model.SecretValue{
		Token:                "still-good",
		RefreshToken:         "rt-1",
		TokenURL:             tokenSrv.server.URL,
		ClientID:             "client-x",
		AccessTokenExpiresAt: time.Now().UTC().Add(time.Hour).UnixMilli(), // well within validity
	})
	mustGrant(t, st, sec.ID, model.SecretGrantScopeProject, "project-1")
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-OB")

	res, err := svc.ResolveSandboxSecret(ctx, "pool-1", "sb-1", "SENTINEL-OB", "api.anthropic.com")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Value == nil || res.Value.Token != "still-good" {
		t.Fatalf("resolved token = %#v, want still-good", res.Value)
	}
	if tokenSrv.calls.Load() != 0 {
		t.Fatalf("token endpoint calls = %d, want 0 (token was fresh)", tokenSrv.calls.Load())
	}
}

func TestResolveOAuthServesStaleTokenWhenRefreshFails(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	// A server that always 400s the refresh (e.g. a revoked refresh token).
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	}))
	t.Cleanup(failSrv.Close)

	//nolint:gosec // Test literals, not real credentials.
	sec := mustOAuthSecret(t, st, "claude", model.SecretValue{
		Token:                "stale-but-present",
		RefreshToken:         "rt-dead",
		TokenURL:             failSrv.URL,
		ClientID:             "client-x",
		AccessTokenExpiresAt: time.Now().UTC().Add(-time.Minute).UnixMilli(),
	})
	mustGrant(t, st, sec.ID, model.SecretGrantScopeProject, "project-1")
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-OC")

	res, err := svc.ResolveSandboxSecret(ctx, "pool-1", "sb-1", "SENTINEL-OC", "api.anthropic.com")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// Refresh failed, but a token is on hand: serve it and let use-time 401 decide.
	if res.Value == nil || res.Value.Token != "stale-but-present" {
		t.Fatalf("resolved token = %#v, want stale-but-present", res.Value)
	}
}

// Scopes describe the grant, and a refresh returns a new token for the same
// grant — so a rotation must carry them forward. Losing them silently downgrades
// the credential: a client that gates on the recorded scopes (Claude Code needs
// `user:profile` for Remote Control) would fall back to inference-only the first
// time the token aged out, long after configure appeared to succeed.
func TestResolveOAuthRotationPreservesCapturedScopes(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	tokenSrv := newOAuthTokenServer(t, "new-access", "rt-2", 28800)
	sec := mustOAuthSecret(t, st, "claude", model.SecretValue{
		Token:                "old-access",
		RefreshToken:         "rt-1",
		TokenURL:             tokenSrv.server.URL,
		ClientID:             "client-x",
		AccessTokenExpiresAt: time.Now().UTC().Add(-time.Minute).UnixMilli(), // expired
		Scopes:               []string{"user:inference", "user:profile"},
		SubscriptionType:     "max",
	})
	mustGrant(t, st, sec.ID, model.SecretGrantScopeProject, "project-1")
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-OA")

	if _, err := svc.ResolveSandboxSecret(ctx, "pool-1", "sb-1", "SENTINEL-OA", "api.anthropic.com"); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	reloaded, err := st.GetSecret(ctx, "project-1", sec.ID)
	if err != nil {
		t.Fatalf("reload secret: %v", err)
	}
	val, err := st.OpenSecretValue(ctx, reloaded)
	if err != nil {
		t.Fatalf("open value: %v", err)
	}
	if val.Token != "new-access" {
		t.Fatalf("token = %q, want the rotated one", val.Token)
	}
	if !slices.Equal(val.Scopes, []string{"user:inference", "user:profile"}) {
		t.Fatalf("scopes after rotation = %#v, want them carried forward", val.Scopes)
	}
	if val.SubscriptionType != "max" {
		t.Fatalf("subscriptionType after rotation = %q, want max", val.SubscriptionType)
	}
}

// A token endpoint may answer without `expires_in` — it is conventional, not
// guaranteed, and OpenAI's is the endpoint the Codex ChatGPT credential
// refreshes against. Recording "unknown" there is not a neutral fallback:
// oauthNeedsRefresh reads unknown as "refresh now", so every later resolve
// refreshes again, and each one spends a refresh token that rotates on use. A
// JWT access token states its own expiry, so the rotation must read it.
func TestResolveOAuthDerivesExpiryFromJWTWhenEndpointOmitsExpiresIn(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	exp := time.Now().UTC().Add(8 * time.Hour).Truncate(time.Second)
	rotatedAccess := testJWT(t, exp)
	callCount := &atomic.Int32{}
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// No expires_in.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  rotatedAccess,
			"refresh_token": "rt-2",
			"token_type":    "Bearer",
		})
	}))
	t.Cleanup(tokenSrv.Close)

	sec := mustOAuthSecret(t, st, "codex", model.SecretValue{
		Token:                "old-access",
		RefreshToken:         "rt-1",
		TokenURL:             tokenSrv.URL,
		ClientID:             "client-x",
		AccessTokenExpiresAt: time.Now().UTC().Add(-time.Minute).UnixMilli(), // expired
	})
	mustGrant(t, st, sec.ID, model.SecretGrantScopeProject, "project-1")
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-OD")

	res, err := svc.ResolveSandboxSecret(ctx, "pool-1", "sb-1", "SENTINEL-OD", "chatgpt.com")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Value == nil || res.Value.Token != rotatedAccess {
		t.Fatalf("resolved token = %#v, want the rotated one", res.Value)
	}

	reloaded, err := st.GetSecret(ctx, "project-1", sec.ID)
	if err != nil {
		t.Fatalf("reload secret: %v", err)
	}
	val, err := st.OpenSecretValue(ctx, reloaded)
	if err != nil {
		t.Fatalf("open value: %v", err)
	}
	if got := val.AccessTokenExpiresAt; got != exp.UnixMilli() {
		t.Fatalf("persisted expiry = %d, want the token's own exp %d", got, exp.UnixMilli())
	}

	// The point of recording it: the next resolve must not refresh again.
	if _, err := svc.ResolveSandboxSecret(ctx, "pool-1", "sb-1", "SENTINEL-OD", "chatgpt.com"); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if calls := callCount.Load(); calls != 1 {
		t.Fatalf("token endpoint calls = %d, want 1 (the second resolve had a known, fresh expiry)", calls)
	}
}

// A non-JWT access token (Anthropic's, for one) leaves the expiry unknown, which
// is the pre-existing behavior and must stay: guessing an expiry for a token
// that never stated one would defer a refresh past the point the token dies.
func TestResolveOAuthLeavesExpiryUnknownForOpaqueToken(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		//nolint:gosec // Test literals, not real credentials.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "opaque-rotated-token-with-no-claims",
			"refresh_token": "rt-2",
			"token_type":    "Bearer",
		})
	}))
	t.Cleanup(tokenSrv.Close)

	sec := mustOAuthSecret(t, st, "claude", model.SecretValue{
		Token:                "old-access",
		RefreshToken:         "rt-1",
		TokenURL:             tokenSrv.URL,
		ClientID:             "client-x",
		AccessTokenExpiresAt: time.Now().UTC().Add(-time.Minute).UnixMilli(),
	})
	mustGrant(t, st, sec.ID, model.SecretGrantScopeProject, "project-1")
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-OE")

	if _, err := svc.ResolveSandboxSecret(ctx, "pool-1", "sb-1", "SENTINEL-OE", "api.anthropic.com"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	reloaded, err := st.GetSecret(ctx, "project-1", sec.ID)
	if err != nil {
		t.Fatalf("reload secret: %v", err)
	}
	val, err := st.OpenSecretValue(ctx, reloaded)
	if err != nil {
		t.Fatalf("open value: %v", err)
	}
	if val.AccessTokenExpiresAt != 0 {
		t.Fatalf("expiry = %d, want 0 (the token states none)", val.AccessTokenExpiresAt)
	}
}

// testJWT builds an unsigned JWT carrying only an exp claim. Nothing verifies
// the signature; the expiry is all the refresh path reads.
func testJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	enc := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return enc(map[string]string{"alg": "none", "typ": "JWT"}) + "." +
		enc(map[string]int64{"exp": exp.Unix()}) + ".signature"
}
