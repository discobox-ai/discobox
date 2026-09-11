package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/endpoint"
)

// The /peers routes through the real router.
//
// The service tests call the service directly, so none of the authorization
// layer runs there — and that layer is the part most likely to be silently
// wrong here: the resource is server-scoped, so neither ProjectAuthorizer nor
// PoolRouteAuthorizer applies, and without an entry in
// authenticatedAllowedPaths every one of these routes 403s (ADR 0095 §1, enrolled iroh IDs).
func TestPeerRoutesEnrollListAndRevoke(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	db := newAppTestDB(ctx, t)
	router, _, _, _, err := NewApp(ctx, db.Write, db.Read)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}

	var key [32]byte
	key[0] = 0xaa
	peer, err := endpoint.IrohIDFromPublicKey(key[:])
	if err != nil {
		t.Fatalf("IrohIDFromPublicKey() error = %v", err)
	}

	do := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		var reader *strings.Reader
		if body == "" {
			reader = strings.NewReader("")
		} else {
			reader = strings.NewReader(body)
		}
		req := httptest.NewRequestWithContext(ctx, method, path, reader)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	if rec := do(http.MethodGet, "/peers", ""); rec.Code != http.StatusOK {
		t.Fatalf("GET /peers = %d (%s), want 200", rec.Code, rec.Body.String())
	}

	rec := do(http.MethodPost, "/peers", `{"peerId":"`+peer.String()+`","name":"laptop"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /peers = %d (%s), want 200", rec.Code, rec.Body.String())
	}

	// Enrolling the same identity twice would leave two rows, so revoking the
	// one an operator can see would look like it worked while the other still
	// admits the peer.
	if rec := do(http.MethodPost, "/peers", `{"peerId":"`+peer.Key()+`"}`); rec.Code != http.StatusConflict {
		t.Fatalf("re-enrolling = %d (%s), want 409", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodPost, "/peers", `{"peerId":"not-a-peer"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("enrolling a non-ID = %d (%s), want 400", rec.Code, rec.Body.String())
	}

	rec = do(http.MethodGet, "/peers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /peers = %d, want 200", rec.Code)
	}
	var listed struct {
		Peers []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"peers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v (%s)", err, rec.Body.String())
	}
	if len(listed.Peers) != 1 || listed.Peers[0].ID != peer.String() || listed.Peers[0].Name != "laptop" {
		t.Fatalf("listed = %+v, want the one enrollment", listed.Peers)
	}

	// Whatever was sent in, what comes back is the one written form, so an
	// operator can compare it against what `discobox admin peer id` printed
	// without decoding either (ADR 0097 §5).

	// A LIKE wildcard must not name a row, even when exactly one row exists
	// and the "unambiguous prefix" rule would otherwise resolve it.
	if rec := do(http.MethodDelete, "/peers/%25", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE /peers/%%25 = %d (%s), want 404", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodDelete, "/peers/"+peer.Key()[:12], ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE by prefix = %d (%s), want 204", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodDelete, "/peers/"+peer.Key()[:12], ""); rec.Code != http.StatusNotFound {
		t.Fatalf("second DELETE = %d, want 404", rec.Code)
	}
}
