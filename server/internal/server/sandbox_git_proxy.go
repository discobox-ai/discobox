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
	workeragentauth "github.com/obot-platform/discobox/server/internal/auth/workeragent"
	services "github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
)

const defaultSandboxWorkerBaseURL = "https://worker"

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
		workerScopes := sandboxGitProxyScopes(r)

		projectID := chi.URLParam(r, "projectId")
		sandboxID := chi.URLParam(r, "sandboxId")
		lease, sandboxModel, err := service.AcquireSandboxHTTPClient(r.Context(), projectID, sandboxID, workerScopes)
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

		target, err := sandboxGitProxyTargetURL(lease.BaseURL, sandboxModel.ProjectID, strings.TrimSpace(*sandboxModel.WorkerID), sandboxModel.ID, repositoryID, suffix)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		proxy := sandboxWorkerReverseProxy(target, lease)
		proxy.ServeHTTP(w, r)
	})
}

func sandboxGitProxyScopes(r *http.Request) []string {
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack"):
		return []string{workeragentauth.ScopeSandboxWrite}
	case r.URL.Query().Get("service") == "git-receive-pack":
		return []string{workeragentauth.ScopeSandboxWrite}
	default:
		return []string{workeragentauth.ScopeSandboxRead}
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

func sandboxGitProxyTargetURL(baseURL, projectID, workerID, sandboxID, repositoryID, suffix string) (*url.URL, error) {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultSandboxWorkerBaseURL
	}
	target, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse sandbox git proxy target: %w", err)
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("sandbox git proxy target %q must include scheme and host", baseURL)
	}
	target.Path = fmt.Sprintf(
		"/api/project/%s/worker/%s/sandboxes/%s/git-repositories/%s.git%s",
		url.PathEscape(projectID),
		url.PathEscape(workerID),
		url.PathEscape(sandboxID),
		url.PathEscape(repositoryID),
		suffix,
	)
	target.RawPath = ""
	target.RawQuery = ""
	return target, nil
}

func sandboxWorkerReverseProxy(target *url.URL, lease *services.HTTPClientLease) *httputil.ReverseProxy {
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
	baseTransport := http.DefaultTransport
	if lease.Client != nil && lease.Client.Transport != nil {
		baseTransport = lease.Client.Transport
	}
	proxy.Transport = workerAgentAuthTransport{base: baseTransport, lease: lease}
	return proxy
}

type workerAgentAuthTransport struct {
	base  http.RoundTripper
	lease *services.HTTPClientLease
}

func (t workerAgentAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	authToken, err := t.lease.AuthorizationToken(req.Context())
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(authToken) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(authToken))
	} else {
		req.Header.Del("Authorization")
	}
	req.Header.Del("X-Discobox-Sandbox-Agent-Authorization")
	forwardAuthToken, err := t.lease.ForwardAuthorizationToken(req.Context())
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(forwardAuthToken) != "" {
		req.Header.Set("X-Discobox-Sandbox-Agent-Authorization", "Bearer "+strings.TrimSpace(forwardAuthToken))
	}
	return t.base.RoundTrip(req)
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
