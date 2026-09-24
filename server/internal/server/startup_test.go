package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// A first start whose default provider cannot run holds, reports the question
// on health, and goes on with the answer given over the local IPC endpoint
// (ADR 0148 §2, §4).
func TestStartupHoldsForADefaultProviderChoice(t *testing.T) {
	handler := newStartupHandler("starting services")
	chosen := make(chan string, 1)
	go func() {
		answer, err := handler.awaitChoice(t.Context(), health.Choice{
			Provider:     "libkrun",
			Reason:       health.ReasonKVMUnavailable,
			Detail:       "open /dev/kvm: no such file or directory",
			Alternatives: []string{"docker"},
		})
		if err != nil {
			t.Error(err)
		}
		chosen <- answer
	}()

	var status health.Status
	for deadline := time.Now().Add(5 * time.Second); ; {
		status = probeStartup(t, handler)
		if status.NeedsChoice() || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !status.NeedsChoice() {
		t.Fatalf("status = %+v, want %q with a choice", status, health.StatusNeedsChoice)
	}
	if status.Choice.Reason != health.ReasonKVMUnavailable || status.Choice.Provider != "libkrun" {
		t.Fatalf("choice = %+v", status.Choice)
	}

	if code := postChoice(t, handler, "docker", false); code != http.StatusServiceUnavailable {
		t.Fatalf("a choice from off the local endpoint answered %d, want the 503 every other path gets", code)
	}
	if code := postChoice(t, handler, "vz", true); code != http.StatusBadRequest {
		t.Fatalf("a provider that is not offered answered %d, want 400", code)
	}
	if code := postChoice(t, handler, "docker", true); code != http.StatusAccepted {
		t.Fatalf("the offered provider answered %d, want 202", code)
	}
	// At once, not once startup has run again: a CLI probes straight after
	// answering, and seeing the question still up it would report it unanswered.
	if status := probeStartup(t, handler); status.NeedsChoice() {
		t.Fatal("the question is still up after it was answered")
	}
	select {
	case answer := <-chosen:
		if answer != "docker" {
			t.Fatalf("startup went on with %q, want docker", answer)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startup is still held after the choice was made")
	}
	if status := probeStartup(t, handler); !status.Starting() {
		t.Fatalf("status after the choice = %q, want %q", status.Status, health.StatusStarting)
	}
	if code := postChoice(t, handler, "docker", true); code != http.StatusConflict {
		t.Fatalf("a choice to a server holding on nothing answered %d, want 409", code)
	}
}

func probeStartup(t *testing.T, handler *startupHandler) health.Status {
	t.Helper()
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequestWithContext(t.Context(), http.MethodGet, health.Path, nil))
	var status health.Status
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func postChoice(t *testing.T, handler *startupHandler, provider string, local bool) int {
	t.Helper()
	ctx := t.Context()
	if local {
		ctx = context.WithValue(ctx, localIPCKey{}, true)
	}
	body := strings.NewReader(`{"provider":"` + provider + `"}`)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequestWithContext(ctx, http.MethodPost, health.SetupDefaultProviderPath, body))
	return resp.Code
}

// A start held for a choice can be stopped: the user who answered no, and a
// newer server reclaiming the socket, both ask /shutdown, which the router
// that serves it has not got yet. A start that is merely slow is not.
func TestAHeldStartCanBeShutDown(t *testing.T) {
	handler := newStartupHandler("starting services")
	stopped := make(chan struct{}, 1)
	handler.stop = func() { stopped <- struct{}{} }
	shutdown := func() int {
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/shutdown", nil))
		return resp.Code
	}
	if code := shutdown(); code != http.StatusServiceUnavailable {
		t.Fatalf("/shutdown during an ordinary start answered %d, want 503", code)
	}
	go func() {
		_, _ = handler.awaitChoice(t.Context(), health.Choice{Provider: "libkrun", Reason: health.ReasonKVMUnavailable, Alternatives: []string{"docker"}})
	}()
	for deadline := time.Now().Add(5 * time.Second); !handler.holding(); {
		if time.Now().After(deadline) {
			t.Fatal("startup never held")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code := shutdown(); code != http.StatusAccepted {
		t.Fatalf("/shutdown while held answered %d, want 202", code)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the held server was not stopped")
	}
	if !handler.stopRequested() {
		t.Fatal("a requested stop is not recorded, so Run would report the abandoned start as a failure")
	}
}
