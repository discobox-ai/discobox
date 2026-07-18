package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/obot-platform/discobox/server/internal/store"
)

// Authorizer authorizes an authenticated request. It returns ok=false when it
// does not apply to the request, allowing the next authorizer to try.
//
// Authorizers should assert that a request is in a positively identified scope,
// not that it is outside another scope. Negative assertions such as "not a
// worker route" can unintentionally authorize new or misspelled routes.
type Authorizer interface {
	Authorize(*http.Request) (bool, error)
}

// ProjectAuthorizer authorizes user membership for project-scoped routes and
// resolves /projects/default and /api/projects/default for downstream handlers.
type ProjectAuthorizer struct {
	Store *store.Store
}

func (a ProjectAuthorizer) Authorize(r *http.Request) (bool, error) {
	projectID, ok := projectIDFromPath(r.URL.Path)
	if !ok {
		return false, nil
	}
	principal, ok := PrincipalFromContext(r.Context())
	if !ok || principal.Type != PrincipalTypeUser || principal.UserID == "" {
		return false, authorizationError{status: http.StatusForbidden, err: errors.New("project access requires a user")}
	}
	if projectID == "default" {
		project, err := a.Store.GetDefaultProjectForUser(r.Context(), principal.UserID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return false, authorizationError{status: http.StatusNotFound, err: errors.New("default project not found")}
			}
			return false, authorizationError{status: http.StatusInternalServerError, err: err}
		}
		projectID = project.ID
		*r = *r.WithContext(context.WithValue(r.Context(), defaultProjectIDContextKey{}, projectID))
	}
	member, err := a.Store.IsProjectMember(r.Context(), projectID, principal.UserID)
	if err != nil {
		return false, authorizationError{status: http.StatusInternalServerError, err: err}
	}
	if !member {
		return false, authorizationError{status: http.StatusForbidden, err: errors.New("project access denied")}
	}
	return true, nil
}

// PoolRouteAuthorizer authorizes authenticated pool agents for pool-scoped
// API routes. Operation-specific handlers still verify resource identity, such
// as matching the authenticated pool principal to a path pool ID.
type PoolRouteAuthorizer struct{}

func (PoolRouteAuthorizer) Authorize(r *http.Request) (bool, error) {
	if !isPoolRuntimePath(r.URL.Path) {
		return false, nil
	}
	principal, ok := PrincipalFromContext(r.Context())
	if !ok || principal.Type != PrincipalTypePool {
		return false, authorizationError{status: http.StatusForbidden, err: errors.New("pool agent access required")}
	}
	return true, nil
}

// AuthenticatedAuthorizer authorizes explicitly listed routes for any
// authenticated principal. It is intentionally allow-list based; routes not
// listed here must be authorized by a more specific authorizer.
type AuthenticatedAuthorizer struct{}

func (AuthenticatedAuthorizer) Authorize(r *http.Request) (bool, error) {
	if !isAuthenticatedAllowedPath(r.URL.Path) {
		return false, nil
	}
	principal, ok := PrincipalFromContext(r.Context())
	if !ok || principal.Type == "" {
		return false, authorizationError{status: http.StatusForbidden, err: errors.New("authenticated access required")}
	}
	return true, nil
}

type authorizationError struct {
	status int
	err    error
}

func (e authorizationError) Error() string {
	return e.err.Error()
}

func (e authorizationError) Unwrap() error {
	return e.err
}

func (e authorizationError) StatusCode() int {
	return e.status
}

var authenticatedAllowedPaths = []string{
	"/harness-definitions",
	"/harness-definitions/",
	"/api/workers/register",
	"/projects",
	"/providers/catalog",
	"/shutdown",
}

func isAuthenticatedAllowedPath(path string) bool {
	for _, allowed := range authenticatedAllowedPaths {
		if strings.HasSuffix(allowed, "/") {
			if strings.HasPrefix(path, allowed) {
				return true
			}
			continue
		}
		if path == allowed {
			return true
		}
	}
	return false
}
