package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/obot-platform/discobox/internal/authctx"
	"github.com/obot-platform/discobox/internal/store"
	"github.com/obot-platform/discobox/internal/tenantctx"
)

// TenantStarter starts tenant-scoped background work after a request tenant is known.
type TenantStarter interface {
	EnsureStarted(context.Context) error
}

// Authentication authenticates requests by trying authenticators in order.
func Authentication(authenticators ...Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if IsTenantOptionalPath(r.URL.Path) {
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
					next.ServeHTTP(w, r.WithContext(authctx.WithPrincipal(r.Context(), principal)))
					return
				}
			}
			http.Error(w, "authentication required", http.StatusUnauthorized)
		})
	}
}

// Tenant resolves the request tenant and ensures tenant background jobs are started.
func Tenant(starter TenantStarter, defaultTenantID string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if IsTenantOptionalPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			principal, _ := authctx.PrincipalFromContext(r.Context())
			tenantID := strings.TrimSpace(principal.TenantID)
			if tenantID == "" {
				tenantID = strings.TrimSpace(r.Header.Get("X-Discobox-Tenant-ID"))
			}
			if tenantID == "" {
				tenantID = defaultTenantID
			}
			ctx := tenantctx.WithTenantID(r.Context(), tenantID)
			if starter != nil {
				if err := starter.EnsureStarted(ctx); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ProjectAuthorization authorizes user membership for project-scoped routes and resolves /projects/default.
func ProjectAuthorization(appStore *store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if IsTenantOptionalPath(r.URL.Path) || !strings.HasPrefix(r.URL.Path, "/projects/") {
				next.ServeHTTP(w, r)
				return
			}
			principal, ok := authctx.PrincipalFromContext(r.Context())
			if !ok || principal.Type != authctx.PrincipalTypeUser || principal.UserID == "" {
				http.Error(w, "project access requires a user", http.StatusForbidden)
				return
			}
			projectID, ok := projectIDFromPath(r.URL.Path)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			if projectID == "default" {
				project, err := appStore.GetDefaultProjectForUser(r.Context(), principal.UserID)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						http.Error(w, "default project not found", http.StatusNotFound)
						return
					}
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				replaceProjectIDInPath(r, project.ID)
				projectID = project.ID
			}
			member, err := appStore.IsProjectMember(r.Context(), projectID, principal.UserID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if !member {
				http.Error(w, "project access denied", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// GenericAuthorization blocks non-user principals from user API routes.
func GenericAuthorization(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if IsTenantOptionalPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		principal, ok := authctx.PrincipalFromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/workers/") {
			next.ServeHTTP(w, r)
			return
		}
		if principal.Type != authctx.PrincipalTypeUser {
			http.Error(w, "user access required", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// IsTenantOptionalPath reports whether a path can be served without request tenant context.
func IsTenantOptionalPath(path string) bool {
	return path == "/openapi.json" || path == "/docs" || strings.HasPrefix(path, "/docs/")
}

func projectIDFromPath(path string) (string, bool) {
	rest := strings.TrimPrefix(path, "/projects/")
	if rest == path || rest == "" {
		return "", false
	}
	projectID, _, _ := strings.Cut(rest, "/")
	return projectID, projectID != ""
}

func replaceProjectIDInPath(r *http.Request, projectID string) {
	rest := strings.TrimPrefix(r.URL.Path, "/projects/")
	_, suffix, hasSuffix := strings.Cut(rest, "/")
	nextPath := "/projects/" + projectID
	if hasSuffix {
		nextPath += "/" + suffix
	}
	r.URL.Path = nextPath
	r.URL.RawPath = ""
	r.RequestURI = r.URL.RequestURI()
}
