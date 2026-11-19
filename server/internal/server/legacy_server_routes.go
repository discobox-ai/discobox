package server

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Freeze the legacy paths at the time of the move. A prefix rewrite would
// also expose unpublished routes, including the sandbox agent's private API.
var legacyServerPaths = map[string]string{ //nolint:gosec // Entries are route templates, including routes that issue tokens.
	"/api/pools/{poolId}/assets/blobs/{digest}":                                         "/pool-agent/{poolId}/assets/blobs/{digest}",
	"/api/pools/{poolId}/assets/{name}":                                                 "/pool-agent/{poolId}/assets/{name}",
	"/api/pools/{poolId}/assets/{name}/manifests/{digest}":                              "/pool-agent/{poolId}/assets/{name}/manifests/{digest}",
	"/api/pools/{poolId}/resolve-gateway-headers":                                       "/pool-agent/{poolId}/resolve-gateway-headers",
	"/api/pools/{poolId}/resolve-sandbox-raw-secret":                                    "/pool-agent/{poolId}/resolve-sandbox-raw-secret",
	"/api/pools/{poolId}/resolve-sandbox-secret":                                        "/pool-agent/{poolId}/resolve-sandbox-secret",
	"/api/pools/{poolId}/resources":                                                     "/pool-agent/{poolId}/resources",
	"/api/pools/{poolId}/sandbox-agent-status":                                          "/pool-agent/{poolId}/sandbox-agent-status",
	"/api/pools/{poolId}/sandbox-agent-status-tokens":                                   "/pool-agent/{poolId}/sandbox-agent-status-tokens",
	"/api/pools/{poolId}/sandbox-credential-requests":                                   "/pool-agent/{poolId}/sandbox-credential-requests",
	"/api/pools/{poolId}/sandbox-credential-requests/{requestId}":                       "/pool-agent/{poolId}/sandbox-credential-requests/{requestId}",
	"/api/pools/{poolId}/sandbox-credentials":                                           "/pool-agent/{poolId}/sandbox-credentials",
	"/api/pools/{poolId}/sandbox-host-trusts":                                           "/pool-agent/{poolId}/sandbox-host-trusts",
	"/api/pools/{poolId}/sandbox-inventories":                                           "/pool-agent/{poolId}/sandbox-inventories",
	"/api/pools/{poolId}/sandbox-secret-rejections":                                     "/pool-agent/{poolId}/sandbox-secret-rejections",
	"/api/pools/{poolId}/sandbox-start-authorization":                                   "/pool-agent/{poolId}/sandbox-start-authorization",
	"/api/pools/{poolId}/sandbox-states":                                                "/pool-agent/{poolId}/sandbox-states",
	"/api/pools/{poolId}/sandbox-suspend-completion":                                    "/pool-agent/{poolId}/sandbox-suspend-completion",
	"/api/pools/{poolId}/sandbox-trust-requests":                                        "/pool-agent/{poolId}/sandbox-trust-requests",
	"/api/pools/{poolId}/sandbox-trust-requests/{requestId}":                            "/pool-agent/{poolId}/sandbox-trust-requests/{requestId}",
	"/api/pools/{poolId}/sandboxes":                                                     "/pool-agent/{poolId}/sandboxes",
	"/api/pools/{poolId}/status":                                                        "/pool-agent/{poolId}/status",
	"/api/projects/{projectId}/events":                                                  "/projects/{projectId}/events",
	"/api/projects/{projectId}/pools/{poolId}/audit/http/{exchangeId}/{artifact}":       "/projects/{projectId}/pools/{poolId}/audit/http/{exchangeId}/{artifact}",
	"/api/projects/{projectId}/pools/{poolId}/console":                                  "/projects/{projectId}/pools/{poolId}/console",
	"/api/projects/{projectId}/pools/{poolId}/guest-image":                              "/projects/{projectId}/pools/{poolId}/guest-image",
	"/api/projects/{projectId}/pools/{poolId}/logs":                                     "/projects/{projectId}/pools/{poolId}/logs",
	"/api/projects/{projectId}/sandboxes/import":                                        "/projects/{projectId}/sandboxes/import",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/exec-events":                       "/projects/{projectId}/sandboxes/{sandboxId}/agent/exec-events",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/execs":                             "/projects/{projectId}/sandboxes/{sandboxId}/agent/execs",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}":                    "/projects/{projectId}/sandboxes/{sandboxId}/agent/execs/{execId}",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/attach":             "/projects/{projectId}/sandboxes/{sandboxId}/agent/execs/{execId}/attach",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/events":             "/projects/{projectId}/sandboxes/{sandboxId}/agent/execs/{execId}/events",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/input":              "/projects/{projectId}/sandboxes/{sandboxId}/agent/execs/{execId}/input",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/logs":               "/projects/{projectId}/sandboxes/{sandboxId}/agent/execs/{execId}/logs",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/resources":          "/projects/{projectId}/sandboxes/{sandboxId}/agent/execs/{execId}/resources",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/resources/history":  "/projects/{projectId}/sandboxes/{sandboxId}/agent/execs/{execId}/resources/history",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/resources/stream":   "/projects/{projectId}/sandboxes/{sandboxId}/agent/execs/{execId}/resources/stream",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/screen":             "/projects/{projectId}/sandboxes/{sandboxId}/agent/execs/{execId}/screen",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/start":              "/projects/{projectId}/sandboxes/{sandboxId}/agent/execs/{execId}/start",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/wait":               "/projects/{projectId}/sandboxes/{sandboxId}/agent/execs/{execId}/wait",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/export":                            "/projects/{projectId}/sandboxes/{sandboxId}/export",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/files/archive":                     "/projects/{projectId}/sandboxes/{sandboxId}/agent/files/archive",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/files/content":                     "/projects/{projectId}/sandboxes/{sandboxId}/agent/files/content",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/files/copy":                        "/projects/{projectId}/sandboxes/{sandboxId}/agent/files/copy",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/files/list":                        "/projects/{projectId}/sandboxes/{sandboxId}/agent/files/list",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/files/metadata":                    "/projects/{projectId}/sandboxes/{sandboxId}/agent/files/metadata",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/files/mkdir":                       "/projects/{projectId}/sandboxes/{sandboxId}/agent/files/mkdir",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/files/move":                        "/projects/{projectId}/sandboxes/{sandboxId}/agent/files/move",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/files/remove":                      "/projects/{projectId}/sandboxes/{sandboxId}/agent/files/remove",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/files/stat":                        "/projects/{projectId}/sandboxes/{sandboxId}/agent/files/stat",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/harness-config-changes":            "/projects/{projectId}/sandboxes/{sandboxId}/harness-config-changes",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/harness-config-changes/discard":    "/projects/{projectId}/sandboxes/{sandboxId}/harness-config-changes/discard",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/harness-config-changes/reconsider": "/projects/{projectId}/sandboxes/{sandboxId}/harness-config-changes/reconsider",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/harness-config-changes/save":       "/projects/{projectId}/sandboxes/{sandboxId}/harness-config-changes/save",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/harness-hooks":                     "/projects/{projectId}/sandboxes/{sandboxId}/agent/harness-hooks",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/reprovision":                       "/projects/{projectId}/sandboxes/{sandboxId}/agent/reprovision",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/services":                          "/projects/{projectId}/sandboxes/{sandboxId}/agent/services",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/services/{serviceId}":              "/projects/{projectId}/sandboxes/{sandboxId}/agent/services/{serviceId}",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/services/{serviceId}/logs":         "/projects/{projectId}/sandboxes/{sandboxId}/agent/services/{serviceId}/logs",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/services/{serviceId}/restart":      "/projects/{projectId}/sandboxes/{sandboxId}/agent/services/{serviceId}/restart",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/services/{serviceId}/start":        "/projects/{projectId}/sandboxes/{sandboxId}/agent/services/{serviceId}/start",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/services/{serviceId}/stop":         "/projects/{projectId}/sandboxes/{sandboxId}/agent/services/{serviceId}/stop",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/tcp/attach":                        "/projects/{projectId}/sandboxes/{sandboxId}/agent/tcp/attach",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/tools":                             "/projects/{projectId}/sandboxes/{sandboxId}/agent/tools",
	"/api/projects/{projectId}/sandboxes/{sandboxId}/udp/attach":                        "/projects/{projectId}/sandboxes/{sandboxId}/agent/udp/attach",
	"/peer": "/server/peer",
}

var legacyServerPathMatcher = func() *chi.Mux {
	router := chi.NewRouter()
	for path := range legacyServerPaths {
		// Only the path is looked up here. The canonical router handles
		// method dispatch, including unsupported methods.
		router.Get(path, http.NotFound)
	}
	return router
}()

func legacyServerRoutes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		canonical := canonicalServerRequest(r)
		if canonical == r && strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, canonical)
	})
}

func canonicalServerRequest(r *http.Request) *http.Request {
	escaped := r.URL.EscapedPath()
	if escaped != "/peer" && !strings.HasPrefix(escaped, "/api/") {
		return r
	}
	match := chi.NewRouteContext()
	pattern := legacyServerPathMatcher.Find(match, http.MethodGet, escaped)
	if pattern == "" {
		return r
	}

	parts := strings.Split(legacyServerPaths[pattern], "/")
	for i, part := range parts {
		if strings.HasPrefix(part, "{") {
			parts[i] = match.URLParam(part[1 : len(part)-1])
		}
	}
	rawPath := strings.Join(parts, "/")
	path, err := url.PathUnescape(rawPath)
	if err != nil {
		return r
	}

	// Copy just the request and URL. The body, headers and response writer
	// pass through untouched, including for upgrades and streaming.
	canonical := r.WithContext(r.Context())
	u := *r.URL
	u.Path = path
	u.RawPath = rawPath
	if u.RawPath == (&url.URL{Path: path}).EscapedPath() {
		u.RawPath = ""
	}
	canonical.URL = &u
	canonical.RequestURI = u.RequestURI()
	return canonical
}
