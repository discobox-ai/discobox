package server

import (
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// TreeMediaType is what a sandbox's durable tree travels as: a plain tar, with
// no compression and no framing of its own.
const TreeMediaType = "application/x-tar"

// registerSandboxTreeRoutes exposes a sandbox's durable tree for export and
// restore (ADR 0123).
//
// Hand-wired beside the git routes, and for the same reason: the body is an
// opaque stream of unbounded length, which the generated contract has no way to
// describe and no reason to buffer.
//
// Neither route is wrapped in autoStart, unlike the git ones. Everything about
// both operations assumes nothing is running -- an export refuses a running
// sandbox outright, and a restore happens for a sandbox that has no container
// at all -- so starting one to serve the request would defeat the request.
func registerSandboxTreeRoutes(router chi.Router, service *sandboxService) {
	path := "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/tree"
	router.Method(http.MethodGet, path, service.exportSandboxTreeHandler())
	router.Method(http.MethodPut, path, service.importSandboxTreeHandler())
}

func (s *sandboxService) exportSandboxTreeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.authorizeTree(r, ScopeSandboxRead); err != nil {
			http.Error(w, err.Error(), statusCodeForTreeError(err))
			return
		}
		stream, err := s.runtime.ExportTree(r.Context(), chi.URLParam(r, "sandboxId"))
		if err != nil {
			// Before any of the body: once bytes are flowing, "it is running"
			// can no longer be a status.
			http.Error(w, err.Error(), statusCodeForTreeError(err))
			return
		}
		defer stream.Close()
		w.Header().Set("Content-Type", TreeMediaType)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		// A walk that fails part way ends the response rather than appending
		// anything to it. The body is a tar; a sentence in the middle of one is
		// not a diagnostic, it is a corrupt archive. The reader learns of the
		// failure as an unexpected EOF, which is what an incomplete tar is.
		_, _ = io.Copy(w, stream)
	})
}

func (s *sandboxService) importSandboxTreeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.authorizeTree(r, ScopeSandboxWrite); err != nil {
			http.Error(w, err.Error(), statusCodeForTreeError(err))
			return
		}
		if err := s.runtime.ImportTree(r.Context(), chi.URLParam(r, "sandboxId"), r.Body); err != nil {
			http.Error(w, err.Error(), statusCodeForTreeError(err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func (s *sandboxService) authorizeTree(r *http.Request, scope string) error {
	if err := s.authorize(chi.URLParam(r, "projectId"), chi.URLParam(r, "poolId")); err != nil {
		return err
	}
	claims, ok := SignedTokenClaimsFromContext(r.Context())
	if !ok {
		return newStatusError(http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized))
	}
	if !claims.HasScope(scope) {
		return newStatusError(http.StatusForbidden, http.StatusText(http.StatusForbidden))
	}
	return nil
}

func statusCodeForTreeError(err error) int {
	var statusErr interface{ StatusCode() int }
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode()
	}
	switch {
	case errors.Is(err, sandboxruntime.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, sandboxruntime.ErrTreeExists),
		errors.Is(err, sandboxruntime.ErrSandboxRunning),
		errors.Is(err, sandboxruntime.ErrArchived):
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}
