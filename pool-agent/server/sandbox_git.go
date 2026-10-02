package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/discobox-ai/discobox/pool-agent/githttp"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	"github.com/discobox-ai/discobox/pool-agent/sandboxtoken"
)

func registerSandboxGitRoutes(router chi.Router, service *sandboxService) {
	router.Handle("/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/git-repositories/*", service.autoStart(failFast, servedByPool, service.sandboxGitHTTPHandler(service.sandboxWorktreeLocation, worktreeScope)))
}

// registerSandboxOriginRoutes serves each source's origin, whichever kind it is
// (ADR 0126 §4). It is a different repository from the worktree above,
// addressed by its own route rather than a synthesized repository id: source
// slugs are client-supplied, so any suffix convention could collide with a
// real one (ADR 0058 §3). It is registered apart from the other routes because
// it alone accepts the sandbox's own token (OriginMiddleware).
func registerSandboxOriginRoutes(router chi.Router, service *sandboxService) {
	router.Handle("/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/git-origins/*", service.autoStart(failFast, servedByPool, service.sandboxGitHTTPHandler(service.sandboxOriginLocation, originScope)))
}

// gitLocator resolves the repository one of the two git routes serves.
type gitLocator func(ctx context.Context, sandboxID, repositoryID string) (sandboxruntime.GitRepositoryLocation, error)

// gitScope authorizes one request on one of the two git routes.
type gitScope func(r *http.Request, claims SignedTokenClaims) error

func (s *sandboxService) sandboxWorktreeLocation(ctx context.Context, sandboxID, repositoryID string) (sandboxruntime.GitRepositoryLocation, error) {
	return s.runtime.GitRepositoryPath(ctx, sandboxID, repositoryID)
}

func (s *sandboxService) sandboxOriginLocation(ctx context.Context, sandboxID, slug string) (sandboxruntime.GitRepositoryLocation, error) {
	return s.runtime.GitOriginPath(ctx, sandboxID, slug)
}

func (s *sandboxService) sandboxGitHTTPHandler(locate gitLocator, authorizeScope gitScope) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.authorize(chi.URLParam(r, "projectId"), chi.URLParam(r, "poolId")); err != nil {
			http.Error(w, err.Error(), statusCodeForGitError(err))
			return
		}
		claims, ok := SignedTokenClaimsFromContext(r.Context())
		if !ok {
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		if err := authorizeScope(r, claims); err != nil {
			http.Error(w, err.Error(), statusCodeForGitError(err))
			return
		}
		repositoryID, suffix, ok := githttp.ParseRepositoryPath(chi.URLParam(r, "*"))
		if !ok {
			http.NotFound(w, r)
			return
		}

		location, err := locate(r.Context(), chi.URLParam(r, "sandboxId"), repositoryID)
		if err != nil {
			http.Error(w, err.Error(), statusCodeForGitError(err))
			return
		}
		githttp.ServeBackend(w, r, githttp.Repository{
			Path: location.Path,
			UID:  location.UID,
			GID:  location.GID,
			Live: location.Live,
			Refs: location.Refs,
		}, suffix)
	})
}

// worktreeScope reads with sandbox:read and pushes with sandbox:write.
func worktreeScope(r *http.Request, claims SignedTokenClaims) error {
	if githttp.IsReceivePack(r) {
		return requireScope(claims, ScopeSandboxWrite)
	}
	return requireScope(claims, ScopeSandboxRead)
}

// originScope is worktreeScope, plus the sandbox's own token fetching: a
// sandbox reads its origins with origin:fetch, and that scope pushes nothing.
func originScope(r *http.Request, claims SignedTokenClaims) error {
	if !githttp.IsReceivePack(r) && claims.HasScope(sandboxtoken.ScopeOriginFetch) {
		return nil
	}
	return worktreeScope(r, claims)
}

func requireScope(claims SignedTokenClaims, scope string) error {
	if !claims.HasScope(scope) {
		return newStatusError(http.StatusForbidden, http.StatusText(http.StatusForbidden))
	}
	return nil
}

func statusCodeForGitError(err error) int {
	var statusErr interface{ StatusCode() int }
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode()
	}
	if errors.Is(err, sandboxruntime.ErrNotFound) || errors.Is(err, sandboxruntime.ErrRepositoryNotFound) {
		return http.StatusNotFound
	}
	if errors.Is(err, context.Canceled) {
		return 499
	}
	return http.StatusInternalServerError
}
