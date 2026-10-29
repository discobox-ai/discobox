package buildkitagent_test

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pool-agent/buildkitagent"
	"github.com/discobox-ai/discobox/proxy"
)

// fakeRegistry records what reaches the registry behind the front.
type fakeRegistry struct {
	mu   sync.Mutex
	seen []*http.Request
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.seen = append(f.seen, r.Clone(r.Context()))
	f.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (f *fakeRegistry) requests() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*http.Request(nil), f.seen...)
}

func newFront(t *testing.T) (*httptest.Server, *fakeRegistry) {
	t.Helper()
	registry := &fakeRegistry{}
	upstream := httptest.NewServer(registry)
	t.Cleanup(upstream.Close)
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(buildkitagent.NewRegistryFront(target, http.DefaultTransport, nil))
	t.Cleanup(front.Close)
	return front, registry
}

func ask(t *testing.T, front *httptest.Server, method, target, sandbox string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, front.URL+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sandbox != "" {
		req.Header.Set(proxy.PoolClientHeader, sandbox)
	}
	resp, err := front.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// A sandbox reads and writes its own namespace, which is its ID, and the API
// root a client checks first (ADR 26-10-29-160 §2).
func TestTheFrontAdmitsTheSandboxsOwnNamespace(t *testing.T) {
	front, registry := newFront(t)
	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, "/v2/"},
		{http.MethodGet, "/v2/sbx_mine/discobox-build/0123/manifests/build"},
		{http.MethodHead, "/v2/sbx_mine/base/blobs/sha256:" + strings.Repeat("a", 64)},
		{http.MethodPost, "/v2/sbx_mine/base/blobs/uploads/"},
		{http.MethodPut, "/v2/sbx_mine/base/manifests/local"},
		{http.MethodGet, "/v2/sbx_mine/base/tags/list"},
	} {
		if got := ask(t, front, tc.method, tc.target, "sbx_mine"); got != http.StatusOK {
			t.Errorf("%s %s = %d, want it forwarded", tc.method, tc.target, got)
		}
	}
	for _, r := range registry.requests() {
		if r.Header.Get(proxy.PoolClientHeader) != "" {
			t.Errorf("%s reached the registry carrying the proxy's client header", r.URL.Path)
		}
	}
}

// Everything else is a 403 the registry never sees: the catalog, another
// sandbox's repositories, a path that could be read as another name, and a
// request the proxy names no sandbox for.
func TestTheFrontRefusesEverythingElse(t *testing.T) {
	front, registry := newFront(t)
	for _, tc := range []struct{ target, sandbox string }{
		{"/v2/_catalog", "sbx_mine"},
		{"/v2/sbx_theirs/discobox-build/0123/manifests/build", "sbx_mine"},
		{"/v2/sbx_mine_too/base/manifests/local", "sbx_mine"},
		{"/v2/sbx_mine/../sbx_theirs/base/manifests/local", "sbx_mine"},
		{"/v2/sbx_mine/%2e%2e/sbx_theirs/base/manifests/local", "sbx_mine"},
		{"/v2/sbx_mine//base/manifests/local", "sbx_mine"},
		{"/v2/sbx_mine%2fbase/manifests/local", "sbx_mine"},
		{"/v2/sbx_mine/base/manifests/local", ""},
		{"/v2/", "Not/A/Namespace"},
		{"/healthz", "sbx_mine"},
	} {
		if got := ask(t, front, http.MethodGet, tc.target, tc.sandbox); got != http.StatusForbidden {
			t.Errorf("GET %s as %q = %d, want 403", tc.target, tc.sandbox, got)
		}
	}
	if seen := registry.requests(); len(seen) != 0 {
		t.Fatalf("the registry saw %d refused requests, the first %s", len(seen), seen[0].URL)
	}
}

// A blob mount from another sandbox's repository would link its blob into
// this one's, so the front drops the mount and the registry opens a plain
// upload; a mount within the namespace is kept.
func TestTheFrontKeepsOnlyMountsFromTheNamespace(t *testing.T) {
	front, registry := newFront(t)
	digest := "sha256:" + strings.Repeat("b", 64)
	for _, from := range []string{"sbx_mine/discobox-build/0123", "sbx_theirs/base"} {
		target := "/v2/sbx_mine/base/blobs/uploads/?mount=" + digest + "&from=" + from
		if got := ask(t, front, http.MethodPost, target, "sbx_mine"); got != http.StatusOK {
			t.Fatalf("POST %s = %d", target, got)
		}
	}
	seen := registry.requests()
	if len(seen) != 2 {
		t.Fatalf("the registry saw %d uploads, want 2", len(seen))
	}
	if q := seen[0].URL.Query(); q.Get("mount") != digest || q.Get("from") != "sbx_mine/discobox-build/0123" {
		t.Errorf("a mount from the sandbox's own repository was changed: %s", seen[0].URL.RawQuery)
	}
	if q := seen[1].URL.Query(); q.Has("mount") || q.Has("from") {
		t.Errorf("a mount from another sandbox's repository reached the registry: %s", seen[1].URL.RawQuery)
	}
}

// The registry reads a mount from a form body too, so a request with one is
// refused rather than let a mount past the query check.
func TestTheFrontRefusesAFormBody(t *testing.T) {
	front, registry := newFront(t)
	for _, contentType := range []string{"application/x-www-form-urlencoded", "multipart/form-data; boundary=x"} {
		body := strings.NewReader("mount=sha256:" + strings.Repeat("c", 64) + "&from=sbx_theirs/base")
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, front.URL+"/v2/sbx_mine/base/blobs/uploads/", body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(proxy.PoolClientHeader, "sbx_mine")
		req.Header.Set("Content-Type", contentType)
		resp, err := front.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("a %s body was answered %d, want 403", contentType, resp.StatusCode)
		}
	}
	if seen := registry.requests(); len(seen) != 0 {
		t.Fatalf("the registry saw %d form requests", len(seen))
	}
}

// The front reaches the registry with the pool builder's certificate, and a
// client without it — anything else that reaches the registry's port — is
// refused before the registry answers it.
func TestTheFrontReachesTheRegistryAsThePoolBuilder(t *testing.T) {
	root := layout.ContainerAt(t.TempDir())
	serverTLS, err := buildkitagent.RegistryServerTLS(root, "proj", "pool")
	if err != nil {
		t.Fatal(err)
	}
	registry := &fakeRegistry{}
	upstream := httptest.NewUnstartedServer(registry)
	upstream.TLS = serverTLS
	upstream.StartTLS()
	t.Cleanup(upstream.Close)
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	clientTLS, err := buildkitagent.RegistryClientTLS(root, "proj", "pool")
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(buildkitagent.NewRegistryFront(target, &http.Transport{TLSClientConfig: clientTLS}, nil))
	t.Cleanup(front.Close)
	if got := ask(t, front, http.MethodGet, "/v2/", "sbx_mine"); got != http.StatusOK {
		t.Fatalf("the front was answered %d through the registry's mTLS", got)
	}

	anonymous := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: clientTLS.RootCAs, MinVersion: tls.VersionTLS12}}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, upstream.URL+"/v2/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := anonymous.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatalf("a client with no certificate was answered %d", resp.StatusCode)
	}
	if n := len(registry.requests()); n != 1 {
		t.Fatalf("the registry answered %d requests, want the front's one", n)
	}
}
