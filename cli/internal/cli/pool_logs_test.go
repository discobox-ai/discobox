package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The log itself goes to stdout and the source line to stderr, so redirecting
// the command captures the host's log and nothing the CLI had to say about it.
func TestStreamPoolLogsSeparatesSourceFromLog(t *testing.T) {
	var gotPath, gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set(poolLogSourceHeader, "vz guest serial console")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("[    0.0] Linux version 6.6\n"))
	}))
	defer server.Close()

	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
	var stdout, stderr bytes.Buffer
	if err := app.streamPoolLogs(context.Background(), "proj_1", "pool_1", 50, true, &stdout, &stderr); err != nil {
		t.Fatalf("stream pool logs: %v", err)
	}

	if gotPath != "/api/projects/proj_1/pools/pool_1/logs" {
		t.Fatalf("request path = %q", gotPath)
	}
	if !strings.Contains(gotQuery, "tail=50") || !strings.Contains(gotQuery, "follow=true") {
		t.Fatalf("request query = %q", gotQuery)
	}
	if stdout.String() != "[    0.0] Linux version 6.6\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "vz guest serial console") {
		t.Fatalf("stderr = %q, want the source named", stderr.String())
	}
}

// A whole-log read asks for no bound at all, rather than sending tail=0 for the
// server to interpret.
func TestStreamPoolLogsOmitsAnUnboundedTail(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
	if err := app.streamPoolLogs(context.Background(), "proj_1", "pool_1", 0, false, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("stream pool logs: %v", err)
	}
	if gotQuery != "" {
		t.Fatalf("request query = %q, want no bound and no follow", gotQuery)
	}
}

// A backend with no host log answers 501 with its reason; that reason is the
// whole point of asking, so it has to reach the operator as the error.
func TestStreamPoolLogsSurfacesTheBackendReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"error":"pool host logs are not available from this backend: no journalctl here"}`))
	}))
	defer server.Close()

	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
	err := app.streamPoolLogs(context.Background(), "proj_1", "pool_1", 0, false, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("stream pool logs succeeded on a backend with no log")
	}
	if !strings.Contains(err.Error(), "no journalctl here") {
		t.Fatalf("error = %v, want the backend's reason", err)
	}
}
