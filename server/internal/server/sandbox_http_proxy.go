package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	poolagentauth "github.com/obot-platform/discobox/server/internal/auth/poolagent"
	services "github.com/obot-platform/discobox/server/internal/services"
)

var sandboxHTTPProxyScopes = []string{poolagentauth.ScopeSandboxHTTP}

func registerSandboxHTTPRoutes(router chi.Router, service services.SandboxService) {
	router.Handle("/projects/{projectId}/sandboxes/{sandboxId}/http/{port}", sandboxHTTPProxyHandler(service))
	router.Handle("/projects/{projectId}/sandboxes/{sandboxId}/http/{port}/*", sandboxHTTPProxyHandler(service))
}

func sandboxHTTPProxyHandler(service services.SandboxService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if service == nil {
			http.Error(w, "sandbox service is not configured", http.StatusServiceUnavailable)
			return
		}
		port, ok := parseSandboxHTTPPort(chi.URLParam(r, "port"))
		if !ok {
			http.NotFound(w, r)
			return
		}

		projectID := chi.URLParam(r, "projectId")
		sandboxID := chi.URLParam(r, "sandboxId")
		lease, sandboxModel, err := service.AcquireSandboxHTTPClient(r.Context(), projectID, sandboxID, sandboxHTTPProxyScopes)
		if err != nil {
			http.Error(w, err.Error(), statusCodeForProxyError(err))
			return
		}
		if lease == nil {
			http.Error(w, "sandbox HTTP client is not available", http.StatusServiceUnavailable)
			return
		}
		defer lease.Release()
		if sandboxModel == nil || strings.TrimSpace(sandboxModel.PoolID) == "" {
			http.Error(w, "sandbox pool is not assigned", http.StatusConflict)
			return
		}

		target, err := sandboxHTTPProxyTargetURL(lease.BaseURL, sandboxModel.ProjectID, strings.TrimSpace(sandboxModel.PoolID), sandboxModel.ID, port, chi.URLParam(r, "*"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		proxy := sandboxPoolReverseProxy(target, lease)
		proxy.ServeHTTP(w, r)
	})
}

func parseSandboxHTTPPort(value string) (int, bool) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, false
	}
	return port, true
}

func sandboxHTTPProxyTargetURL(baseURL, projectID, poolID, sandboxID string, port int, suffix string) (*url.URL, error) {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultSandboxPoolBaseURL
	}
	target, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse sandbox HTTP proxy target: %w", err)
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("sandbox HTTP proxy target %q must include scheme and host", baseURL)
	}
	if suffix != "" {
		suffix = "/" + suffix
	}
	target.Path = fmt.Sprintf(
		"/api/project/%s/pool/%s/sandboxes/%s/http/%d%s",
		url.PathEscape(projectID),
		url.PathEscape(poolID),
		url.PathEscape(sandboxID),
		port,
		suffix,
	)
	target.RawPath = ""
	target.RawQuery = ""
	return target, nil
}
