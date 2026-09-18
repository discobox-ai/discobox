package server

import (
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/discobox-ai/discobox/auditid"
	"github.com/go-chi/chi/v5"

	services "github.com/discobox-ai/discobox/server/internal/services"
)

// httpAuditFormatHeader carries the spool format of a recorded artifact: raw
// bytes for a body, framed chunks for an upgraded stream. A client needs it to
// read a stream back into its two directions.
const httpAuditFormatHeader = "X-Discobox-Audit-Format"

// registerPoolHTTPAuditRoutes exposes the bodies and upgraded streams a pool's
// proxy recorded beside its HTTP audit rows (ADR 0130 §5).
//
// Hand-wired beside the list, which is in the contract, for the reason the
// sandbox export is: a body is an unbounded stream of opaque bytes that the
// generated scaffold would buffer. The pool is in the path because an audit
// row's ID is only unique within the pool that recorded it.
func registerPoolHTTPAuditRoutes(router chi.Router, service services.PoolService) {
	router.Method(http.MethodGet, "/api/projects/{projectId}/pools/{poolId}/audit/http/{exchangeId}/{artifact}", poolHTTPAuditArtifactHandler(service))
}

func poolHTTPAuditArtifactHandler(service services.PoolService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if service == nil {
			writeSandboxAgentProxyError(w, http.StatusServiceUnavailable, "pool service is not configured")
			return
		}
		id, err := auditid.ParseExchange(chi.URLParam(r, "exchangeId"))
		if err != nil {
			writeSandboxAgentProxyError(w, http.StatusBadRequest, err.Error())
			return
		}
		artifact := chi.URLParam(r, "artifact")
		switch artifact {
		case "request-body", "response-body", "stream":
		default:
			writeSandboxAgentProxyError(w, http.StatusBadRequest, "artifact must be request-body, response-body or stream")
			return
		}
		opened, err := service.OpenHTTPAuditArtifact(r.Context(), chi.URLParam(r, "projectId"), chi.URLParam(r, "poolId"),
			strings.TrimSpace(r.URL.Query().Get("sandboxId")), id, artifact)
		if err != nil {
			writeSandboxAgentProxyError(w, statusCodeForProxyError(err), err.Error())
			return
		}
		// The body holds a pool-agent transport lease; a client that goes away
		// mid read has to release it rather than leave it held until the copy
		// next writes.
		var closeOnce sync.Once
		closeBody := func() { closeOnce.Do(func() { _ = opened.Body.Close() }) }
		defer closeBody()
		copying := make(chan struct{})
		defer close(copying)
		go func() {
			select {
			case <-r.Context().Done():
				closeBody()
			case <-copying:
			}
		}()

		contentType := opened.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set(httpAuditFormatHeader, opened.Format)
		w.Header().Set("Cache-Control", "no-store")
		// What a proxied service answered is served back to whoever reads the
		// trail; it must never be rendered as this server's own page.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", "attachment")
		w.WriteHeader(http.StatusOK)
		// A failure part way through aborts the connection: a truncated body
		// finished cleanly would read as the whole recorded body.
		if _, err := io.Copy(w, opened.Body); err != nil {
			panic(http.ErrAbortHandler)
		}
	})
}
