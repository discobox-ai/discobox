package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/discobox-ai/discobox/health"
)

// Until the router exists, every request is answered with what startup is
// doing. 503 rather than a refused connection is the whole point: a caller can
// tell a server that is coming up from one that is not there.
func TestStartupHandlerReportsItsPhase(t *testing.T) {
	handler := newStartupHandler("opening the database")

	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequestWithContext(t.Context(), http.MethodGet, health.Path, nil))
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusServiceUnavailable)
	}
	var status health.Status
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if !status.Starting() {
		t.Fatalf("status = %q, want %q", status.Status, health.StatusStarting)
	}
	if status.Phase != "opening the database" {
		t.Fatalf("phase = %q", status.Phase)
	}

	handler.setPhase("migrating the database")
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequestWithContext(t.Context(), http.MethodGet, health.Path, nil))
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.Phase != "migrating the database" {
		t.Fatalf("phase = %q, want the phase set most recently", status.Phase)
	}
}

// Handing over swaps the whole handler, so the listeners never rebind and a
// request in flight at the moment of readiness is served by the real router.
func TestStartupHandlerHandsOverToTheRouter(t *testing.T) {
	handler := newStartupHandler("starting services")
	handler.setReady(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/anything", nil))
	if resp.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want the real handler's %d", resp.Code, http.StatusTeapot)
	}
}

// A ready server says so, and says which server it is: a caller that just
// launched one wants to know it reached the binary it started.
func TestReadyStatusIdentifiesTheServer(t *testing.T) {
	status := readyStatus()
	if status.Starting() {
		t.Fatalf("status = %q, want %q", status.Status, health.StatusReady)
	}
	if status.Version == "" {
		t.Fatal("ready status carries no version")
	}
	if status.UptimeSeconds <= 0 {
		t.Fatalf("uptimeSeconds = %v, want a positive uptime", status.UptimeSeconds)
	}
}
