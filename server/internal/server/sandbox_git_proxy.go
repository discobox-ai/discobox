package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/discobox-ai/discobox/pkg/sourcecache"
	"github.com/discobox-ai/discobox/server/internal/auth"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandboxagentclient"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/go-chi/chi/v5"
)

func registerSandboxGitRoutes(router chi.Router, service services.SandboxService) {
	router.Handle("/projects/{projectId}/sandboxes/{sandboxId}/git-repositories/*", sandboxGitProxyHandler(service, gitRouteWorktree))
	// The origin a client delivers a source into is a different repository
	// from the sandbox's worktree, on its own route for the same reason the
	// pool agent gives it one (ADR 0058 §3). Both proxy the same way and
	// derive scopes identically.
	router.Handle("/projects/{projectId}/sandboxes/{sandboxId}/git-origins/*", sandboxGitProxyHandler(service, gitRouteOrigin))
	// What the pool's cache remembers of a source's remote, which a client
	// fetches before it fetches the remote itself (ADR 26-10-10-545 §3). It
	// is a project member's: no sandbox role route names it (§7).
	router.Handle("/projects/{projectId}/sandboxes/{sandboxId}/git-caches/*", sandboxGitProxyHandler(service, gitRouteCache))
}

// gitRoute is which of a sandbox's repositories a route serves.
type gitRoute string

const (
	gitRouteWorktree gitRoute = "git-repositories"
	gitRouteOrigin   gitRoute = "git-origins"
	gitRouteCache    gitRoute = "git-caches"
)

func sandboxGitProxyHandler(service services.SandboxService, route gitRoute) http.Handler {
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
		// The worktree is inside the sandbox, served by its agent, so reaching
		// it is an action: the sandbox is started, and repaired once if it
		// failed and lost its container. An origin, and what the pool's cache
		// holds of a source, are the pool's own repositories, so reaching one
		// repairs nothing and waits for nothing: it is a read. Not waiting is
		// what lets a source be delivered to a sandbox that is still being
		// made, and keeps the push a parked sandbox is waiting on from waiting
		// on that sandbox (ADR 26-10-12-711 §1, ADR 26-10-10-545 §3).
		use := services.SandboxRead
		if route == gitRouteWorktree {
			use = services.SandboxAction("git")
		}
		lease, sandboxModel, err := service.AcquireSandboxHTTPClient(r.Context(), projectID, sandboxID, agentScopes, use)
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

		kind := string(route)
		headers := http.Header{}
		cacheKey := sandboxSourceCacheKey(sandboxModel, repositoryID)
		switch {
		case route == gitRouteOrigin && cacheKey != "":
			kind = sourcecache.PoolOriginRoute
			headers.Set(sourcecache.KeyHeader, cacheKey)
		case route == gitRouteCache && cacheKey != "":
			kind = sourcecache.PoolRemoteRoute
			headers.Set(sourcecache.KeyHeader, cacheKey)
		case route == gitRouteCache:
			http.Error(w, fmt.Sprintf("source %q is not delivered into a pool cache", repositoryID), http.StatusNotFound)
			return
		}
		if principal, ok := auth.PrincipalFromContext(r.Context()); ok && principal.Type == auth.PrincipalTypeSandbox {
			headers.Set(sourcecache.CallerHeader, principal.SandboxID)
		}

		target, err := sandboxGitProxyTargetURL(lease.BaseURL, sandboxModel.ProjectID, strings.TrimSpace(sandboxModel.PoolID), sandboxModel.ID, kind, repositoryID, suffix)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		proxy := sandboxPoolReverseProxy(target, lease)
		rewrite := proxy.Rewrite
		proxy.Rewrite = func(req *httputil.ProxyRequest) {
			rewrite(req)
			// Never the client's word: these say which cache a request reaches
			// and whose reach it is held to.
			req.Out.Header.Del(sourcecache.KeyHeader)
			req.Out.Header.Del(sourcecache.CallerHeader)
			for name, values := range headers {
				req.Out.Header[name] = values
			}
		}
		proxy.ServeHTTP(w, r)
	})
}

// sandboxSourceCacheKey is the pool cache the sandbox's source named slug is
// delivered into, empty for a source that is not cache-delivered or that the
// sandbox does not have.
func sandboxSourceCacheKey(sandbox *model.Sandbox, slug string) string {
	match := func(source *model.GitSource) string {
		if source == nil || source.Slug == nil || strings.TrimSpace(*source.Slug) != slug {
			return ""
		}
		if source.Delivery != model.GitSourceDeliveryCache || source.CacheKey == nil {
			return ""
		}
		return strings.TrimSpace(*source.CacheKey)
	}
	if key := match(sandbox.Source); key != "" {
		return key
	}
	for _, source := range sandbox.SourceCodeReferences {
		if key := match(&source); key != "" {
			return key
		}
	}
	return ""
}

func sandboxGitProxyScopes(r *http.Request) []string {
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack"):
		return []string{poolagentauth.ScopeSandboxWrite}
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ".git"+sourcecache.PinSuffix):
		// A pin creates refs, as a push does (ADR 26-10-10-545 §3).
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

func sandboxGitProxyTargetURL(baseURL, projectID, poolID, sandboxID, kind, repositoryID, suffix string) (*url.URL, error) {
	return sandboxagentclient.TargetURL(baseURL, projectID, poolID, sandboxID, fmt.Sprintf("/%s/%s.git%s", kind, url.PathEscape(repositoryID), suffix))
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
		ErrorHandler: sandboxPoolProxyError,
	}
	proxy.Transport = sandboxagentclient.AuthTransport{Base: baseTransportFor(lease), Lease: lease}
	return proxy
}

// sandboxPoolProxyError answers a request the pool could not be asked, as the
// default handler does, except for a request whose client has already gone.
// That one ended the exchange itself — a closed terminal, an abandoned fetch, a
// server shutting down under every attached client at once — so there is
// nobody to answer, and logging it would bury a real failure under one line
// per connection.
func sandboxPoolProxyError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	log.Printf("http: proxy error: %v", err)
	w.WriteHeader(http.StatusBadGateway)
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
