package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

func TestConsoleSnapshotsKeepIndependentServersWithTheSameBoxID(t *testing.T) {
	useTempServersFile(t)
	var alphaStops, betaStops atomic.Int32
	handler := func(name string, stops *atomic.Int32) http.Handler {
		fallback := fakeServerHandler(name, sandboxA)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop") {
				stops.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(listedSandboxJSON(sandboxA, "2026-08-07T12:00:00Z")))
				return
			}
			fallback.ServeHTTP(w, r)
		})
	}
	alpha := httptest.NewServer(handler("alpha", &alphaStops))
	t.Cleanup(alpha.Close)
	beta := httptest.NewServer(handler("beta", &betaStops))
	t.Cleanup(beta.Close)
	registerForTest(t, registeredServer{Name: "beta", Address: beta.URL})
	app := &App{serverURL: alpha.URL, projectID: "project-1"}
	client, err := app.apiClient()
	if err != nil {
		t.Fatal(err)
	}
	ds, err := newAPIDataSource(t.Context(), app, client, "project-1")
	if err != nil {
		t.Fatal(err)
	}
	listing, err := ds.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Sandboxes) != 2 {
		t.Fatalf("listed %d boxes, want same ID on both independent servers", len(listing.Sandboxes))
	}
	pinned := ds.ForServer("alpha")
	if pinned == nil {
		t.Fatal("alpha source missing")
	}
	// A newer snapshot can change which server is listed first before the UI sees it.
	ds.mu.Lock()
	ds.servers[0], ds.servers[1] = ds.servers[1], ds.servers[0]
	ds.mu.Unlock()
	if _, err := pinned.Do(context.Background(), console.VerbStop, sandboxA, nil); err != nil {
		t.Fatal(err)
	}
	if alphaStops.Load() != 1 || betaStops.Load() != 0 {
		t.Fatalf("stop redirected: alpha=%d beta=%d", alphaStops.Load(), betaStops.Load())
	}
	if _, err := ds.ForServer("beta").Do(t.Context(), console.VerbStop, sandboxA, nil); err != nil {
		t.Fatal(err)
	}
	if betaStops.Load() != 1 {
		t.Fatal("explicit beta target did not reach beta")
	}
	// Two aliases of one peer remain one box, unlike independent peers.
	ds.mu.Lock()
	ds.servers[0].id = "same-peer"
	ds.servers[1].id = "same-peer"
	ds.mu.Unlock()
	listing, err = ds.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Sandboxes) != 1 {
		t.Fatalf("peer aliases produced %d duplicate boxes", len(listing.Sandboxes))
	}
}
