package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
)

func boxdRecipe() *model.ExchangeRecipe {
	return &model.ExchangeRecipe{
		URL:           "https://app.boxd.sh/api/v1/auth/token",
		Fields:        []string{"api_key"},
		Body:          map[string]string{"api_key": "{api_key}"},
		TokenPath:     "token",
		ExpiresAtPath: "expires_at",
	}
}

func TestARecipeSendsAStoredValueAsData(t *testing.T) {
	req, err := recipeRequest(context.Background(), boxdRecipe(), map[string]string{"api_key": `bxd_"quoted\{api_key}`})
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != "POST" || req.URL.Host != "app.boxd.sh" || req.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("request is %s %s (%s)", req.Method, req.URL, req.Header.Get("Content-Type"))
	}
	raw, _ := io.ReadAll(req.Body)
	var body map[string]string
	if err := json.Unmarshal(raw, &body); err != nil || body["api_key"] != `bxd_"quoted\{api_key}` {
		t.Fatalf("body %s does not carry the key as one JSON string, unexpanded: %v", raw, err)
	}
	if _, err := recipeRequest(context.Background(), boxdRecipe(), map[string]string{"api_key": " "}); err == nil || !strings.Contains(err.Error(), "api_key") {
		t.Fatalf("a blank key = %v, want it refused naming api_key", err)
	}
}

func TestAFormRecipeFillsItsTemplates(t *testing.T) {
	r := &model.ExchangeRecipe{
		URL:    "https://login.example/token",
		Form:   true,
		Fields: []string{"id", "secret"},
		Body:   map[string]string{"grant_type": "client_credentials", "client_id": "{id}", "client_secret": "{secret}"},
		Header: map[string]string{"X-Client": "c-{id}"},
	}
	req, err := recipeRequest(context.Background(), r, map[string]string{"id": "abc", "secret": "s&t="})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(req.Body)
	form, err := url.ParseQuery(string(raw))
	if err != nil || form.Get("grant_type") != "client_credentials" || form.Get("client_id") != "abc" || form.Get("client_secret") != "s&t=" {
		t.Fatalf("form %q: %v", raw, err)
	}
	if req.Header.Get("X-Client") != "c-abc" || req.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		t.Fatalf("headers %v", req.Header)
	}
}

func TestTheAnswerSaysTheTokenAndWhenItGoesStale(t *testing.T) {
	at := time.Now().Add(time.Hour).Truncate(time.Second).UTC()
	unix := strconv.FormatInt(at.Unix(), 10)
	nested := &model.ExchangeRecipe{TokenPath: "auth.client_token", ExpiresInPath: "auth.lease_duration"} //nolint:gosec // Paths into an answer, not a credential.
	token, expiry, err := recipeToken(nested, []byte(`{"auth":{"client_token":"hvs.x","lease_duration":3600}}`))
	if err != nil || token != "hvs.x" || expiry.Sub(at).Abs() > 5*time.Second {
		t.Fatalf("nested = %q %v %v", token, expiry, err)
	}
	token, expiry, err = recipeToken(boxdRecipe(), []byte(`{"token":"a.b.c","expires_at":`+unix+`}`))
	if err != nil || token != "a.b.c" || !expiry.Equal(at) {
		t.Fatalf("boxd = %q %v %v", token, expiry, err)
	}
	jwt := "h." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":`+unix+`}`)) + ".s"
	if _, expiry, _ = recipeToken(boxdRecipe(), []byte(`{"token":"`+jwt+`"}`)); !expiry.Equal(at) {
		t.Fatalf("an answer without expires_at read %v, want the JWT's exp %v", expiry, at)
	}
	if _, _, err := recipeToken(boxdRecipe(), []byte(`{"error":"nope"}`)); err == nil {
		t.Fatal("an answer with no token was read as one")
	}
}

func TestARecipeIsHeldToTheSecretsBinding(t *testing.T) {
	if err := checkRecipe(boxdRecipe(), "boxd.sh"); err != nil {
		t.Fatalf("boxd's recipe under boxd.sh: %v", err)
	}
	for host, want := range map[string]string{"": "bound to a host", "example.com": "outside the secret's binding", "api.boxd.sh": "outside"} {
		if err := checkRecipe(boxdRecipe(), host); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("binding %q = %v, want %q", host, err, want)
		}
	}
}

// The dial to an exchange's endpoint checks the address it connects to, so a
// name that resolved outward when it was checked and inward when it is dialed
// still never reaches an internal address.
func TestTheExchangeDialRefusesAnInternalAddress(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	previous := tokenHTTPClient
	withTLS := *previous
	withTLS.Transport = srv.Client().Transport
	tokenHTTPClient = &withTLS
	t.Cleanup(func() { tokenHTTPClient = previous })

	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := exchangeClient(target).Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("the dial reached a loopback endpoint")
	}
	if !strings.Contains(err.Error(), "internal address") {
		t.Fatalf("dial = %v, want refused as an internal address", err)
	}
}
