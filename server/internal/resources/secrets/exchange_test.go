package secrets_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apigen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	"github.com/discobox-ai/discobox/server/internal/services"
)

// boxdTokenServer stands in for boxd's key exchange: it takes {"api_key"} and
// answers a fresh token, expiring after lifetime, for the one key it knows.
type boxdTokenServer struct {
	url      string
	calls    atomic.Int32
	lifetime time.Duration
	mu       sync.Mutex
	key      string
	lastKey  string
	// answer, when set, is what the server says instead of a token.
	answer func(w http.ResponseWriter, r *http.Request) bool
}

func (ts *boxdTokenServer) setKey(key string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.key = key
}

func newBoxdTokenServer(t *testing.T, key string, lifetime time.Duration) *boxdTokenServer {
	t.Helper()
	ts := &boxdTokenServer{key: key, lifetime: lifetime}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The sandbox-agent's port probe is not an exchange (REVIEW.md).
		if r.Header.Get("User-Agent") == "discobox-sandbox-agent (port probe)" {
			return
		}
		n := ts.calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var body map[string]string
		_ = json.Unmarshal(raw, &body)
		ts.mu.Lock()
		ts.lastKey = body["api_key"]
		key, answer := ts.key, ts.answer
		ts.mu.Unlock()
		if answer != nil && answer(w, r) {
			return
		}
		if r.Header.Get("Content-Type") != "application/json" || body["api_key"] != key {
			http.Error(w, `{"error":"invalid API key"}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "eyJhbGciOiJIUzI1NiJ9.token-" + strings.Repeat("x", int(n)) + ".sig",
			"expires_at": time.Now().Add(ts.lifetime).Unix(),
		})
	}))
	t.Cleanup(srv.Close)
	ts.url = srv.URL + "/api/v1/auth/token"
	resourcesecrets.SetTokenHTTPClient(t, srv.Client())
	resourcesecrets.AllowInternalExchangeTargets(t)
	return ts
}

// boxdRecipe is boxd's exchange, at the test server: the key in a JSON body,
// the token and its unix expiry in the answer.
func (ts *boxdTokenServer) recipe() apigen.ExchangeRecipe {
	return apigen.ExchangeRecipe{
		URL:           ts.url,
		Fields:        []string{"api_key"},
		Body:          apigen.NewOptExchangeRecipeBody(apigen.ExchangeRecipeBody{"api_key": "{api_key}"}),
		TokenPath:     "token",
		ExpiresAtPath: apigen.NewOptString("expires_at"),
	}
}

func boxdValue(key string) apigen.SecretValue {
	return apigen.SecretValue{Exchange: apigen.NewOptSecretValueExchange(apigen.SecretValueExchange{"api_key": key})}
}

// createBoxdSecret stores an exchange secret the way the boxd helper script
// does, bound to the test server's host.
func createBoxdSecret(t *testing.T, svc *resourcesecrets.Service, ts *boxdTokenServer, key string) (*model.Secret, error) {
	t.Helper()
	return svc.CreateSecret(testPrincipalContext(), "project-1", services.CreateSecretBody{
		Name:     "boxd",
		Type:     apigen.CreateSecretBodyTypeExchange,
		Host:     apigen.NewOptString("127.0.0.1"),
		Exchange: apigen.NewOptExchangeRecipe(ts.recipe()),
		Value:    boxdValue(key),
	})
}

// Storing an exchange secret exchanges it at once: the key is checked by the
// endpoint that will judge it, and the sentinel takes the token's shape.
func TestAnExchangeSecretIsExchangedWhenStored(t *testing.T) {
	svc, st := newResolveFixture(t)
	ts := newBoxdTokenServer(t, "bxd_good", time.Hour)

	sec, err := createBoxdSecret(t, svc, ts, "bxd_good")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if ts.calls.Load() != 1 {
		t.Fatalf("exchanges at create = %d, want 1", ts.calls.Load())
	}
	if sec.Exchange == nil || sec.Exchange.Recipe == nil || sec.Exchange.Recipe.URL != ts.url ||
		len(sec.Exchange.Recipe.Fields) != 1 || sec.Exchange.Recipe.Fields[0] != "api_key" || sec.Exchange.TokenExpiresAt == 0 {
		t.Fatalf("created secret = %+v, exchange %+v", sec, sec.Exchange)
	}
	if strings.Count(sec.Format, ".") != 2 || strings.Contains(sec.Format, "bxd_") {
		t.Fatalf("format = %q, want the token's three-part shape, not the key's", sec.Format)
	}
	val, err := st.OpenSecretValue(context.Background(), sec)
	if err != nil || val.Exchange["api_key"] != "bxd_good" || !strings.HasPrefix(val.Token, "eyJ") {
		t.Fatalf("stored value = %+v, %v", val, err)
	}
}

func TestAKeyTheEndpointRefusesIsNotStored(t *testing.T) {
	svc, st := newResolveFixture(t)
	ts := newBoxdTokenServer(t, "bxd_good", time.Hour)

	_, err := createBoxdSecret(t, svc, ts, "bxd_wrong")
	var status interface{ StatusCode() int }
	if err == nil || !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("create with a refused key = %v, want a 400 saying it was refused", err)
	}
	if secrets, _ := st.ListSecrets(context.Background(), "project-1"); len(secrets) != 0 {
		t.Fatalf("a refused key was stored: %+v", secrets)
	}
}

func TestAnExchangeSecretNeedsASoundRecipeAndItsFields(t *testing.T) {
	svc, _ := newResolveFixture(t)
	ts := newBoxdTokenServer(t, "bxd_good", time.Hour)
	ctx := testPrincipalContext()
	with := func(edit func(*apigen.ExchangeRecipe)) apigen.OptExchangeRecipe {
		r := ts.recipe()
		edit(&r)
		return apigen.NewOptExchangeRecipe(r)
	}
	body := func(name, host string, recipe apigen.OptExchangeRecipe, value apigen.SecretValue) services.CreateSecretBody {
		return services.CreateSecretBody{Name: name, Type: apigen.CreateSecretBodyTypeExchange, Host: apigen.NewOptString(host), Exchange: recipe, Value: value}
	}
	cases := map[string]services.CreateSecretBody{
		"no recipe":                         body("a", "127.0.0.1", apigen.OptExchangeRecipe{}, boxdValue("bxd_good")),
		"no host binding":                   body("b", "", apigen.NewOptExchangeRecipe(ts.recipe()), boxdValue("bxd_good")),
		"a url outside the binding":         body("c", "boxd.sh", apigen.NewOptExchangeRecipe(ts.recipe()), boxdValue("bxd_good")),
		"plain http":                        body("d", "127.0.0.1", with(func(r *apigen.ExchangeRecipe) { r.URL = strings.Replace(r.URL, "https:", "http:", 1) }), boxdValue("bxd_good")),
		"a template naming no field":        body("e", "127.0.0.1", with(func(r *apigen.ExchangeRecipe) { r.Body.Value["api_key"] = "{apikey}" }), boxdValue("bxd_good")),
		"a field nothing sends":             body("f", "127.0.0.1", with(func(r *apigen.ExchangeRecipe) { r.Fields = append(r.Fields, "spare") }), boxdValue("bxd_good")),
		"no key":                            body("g", "127.0.0.1", apigen.NewOptExchangeRecipe(ts.recipe()), apigen.SecretValue{}),
		"a field the recipe does not store": body("h", "127.0.0.1", apigen.NewOptExchangeRecipe(ts.recipe()), apigen.SecretValue{Exchange: apigen.NewOptSecretValueExchange(apigen.SecretValueExchange{"api_key": "bxd_good", "password": "x"})}),
	}
	for name, b := range cases {
		if _, err := svc.CreateSecret(ctx, "project-1", b); err == nil {
			t.Errorf("%s: created, want refused", name)
		}
	}
	token := services.CreateSecretBody{Name: "i", Type: apigen.CreateSecretBodyTypeToken, Host: apigen.NewOptString("127.0.0.1"),
		Exchange: apigen.NewOptExchangeRecipe(ts.recipe()), Value: apigen.SecretValue{Token: apigen.NewOptString("t")}}
	if _, err := svc.CreateSecret(ctx, "project-1", token); err == nil {
		t.Error("a token with an exchange recipe was created")
	}
	if ts.calls.Load() != 0 {
		t.Fatalf("exchanges = %d, want none for a refused recipe", ts.calls.Load())
	}
}

// A recipe says where a stored key is sent, so a new one comes only with the
// key: an update cannot point a stored key at somebody else's endpoint, and a
// new binding cannot leave the recipe's endpoint outside it.
func TestAStoredKeyIsNeverSentSomewhereNew(t *testing.T) {
	svc, _ := newResolveFixture(t)
	ts := newBoxdTokenServer(t, "bxd_good", time.Hour)
	ctx := testPrincipalContext()
	sec, err := createBoxdSecret(t, svc, ts, "bxd_good")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	moved := ts.recipe()
	moved.URL = strings.Replace(moved.URL, "/api/v1/auth/token", "/elsewhere", 1)
	if _, err := svc.UpdateSecret(ctx, "project-1", sec.ID, services.UpdateSecretBody{Exchange: apigen.NewOptExchangeRecipe(moved)}); err == nil {
		t.Fatal("a recipe was replaced without the key")
	}
	if _, err := svc.UpdateSecret(ctx, "project-1", sec.ID, services.UpdateSecretBody{Host: apigen.NewOptString("boxd.sh")}); err == nil {
		t.Fatal("a binding was moved off the recipe's endpoint")
	}
	updated, err := svc.UpdateSecret(ctx, "project-1", sec.ID, services.UpdateSecretBody{
		Exchange: apigen.NewOptExchangeRecipe(ts.recipe()),
		Value:    apigen.NewOptSecretValue(boxdValue("bxd_good")),
	})
	if err != nil || updated.Exchange == nil || updated.Exchange.Recipe.URL != ts.url {
		t.Fatalf("replacing recipe and key together = %+v, %v", updated, err)
	}
}

func TestADiscoboxMayNotGiveAnExchangeRecipe(t *testing.T) {
	svc, _ := newResolveFixture(t)
	ts := newBoxdTokenServer(t, "bxd_good", time.Hour)
	_, err := svc.CreateSecret(auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.PrincipalTypeSandbox, SandboxID: "sb-1", ProjectID: "project-1", UserID: "user-1"}), "project-1", services.CreateSecretBody{
		Name: "boxd", Type: apigen.CreateSecretBodyTypeExchange, Host: apigen.NewOptString("127.0.0.1"),
		Exchange: apigen.NewOptExchangeRecipe(ts.recipe()), Value: boxdValue("bxd_good"),
	})
	var status interface{ StatusCode() int }
	if err == nil || !errors.As(err, &status) || status.StatusCode() != http.StatusForbidden {
		t.Fatalf("a discobox's recipe = %v, want 403", err)
	}
}

// Resolve hands out the token, and exchanges again once it is within the skew
// of expiring; the key itself never leaves.
func TestResolveRenewsAnExchangedTokenAsItAgesOut(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)
	ts := newBoxdTokenServer(t, "bxd_good", 2*time.Minute) // inside the 5m skew: every resolve renews

	sec, err := createBoxdSecret(t, svc, ts, "bxd_good")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	mustGrant(t, st, sec.ID, model.SecretGrantScopeProject, "project-1")
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-BX")

	res, err := svc.ResolveSandboxSecret(ctx, "pool-1", "sb-1", "SENTINEL-BX", "127.0.0.1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Status != model.SecretRequestStatusApproved || res.Value == nil || !strings.HasPrefix(res.Value.Token, "eyJ") ||
		!strings.Contains(res.Value.Token, "token-xx.") {
		t.Fatalf("resolution = %+v, want the second exchange's token", res)
	}
	if res.ExpiresAt == nil || res.ExpiresAt.After(time.Now().Add(3*time.Minute)) {
		t.Fatalf("resolution expires %v, want capped by the token's own expiry", res.ExpiresAt)
	}
	if ts.calls.Load() != 2 {
		t.Fatalf("exchanges = %d, want 2 (create, renew)", ts.calls.Load())
	}
}

func TestResolveServesAFreshExchangedTokenWithoutExchanging(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)
	ts := newBoxdTokenServer(t, "bxd_good", time.Hour)

	sec, err := createBoxdSecret(t, svc, ts, "bxd_good")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	mustGrant(t, st, sec.ID, model.SecretGrantScopeProject, "project-1")
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-BX")
	for range 3 {
		if _, err := svc.ResolveSandboxSecret(ctx, "pool-1", "sb-1", "SENTINEL-BX", "127.0.0.1"); err != nil {
			t.Fatalf("resolve: %v", err)
		}
	}
	if ts.calls.Load() != 1 {
		t.Fatalf("exchanges = %d, want only the one at create", ts.calls.Load())
	}
}

// A token an upstream refuses is exchanged again before anything is recorded;
// a key the endpoint then refuses is recorded as a renewal that failed.
func TestARefusedExchangedTokenIsRenewedThenRecorded(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)
	ts := newBoxdTokenServer(t, "bxd_good", time.Hour)

	sec, err := createBoxdSecret(t, svc, ts, "bxd_good")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	mustGrant(t, st, sec.ID, model.SecretGrantScopeProject, "project-1")
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-BX")

	if err := svc.RecordSandboxSecretRejection(ctx, "pool-1", "sb-1", "SENTINEL-BX", "127.0.0.1", resourcesecrets.SecretRejectedOutcome, ""); err != nil {
		t.Fatalf("record: %v", err)
	}
	if ts.calls.Load() != 2 {
		t.Fatalf("exchanges = %d, want a forced one after the rejection", ts.calls.Load())
	}
	if rejections, _ := svc.ListSecretRejections(testPrincipalContext(), "project-1"); len(rejections) != 0 {
		t.Fatalf("a renewed token was recorded as rejected: %+v", rejections)
	}

}

// A refusal of a token whose key has since been revoked cannot be renewed
// away, and is recorded as a renewal that failed: a person has to store a new
// key.
func TestARevokedExchangeKeyIsRecordedAsARenewalThatFailed(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)
	ts := newBoxdTokenServer(t, "bxd_good", time.Hour)

	sec, err := createBoxdSecret(t, svc, ts, "bxd_good")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	mustGrant(t, st, sec.ID, model.SecretGrantScopeProject, "project-1")
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-BX")

	ts.setKey("bxd_rotated")
	if err := svc.RecordSandboxSecretRejection(ctx, "pool-1", "sb-1", "SENTINEL-BX", "127.0.0.1", resourcesecrets.SecretRejectedOutcome, ""); err != nil {
		t.Fatalf("record: %v", err)
	}
	rejections, err := svc.ListSecretRejections(testPrincipalContext(), "project-1")
	if err != nil || len(rejections) != 1 || rejections[0].Reason != model.SecretRejectionReasonRefreshFailed {
		t.Fatalf("rejections = %+v, %v; want one, refresh-failed", rejections, err)
	}
}

// A token endpoint that redirects is not followed: a 307 would re-send the key
// in the body to wherever it names, past the checks made on the recipe's URL.
func TestAnExchangeFollowsNoRedirect(t *testing.T) {
	svc, _ := newResolveFixture(t)
	var elsewhere atomic.Int32
	other := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "discobox-sandbox-agent (port probe)" {
			elsewhere.Add(1)
		}
	}))
	t.Cleanup(other.Close)
	ts := newBoxdTokenServer(t, "bxd_good", time.Hour)
	ts.answer = func(w http.ResponseWriter, r *http.Request) bool {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
		return true
	}
	if _, err := createBoxdSecret(t, svc, ts, "bxd_good"); err == nil {
		t.Fatal("a redirecting endpoint stored the secret")
	}
	if elsewhere.Load() != 0 {
		t.Fatal("the key followed the redirect")
	}
}

// 429 is the endpoint asking to be called less often: it says nothing about
// the key, so storing it fails as unreachable, not as refused.
func TestATooManyRequestsAnswerIsNotARefusal(t *testing.T) {
	svc, _ := newResolveFixture(t)
	ts := newBoxdTokenServer(t, "bxd_good", time.Hour)
	ts.answer = func(w http.ResponseWriter, _ *http.Request) bool {
		http.Error(w, strings.Repeat("slow down ", 100), http.StatusTooManyRequests)
		return true
	}
	_, err := createBoxdSecret(t, svc, ts, "bxd_good")
	var status interface{ StatusCode() int }
	if err == nil || !errors.As(err, &status) || status.StatusCode() != http.StatusBadGateway {
		t.Fatalf("create on a 429 = %v, want a 502", err)
	}
	if len(err.Error()) > 400 {
		t.Fatalf("the error carries %d bytes of the upstream's answer, want it cut short", len(err.Error()))
	}
}

// An answer that says nothing about expiry, with a token that is not a JWT, is
// trusted for a while rather than exchanged again on every resolve.
func TestATokenOfUnknownLifetimeIsNotExchangedOnEveryResolve(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)
	ts := newBoxdTokenServer(t, "bxd_good", time.Hour)
	ts.answer = func(w http.ResponseWriter, _ *http.Request) bool {
		_, _ = w.Write([]byte(`{"token":"opaque-token"}`)) //nolint:gosec // A test endpoint's answer, not a credential.
		return true
	}
	sec, err := createBoxdSecret(t, svc, ts, "bxd_good")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sec.Exchange == nil || sec.Exchange.TokenExpiresAt == 0 {
		t.Fatalf("exchange = %+v, want a lifetime assumed", sec.Exchange)
	}
	mustGrant(t, st, sec.ID, model.SecretGrantScopeProject, "project-1")
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-BX")
	for range 3 {
		if _, err := svc.ResolveSandboxSecret(ctx, "pool-1", "sb-1", "SENTINEL-BX", "127.0.0.1"); err != nil {
			t.Fatalf("resolve: %v", err)
		}
	}
	if ts.calls.Load() != 1 {
		t.Fatalf("exchanges = %d, want only the one at create", ts.calls.Load())
	}
}

// Approving a request may rebind the secret it answers with, and an exchange
// secret's binding is where its key may be sent: the approval is held to the
// recipe the way an update is, and refused rather than moving the key.
func TestApprovingCannotMoveAnExchangeSecretOffItsRecipe(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	ts := newBoxdTokenServer(t, "bxd_good", time.Hour)
	sec, err := createBoxdSecret(t, svc, ts, "bxd_good")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	req := createAgentRequest(ctx, t, svc)
	for _, host := range []string{"api.github.com", ""} {
		_, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, services.ApproveSecretRequestBody{
			SecretId:   apigen.NewOptString(sec.ID),
			SecretHost: apigen.NewOptString(host),
		})
		if err == nil || !strings.Contains(err.Error(), "exchange recipe") {
			t.Fatalf("approve rebinding to %q = %v, want refused by the recipe", host, err)
		}
	}
	if stored, _ := st.GetSecret(ctx, "project-1", sec.ID); stored == nil || stored.Host != "127.0.0.1" {
		t.Fatalf("secret = %#v, want its binding kept", stored)
	}
}

// A recipe may not send the server to an address only it can reach — loopback,
// private, link-local — and read back the answer.
func TestAnExchangeIsNeverSentToAnInternalAddress(t *testing.T) {
	svc, _ := newResolveFixture(t)
	ctx := testPrincipalContext()
	for target, host := range map[string]string{
		"https://127.0.0.1:8443/token":  "127.0.0.1",
		"https://10.1.2.3/token":        "10.1.2.3",
		"https://169.254.169.254/token": "169.254.169.254",
		"https://[::1]/token":           "::1",
		"https://100.64.0.1/token":      "100.64.0.1",
		"https://localhost/token":       "localhost",
	} {
		_, err := svc.CreateSecret(ctx, "project-1", services.CreateSecretBody{
			Name: "internal", Type: apigen.CreateSecretBodyTypeExchange, Host: apigen.NewOptString(host),
			Exchange: apigen.NewOptExchangeRecipe(apigen.ExchangeRecipe{URL: target, Fields: []string{"api_key"},
				Body: apigen.NewOptExchangeRecipeBody(apigen.ExchangeRecipeBody{"api_key": "{api_key}"}), TokenPath: "token"}),
			Value: boxdValue("bxd_good"),
		})
		var status interface{ StatusCode() int }
		if err == nil || !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest || !strings.Contains(err.Error(), "internal address") {
			t.Errorf("%s = %v, want a 400 refusing an internal address", target, err)
		}
	}
}
