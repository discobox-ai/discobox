package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	poolagentauth "github.com/obot-platform/discobox/server/internal/auth/poolagent"
	services "github.com/obot-platform/discobox/server/internal/services"
)

const defaultSandboxAgentBaseURL = "http://sandbox.local"

func registerSandboxAgentTerminalRoutes(router chi.Router, service services.SandboxService) {
	router.Method(http.MethodGet, "/api/projects/{projectId}/sandboxes/{sandboxId}/harness-hooks", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodGet, "/api/projects/{projectId}/sandboxes/{sandboxId}/execs", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodPost, "/api/projects/{projectId}/sandboxes/{sandboxId}/execs", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodGet, "/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodDelete, "/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodGet, "/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/logs", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodPost, "/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/attach", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodGet, "/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/attach", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodPost, "/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/start", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodGet, "/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/events", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodGet, "/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/resources", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodGet, "/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/resources/history", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodGet, "/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/resources/stream", sandboxAgentTerminalProxyHandler(service))
}

func sandboxAgentTerminalProxyHandler(service services.SandboxService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if service == nil {
			writeSandboxAgentProxyError(w, http.StatusServiceUnavailable, "sandbox service is not configured")
			return
		}
		projectID := chi.URLParam(r, "projectId")
		sandboxID := chi.URLParam(r, "sandboxId")
		scopes := sandboxAgentTerminalProxyScopes(r)
		if len(scopes) == 0 {
			writeSandboxAgentProxyError(w, http.StatusMethodNotAllowed, "unsupported sandbox agent-terminal operation")
			return
		}

		lease, sandboxModel, err := service.AcquireSandboxHTTPClient(r.Context(), projectID, sandboxID, scopes)
		if err != nil {
			writeSandboxAgentProxyError(w, statusCodeForProxyError(err), err.Error())
			return
		}
		if lease == nil {
			writeSandboxAgentProxyError(w, http.StatusServiceUnavailable, "sandbox HTTP client is not available")
			return
		}
		defer lease.Release()
		if sandboxModel == nil || strings.TrimSpace(sandboxModel.PoolID) == "" {
			writeSandboxAgentProxyError(w, http.StatusConflict, "sandbox pool is not assigned")
			return
		}

		target, err := sandboxAgentTerminalProxyTargetURL(lease.BaseURL, projectID, sandboxModel.ProjectID, strings.TrimSpace(sandboxModel.PoolID), sandboxID, sandboxModel.ID, r.URL.Path)
		if err != nil {
			writeSandboxAgentProxyError(w, http.StatusInternalServerError, err.Error())
			return
		}
		proxy := sandboxPoolReverseProxy(target, lease)
		proxy.ServeHTTP(w, r)
	})
}

func writeSandboxAgentProxyError(w http.ResponseWriter, status int, message string) {
	body, err := json.Marshal(map[string]string{"error": message})
	if err != nil {
		http.Error(w, message, status)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(append(body, '\n')); err != nil {
		return
	}
}

func sandboxAgentTerminalProxyScopes(r *http.Request) []string {
	if strings.Contains(r.URL.Path, "/harness-hooks") {
		if r.Method == http.MethodGet {
			return []string{poolagentauth.ScopeExecRead}
		}
		return nil
	}
	if strings.Contains(r.URL.Path, "/execs") {
		if strings.HasSuffix(r.URL.Path, "/attach") {
			return []string{poolagentauth.ScopeExecWrite, poolagentauth.ScopeExecRead}
		}
		switch r.Method {
		case http.MethodGet:
			return []string{poolagentauth.ScopeExecRead}
		case http.MethodPost, http.MethodDelete:
			return []string{poolagentauth.ScopeExecWrite}
		default:
			return nil
		}
	}
	return nil
}

func sandboxAgentTerminalProxyTargetURL(baseURL, routeProjectID, projectID, poolID, routeSandboxID, sandboxID, path string) (*url.URL, error) {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultSandboxAgentBaseURL
	}
	target, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse sandbox agent-terminal proxy target: %w", err)
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("sandbox agent-terminal proxy target %q must include scheme and host", baseURL)
	}
	if !strings.HasPrefix(path, "/api/projects/") {
		return nil, fmt.Errorf("sandbox agent-terminal proxy path %q must start with /api/projects/", path)
	}
	suffix := strings.TrimPrefix(path, "/api/projects/"+routeProjectID+"/sandboxes/"+routeSandboxID)
	if suffix == path {
		return nil, fmt.Errorf("sandbox agent-terminal proxy path identity does not match route")
	}
	target.Path = fmt.Sprintf(
		"/api/project/%s/pool/%s/sandboxes/%s%s",
		url.PathEscape(projectID),
		url.PathEscape(poolID),
		url.PathEscape(sandboxID),
		suffix,
	)
	target.RawPath = ""
	target.RawQuery = ""
	return target, nil
}
