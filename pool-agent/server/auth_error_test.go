package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	workerapi "github.com/discobox-ai/discobox/pool-agent/api/gen"
)

// Exercise the generated decoder: valid JSON alone is not enough when the
// contract requires application/problem+json.
func TestRefusalDecodesAsTheDeclaredErrorShape(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewRouter(Config{
		Identity:              Identity{ProjectID: "p1", PoolID: "pool_1"},
		ControlPlanePublicKey: base64.StdEncoding.EncodeToString(public),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	for _, tc := range []struct{ name, token, reason string }{
		{"missing token", "", reasonMissingToken},
		{"invalid token", "v4.public.not-a-real-token", reasonInvalidToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := workerapi.NewClient(server.URL, refusalSecuritySource(tc.token), workerapi.WithClient(server.Client()))
			if err != nil {
				t.Fatal(err)
			}
			err = client.PoolSync(t.Context(), &workerapi.PoolSyncRequest{KnownPoolIds: []string{"pool_1"}}, workerapi.PoolSyncParams{ProjectId: "p1", PoolId: "pool_1"})
			var refusal *workerapi.ErrorModelStatusCode
			if !errors.As(err, &refusal) {
				t.Fatalf("PoolSync error = %v, want decoded API error", err)
			}
			if refusal.StatusCode != http.StatusUnauthorized || refusal.Response.Status.Value != http.StatusUnauthorized || refusal.Response.Title.Value != "Unauthorized" || refusal.Response.Detail.Value != tc.reason {
				t.Fatalf("refusal = %+v, want 401 Unauthorized with detail %q", refusal, tc.reason)
			}
		})
	}
}

type refusalSecuritySource string

func (s refusalSecuritySource) PoolBearerAuth(context.Context, workerapi.OperationName) (workerapi.PoolBearerAuth, error) {
	return workerapi.PoolBearerAuth{Token: string(s)}, nil
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

	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/projects/p1/pools/pool_1/sync", strings.NewReader("{}"))
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
