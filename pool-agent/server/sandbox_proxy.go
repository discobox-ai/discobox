package server

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"github.com/discobox-ai/discobox/pool-agent/internalhttp"
	"github.com/go-chi/chi/v5"
)

const sandboxAgentAuthorizationHeader = "X-Discobox-Sandbox-Agent-Authorization"

func registerSandboxProxyRoutes(router chi.Router, service *sandboxService) {
	router.Handle("/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/http/{port}", service.autoStart(service.sandboxHTTPProxyHandler()))
	router.Handle("/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/http/{port}/*", service.autoStart(service.sandboxHTTPProxyHandler()))

	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/harness-hooks", service.autoStart(service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs", service.autoStart(service.sandboxAgentProxyHandler()))
	router.Method(http.MethodPost, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs", service.autoStart(service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}", service.autoStart(service.sandboxAgentProxyHandler()))
	router.Method(http.MethodDelete, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}", service.autoStart(service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/logs", service.autoStart(service.sandboxAgentProxyHandler()))
	router.Method(http.MethodPost, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/attach", service.autoStart(service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/attach", service.autoStart(service.sandboxAgentProxyHandler()))
	router.Method(http.MethodPost, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/start", service.autoStart(service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/events", service.autoStart(service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/resources", service.autoStart(service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/resources/history", service.autoStart(service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/resources/stream", service.autoStart(service.sandboxAgentProxyHandler()))

	// direct-tcpip tunnel (ADR 0024 §3). Reuses sandboxAgentProxyHandler and
	// autoStart unchanged: the handler already generically forwards any
	// /api/project/.../sandboxes/{sandboxId}/* suffix to the sandbox-agent,
	// and sandboxAgentRequiredScope below already knows this path needs
	// tcp:connect — this registration is the entire auto-start inheritance
	// the ADR asks for, achieved by literally reusing autoStart rather than
	// reimplementing it.
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/tcp/attach", service.autoStart(service.sandboxAgentProxyHandler()))
}

func (s *sandboxService) sandboxHTTPProxyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.authorize(chi.URLParam(r, "projectId"), chi.URLParam(r, "poolId")); err != nil {
			http.Error(w, err.Error(), statusCodeForGitError(err))
			return
		}
		if err := authorizeProxyScope(r, ScopeSandboxHTTP); err != nil {
			http.Error(w, err.Error(), statusCodeForGitError(err))
			return
		}
		port, ok := parsePort(chi.URLParam(r, "port"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		base, err := s.runtime.HTTPBaseURL(r.Context(), chi.URLParam(r, "sandboxId"), port)
		if err != nil {
			http.Error(w, err.Error(), statusCodeForGitError(mapRuntimeError(err)))
			return
		}
		suffix := chi.URLParam(r, "*")
		target := *base
		if suffix != "" {
			target.Path = "/" + suffix
		}
		sandboxProxy(&target, "").ServeHTTP(w, r)
	})
}

func (s *sandboxService) sandboxAgentProxyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.authorize(chi.URLParam(r, "projectId"), chi.URLParam(r, "poolId")); err != nil {
			http.Error(w, err.Error(), statusCodeForGitError(err))
			return
		}
		if err := authorizeProxyScope(r, sandboxAgentRequiredScope(r)); err != nil {
			http.Error(w, err.Error(), statusCodeForGitError(err))
			return
		}
		downstreamAuth := strings.TrimSpace(r.Header.Get(sandboxAgentAuthorizationHeader))
		if downstreamAuth == "" {
			http.Error(w, "sandbox-agent authorization is required", http.StatusUnauthorized)
			return
		}
		base, err := s.runtime.HTTPBaseURL(r.Context(), chi.URLParam(r, "sandboxId"), 3003)
		if err != nil {
			http.Error(w, err.Error(), statusCodeForGitError(mapRuntimeError(err)))
			return
		}
		target := *base
		target.Path = sandboxAgentPath(
			chi.URLParam(r, "projectId"),
			chi.URLParam(r, "sandboxId"),
			strings.TrimPrefix(r.URL.Path, fmt.Sprintf(
				"/api/project/%s/pool/%s/sandboxes/%s",
				chi.URLParam(r, "projectId"),
				chi.URLParam(r, "poolId"),
				chi.URLParam(r, "sandboxId"),
			)),
		)
		sandboxProxy(&target, downstreamAuth).ServeHTTP(w, r)
	})
}

func authorizeProxyScope(r *http.Request, scope string) error {
	claims, ok := SignedTokenClaimsFromContext(r.Context())
	if !ok {
		return newStatusError(http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized))
	}
	if scope != "" && !claims.HasScope(scope) {
		return newStatusError(http.StatusForbidden, http.StatusText(http.StatusForbidden))
	}
	return nil
}

func sandboxAgentRequiredScope(r *http.Request) string {
	if strings.Contains(r.URL.Path, "/harness-hooks") {
		if r.Method == http.MethodGet {
			return ScopeExecRead
		}
		return ""
	}
	if strings.Contains(r.URL.Path, "/tcp/attach") {
		if r.Method == http.MethodGet {
			return ScopeTCPConnect
		}
		return ""
	}
	if strings.Contains(r.URL.Path, "/execs") {
		if strings.HasSuffix(r.URL.Path, "/attach") {
			return ScopeExecWrite
		}
		switch r.Method {
		case http.MethodGet:
			return ScopeExecRead
		case http.MethodPost, http.MethodDelete:
			return ScopeExecWrite
		default:
			return ""
		}
	}
	return ""
}

func parsePort(value string) (int, bool) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, false
	}
	return port, true
}

func sandboxAgentPath(projectID, sandboxID, suffix string) string {
	if suffix == "" {
		suffix = "/"
	}
	return fmt.Sprintf(
		"/api/projects/%s/sandboxes/%s%s",
		url.PathEscape(projectID),
		url.PathEscape(sandboxID),
		suffix,
	)
}

func sandboxProxy(target *url.URL, downstreamAuth string) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		// Not the default transport: it honors HTTP_PROXY, and a pool running
		// inside a Discobox sandbox has proxy env injected for its egress.
		// This request goes to a sandbox on the pool's own network, so it must
		// never leave through the egress proxy -- and must not depend on
		// NO_PROXY being right for that.
		Transport: internalhttp.Transport(),
		Rewrite: func(req *httputil.ProxyRequest) {
			rawQuery := req.In.URL.RawQuery
			req.SetURL(target)
			req.Out.URL.Path = target.Path
			req.Out.URL.RawPath = ""
			req.Out.URL.RawQuery = rawQuery
			req.Out.Host = target.Host
			req.Out.Header.Del(sandboxAgentAuthorizationHeader)
			if strings.TrimSpace(downstreamAuth) != "" {
				req.Out.Header.Set("Authorization", downstreamAuth)
			}
			req.SetXForwarded()
		},
	}
}
