package server

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/transport"
)

// captureLog redirects the standard logger, which is where the proxy reports a
// failure, for the length of a test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(previous) })
	return &buf
}

// A pool the proxy cannot reach is a failure worth a line and a 502.
func TestSandboxPoolProxyReportsAnUnreachablePool(t *testing.T) {
	logged := captureLog(t)
	upstream := httptest.NewServer(http.NotFoundHandler())
	lease := transport.NewHTTPClientLeaseWithBaseURLAndAuth(upstream.Client(), upstream.URL, "worker-token", func() {})
	target, err := url.Parse(upstream.URL + "/api/sandbox")
	if err != nil {
		t.Fatal(err)
	}
	upstream.Close()

	recorder := httptest.NewRecorder()
	sandboxPoolReverseProxy(target, lease).ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/sandbox", nil))

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
	if !bytes.Contains(logged.Bytes(), []byte("http: proxy error")) {
		t.Fatalf("log = %q, want the proxy error reported", logged.String())
	}
}

// A client that has gone away ended the request itself: nothing is logged, so a
// server shutting down under many attached clients does not print a line for
// each of them.
func TestSandboxPoolProxyIgnoresAClientThatWentAway(t *testing.T) {
	logged := captureLog(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the proxy reached the pool for a request whose client had gone")
	}))
	t.Cleanup(upstream.Close)
	lease := transport.NewHTTPClientLeaseWithBaseURLAndAuth(upstream.Client(), upstream.URL, "worker-token", func() {})
	target, err := url.Parse(upstream.URL + "/api/sandbox")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/sandbox", nil)
	sandboxPoolReverseProxy(target, lease).ServeHTTP(httptest.NewRecorder(), request)

	if logged.Len() != 0 {
		t.Fatalf("log = %q, want nothing for a client that went away", logged.String())
	}
}
