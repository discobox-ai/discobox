package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/obot-platform/discobox/pool-agent/githttp"
	"github.com/obot-platform/discobox/pool-agent/sandboxruntime"
)

func registerSandboxGitRoutes(router chi.Router, service *sandboxService) {
	router.Handle("/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/git-repositories/*", service.sandboxGitHTTPHandler())
}

func (s *sandboxService) sandboxGitHTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.authorize(chi.URLParam(r, "projectId"), chi.URLParam(r, "poolId")); err != nil {
			http.Error(w, err.Error(), statusCodeForGitError(err))
			return
		}
		if err := authorizeSandboxGitScope(r); err != nil {
			http.Error(w, err.Error(), statusCodeForGitError(err))
			return
		}
		repositoryID, suffix, ok := githttp.ParseRepositoryPath(chi.URLParam(r, "*"))
		if !ok {
			http.NotFound(w, r)
			return
		}

		location, err := s.runtime.GitRepositoryPath(r.Context(), chi.URLParam(r, "sandboxId"), repositoryID)
		if err != nil {
			http.Error(w, err.Error(), statusCodeForGitError(err))
			return
		}
		githttp.ServeBackend(w, r, location.Path, suffix, location.UID, location.GID)
	})
}

func authorizeSandboxGitScope(r *http.Request) error {
	claims, ok := SignedTokenClaimsFromContext(r.Context())
	if !ok {
		return newStatusError(http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized))
	}
	scope := sandboxGitRequiredScope(r)
	if scope != "" && !claims.HasScope(scope) {
		return newStatusError(http.StatusForbidden, http.StatusText(http.StatusForbidden))
	}
	return nil
}

func sandboxGitRequiredScope(r *http.Request) string {
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack"):
		return ScopeSandboxWrite
	case r.URL.Query().Get("service") == "git-receive-pack":
		return ScopeSandboxWrite
	default:
		return ScopeSandboxRead
	}
}

func statusCodeForGitError(err error) int {
	var statusErr interface{ StatusCode() int }
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode()
	}
	if errors.Is(err, sandboxruntime.ErrNotFound) {
		return http.StatusNotFound
	}
	if errors.Is(err, context.Canceled) {
		return 499
	}
	return http.StatusInternalServerError
}
