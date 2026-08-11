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
		Events:         stubs,
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

// TestSSHIngressRouteAdvertisesTheDialableEndpoint is what lets a client stop
// hard-coding a port: the address and host key come from the server.
func TestSSHIngressRouteAdvertisesTheDialableEndpoint(t *testing.T) {
	router := newSSHIngressRouterForTest(services.SSHIngress{
		Enabled: true,
		Address: "ssh.example.com:3222",
		HostKey: "ssh-ed25519 AAAAfakehostkey==",
	})

	status, body := getSSHIngress(t, router)
	if status != http.StatusOK {
		t.Fatalf("GET /ssh status = %d, want %d", status, http.StatusOK)
	}
	if body["enabled"] != true {
		t.Fatalf("enabled = %v, want true", body["enabled"])
	}
	if body["address"] != "ssh.example.com:3222" {
		t.Fatalf("address = %v, want ssh.example.com:3222", body["address"])
	}
	if body["hostKey"] != "ssh-ed25519 AAAAfakehostkey==" {
		t.Fatalf("hostKey = %v, want the advertised host key", body["hostKey"])
	}
}

// TestSSHIngressRouteAnswersWhenDisabled: the ingress is opt-in, so a client
// must be able to tell "this server has no SSH" from "this server has no such
// route", which a 404 cannot express.
func TestSSHIngressRouteAnswersWhenDisabled(t *testing.T) {
	router := newSSHIngressRouterForTest(services.SSHIngress{})

	status, body := getSSHIngress(t, router)
	if status != http.StatusOK {
		t.Fatalf("GET /ssh status = %d, want %d", status, http.StatusOK)
	}
	if body["enabled"] != false {
		t.Fatalf("enabled = %v, want false", body["enabled"])
	}
	if _, ok := body["address"]; ok {
		t.Fatalf("a disabled ingress must not advertise an address, got %v", body["address"])
	}
	if _, ok := body["hostKey"]; ok {
		t.Fatalf("a disabled ingress must not advertise a host key, got %v", body["hostKey"])
	}
}

// TestSSHIngressIsPublic pins the auth exemption: ssh-config has to read this
// before any other credential exists, and neither field is a credential.
func TestSSHIngressIsPublic(t *testing.T) {
	if !auth.IsPublicPath("/ssh") {
		t.Fatal("/ssh must be reachable without authentication")
	}
}
