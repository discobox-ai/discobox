package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	workeragentauth "github.com/obot-platform/discobox/server/internal/auth/workeragent"
	services "github.com/obot-platform/discobox/server/internal/services"
)

const defaultSandboxAgentBaseURL = "http://sandbox.local"

func registerSandboxAgentTerminalRoutes(router chi.Router, service services.SandboxService) {
	router.Method(http.MethodGet, "/api/projects/{projectId}/sandboxes/{sandboxId}/agent-terminals", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodGet, "/api/projects/{projectId}/sandboxes/{sandboxId}/agent-hooks", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodPost, "/api/projects/{projectId}/sandboxes/{sandboxId}/agent-terminals", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodDelete, "/api/projects/{projectId}/sandboxes/{sandboxId}/agent-terminals/{terminalId}", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodPost, "/api/projects/{projectId}/sandboxes/{sandboxId}/agent-terminals/{terminalId}/attach", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodPost, "/api/projects/{projectId}/sandboxes/{sandboxId}/agent-terminals/{terminalId}/start", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodGet, "/api/projects/{projectId}/sandboxes/{sandboxId}/agent-terminals/{terminalId}/events", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodGet, "/api/projects/{projectId}/sandboxes/{sandboxId}/agent-terminals/{terminalId}/logs", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodGet, "/api/projects/{projectId}/sandboxes/{sandboxId}/agent-terminals/{terminalId}/resources", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodGet, "/api/projects/{projectId}/sandboxes/{sandboxId}/agent-terminals/{terminalId}/resources/history", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodGet, "/api/projects/{projectId}/sandboxes/{sandboxId}/agent-terminals/{terminalId}/resources/stream", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodGet, "/api/projects/{projectId}/sandboxes/{sandboxId}/execs", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodPost, "/api/projects/{projectId}/sandboxes/{sandboxId}/execs", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodGet, "/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodGet, "/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/logs", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodPost, "/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/attach", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodGet, "/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/attach", sandboxAgentTerminalProxyHandler(service))
	router.Method(http.MethodPost, "/api/projects/{projectId}/sandboxes/{sandboxId}/execs/{execId}/start", sandboxAgentTerminalProxyHandler(service))
}

func sandboxAgentTerminalProxyHandler(service services.SandboxService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if service == nil {
			http.Error(w, "sandbox service is not configured", http.StatusServiceUnavailable)
			return
		}
		projectID := chi.URLParam(r, "projectId")
		sandboxID := chi.URLParam(r, "sandboxId")
		scopes := sandboxAgentTerminalProxyScopes(r)
		if len(scopes) == 0 {
			http.Error(w, "unsupported sandbox agent-terminal operation", http.StatusMethodNotAllowed)
			return
		}

		lease, sandboxModel, err := service.AcquireSandboxHTTPClient(r.Context(), projectID, sandboxID, scopes)
		if err != nil {
			http.Error(w, err.Error(), statusCodeForProxyError(err))
			return
		}
		if lease == nil {
			http.Error(w, "sandbox HTTP client is not available", http.StatusServiceUnavailable)
			return
		}
		defer lease.Release()
		if sandboxModel == nil || sandboxModel.WorkerID == nil || strings.TrimSpace(*sandboxModel.WorkerID) == "" {
			http.Error(w, "sandbox worker is not assigned", http.StatusConflict)
			return
		}

		target, err := sandboxAgentTerminalProxyTargetURL(lease.BaseURL, projectID, sandboxModel.ProjectID, strings.TrimSpace(*sandboxModel.WorkerID), sandboxID, sandboxModel.ID, r.URL.Path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		proxy := sandboxWorkerReverseProxy(target, lease)
		proxy.ServeHTTP(w, r)
	})
}

func sandboxAgentTerminalProxyScopes(r *http.Request) []string {
	if strings.Contains(r.URL.Path, "/agent-terminals") || strings.Contains(r.URL.Path, "/agent-hooks") {
		switch r.Method {
		case http.MethodGet:
			return []string{workeragentauth.ScopeTerminalRead}
		case http.MethodPost, http.MethodDelete:
			return []string{workeragentauth.ScopeTerminalWrite}
		default:
			return nil
		}
	}
	if strings.Contains(r.URL.Path, "/execs") {
		if strings.HasSuffix(r.URL.Path, "/attach") {
			return []string{workeragentauth.ScopeExecWrite, workeragentauth.ScopeExecRead}
		}
		switch r.Method {
		case http.MethodGet:
			return []string{workeragentauth.ScopeExecRead}
		case http.MethodPost, http.MethodDelete:
			return []string{workeragentauth.ScopeExecWrite}
		default:
			return nil
		}
	}
	return nil
}

func sandboxAgentTerminalProxyTargetURL(baseURL, routeProjectID, projectID, workerID, routeSandboxID, sandboxID, path string) (*url.URL, error) {
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
		"/api/project/%s/worker/%s/sandboxes/%s%s",
		url.PathEscape(projectID),
		url.PathEscape(workerID),
		url.PathEscape(sandboxID),
		suffix,
	)
	target.RawPath = ""
	target.RawQuery = ""
	return target, nil
}
