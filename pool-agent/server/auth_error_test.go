package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A refusal has to arrive as the API says it will. The spec declares JSON for
// every error, so a text/plain body means the generated client cannot decode
// the response at all: the control plane sees "unexpected Content-Type" with
// decoder frames around it, and never learns the status was 401.
func TestRefusalDecodesAsTheDeclaredErrorShape(t *testing.T) {
	recorder := refuse(t, "")

	if got := recorder.Code; got != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", got)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var body struct {
		Status int    `json:"status"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	if body.Status != http.StatusUnauthorized || body.Title != "Unauthorized" {
		t.Fatalf("body = %+v, want a 401 Unauthorized", body)
	}
}

// The four refusals have to be distinguishable. An expired token means the
// clocks disagree and a bad signature means this agent holds a different
// control-plane key than the caller signs with -- opposite problems that were
// reported with the same bare word.
func TestRefusalsSayWhichOneHappened(t *testing.T) {
	missing := detailOf(t, refuse(t, ""))
	if missing != reasonMissingToken {
		t.Fatalf("no-token detail = %q, want %q", missing, reasonMissingToken)
	}

	garbage := detailOf(t, refuse(t, "Bearer v4.public.not-a-real-token"))
	if garbage != reasonInvalidToken {
		t.Fatalf("bad-token detail = %q, want %q", garbage, reasonInvalidToken)
	}
	if missing == garbage {
		t.Fatal("a missing token and an unverifiable one are indistinguishable")
	}
}

func refuse(t *testing.T, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	// A key nothing signs with: every request reaching this authenticator is
	// refused, which is what these tests are about.
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	authenticator, err := NewSignedTokenAuthenticator(
		Identity{ProjectID: "p1", PoolID: "pool_1"},
		base64.StdEncoding.EncodeToString(public),
	)
	if err != nil {
		t.Fatalf("NewSignedTokenAuthenticator: %v", err)
	}
	handler := authenticator.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("a refused request reached the handler")
	}))

	request := httptest.NewRequest(http.MethodPost, "/api/projects/p1/pools/pool_1/sync", strings.NewReader("{}"))
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func detailOf(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	return body.Detail
}
