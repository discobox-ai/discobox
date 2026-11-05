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

// A pool the proxy cannot reach is a failure worth a line and a 502.
func TestSandboxPoolProxyReportsAnUnreachablePool(t *testing.T) {
	var logged bytes.Buffer
	upstream := httptest.NewServer(http.NotFoundHandler())
	lease := transport.NewHTTPClientLeaseWithBaseURLAndAuth(upstream.Client(), upstream.URL, "worker-token", func() {})
	target, err := url.Parse(upstream.URL + "/api/sandbox")
	if err != nil {
		t.Fatal(err)
	}
	upstream.Close()

	recorder := httptest.NewRecorder()
	proxy := sandboxPoolReverseProxy(target, lease)
	proxy.ErrorLog = log.New(&logged, "", 0)
	proxy.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/sandbox", nil))

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
	var logged bytes.Buffer
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the proxy reached the pool for a request whose client had gone")
	}))
	t.Cleanup(upstream.Close)
	lease := transport.NewHTTPClientLeaseWithBaseURLAndAuth(upstream.Client(), upstream.URL, "worker-token", func() {})
	target, err := url.Parse(upstream.URL + "/api/sandbox")
	if err != nil {
		t.Fatal(err)
	}

	// Background services share the default logger, but do not belong to this proxy.
	log.Print("unrelated background service warning")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/sandbox", nil)
	proxy := sandboxPoolReverseProxy(target, lease)
	proxy.ErrorLog = log.New(&logged, "", 0)
	proxy.ServeHTTP(httptest.NewRecorder(), request)

	if logged.Len() != 0 {
		t.Fatalf("log = %q, want nothing for a client that went away", logged.String())
	}
}
