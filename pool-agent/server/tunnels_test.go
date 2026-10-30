package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A machine pool reached only through an edge carries its sandboxes' tunnels
// on its agent's own listener, with no token, since the mTLS inside names
// the caller (ADR 26-10-29-481 §2); on any other pool the path is the API's,
// which asks for a token.
func TestRouterServesTunnelsOnlyWhereTheyAreGiven(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Identity:              Identity{ProjectID: "p1", PoolID: "pool_1"},
		ControlPlanePublicKey: base64.StdEncoding.EncodeToString(public),
	}
	for _, tc := range []struct {
		name    string
		tunnels http.Handler
		want    int
	}{
		{name: "none", want: http.StatusUnauthorized},
		{name: "given", tunnels: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		}), want: http.StatusTeapot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg.Tunnels = tc.tunnels
			router, err := NewRouter(cfg)
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/tunnel/proxy", nil))
			if recorder.Code != tc.want {
				t.Fatalf("GET /tunnel/proxy = %d, want %d", recorder.Code, tc.want)
			}
		})
	}
}
