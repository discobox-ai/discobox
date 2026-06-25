package auth

import (
	"errors"
	"net/http"
	"strings"

	"github.com/obot-platform/discobox/server/internal/store"
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
							replaceProjectIDInPath(r, projectID)
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
	return path == "/healthz" || path == "/openapi.yaml" || path == "/docs" || strings.HasPrefix(path, "/docs/")
}

type defaultProjectIDContextKey struct{}

func projectIDFromPath(path string) (string, bool) {
	prefix := "/projects/"
	rest := strings.TrimPrefix(path, prefix)
	if rest == path {
		prefix = "/api/projects/"
		rest = strings.TrimPrefix(path, prefix)
	}
	if rest == path || rest == "" {
		return "", false
	}
	projectID, _, _ := strings.Cut(rest, "/")
	return projectID, projectID != ""
}

func replaceProjectIDInPath(r *http.Request, projectID string) {
	prefix := "/projects/"
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	if rest == r.URL.Path {
		prefix = "/api/projects/"
		rest = strings.TrimPrefix(r.URL.Path, prefix)
	}
	_, suffix, hasSuffix := strings.Cut(rest, "/")
	nextPath := prefix + projectID
	if hasSuffix {
		nextPath += "/" + suffix
	}
	r.URL.Path = nextPath
	r.URL.RawPath = ""
	r.RequestURI = r.URL.RequestURI()
}
