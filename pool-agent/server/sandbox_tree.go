package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/discobox-ai/discobox/pkg/imagecache"
	"github.com/discobox-ai/discobox/pool-agent/assetcache"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// TreeMediaType is what a sandbox's durable tree travels as: a plain tar, with
// no compression and no framing of its own.
const TreeMediaType = "application/x-tar"

// TreeBundleParam carries, once for each, the bundles an export's sandbox is
// pinned to, as <kind>:<name>@<digest>: the pins a create would carry, whose
// platform's agent the tree is read with.
const TreeBundleParam = "bundle"

// registerSandboxTreeRoutes exposes a sandbox's durable tree for export and
// restore (ADR 0123).
//
// Hand-wired beside the git routes, and for the same reason: the body is an
// opaque stream of unbounded length, which the generated contract has no way to
// describe and no reason to buffer.
//
// Neither route is wrapped in onUse, unlike the git ones. Everything about
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
		// The pins come from the control plane, which owns them; an archived
		// sandbox has no container here to read them from (ADR 0129 §1).
		pins, err := treeBundlePins(r.URL.Query()[TreeBundleParam])
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		stream, err := s.runtime.ExportTree(r.Context(), chi.URLParam(r, "sandboxId"), pins)
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
		// not a diagnostic, it is a corrupt archive.
		//
		// Ending it means aborting the connection. A handler that just returned
		// would have net/http finish the chunked body cleanly, and the reader
		// would see a short archive end the way a whole one does. The archive's
		// missing SHA256SUMS would still refuse it at the end; the abort is what
		// makes the failure arrive when it happens.
		if _, err := io.Copy(w, stream); err != nil {
			panic(http.ErrAbortHandler)
		}
	})
}

// treeBundlePins parses each <kind>:<name>@<digest> an export names.
func treeBundlePins(values []string) ([]assetcache.Pin, error) {
	pins := make([]assetcache.Pin, 0, len(values))
	for _, value := range values {
		kind, rest, ok := strings.Cut(value, ":")
		name, digest, ok2 := strings.Cut(rest, "@")
		pin := assetcache.Pin{Kind: assetcache.Kind(kind), Name: name, Digest: digest}
		if !ok || !ok2 || (pin.Kind != assetcache.KindPlatform && pin.Kind != assetcache.KindHarness) || name == "" || !imagecache.IsDigest(digest) {
			return nil, fmt.Errorf("bundle %q is not <platform|harness>:<name>@sha256:<digest>", value)
		}
		pins = append(pins, pin)
	}
	return pins, nil
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
		errors.Is(err, sandboxruntime.ErrArchived),
		errors.Is(err, sandboxruntime.ErrExportInProgress):
		return http.StatusConflict
	case errors.Is(err, sandboxruntime.ErrImageUnavailable):
		return http.StatusUnprocessableEntity
	}
	return http.StatusInternalServerError
}
