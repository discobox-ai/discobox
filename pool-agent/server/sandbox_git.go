package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/discobox-ai/discobox/gitbackend"
	"github.com/discobox-ai/discobox/pool-agent/githttp"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	"github.com/discobox-ai/discobox/pool-agent/sandboxtoken"
)

// registerSandboxGitRoutes forwards the worktree route to the sandbox's agent,
// which serves its own repository (ADR 0126 §4): the pool runs no git in a
// sandbox's checkout, so the route needs the sandbox up, and starts it if it
// is not.
func registerSandboxGitRoutes(router chi.Router, service *sandboxService) {
	router.Handle("/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/git-repositories/*", service.autoStart(failFast, needsSandbox, service.sandboxWorktreeHandler()))
}

// registerSandboxOriginRoutes serves each source's origin, whichever kind it is
// (ADR 0126 §4). It is a different repository from the worktree above,
// addressed by its own route rather than a synthesized repository id: source
// slugs are client-supplied, so any suffix convention could collide with a
// real one (ADR 0058 §3). It is registered apart from the other routes because
// it alone accepts the sandbox's own token (OriginMiddleware).
func registerSandboxOriginRoutes(router chi.Router, service *sandboxService) {
	router.Handle("/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/git-origins/*", service.autoStart(failFast, servedByPool, service.sandboxOriginHandler()))
}

// sandboxWorktreeHandler checks the pool's own token on the worktree route and
// forwards the request to the sandbox's agent, which checks its token again.
func (s *sandboxService) sandboxWorktreeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.authorizeGitScope(r, worktreeScope); err != nil {
			http.Error(w, err.Error(), statusCodeForGitError(err))
			return
		}
		// An agent from before the route answers it with its router's bare
		// 404, which git reports as no such repository; say what is true
		// instead, and what to do about it.
		if err := s.runtime.SandboxServesWorktree(r.Context(), chi.URLParam(r, "sandboxId")); err != nil {
			http.Error(w, err.Error(), statusCodeForGitError(err))
			return
		}
		s.forwardToSandboxAgent(w, r)
	})
}

// sandboxOriginHandler serves a source's origin from the repository the
// runtime says is behind it.
func (s *sandboxService) sandboxOriginHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.authorizeGitScope(r, originScope); err != nil {
			http.Error(w, err.Error(), statusCodeForGitError(err))
			return
		}
		slug, suffix, ok := gitbackend.ParseRepositoryPath(chi.URLParam(r, "*"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		location, err := s.runtime.GitOriginPath(r.Context(), chi.URLParam(r, "sandboxId"), slug)
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

// authorizeGitScope checks a request on one of the two git routes against this
// pool and the scope its route asks for.
func (s *sandboxService) authorizeGitScope(r *http.Request, scope func(*http.Request, SignedTokenClaims) error) error {
	if err := s.authorize(chi.URLParam(r, "projectId"), chi.URLParam(r, "poolId")); err != nil {
		return err
	}
	claims, ok := SignedTokenClaimsFromContext(r.Context())
	if !ok {
		return newStatusError(http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized))
	}
	return scope(r, claims)
}

// worktreeScope reads with sandbox:read and pushes with sandbox:write.
func worktreeScope(r *http.Request, claims SignedTokenClaims) error {
	if gitbackend.IsReceivePack(r) {
		return requireScope(claims, ScopeSandboxWrite)
	}
	return requireScope(claims, ScopeSandboxRead)
}

// originScope is worktreeScope, plus the sandbox's own token fetching: a
// sandbox reads its origins with origin:fetch, and that scope pushes nothing.
func originScope(r *http.Request, claims SignedTokenClaims) error {
	if !gitbackend.IsReceivePack(r) && claims.HasScope(sandboxtoken.ScopeOriginFetch) {
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
	if errors.Is(err, sandboxruntime.ErrWorktreeUnsupported) {
		return http.StatusConflict
	}
	if errors.Is(err, context.Canceled) {
		return 499
	}
	return http.StatusInternalServerError
}
