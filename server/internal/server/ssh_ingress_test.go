package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/obot-platform/discobox/server/internal/auth"
	"github.com/obot-platform/discobox/server/internal/services"
)

func newSSHIngressRouterForTest(ingress services.SSHIngress) *chi.Mux {
	stubs := newRouterTestServices()
	router, _ := NewRouter(services.Services{
		SSH:            ingress,
		Projects:       stubs,
		HarnessConfigs: stubs,
		Sandboxes:      stubs,
		Providers:      stubs,
		Pools:          stubs,
		Jobs:           stubs,
	})
	return router
}

func getSSHIngress(t *testing.T, router http.Handler) (int, map[string]any) {
	t.Helper()
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/ssh", nil))
	var body map[string]any
	if resp.Body.Len() > 0 {
		if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode GET /ssh body %q: %v", resp.Body.String(), err)
		}
	}
	return resp.Code, body
}

// TestSSHIngressRouteServesTheHostKey is what a client needs before it holds
// any other credential: the key to pin, and nothing else — there is no address
// to discover, because SSH reaches this server the one way (ADR 0057).
func TestSSHIngressRouteServesTheHostKey(t *testing.T) {
	router := newSSHIngressRouterForTest(services.SSHIngress{
		HostKey: "ssh-ed25519 AAAAfakehostkey==",
	})

	status, body := getSSHIngress(t, router)
	if status != http.StatusOK {
		t.Fatalf("GET /ssh status = %d, want %d", status, http.StatusOK)
	}
	if body["hostKey"] != "ssh-ed25519 AAAAfakehostkey==" {
		t.Fatalf("hostKey = %v, want the server's host key", body["hostKey"])
	}
	// The address and the enabled flag are gone, not empty: a client that still
	// reads them would be reading a distinction this server no longer draws.
	for _, dropped := range []string{"address", "enabled"} {
		if _, ok := body[dropped]; ok {
			t.Fatalf("GET /ssh still carries %q: %v", dropped, body[dropped])
		}
	}
}

// TestSSHIngressIsPublic pins the auth exemption: ssh-config has to read this
// before any other credential exists, and a host public key is not one.
func TestSSHIngressIsPublic(t *testing.T) {
	if !auth.IsPublicPath("/ssh") {
		t.Fatal("/ssh must be reachable without authentication")
	}
}
