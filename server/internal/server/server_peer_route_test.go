package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/discobox-ai/discobox/endpoint"
	"github.com/discobox-ai/discobox/server/internal/services"
)

func newServerPeerRouterForTest(peer services.ServerPeer) *chi.Mux {
	stubs := newRouterTestServices()
	router, _ := NewRouter(services.Services{
		ServerPeer:     peer,
		Projects:       stubs,
		HarnessConfigs: stubs,
		Sandboxes:      stubs,
		Providers:      stubs,
		Pools:          stubs,
		Jobs:           stubs,
	})
	return router
}

func getServerPeer(t *testing.T, router http.Handler) (int, map[string]any) {
	t.Helper()
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/peer", nil))
	var body map[string]any
	if resp.Body.Len() > 0 {
		if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode GET /peer body %q: %v", resp.Body.String(), err)
		}
	}
	return resp.Code, body
}

// What a client asks when it wants to know what to dial (ADR 0098).
//
// This covers routing and the handler only: NewRouter installs no middleware,
// so the authorization layer does not run here. That layer has its own test —
// TestAuthenticatedAllowedPaths in internal/auth — because it is the part that
// was silently wrong the first time this route was wired, and no test that
// builds a bare router can catch it.
func TestServerPeerRouteServesThePeerID(t *testing.T) {
	var key [32]byte
	key[0] = 0xaa
	id, err := endpoint.IrohIDFromPublicKey(key[:])
	if err != nil {
		t.Fatalf("IrohIDFromPublicKey() error = %v", err)
	}
	router := newServerPeerRouterForTest(services.ServerPeer{ID: id.String()})

	status, body := getServerPeer(t, router)
	if status != http.StatusOK {
		t.Fatalf("GET /peer status = %d, want %d", status, http.StatusOK)
	}
	// The one written form, so what this serves can be compared by eye against
	// what the server logged and what a client dials (ADR 0097 §5).
	if body["peerId"] != id.String() {
		t.Fatalf("peerId = %v, want %q", body["peerId"], id.String())
	}
}

// A service holding no ID omits the field rather than serving the zero one,
// and the request still succeeds. Every server has an ID now (ADR 0117), so
// this is the shape of a server that predates that, which a client still has
// to read.
func TestServerPeerRouteOmitsTheIDWhenThereIsNone(t *testing.T) {
	router := newServerPeerRouterForTest(services.ServerPeer{})

	status, body := getServerPeer(t, router)
	if status != http.StatusOK {
		t.Fatalf("GET /peer status = %d, want %d", status, http.StatusOK)
	}
	if value, ok := body["peerId"]; ok {
		t.Fatalf("peerId = %v, want it absent on a server with no peer identity", value)
	}
}

// The zero ID renders as a perfectly well-formed peer ID that reaches nothing,
// so a server without an iroh endpoint must report no ID rather than that one.
func TestServerPeerFromZeroIdentityIsEmpty(t *testing.T) {
	if peer := serverPeer(endpoint.IrohID{}); peer.ID != "" {
		t.Fatalf("serverPeer(zero) = %q, want empty", peer.ID)
	}
	var key [32]byte
	key[0] = 0xbb
	id, err := endpoint.IrohIDFromPublicKey(key[:])
	if err != nil {
		t.Fatalf("IrohIDFromPublicKey() error = %v", err)
	}
	if peer := serverPeer(id); peer.ID != id.String() {
		t.Fatalf("serverPeer(id).ID = %q, want %q", peer.ID, id.String())
	}
}
