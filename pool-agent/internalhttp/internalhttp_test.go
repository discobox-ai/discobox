package internalhttp

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A pool running inside a Discobox sandbox has HTTP_PROXY injected so its
// egress can cross the surrounding MITM proxy. That must never apply to the
// agent's calls to its own sandboxes: routing them through the egress proxy is
// what surfaced as "sandbox-agent health returned 500" during sandbox creation.
func TestClientIgnoresProxyEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://198.51.100.1:9")
	t.Setenv("HTTPS_PROXY", "http://198.51.100.1:9")
	t.Setenv("NO_PROXY", "127.0.0.1,localhost,::1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// The transport must resolve no proxy even for a non-exempt destination.
	req, err := http.NewRequest(http.MethodGet, "http://172.17.0.4:3003/healthz", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	tr := Transport()
	if tr.Proxy != nil {
		if u, err := tr.Proxy(req); err == nil && u != nil {
			t.Fatalf("internal client would proxy to %v", u)
		}
	}

	resp, err := Client.Get(srv.URL)
	if err != nil {
		t.Fatalf("internal request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s", resp.Status)
	}
}

// http.DefaultTransport would reintroduce proxying, so callers must build on
// Transport() instead.
func TestTransportHasNoProxy(t *testing.T) {
	if Transport().Proxy != nil {
		t.Fatal("Transport().Proxy must be nil")
	}
}
