package server

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"

	services "github.com/discobox-ai/discobox/server/internal/services"
)

// registerPoolPprofRoutes exposes a profile of one of a pool's processes,
// fetched through the pool's agent (ADR 26-11-24-418 §3).
//
// Hand-wired beside the console, logs and audit recordings, for the transfer
// routes' reason: a profile is opaque bytes, and a CPU profile or trace answers
// only after the seconds it samples, which the generated scaffold would hold
// and buffer. It is a person's, as the console is: a discobox's role reaches no
// pool route but the pool's own record.
func registerPoolPprofRoutes(router chi.Router, service services.PoolService) {
	router.Method(http.MethodGet, "/projects/{projectId}/pools/{poolId}/pprof/{process}/{profile}", poolPprofHandler(service))
}

func poolPprofHandler(service services.PoolService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if service == nil {
			writeSandboxAgentProxyError(w, http.StatusServiceUnavailable, "pool service is not configured")
			return
		}
		process := chi.URLParam(r, "process")
		switch process {
		case "agent", "proxy", "mediator":
		default:
			writeSandboxAgentProxyError(w, http.StatusBadRequest, "process must be agent, proxy or mediator")
			return
		}
		seconds := 0
		if raw := strings.TrimSpace(r.URL.Query().Get("seconds")); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value <= 0 {
				writeSandboxAgentProxyError(w, http.StatusBadRequest, "seconds must be a positive whole number")
				return
			}
			seconds = value
		}
		profiled, err := service.OpenPoolPprof(r.Context(), chi.URLParam(r, "projectId"), chi.URLParam(r, "poolId"), process, chi.URLParam(r, "profile"), seconds)
		if err != nil {
			writeSandboxAgentProxyError(w, statusCodeForProxyError(err), err.Error())
			return
		}
		// The body holds a pool-agent transport lease; a client that goes away
		// while a profile is sampled releases it rather than holding it to the
		// end of the sampling.
		var closeOnce sync.Once
		closeBody := func() { closeOnce.Do(func() { _ = profiled.Body.Close() }) }
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

		contentType := profiled.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", "attachment")
		w.WriteHeader(http.StatusOK)
		// A failure part way through aborts the connection: a truncated
		// profile finished cleanly would read as a whole one.
		if _, err := io.Copy(w, profiled.Body); err != nil {
			panic(http.ErrAbortHandler)
		}
	})
}
