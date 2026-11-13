package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/transport"
)

// A pool the proxy cannot reach is a failure worth a line and a 502, in the
// API's error shape a client can decode: not a bare status, and not the
// transport's error, an address only the log needs.
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
	proxy := sandboxPoolReverseProxy(target, lease, writeSandboxAgentProxyError)
	proxy.ErrorLog = log.New(&logged, "", 0)
	proxy.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/sandbox", nil))

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
	if !bytes.Contains(logged.Bytes(), []byte("http: proxy error")) {
		t.Fatalf("log = %q, want the proxy error reported", logged.String())
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want the API's JSON error", got)
	}
	var answer struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("body %q is not the API's error shape: %v", recorder.Body.String(), err)
	}
	if !strings.Contains(answer.Error, "could not be reached") || strings.Contains(answer.Error, target.Host) {
		t.Fatalf("error = %q, want it to say the pool could not be reached, without %s", answer.Error, target.Host)
	}
}

// A route that is no API call, git's or a box port's, answers an unreachable
// pool in plain text, which git and a browser show as it is, and never the
// API's JSON.
func TestSandboxPoolProxyAnswersPlainRoutesInPlainText(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	lease := transport.NewHTTPClientLeaseWithBaseURLAndAuth(upstream.Client(), upstream.URL, "worker-token", func() {})
	target, err := url.Parse(upstream.URL + "/api/sandbox")
	if err != nil {
		t.Fatal(err)
	}
	upstream.Close()

	recorder := httptest.NewRecorder()
	proxy := sandboxPoolReverseProxy(target, lease, writePlainProxyError)
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	proxy.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/sandbox", nil))

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Fatalf("Content-Type = %q, want plain text", got)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "could not be reached") || strings.Contains(body, target.Host) {
		t.Fatalf("body = %q, want it to say the pool could not be reached, without %s", body, target.Host)
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
	proxy := sandboxPoolReverseProxy(target, lease, writeSandboxAgentProxyError)
	proxy.ErrorLog = log.New(&logged, "", 0)
	proxy.ServeHTTP(httptest.NewRecorder(), request)

	if logged.Len() != 0 {
		t.Fatalf("log = %q, want nothing for a client that went away", logged.String())
	}
}
