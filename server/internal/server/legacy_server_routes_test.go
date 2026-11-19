package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	apigen "github.com/discobox-ai/discobox/api/gen"
)

func TestLegacyServerRoutesTargetRegisteredPaths(t *testing.T) {
	generated, err := apigen.NewServer(nil)
	if err != nil {
		t.Fatal(err)
	}
	handWired := chi.NewRouter()
	registerSandboxTransferRoutes(handWired, nil)
	registerSandboxTunnelRoutes(handWired, nil)
	registerPoolConsoleRoutes(handWired, nil)
	registerPoolLogsRoutes(handWired, nil)
	registerPoolHTTPAuditRoutes(handWired, nil)
	registerPoolGuestImageRoutes(handWired, nil)
	param := regexp.MustCompile(`\{[^}]+\}`)
	for old, canonical := range legacyServerPaths {
		old = param.ReplaceAllString(old, "example")
		canonical = param.ReplaceAllString(canonical, "example")
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, old, nil)
		if got := canonicalServerRequest(r).URL.Path; got != canonical {
			t.Errorf("%s rewrote to %s, want %s", old, got, canonical)
		}
		found := false
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			_, published := generated.FindRoute(method, canonical)
			if published || handWired.Match(chi.NewRouteContext(), method, canonical) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: alias target %s is missing", old, canonical)
		}
	}
}

func TestLegacyServerRoutePreservesRequest(t *testing.T) {
	const legacy = "/api/projects/p/sandboxes/s/execs/my%2Fexec/attach?after=%2f&after=3"
	const canonical = "/projects/p/sandboxes/s/agent/execs/my%2Fexec/attach?after=%2f&after=3"
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, legacy, strings.NewReader("attach body"))
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	resp := httptest.NewRecorder()
	legacyServerRoutes(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.RequestURI != canonical || r.URL.RequestURI() != canonical {
			t.Fatalf("request = %s %s (%s)", r.Method, r.RequestURI, r.URL.RequestURI())
		}
		if r.Header.Get("Connection") != "Upgrade" || r.Header.Get("Upgrade") != "websocket" {
			t.Fatalf("upgrade headers = %v", r.Header)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != "attach body" {
			t.Fatalf("body = %q, error = %v", body, err)
		}
		if w != resp {
			t.Fatal("response writer was wrapped")
		}
		w.WriteHeader(http.StatusSwitchingProtocols)
	})).ServeHTTP(resp, req)
	if req.RequestURI != legacy || resp.Code != http.StatusSwitchingProtocols {
		t.Fatalf("original URI = %q, response = %d", req.RequestURI, resp.Code)
	}
}

func TestLegacyServerRoutesDoNotRewriteUnpublishedPaths(t *testing.T) {
	for _, path := range []string{
		"/api/projects/example/sandboxes/example/meta",
		"/api/projects/example/sandboxes/example/status",
		"/api/projects/example/sandboxes/example/agent/execs",
		"/api/projects/example/secrets",
		"/api/pools/example/status/extra",
		"/api/pools/example/retired-action",
		"/peer/",
	} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		if got := canonicalServerRequest(req).RequestURI; got != path {
			t.Errorf("%s was rewritten to %s", path, got)
		}
		if strings.HasPrefix(path, "/api/") {
			resp := httptest.NewRecorder()
			legacyServerRoutes(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Errorf("%s reached the API handler", path)
			})).ServeHTTP(resp, req)
			if resp.Code != http.StatusNotFound {
				t.Errorf("%s returned %d, want 404", path, resp.Code)
			}
		}
	}
}
