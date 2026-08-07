package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	poolagentauth "github.com/obot-platform/discobox/server/internal/auth/poolagent"
	"github.com/obot-platform/discobox/server/internal/sandboxagentclient"
	services "github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
)

func registerSandboxGitRoutes(router chi.Router, service services.SandboxService) {
	router.Handle("/projects/{projectId}/sandboxes/{sandboxId}/git-repositories/*", sandboxGitProxyHandler(service))
}

func sandboxGitProxyHandler(service services.SandboxService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if service == nil {
			http.Error(w, "sandbox service is not configured", http.StatusServiceUnavailable)
			return
		}
		repositoryID, suffix, ok := parseSandboxGitRepositoryPath(chi.URLParam(r, "*"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		agentScopes := sandboxGitProxyScopes(r)

		projectID := chi.URLParam(r, "projectId")
		sandboxID := chi.URLParam(r, "sandboxId")
		lease, sandboxModel, err := service.AcquireSandboxHTTPClient(r.Context(), projectID, sandboxID, agentScopes)
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

		target, err := sandboxGitProxyTargetURL(lease.BaseURL, sandboxModel.ProjectID, strings.TrimSpace(sandboxModel.PoolID), sandboxModel.ID, repositoryID, suffix)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		proxy := sandboxPoolReverseProxy(target, lease)
		proxy.ServeHTTP(w, r)
	})
}

func sandboxGitProxyScopes(r *http.Request) []string {
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack"):
		return []string{poolagentauth.ScopeSandboxWrite}
	case r.URL.Query().Get("service") == "git-receive-pack":
		return []string{poolagentauth.ScopeSandboxWrite}
	default:
		return []string{poolagentauth.ScopeSandboxRead}
	}
}

func parseSandboxGitRepositoryPath(path string) (repositoryID, suffix string, ok bool) {
	repositoryID, suffix, ok = strings.Cut(path, ".git")
	if !ok || !validSandboxGitRepositoryID(repositoryID) {
		return "", "", false
	}
	if suffix != "" && !strings.HasPrefix(suffix, "/") {
		return "", "", false
	}
	return repositoryID, suffix, true
}

func validSandboxGitRepositoryID(value string) bool {
	if value == "" || len(value) > 63 {
		return false
	}
	for i, r := range value {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if !valid {
			return false
		}
		if (i == 0 || i == len(value)-1) && r == '-' {
			return false
		}
	}
	return true
}

func sandboxGitProxyTargetURL(baseURL, projectID, poolID, sandboxID, repositoryID, suffix string) (*url.URL, error) {
	return sandboxagentclient.TargetURL(baseURL, projectID, poolID, sandboxID, fmt.Sprintf("/git-repositories/%s.git%s", url.PathEscape(repositoryID), suffix))
}

func sandboxPoolReverseProxy(target *url.URL, lease *services.HTTPClientLease) *httputil.ReverseProxy {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(req *httputil.ProxyRequest) {
			rawQuery := req.In.URL.RawQuery
			req.SetURL(target)
			req.Out.URL.Path = target.Path
			req.Out.URL.RawPath = ""
			req.Out.URL.RawQuery = rawQuery
			req.Out.Host = target.Host
			req.SetXForwarded()
		},
	}
	proxy.Transport = sandboxagentclient.AuthTransport{Base: baseTransportFor(lease), Lease: lease}
	return proxy
}

func baseTransportFor(lease *services.HTTPClientLease) http.RoundTripper {
	if lease.Client != nil && lease.Client.Transport != nil {
		return lease.Client.Transport
	}
	return http.DefaultTransport
}

func statusCodeForProxyError(err error) int {
	var statusErr interface{ StatusCode() int }
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode()
	}
	if errors.Is(err, store.ErrNotFound) {
		return http.StatusNotFound
	}
	if errors.Is(err, context.Canceled) {
		return 499
	}
	return http.StatusInternalServerError
}
