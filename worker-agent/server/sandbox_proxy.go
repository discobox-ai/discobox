package server

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

const sandboxAgentAuthorizationHeader = "X-Discobox-Sandbox-Agent-Authorization"

func registerSandboxProxyRoutes(router chi.Router, service *sandboxService) {
	router.Handle("/api/project/{projectId}/worker/{workerId}/sandboxes/{sandboxId}/http/{port}", service.sandboxHTTPProxyHandler())
	router.Handle("/api/project/{projectId}/worker/{workerId}/sandboxes/{sandboxId}/http/{port}/*", service.sandboxHTTPProxyHandler())

	router.Method(http.MethodGet, "/api/project/{projectId}/worker/{workerId}/sandboxes/{sandboxId}/agent-terminals", service.sandboxAgentProxyHandler())
	router.Method(http.MethodPost, "/api/project/{projectId}/worker/{workerId}/sandboxes/{sandboxId}/agent-terminals", service.sandboxAgentProxyHandler())
	router.Method(http.MethodDelete, "/api/project/{projectId}/worker/{workerId}/sandboxes/{sandboxId}/agent-terminals/{terminalId}", service.sandboxAgentProxyHandler())
	router.Method(http.MethodPost, "/api/project/{projectId}/worker/{workerId}/sandboxes/{sandboxId}/agent-terminals/{terminalId}/attach", service.sandboxAgentProxyHandler())
	router.Method(http.MethodGet, "/api/project/{projectId}/worker/{workerId}/sandboxes/{sandboxId}/agent-terminals/{terminalId}/events", service.sandboxAgentProxyHandler())
	router.Method(http.MethodGet, "/api/project/{projectId}/worker/{workerId}/sandboxes/{sandboxId}/agent-terminals/{terminalId}/logs", service.sandboxAgentProxyHandler())
	router.Method(http.MethodGet, "/api/project/{projectId}/worker/{workerId}/sandboxes/{sandboxId}/agent-terminals/{terminalId}/resources", service.sandboxAgentProxyHandler())
	router.Method(http.MethodGet, "/api/project/{projectId}/worker/{workerId}/sandboxes/{sandboxId}/agent-terminals/{terminalId}/resources/history", service.sandboxAgentProxyHandler())
	router.Method(http.MethodGet, "/api/project/{projectId}/worker/{workerId}/sandboxes/{sandboxId}/agent-terminals/{terminalId}/resources/stream", service.sandboxAgentProxyHandler())
}

func (s *sandboxService) sandboxHTTPProxyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.authorize(chi.URLParam(r, "projectId"), chi.URLParam(r, "workerId")); err != nil {
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
		if err := s.authorize(chi.URLParam(r, "projectId"), chi.URLParam(r, "workerId")); err != nil {
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
				"/api/project/%s/worker/%s/sandboxes/%s",
				chi.URLParam(r, "projectId"),
				chi.URLParam(r, "workerId"),
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
	switch r.Method {
	case http.MethodGet:
		return ScopeTerminalRead
	case http.MethodPost, http.MethodDelete:
		return ScopeTerminalWrite
	default:
		return ""
	}
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
