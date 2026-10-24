package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/auth/discobot"
)

// With authentication required the server never answers as the default user:
// a request with no credential is refused on any listener, the public paths
// still answer, and a discobot assertion still authenticates (ADR
// 26-10-07-005).
func TestAuthRequiredServesNoDefaultUser(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	db := newAppTestDB(ctx, t)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	opts := DefaultAppOptions()
	opts.DiscobotPublicKey = public
	opts.AuthRequired = true
	router, _, _, stop, err := NewApp(ctx, db.Write, db.Read, opts)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := stop(stopCtx); err != nil {
			t.Errorf("stop services: %v", err)
		}
	})
	call := func(path, assertion string) int {
		request := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
		if assertion != "" {
			request.Header.Set(discobot.AssertionHeader, assertion)
		}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		return recorder.Code
	}
	token, err := discobot.Sign(private, discobot.Claims{UserID: "usr_priya", ID: "a1"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	if status := call("/projects", ""); status != http.StatusUnauthorized {
		t.Fatalf("GET /projects with no credential = %d, want 401", status)
	}
	if status := call("/healthz", ""); status != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", status)
	}
	if status := call("/projects", token); status != http.StatusOK {
		t.Fatalf("GET /projects with an assertion = %d, want 200", status)
	}
	// SSH is the CLI's, as its own user, and with authentication required
	// there is none: unauthenticated it is a 401, asserted a 403.
	if status := call("/ssh/connect", ""); status != http.StatusUnauthorized {
		t.Fatalf("GET /ssh/connect with no credential = %d, want 401", status)
	}
	if status := call("/ssh/connect", token); status != http.StatusForbidden {
		t.Fatalf("GET /ssh/connect with an assertion = %d, want 403", status)
	}
}
