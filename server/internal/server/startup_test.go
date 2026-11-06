package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/health"
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

func TestStartupWarningsSurviveReadiness(t *testing.T) {
	h := newStartupHandler("starting services")
	h.addWarning("KVM unavailable; using Docker without VM isolation")
	for _, ready := range []bool{false, true} {
		if ready {
			h.setReady(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { t.Error("health bypassed startup warning status") }))
		}
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, httptest.NewRequestWithContext(t.Context(), http.MethodGet, health.Path, nil))
		var status health.Status
		if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
			t.Fatal(err)
		}
		if len(status.Warnings) != 1 || status.Warnings[0] != "KVM unavailable; using Docker without VM isolation" {
			t.Fatalf("warnings = %q", status.Warnings)
		}
		if ready && (resp.Code != http.StatusOK || status.Status != health.StatusReady) {
			t.Fatalf("ready status: %d %+v", resp.Code, status)
		}
	}
}

func TestStartupHarnessChangesReturnsDecodableUnavailable(t *testing.T) {
	const phase = "starting services"
	server := httptest.NewServer(newStartupHandler(phase))
	defer server.Close()
	client, err := serverapi.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.GetSandboxHarnessConfigChanges(t.Context(), serverapi.GetSandboxHarnessConfigChangesParams{ProjectId: "default", SandboxId: "box"})
	if err != nil {
		t.Fatalf("startup 503 must decode as an API error carrying phase %q: %v", phase, err)
	}
	unavailable, ok := response.(*serverapi.ErrorResponseStatusCode)
	if !ok || unavailable.StatusCode != http.StatusServiceUnavailable || !strings.Contains(unavailable.Response.Error, phase) {
		t.Fatalf("response = %#v, want 503 with phase %q", response, phase)
	}
}

func TestStartupProblemErrorsDecodeWithGeneratedClient(t *testing.T) {
	const phase = "opening the database"
	server := httptest.NewServer(newStartupHandler(phase))
	defer server.Close()
	client, err := serverapi.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.GetServerInfo(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	unavailable, ok := response.(*serverapi.ErrorModelStatusCode)
	if !ok || unavailable.StatusCode != http.StatusServiceUnavailable || unavailable.Response.Status.Value != http.StatusServiceUnavailable || !strings.Contains(unavailable.Response.Detail.Value, phase) {
		t.Fatalf("response = %#v, want 503 with phase %q", response, phase)
	}
}

func TestStartupSaveChangesUsesProblemJSON(t *testing.T) {
	h := newStartupHandler("starting services")
	// Save is a problem-JSON operation beside the legacy capture operations.
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/projects/default/sandboxes/box/harness-config-changes/save", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Content-Type") != "application/problem+json" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("startup response: %d %v %s", w.Code, w.Header(), w.Body)
	}
	var body serverapi.ErrorModel
	if err := body.UnmarshalJSON(w.Body.Bytes()); err != nil {
		t.Fatal(err)
	}
	if body.Detail.Value != "server is starting: starting services" {
		t.Fatalf("detail = %q", body.Detail.Value)
	}
}
