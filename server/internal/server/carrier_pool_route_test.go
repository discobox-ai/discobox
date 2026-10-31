package server

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/poolauth"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/transport/carrierhub"
)

// A stream a pool's guest opened reaches that pool's routes, and the sandbox
// calls that pool forwards, and nothing else (ADR 26-10-30-875 §3). The same
// request with no credential is the default user's on any other connection;
// on a pool's stream it must never be.
func TestAPoolsControlPlaneStreamReachesOnlyThatPoolsRoutes(t *testing.T) {
	skipWithoutDocker(t)
	ctx := context.Background()
	db := newAppTestDB(ctx, t)
	router := newTestApp(ctx, t, db)
	projectID, key := seedCredentialRoutePool(ctx, t, db.Write, router)
	allowDiscoboxAPI(ctx, t, db.Write, routeTestSandboxID)
	otherKey := seedPool(ctx, t, db.Write, projectID, "pool-other")
	if err := db.Write.WithContext(ctx).Create(&model.Sandbox{
		ID: "sbx-other", ProjectID: projectID, Name: "other", PoolID: "pool-other",
	}).Error; err != nil {
		t.Fatalf("create other sandbox: %v", err)
	}
	allowDiscoboxAPI(ctx, t, db.Write, "sbx-other")

	// What the server's ConnContext gives every request on a carrier stream.
	onStream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		router.ServeHTTP(w, r.WithContext(auth.WithCarrierPool(r.Context(), routeTestPoolID)))
	})

	broker := signPoolAssertion(t, projectID, routeTestPoolID, key, poolauth.ScopeCredentialBroker)
	otherBroker := signPoolAssertion(t, projectID, "pool-other", otherKey, poolauth.ScopeCredentialBroker)
	forward := signPoolAssertion(t, projectID, routeTestPoolID, key, poolauth.ScopeSandboxForward)
	otherForward := signPoolAssertion(t, projectID, "pool-other", otherKey, poolauth.ScopeSandboxForward)
	sandboxes := "/projects/" + projectID + "/sandboxes"

	if resp := callRoute(t, router, http.MethodGet, sandboxes, "", ""); resp.Code != http.StatusOK {
		t.Fatalf("off a stream, an uncredentialed call is the default user's: status = %d, want 200", resp.Code)
	}

	// The same through the server's own wiring: a stream pushed to a real
	// carrier hub, served with the server's connContext, which is all that
	// tags it. A listener that loses the tag fails here, not in production.
	hub := carrierhub.New()
	streams := &http.Server{Handler: router, ConnContext: connContext, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = streams.Serve(serverListener{Listener: hub}) }()
	t.Cleanup(func() { _ = streams.Close(); _ = hub.Close() })
	overHub := &http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			guest, server := net.Pipe()
			go func() { _ = hub.Push(routeTestPoolID, server, nil) }()
			return guest, nil
		},
	}}
	hubGet := func(path, token string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://control-plane"+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := overHub.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if got := hubGet(sandboxes, ""); got != http.StatusUnauthorized {
		t.Fatalf("over the hub, an uncredentialed call = %d, want 401", got)
	}
	if got := hubGet("/api/pools/"+routeTestPoolID+"/sandbox-credentials?sandboxId="+routeTestSandboxID, broker); got != http.StatusOK {
		t.Fatalf("over the hub, the pool's own route = %d, want 200", got)
	}

	for _, tc := range []struct {
		name string
		call func() int
		want int
	}{
		{"its own pool route", func() int {
			return callRoute(t, onStream, http.MethodGet, "/api/pools/"+routeTestPoolID+"/sandbox-credentials?sandboxId="+routeTestSandboxID, "", broker).Code
		}, http.StatusOK},
		{"another pool's route, with that pool's assertion", func() int {
			return callRoute(t, onStream, http.MethodGet, "/api/pools/pool-other/sandbox-credentials?sandboxId=sbx-other", "", otherBroker).Code
		}, http.StatusUnauthorized},
		{"a call with no credential", func() int {
			return callRoute(t, onStream, http.MethodGet, sandboxes, "", "").Code
		}, http.StatusUnauthorized},
		{"a sandbox call its pool forwards", func() int {
			return forwardRoute(t, onStream, http.MethodGet, sandboxes, "", forward, routeTestPoolID, routeTestSandboxID).Code
		}, http.StatusOK},
		{"a sandbox call another pool forwards", func() int {
			return forwardRoute(t, onStream, http.MethodGet, sandboxes, "", otherForward, "pool-other", "sbx-other").Code
		}, http.StatusUnauthorized},
		{"a public path", func() int {
			return callRoute(t, onStream, http.MethodGet, "/healthz", "", "").Code
		}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.call(); got != tc.want {
				t.Fatalf("status = %d, want %d", got, tc.want)
			}
		})
	}
}
