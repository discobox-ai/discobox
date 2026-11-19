package auth

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/discobox-ai/discobox/server/internal/store"
)

// Authentication authenticates requests by trying authenticators in order.
func Authentication(authenticators ...Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if IsPublicPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			for _, authenticator := range authenticators {
				principal, ok, err := authenticator.Authenticate(r)
				if err != nil {
					http.Error(w, err.Error(), http.StatusUnauthorized)
					return
				}
				if ok {
					next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), principal)))
					return
				}
			}
			http.Error(w, "authentication required", http.StatusUnauthorized)
		})
	}
}

// Authorization authorizes requests by trying authorizers in order.
func Authorization(authorizers ...Authorizer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if IsPublicPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			for _, authorizer := range authorizers {
				ok, err := authorizer.Authorize(r)
				if err != nil {
					status := http.StatusForbidden
					var statusErr interface{ StatusCode() int }
					if errors.As(err, &statusErr) {
						status = statusErr.StatusCode()
					} else if errors.Is(err, store.ErrNotFound) {
						status = http.StatusNotFound
					}
					http.Error(w, err.Error(), status)
					return
				}
				if ok {
					if resolvedProjectID := r.Context().Value(defaultProjectIDContextKey{}); resolvedProjectID != nil {
						if projectID, ok := resolvedProjectID.(string); ok && projectID != "" {
							if !replaceDefaultProjectInPath(r, projectID) {
								http.Error(w, "invalid default project path", http.StatusForbidden)
								return
							}
						}
					}
					next.ServeHTTP(w, r)
					return
				}
			}
			http.Error(w, "access denied", http.StatusForbidden)
		})
	}
}

// IsPublicPath reports whether a path can be served without authentication.
func IsPublicPath(path string) bool {
	// /ssh (ADR 0024) serves the SSH endpoint discovery document — only the
	// server's host *public* key, which is not a credential — and it must be
	// fetchable (discobox admin ssh-config) before any other credential exists,
	// the same rule that puts docs/openapi/health here. /ssh/connect, the SSH
	// connection itself, is not public: see SSHConnectAuthorizer.
	return path == "/healthz" || path == "/openapi.yaml" || path == "/docs" || strings.HasPrefix(path, "/docs/") || path == "/ssh"
}

type defaultProjectIDContextKey struct{}

func projectIDFromPath(path string) (string, bool) {
	prefix := "/projects/"
	rest := strings.TrimPrefix(path, prefix)
	if rest == path || rest == "" {
		return "", false
	}
	projectID, _, _ := strings.Cut(rest, "/")
	return projectID, projectID != ""
}

func replaceDefaultProjectInPath(r *http.Request, projectID string) bool {
	const prefix = "/projects/"
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), prefix)
	segment, suffix, hasSuffix := strings.Cut(rest, "/")
	project, err := url.PathUnescape(segment)
	// Replace the same project segment the authorizer checked. A separator
	// embedded in the escaped segment can otherwise change the route.
	if !ok || err != nil || project != "default" {
		return false
	}
	escaped := prefix + url.PathEscape(projectID)
	if hasSuffix {
		escaped += "/" + suffix
	}
	path, err := url.PathUnescape(escaped)
	if err != nil {
		return false
	}
	r.URL.Path = path
	r.URL.RawPath = escaped
	if escaped == (&url.URL{Path: path}).EscapedPath() {
		r.URL.RawPath = ""
	}
	r.RequestURI = r.URL.RequestURI()
	return true
}
